package main

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

const (
	testTable = "test-table"
	testARN   = "arn:aws:lambda:us-east-1:111111111111:function:key-generator"
)

// fakeKeygen stands in for the key-generator Lambda client. It records every
// input and returns, in order, a key from keys (the last entry repeats). Set
// out to return a canned InvokeOutput instead (FunctionError / bad payload
// cases); set err to fail the Invoke call itself.
type fakeKeygen struct {
	keys  []string
	out   *awslambda.InvokeOutput
	err   error
	got   []*awslambda.InvokeInput
	calls int
}

func (f *fakeKeygen) Invoke(
	_ context.Context,
	in *awslambda.InvokeInput,
	_ ...func(*awslambda.Options),
) (*awslambda.InvokeOutput, error) {
	f.got = append(f.got, in)
	i := f.calls
	f.calls++

	if f.err != nil {
		return nil, f.err
	}
	if f.out != nil {
		return f.out, nil
	}
	if i >= len(f.keys) {
		i = len(f.keys) - 1
	}
	return keygenOK(f.keys[i]), nil
}

// fakePutter stands in for the DynamoDB client. err (if set) fails every call;
// otherwise results is consumed one per call, the last entry repeating, and a
// nil entry means the write succeeded.
type fakePutter struct {
	err     error
	results []error
	got     []*dynamodb.PutItemInput
	calls   int
}

func (f *fakePutter) PutItem(
	_ context.Context,
	in *dynamodb.PutItemInput,
	_ ...func(*dynamodb.Options),
) (*dynamodb.PutItemOutput, error) {
	f.got = append(f.got, in)
	i := f.calls
	f.calls++

	if f.err != nil {
		return nil, f.err
	}
	if len(f.results) > 0 {
		if i >= len(f.results) {
			i = len(f.results) - 1
		}
		if err := f.results[i]; err != nil {
			return nil, err
		}
	}
	return &dynamodb.PutItemOutput{}, nil
}

// keygenOK builds the Invoke response the generator returns on success.
func keygenOK(key string) *awslambda.InvokeOutput {
	return &awslambda.InvokeOutput{Payload: []byte(`{"key":"` + key + `"}`)}
}

// keygenFuncErr builds an Invoke response where the Invoke succeeded but the
// generator's handler itself returned an error.
func keygenFuncErr() *awslambda.InvokeOutput {
	return &awslambda.InvokeOutput{
		FunctionError: aws.String("Unhandled"),
		Payload:       []byte(`{"errorMessage":"boom"}`),
	}
}

// collisionErr is what the SDK returns when the attribute_not_exists condition
// fails, i.e. the key is already taken.
func collisionErr() error {
	return &ddbtypes.ConditionalCheckFailedException{
		Message: aws.String("The conditional request failed"),
	}
}

// jsonBody builds the event API Gateway sends for POST /shorten with a raw
// (not base64) JSON body.
func jsonBody(s string) events.APIGatewayV2HTTPRequest {
	return events.APIGatewayV2HTTPRequest{Body: s}
}

