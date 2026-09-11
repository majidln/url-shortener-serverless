package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeGetter stands in for the DynamoDB client. It returns whatever the test
// sets up, and records the input so the test can assert what we sent.
type fakeGetter struct {
	out *dynamodb.GetItemOutput
	err error
	got *dynamodb.GetItemInput
	//nolint:unused // calls counts invocations; handy when a case must not hit DynamoDB.
	calls int
}

func (f *fakeGetter) GetItem(
	_ context.Context,
	in *dynamodb.GetItemInput,
	_ ...func(*dynamodb.Options),
) (*dynamodb.GetItemOutput, error) {
	f.got = in
	f.calls++
	return f.out, f.err
}

// item builds a stored row the way url-shortener writes it.
func item(key, url string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		"key": &ddbtypes.AttributeValueMemberS{Value: key},
		"url": &ddbtypes.AttributeValueMemberS{Value: url},
	}
}

// requestFor builds the event API Gateway sends for GET /{key}.
func requestFor(key string) events.APIGatewayV2HTTPRequest {
	return events.APIGatewayV2HTTPRequest{
		PathParameters: map[string]string{"key": key},
	}
}

func TestRedirect(t *testing.T) {
	cases := []struct {
		name         string
		request      events.APIGatewayV2HTTPRequest
		fake         *fakeGetter
		wantStatus   int
		wantLocation string
	}{
		{
			name:       "no key in path",
			request:    events.APIGatewayV2HTTPRequest{},
			fake:       &fakeGetter{},
			wantStatus: 400,
		},
		{
			name:       "empty key in path",
			request:    requestFor(""),
			fake:       &fakeGetter{},
			wantStatus: 400,
		},
		{
			name:       "lookup fails",
			request:    requestFor("abc1234"),
			fake:       &fakeGetter{err: errors.New("dynamodb is having a day")},
			wantStatus: 500,
		},
		{
			// A miss is a *successful* GetItem with no item, not an error.
			name:       "key not found",
			request:    requestFor("abc1234"),
			fake:       &fakeGetter{out: &dynamodb.GetItemOutput{}},
			wantStatus: 404,
		},
		{
			name:    "row has no url attribute",
			request: requestFor("abc1234"),
			fake: &fakeGetter{out: &dynamodb.GetItemOutput{
				Item: map[string]ddbtypes.AttributeValue{
					"key": &ddbtypes.AttributeValueMemberS{Value: "abc1234"},
				},
			}},
			wantStatus: 500,
		},
		{
			name:    "url stored as the wrong type",
			request: requestFor("abc1234"),
			fake: &fakeGetter{out: &dynamodb.GetItemOutput{
				Item: map[string]ddbtypes.AttributeValue{
					"key": &ddbtypes.AttributeValueMemberS{Value: "abc1234"},
					"url": &ddbtypes.AttributeValueMemberN{Value: "42"},
				},
			}},
			wantStatus: 500,
		},
		{
			name:    "url stored empty",
			request: requestFor("abc1234"),
			fake: &fakeGetter{out: &dynamodb.GetItemOutput{
				Item: item("abc1234", ""),
			}},
			wantStatus: 500,
		},
		{
			name:    "redirects to the stored url",
			request: requestFor("abc1234"),
			fake: &fakeGetter{out: &dynamodb.GetItemOutput{
				Item: item("abc1234", "https://example.com/a?b=1"),
			}},
			wantStatus:   302,
			wantLocation: "https://example.com/a?b=1",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &handler{ddb: c.fake, table: "test-table"}

			resp, err := h.Redirect(context.Background(), c.request)
			if err != nil {
				t.Fatalf("Redirect() returned error: %v", err)
			}
			if resp.StatusCode != c.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, c.wantStatus)
			}
			if got := resp.Headers["Location"]; got != c.wantLocation {
				t.Errorf("Location = %q, want %q", got, c.wantLocation)
			}
		})
	}
}

// A redirect carries whatever Cache-Control the handler was configured with
// (set from env at cold start) — CloudFront and browsers are meant to cache
// it deliberately now, not the old hardcoded no-store.
func TestRedirectSendsConfiguredCacheControl(t *testing.T) {
	f := &fakeGetter{out: &dynamodb.GetItemOutput{
		Item: item("abc1234", "https://example.com/"),
	}}
	want := "public, max-age=60, s-maxage=300"
	h := &handler{ddb: f, table: "test-table", redirectCacheControl: want}

	resp, err := h.Redirect(context.Background(), requestFor("abc1234"))
	if err != nil {
		t.Fatalf("Redirect() returned error: %v", err)
	}
	if got := resp.Headers["Cache-Control"]; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
}

