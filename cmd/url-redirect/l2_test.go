package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// fakeL2 stands in for the shared cache. Get returns whatever the test sets;
// Set records what it was handed.
type fakeL2 struct {
	getURL  string
	getErr  error
	getKeys []string
	setKV   map[string]string
}

func newFakeL2() *fakeL2 { return &fakeL2{setKV: map[string]string{}} }

func (f *fakeL2) Get(_ context.Context, key string) (string, error) {
	f.getKeys = append(f.getKeys, key)
	return f.getURL, f.getErr
}

func (f *fakeL2) Set(_ context.Context, key, url string) { f.setKV[key] = url }

func foundOutput(url string) *dynamodb.GetItemOutput {
	return &dynamodb.GetItemOutput{Item: item("abc1234", url)}
}

func TestRedirectL1HitSkipsEverything(t *testing.T) {
	f := &fakeGetter{}
	l2 := newFakeL2()
	c := newL1Cache(time.Minute, 10)
	c.Put("abc1234", "https://example.com/l1")
	h := &handler{ddb: f, table: "t", l1: c, l2: l2}

	resp, _ := h.Redirect(context.Background(), requestFor("abc1234"))
	if resp.Headers["Location"] != "https://example.com/l1" {
		t.Fatalf("Location = %q", resp.Headers["Location"])
	}
	if f.calls != 0 {
		t.Errorf("GetItem called %d times, want 0", f.calls)
	}
	if len(l2.getKeys) != 0 {
		t.Errorf("L2 consulted %d times, want 0", len(l2.getKeys))
	}
}

func TestRedirectL2HitPopulatesL1(t *testing.T) {
	f := &fakeGetter{}
	l2 := newFakeL2()
	l2.getURL = "https://example.com/l2"
	c := newL1Cache(time.Minute, 10)
	h := &handler{ddb: f, table: "t", l1: c, l2: l2}

	resp, _ := h.Redirect(context.Background(), requestFor("abc1234"))
	if resp.StatusCode != 302 || resp.Headers["Location"] != "https://example.com/l2" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Headers["Location"])
	}
	if resp.Headers["Cache-Control"] != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on the L2 path too", resp.Headers["Cache-Control"])
	}
	if f.calls != 0 {
		t.Errorf("GetItem called %d times, want 0 (L2 hit)", f.calls)
	}
	if got, ok := c.Get("abc1234"); !ok || got != "https://example.com/l2" {
		t.Errorf("L1 not populated from L2 hit: (%q, %v)", got, ok)
	}
}

func TestRedirectL2MissFallsToDynamoAndFillsBothTiers(t *testing.T) {
	f := &fakeGetter{out: foundOutput("https://example.com/origin")}
	l2 := newFakeL2()
	l2.getErr = errL2Miss
	c := newL1Cache(time.Minute, 10)
	h := &handler{ddb: f, table: "t", l1: c, l2: l2}

	resp, _ := h.Redirect(context.Background(), requestFor("abc1234"))
	if resp.Headers["Location"] != "https://example.com/origin" {
		t.Fatalf("Location = %q", resp.Headers["Location"])
	}
	if f.calls != 1 {
		t.Errorf("GetItem calls = %d, want 1", f.calls)
	}
	if l2.setKV["abc1234"] != "https://example.com/origin" {
		t.Errorf("L2 not written back: %v", l2.setKV)
	}
	if got, ok := c.Get("abc1234"); !ok || got != "https://example.com/origin" {
		t.Errorf("L1 not written back: (%q, %v)", got, ok)
	}
}

// The cache-stack-is-gone path: L2 errors on every call, the service must still
// serve from DynamoDB.
func TestRedirectSurvivesL2Errors(t *testing.T) {
	f := &fakeGetter{out: foundOutput("https://example.com/origin")}
	l2 := newFakeL2()
	l2.getErr = errors.New("dial tcp: i/o timeout")
	h := &handler{ddb: f, table: "t", l1: newL1Cache(time.Minute, 10), l2: l2}

	resp, err := h.Redirect(context.Background(), requestFor("abc1234"))
	if err != nil {
		t.Fatalf("Redirect() returned error: %v", err)
	}
	if resp.StatusCode != 302 || resp.Headers["Location"] != "https://example.com/origin" {
		t.Errorf("got %d %q, want 302 origin", resp.StatusCode, resp.Headers["Location"])
	}
	if f.calls != 1 {
		t.Errorf("GetItem calls = %d, want 1", f.calls)
	}
}

// redisL2 against an in-process Redis: the redis.Nil -> errL2Miss mapping is
// the bit that breaks silently if wrong.
func TestRedisL2(t *testing.T) {
	mr := miniredis.RunT(t)
	l2 := newRedisL2(mr.Addr(), time.Minute)
	ctx := context.Background()

	if _, err := l2.Get(ctx, "nope"); !errors.Is(err, errL2Miss) {
		t.Fatalf("Get(missing) err = %v, want errL2Miss", err)
	}

	l2.Set(ctx, "k", "https://example.com/x")
	got, err := l2.Get(ctx, "k")
	if err != nil || got != "https://example.com/x" {
		t.Fatalf("Get after Set = (%q, %v)", got, err)
	}

	if ttl := mr.TTL("k"); ttl <= 0 || ttl > time.Minute {
		t.Errorf("TTL on k = %v, want (0, 1m]", ttl)
	}
}

// fakeSSM stands in for the SSM client in newL2.
type fakeSSM struct {
	out *ssm.GetParameterOutput
	err error
}

func (f *fakeSSM) GetParameter(
	_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options),
) (*ssm.GetParameterOutput, error) {
	return f.out, f.err
}

func paramOut(v string) *ssm.GetParameterOutput {
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}
}

func TestNewL2(t *testing.T) {
	cases := []struct {
		name   string
		ssm    *fakeSSM
		wantL2 bool
	}{
		{"valid endpoint enables L2", &fakeSSM{out: paramOut("cache.example:6379")}, true},
		{"parameter not found disables L2", &fakeSSM{err: &ssmtypes.ParameterNotFound{}}, false},
		{"read error disables L2", &fakeSSM{err: errors.New("throttled")}, false},
		{"empty value disables L2", &fakeSSM{out: paramOut("")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := newL2(context.Background(), c.ssm, "/url-shortener/cache/endpoint", time.Minute)
			if (got != nil) != c.wantL2 {
				t.Fatalf("newL2() = %v, want non-nil: %v", got, c.wantL2)
			}
		})
	}
}