func TestShorten(t *testing.T) {
	cases := []struct {
		name       string
		request    events.APIGatewayV2HTTPRequest
		keygen     *fakeKeygen
		putter     *fakePutter
		wantErr    bool // true => handler returns a non-nil error
		wantStatus int  // checked only when wantErr is false
		wantBody   string
		wantKeygen int // expected keygen.calls
		wantPut    int // expected putter.calls
	}{
		{
			name:       "body is not valid base64",
			request:    events.APIGatewayV2HTTPRequest{Body: "!!!not base64!!!", IsBase64Encoded: true},
			keygen:     &fakeKeygen{},
			putter:     &fakePutter{},
			wantStatus: 400,
		},
		{
			name:       "body is not valid JSON",
			request:    jsonBody("{"),
			keygen:     &fakeKeygen{},
			putter:     &fakePutter{},
			wantStatus: 400,
		},
		{
			name:       "url missing from body",
			request:    jsonBody(`{}`),
			keygen:     &fakeKeygen{},
			putter:     &fakePutter{},
			wantStatus: 400,
		},
		{
			name:       "url present but empty",
			request:    jsonBody(`{"url":""}`),
			keygen:     &fakeKeygen{},
			putter:     &fakePutter{},
			wantStatus: 400,
		},
		{
			name:       "key-generator invoke fails",
			request:    jsonBody(`{"url":"https://example.com/"}`),
			keygen:     &fakeKeygen{err: errors.New("lambda is having a day")},
			putter:     &fakePutter{},
			wantErr:    true,
			wantKeygen: 1,
		},
		{
			name:       "key-generator handler returned an error",
			request:    jsonBody(`{"url":"https://example.com/"}`),
			keygen:     &fakeKeygen{out: keygenFuncErr()},
			putter:     &fakePutter{},
			wantErr:    true,
			wantKeygen: 1,
		},
		{
			name:       "key-generator payload is not JSON",
			request:    jsonBody(`{"url":"https://example.com/"}`),
			keygen:     &fakeKeygen{out: &awslambda.InvokeOutput{Payload: []byte("not json")}},
			putter:     &fakePutter{},
			wantErr:    true,
			wantKeygen: 1,
		},
		{
			name:       "key-generator returned an empty key",
			request:    jsonBody(`{"url":"https://example.com/"}`),
			keygen:     &fakeKeygen{out: keygenOK("")},
			putter:     &fakePutter{},
			wantErr:    true,
			wantKeygen: 1,
		},
		{
			name:       "saving to dynamodb fails",
			request:    jsonBody(`{"url":"https://example.com/"}`),
			keygen:     &fakeKeygen{keys: []string{"abc1234"}},
			putter:     &fakePutter{err: errors.New("dynamodb is having a day")},
			wantErr:    true,
			wantKeygen: 1,
			wantPut:    1,
		},
		{
			name:       "one collision then success",
			request:    jsonBody(`{"url":"https://example.com/"}`),
			keygen:     &fakeKeygen{keys: []string{"taken00", "free123"}},
			putter:     &fakePutter{results: []error{collisionErr(), nil}},
			wantStatus: 200,
			wantBody:   `{"key":"free123"}`,
			wantKeygen: 2,
			wantPut:    2,
		},
		{
			name:       "collides every time, gives up at the bound",
			request:    jsonBody(`{"url":"https://example.com/"}`),
			keygen:     &fakeKeygen{keys: []string{"dup0000"}},
			putter:     &fakePutter{results: []error{collisionErr()}},
			wantErr:    true,
			wantKeygen: maxKeyAttempts,
			wantPut:    maxKeyAttempts,
		},
		{
			name:       "happy path returns the key",
			request:    jsonBody(`{"url":"https://example.com/a?b=1"}`),
			keygen:     &fakeKeygen{keys: []string{"abc1234"}},
			putter:     &fakePutter{},
			wantStatus: 200,
			wantBody:   `{"key":"abc1234"}`,
			wantKeygen: 1,
			wantPut:    1,
		},
		{
			name: "happy path with a base64-encoded body",
			request: events.APIGatewayV2HTTPRequest{
				Body:            base64.StdEncoding.EncodeToString([]byte(`{"url":"https://example.com/"}`)),
				IsBase64Encoded: true,
			},
			keygen:     &fakeKeygen{keys: []string{"xY7zAb0"}},
			putter:     &fakePutter{},
			wantStatus: 200,
			wantBody:   `{"key":"xY7zAb0"}`,
			wantKeygen: 1,
			wantPut:    1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &handler{
				keygen:          c.keygen,
				ddb:             c.putter,
				tableName:       testTable,
				keyGeneratorARN: testARN,
			}

			resp, err := h.Shorten(context.Background(), c.request)

			if c.wantErr {
				if err == nil {
					t.Fatalf("Shorten() = nil error, want an error")
				}
			} else {
				if err != nil {
					t.Fatalf("Shorten() returned error: %v", err)
				}
				if resp.StatusCode != c.wantStatus {
					t.Errorf("status = %d, want %d", resp.StatusCode, c.wantStatus)
				}
				if c.wantBody != "" && resp.Body != c.wantBody {
					t.Errorf("body = %q, want %q", resp.Body, c.wantBody)
				}
			}

			if c.keygen.calls != c.wantKeygen {
				t.Errorf("key-generator called %d times, want %d", c.keygen.calls, c.wantKeygen)
			}
			if c.putter.calls != c.wantPut {
				t.Errorf("PutItem called %d times, want %d", c.putter.calls, c.wantPut)
			}
		})
	}
}

// A bad request must be rejected before we spend a Lambda invoke or a write on it.
func TestShortenSkipsWorkOnBadInput(t *testing.T) {
	f := &fakeKeygen{}
	p := &fakePutter{}
	h := &handler{keygen: f, ddb: p, tableName: testTable, keyGeneratorARN: testARN}

	if _, err := h.Shorten(context.Background(), jsonBody(`{"url":""}`)); err != nil {
		t.Fatalf("Shorten() returned error: %v", err)
	}
	if f.calls != 0 {
		t.Errorf("key-generator called %d times, want 0", f.calls)
	}
	if p.calls != 0 {
		t.Errorf("PutItem called %d times, want 0", p.calls)
	}
}

