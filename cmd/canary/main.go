package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

const (
	namespace  = "UrlShortener/Canary"
	metricName = "Success"
)

type publisher interface {
	PutMetricData(context.Context, *cloudwatch.PutMetricDataInput, ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

type canary struct {
	http    *http.Client
	cw      publisher
	baseURL string
}

// probe creates a short URL then follows it, asserting the 302 round-trips.
func (c *canary) probe(ctx context.Context) (bool, string) {
	res, err := c.do(ctx, http.MethodPost, "/shorten",
		strings.NewReader(`{"url":"https://example.com/canary"}`))
	if err != nil {
		return false, "create failed: " + err.Error()
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("create status %d", res.StatusCode)
	}
	var created struct {
		Key string `json:"key"`
	}
	if json.NewDecoder(res.Body).Decode(&created); created.Key == "" {
		return false, "create response had no key"
	}

	rr, err := c.do(ctx, http.MethodGet, "/"+created.Key, nil)
	if err != nil {
		return false, "redirect failed: " + err.Error()
	}
	defer rr.Body.Close()
	if rr.StatusCode != http.StatusFound {
		return false, fmt.Sprintf("redirect status %d, want 302", rr.StatusCode)
	}
	if rr.Header.Get("Location") == "" {
		return false, "redirect had no Location"
	}
	return true, "ok"
}

func (c *canary) do(ctx context.Context, method, path string, body *strings.Reader) (*http.Response, error) {
	var r *http.Request
	var err error
	if body == nil {
		r, err = http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	} else {
		r, err = http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
		r.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		return nil, err
	}
	return c.http.Do(r)
}

func (c *canary) run(ctx context.Context) error {
	start := time.Now()
	ok, detail := c.probe(ctx)
	log := slog.With("ok", ok, "detail", detail, "elapsed_ms", time.Since(start).Milliseconds())
	if ok {
		log.Info("canary passed")
	} else {
		log.Error("canary failed")
	}

	value := 0.0
	if ok {
		value = 1.0
	}
	_, err := c.cw.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(namespace),
		MetricData: []cwtypes.MetricDatum{{
			MetricName: aws.String(metricName),
			Value:      aws.Float64(value),
			Unit:       cwtypes.StandardUnitNone,
			Dimensions: []cwtypes.Dimension{{Name: aws.String("Probe"), Value: aws.String("redirect")}},
		}},
	})
	if err != nil {
		return fmt.Errorf("put metric: %w", err) // don't fail silently
	}
	return nil
}

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	c := &canary{
		http: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		cw:      cloudwatch.NewFromConfig(cfg),
		baseURL: strings.TrimRight(os.Getenv("PROBE_BASE_URL"), "/"),
	}
	lambda.Start(c.run)
}