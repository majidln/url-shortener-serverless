package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
)

// fakePublisher stands in for the CloudWatch client: records the input, returns
// a canned error, counts calls.
type fakePublisher struct {
	err   error
	got   *cloudwatch.PutMetricDataInput
	calls int
}

func (f *fakePublisher) PutMetricData(
	_ context.Context,
	in *cloudwatch.PutMetricDataInput,
	_ ...func(*cloudwatch.Options),
) (*cloudwatch.PutMetricDataOutput, error) {
	f.got = in
	f.calls++
	return &cloudwatch.PutMetricDataOutput{}, f.err
}

// stubBackend answers POST /shorten and GET /{key} with configurable responses,
// standing in for the whole url-shortener service.
type stubBackend struct {
	createStatus     int
	createBody       string
	redirectStatus   int
	redirectLocation string
}

func (s *stubBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/shorten" {
		w.WriteHeader(s.createStatus)
		io.WriteString(w, s.createBody)
		return
	}
	// anything else is the redirect lookup
	if s.redirectLocation != "" {
		w.Header().Set("Location", s.redirectLocation)
	}
	w.WriteHeader(s.redirectStatus)
}

// newCanary wires a canary at a throwaway server running h, with an HTTP client
// that does NOT follow redirects (as in main).
func newCanary(t *testing.T, h http.Handler, pub publisher) *canary {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &canary{
		http: &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		cw:      pub,
		baseURL: srv.URL,
	}
}

// okBackend is a healthy service: create returns a key, redirect returns 302.
func okBackend() *stubBackend {
	return &stubBackend{
		createStatus:     http.StatusOK,
		createBody:       `{"key":"abc1234"}`,
		redirectStatus:   http.StatusFound,
		redirectLocation: "https://example.com/canary",
	}
}

func TestProbe(t *testing.T) {
	cases := []struct {
		name    string
		backend *stubBackend
		wantOK  bool
		wantSub string
	}{
		{
			name:    "healthy round trip",
			backend: okBackend(),
			wantOK:  true, wantSub: "ok",
		},
		{
			name:    "create returns non-200",
			backend: &stubBackend{createStatus: 500, redirectStatus: 302},
			wantOK:  false, wantSub: "create status 500",
		},
		{
			name:    "create body has no key",
			backend: &stubBackend{createStatus: 200, createBody: `{}`, redirectStatus: 302},
			wantOK:  false, wantSub: "no key",
		},
		{
			name:    "create body is not JSON",
			backend: &stubBackend{createStatus: 200, createBody: `nonsense`, redirectStatus: 302},
			wantOK:  false, wantSub: "no key",
		},
		{
			name: "redirect returns 200 instead of 302",
			backend: &stubBackend{
				createStatus: 200, createBody: `{"key":"abc1234"}`,
				redirectStatus: 200,
			},
			wantOK: false, wantSub: "redirect status 200, want 302",
		},
		{
			name: "redirect is 302 but has no Location",
			backend: &stubBackend{
				createStatus: 200, createBody: `{"key":"abc1234"}`,
				redirectStatus: 302, redirectLocation: "",
			},
			wantOK: false, wantSub: "no Location",
		},
		{
			name: "redirect throttled to 503",
			backend: &stubBackend{
				createStatus: 200, createBody: `{"key":"abc1234"}`,
				redirectStatus: 503,
			},
			wantOK: false, wantSub: "redirect status 503, want 302",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cn := newCanary(t, c.backend, &fakePublisher{})
			ok, detail := cn.probe(context.Background())
			if ok != c.wantOK {
				t.Fatalf("probe() ok = %v (%q), want %v", ok, detail, c.wantOK)
			}
			if !strings.Contains(detail, c.wantSub) {
				t.Errorf("detail = %q, want to contain %q", detail, c.wantSub)
			}
		})
	}
}

// A connection that never completes must fail cleanly, not hang or panic.
func TestProbeTransportError(t *testing.T) {
	cn := &canary{
		http:    &http.Client{Timeout: time.Second},
		cw:      &fakePublisher{},
		baseURL: "http://127.0.0.1:1", // nothing listens here
	}
	ok, detail := cn.probe(context.Background())
	if ok || !strings.HasPrefix(detail, "create failed:") {
		t.Fatalf("probe() = %v, %q; want false and a \"create failed:\" detail", ok, detail)
	}
}

func TestRun(t *testing.T) {
	t.Run("healthy probe publishes Success=1", func(t *testing.T) {
		pub := &fakePublisher{}
		cn := newCanary(t, okBackend(), pub)
		if err := cn.run(context.Background()); err != nil {
			t.Fatalf("run() returned error: %v", err)
		}
		assertMetric(t, pub, 1.0)
	})

	t.Run("failing probe publishes Success=0", func(t *testing.T) {
		pub := &fakePublisher{}
		cn := newCanary(t, &stubBackend{createStatus: 503, redirectStatus: 302}, pub)
		if err := cn.run(context.Background()); err != nil {
			t.Fatalf("run() returned error: %v", err)
		}
		assertMetric(t, pub, 0.0)
	})

	t.Run("PutMetricData failure is surfaced, not swallowed", func(t *testing.T) {
		pub := &fakePublisher{err: errors.New("cloudwatch is down")}
		cn := newCanary(t, okBackend(), pub)
		err := cn.run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "put metric") {
			t.Fatalf("run() error = %v, want it to wrap \"put metric\"", err)
		}
	})
}

// The metric identity is the contract the CanaryAlarm matches on — assert it,
// not just that some metric was sent.
func assertMetric(t *testing.T, pub *fakePublisher, wantValue float64) {
	t.Helper()
	if pub.calls != 1 {
		t.Fatalf("PutMetricData called %d times, want 1", pub.calls)
	}
	in := pub.got
	if aws.ToString(in.Namespace) != "UrlShortener/Canary" {
		t.Errorf("Namespace = %q, want %q", aws.ToString(in.Namespace), "UrlShortener/Canary")
	}
	if len(in.MetricData) != 1 {
		t.Fatalf("MetricData has %d entries, want 1", len(in.MetricData))
	}
	d := in.MetricData[0]
	if aws.ToString(d.MetricName) != "Success" {
		t.Errorf("MetricName = %q, want %q", aws.ToString(d.MetricName), "Success")
	}
	if aws.ToFloat64(d.Value) != wantValue {
		t.Errorf("Value = %v, want %v", aws.ToFloat64(d.Value), wantValue)
	}
	if len(d.Dimensions) != 1 ||
		aws.ToString(d.Dimensions[0].Name) != "Probe" ||
		aws.ToString(d.Dimensions[0].Value) != "redirect" {
		t.Errorf("Dimensions = %+v, want one {Probe: redirect}", d.Dimensions)
	}
}