// Asserting the response alone would pass even if we invoked the wrong function
// or sent it the wrong invocation type.
func TestShortenInvokesTheGeneratorCorrectly(t *testing.T) {
	f := &fakeKeygen{keys: []string{"abc1234"}}
	h := &handler{keygen: f, ddb: &fakePutter{}, tableName: testTable, keyGeneratorARN: testARN}

	if _, err := h.Shorten(context.Background(), jsonBody(`{"url":"https://example.com/"}`)); err != nil {
		t.Fatalf("Shorten() returned error: %v", err)
	}

	if len(f.got) != 1 {
		t.Fatalf("Invoke called %d times, want 1", len(f.got))
	}
	if f.got[0].FunctionName == nil || *f.got[0].FunctionName != testARN {
		t.Errorf("FunctionName = %v, want %q", f.got[0].FunctionName, testARN)
	}
	if f.got[0].InvocationType != types.InvocationTypeRequestResponse {
		t.Errorf("InvocationType = %q, want %q", f.got[0].InvocationType, types.InvocationTypeRequestResponse)
	}
}

// Asserting the response alone would pass even if we wrote to the wrong table,
// stored the wrong key, dropped the url, or forgot the uniqueness condition.
func TestShortenWritesTheRightRow(t *testing.T) {
	p := &fakePutter{}
	h := &handler{keygen: &fakeKeygen{keys: []string{"abc1234"}}, ddb: p, tableName: testTable, keyGeneratorARN: testARN}

	const url = "https://example.com/a?b=1"
	if _, err := h.Shorten(context.Background(), jsonBody(`{"url":"`+url+`"}`)); err != nil {
		t.Fatalf("Shorten() returned error: %v", err)
	}

	if len(p.got) != 1 {
		t.Fatalf("PutItem called %d times, want 1", len(p.got))
	}
	in := p.got[0]

	if in.TableName == nil || *in.TableName != testTable {
		t.Errorf("TableName = %v, want %q", in.TableName, testTable)
	}
	keyAttr, ok := in.Item["key"].(*ddbtypes.AttributeValueMemberS)
	if !ok || keyAttr.Value != "abc1234" {
		t.Errorf("Item[key] = %#v, want string %q", in.Item["key"], "abc1234")
	}
	urlAttr, ok := in.Item["url"].(*ddbtypes.AttributeValueMemberS)
	if !ok || urlAttr.Value != url {
		t.Errorf("Item[url] = %#v, want string %q", in.Item["url"], url)
	}
	if _, ok := in.Item["created_at"].(*ddbtypes.AttributeValueMemberS); !ok {
		t.Errorf("Item[created_at] = %#v, want a string attribute", in.Item["created_at"])
	}

	// The write must refuse to overwrite an existing key.
	if in.ConditionExpression == nil || *in.ConditionExpression != "attribute_not_exists(#k)" {
		t.Errorf("ConditionExpression = %v, want %q", in.ConditionExpression, "attribute_not_exists(#k)")
	}
	if in.ExpressionAttributeNames["#k"] != "key" {
		t.Errorf("ExpressionAttributeNames[#k] = %q, want %q", in.ExpressionAttributeNames["#k"], "key")
	}
}

// On a collision the handler must ask for a fresh key and store that one.
func TestShortenRegeneratesOnCollision(t *testing.T) {
	k := &fakeKeygen{keys: []string{"taken00", "taken00", "free123"}}
	p := &fakePutter{results: []error{collisionErr(), collisionErr(), nil}}
	h := &handler{keygen: k, ddb: p, tableName: testTable, keyGeneratorARN: testARN}

	resp, err := h.Shorten(context.Background(), jsonBody(`{"url":"https://example.com/"}`))
	if err != nil {
		t.Fatalf("Shorten() returned error: %v", err)
	}
	if resp.Body != `{"key":"free123"}` {
		t.Errorf("body = %q, want %q", resp.Body, `{"key":"free123"}`)
	}
	if k.calls != 3 || p.calls != 3 {
		t.Fatalf("keygen calls = %d, put calls = %d, want 3 and 3", k.calls, p.calls)
	}

	stored := p.got[2].Item["key"].(*ddbtypes.AttributeValueMemberS).Value
	if stored != "free123" {
		t.Errorf("stored key = %q, want %q", stored, "free123")
	}
}