// A miss is also cacheable at the edge, briefly, to blunt a scan of random
// keys — separate from L1/L2/DynamoDB, which never cache a miss (see
// TestRedirectDoesNotCacheMisses).
func TestNotFoundSendsConfiguredCacheControl(t *testing.T) {
	f := &fakeGetter{out: &dynamodb.GetItemOutput{}}
	want := "public, max-age=30"
	h := &handler{ddb: f, table: "test-table", notFoundCacheControl: want}

	resp, err := h.Redirect(context.Background(), requestFor("abc1234"))
	if err != nil {
		t.Fatalf("Redirect() returned error: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Headers["Cache-Control"]; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
}

// Asserting the response alone would pass even if we looked up the wrong
// table, or sent a key we made up.
func TestRedirectQueriesTheRightRow(t *testing.T) {
	f := &fakeGetter{out: &dynamodb.GetItemOutput{
		Item: item("abc1234", "https://example.com/"),
	}}
	h := &handler{ddb: f, table: "test-table"}

	if _, err := h.Redirect(context.Background(), requestFor("abc1234")); err != nil {
		t.Fatalf("Redirect() returned error: %v", err)
	}

	if f.got == nil {
		t.Fatal("GetItem was never called")
	}
	if f.got.TableName == nil || *f.got.TableName != "test-table" {
		t.Errorf("TableName = %v, want %q", f.got.TableName, "test-table")
	}

	keyAttr, ok := f.got.Key["key"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		t.Fatalf("Key[\"key\"] = %T, want *AttributeValueMemberS", f.got.Key["key"])
	}
	if keyAttr.Value != "abc1234" {
		t.Errorf("looked up key %q, want %q", keyAttr.Value, "abc1234")
	}
}

// A missing key must be rejected before we spend a DynamoDB read on it.
func TestRedirectSkipsLookupWithoutKey(t *testing.T) {
	f := &fakeGetter{}
	h := &handler{ddb: f, table: "test-table"}

	if _, err := h.Redirect(context.Background(), requestFor("")); err != nil {
		t.Fatalf("Redirect() returned error: %v", err)
	}
	if f.calls != 0 {
		t.Errorf("GetItem called %d times, want 0", f.calls)
	}
}

// Layer 1: a second request for the same key is served from memory, no GetItem.
func TestRedirectServesSecondCallFromCache(t *testing.T) {
	f := &fakeGetter{out: &dynamodb.GetItemOutput{Item: item("abc1234", "https://example.com/x")}}
	h := &handler{ddb: f, table: "test-table", l1: newL1Cache(time.Minute, 10)}

	for i := 1; i <= 3; i++ {
		resp, err := h.Redirect(context.Background(), requestFor("abc1234"))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if resp.StatusCode != 302 || resp.Headers["Location"] != "https://example.com/x" {
			t.Fatalf("call %d: status %d, Location %q", i, resp.StatusCode, resp.Headers["Location"])
		}
	}
	if f.calls != 1 {
		t.Errorf("GetItem called %d times, want 1 (calls 2 and 3 served from cache)", f.calls)
	}
}

// Once the TTL passes, the next request goes back to DynamoDB.
func TestRedirectCacheExpiresBackToDynamo(t *testing.T) {
	f := &fakeGetter{out: &dynamodb.GetItemOutput{Item: item("abc1234", "https://example.com/x")}}
	c := newL1Cache(time.Minute, 10)
	now, advance := fixedClock(time.Unix(0, 0))
	c.now = now
	h := &handler{ddb: f, table: "test-table", l1: c}

	h.Redirect(context.Background(), requestFor("abc1234")) // miss -> GetItem, cached
	h.Redirect(context.Background(), requestFor("abc1234")) // hit
	if f.calls != 1 {
		t.Fatalf("before expiry: GetItem calls = %d, want 1", f.calls)
	}

	advance(61 * time.Second)
	h.Redirect(context.Background(), requestFor("abc1234")) // expired -> GetItem again
	if f.calls != 2 {
		t.Errorf("after expiry: GetItem calls = %d, want 2", f.calls)
	}
}

// A 404 must not be cached: if the row later appears, the next request finds it.
func TestRedirectDoesNotCacheMisses(t *testing.T) {
	f := &fakeGetter{out: &dynamodb.GetItemOutput{}} // no item -> 404
	h := &handler{ddb: f, table: "test-table", l1: newL1Cache(time.Minute, 10)}

	if resp, _ := h.Redirect(context.Background(), requestFor("abc1234")); resp.StatusCode != 404 {
		t.Fatalf("first call status = %d, want 404", resp.StatusCode)
	}

	f.out = &dynamodb.GetItemOutput{Item: item("abc1234", "https://example.com/late")}
	resp, _ := h.Redirect(context.Background(), requestFor("abc1234"))
	if resp.StatusCode != 302 || resp.Headers["Location"] != "https://example.com/late" {
		t.Errorf("second call = %d %q, want 302 https://example.com/late", resp.StatusCode, resp.Headers["Location"])
	}
	if f.calls != 2 {
		t.Errorf("GetItem calls = %d, want 2 (the 404 was not cached)", f.calls)
	}
}
