package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// What this Lambda needs from DynamoDB. Small on purpose: a test fake
// only has to implement what we actually call.
type dynamoGetter interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

type handler struct {
	ddb   dynamoGetter
	table string
	l1    *l1Cache // in-process, per execution environment. nil disables it.
	l2    l2Cache  // shared Valkey. nil = cache stack absent, run on l1 + DynamoDB.
}

func errorResponse(status int, message string) events.APIGatewayV2HTTPResponse {
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:       message,
	}
}

func redirect(url string) events.APIGatewayV2HTTPResponse {
	return events.APIGatewayV2HTTPResponse{
		StatusCode: 302,
		Headers: map[string]string{
			"Location": url,
			// Keep the redirect out of downstream caches while the mapping can
			// still change. Our own tiers' staleness is bounded by their TTL.
			"Cache-Control": "no-store",
		},
	}
}

func (h *handler) Redirect(
	ctx context.Context,
	request events.APIGatewayV2HTTPRequest,
) (events.APIGatewayV2HTTPResponse, error) {
	// Tags every line below, so one request is greppable end to end.
	log := slog.With("request_id", request.RequestContext.RequestID)

	// Comes from the {key} in the route path.
	key := request.PathParameters["key"]
	if key == "" {
		log.Warn("no key in path")
		return errorResponse(400, "key is required"), nil
	}

	// L1: in-process. A hit skips both Valkey and DynamoDB.
	if h.l1 != nil {
		if url, ok := h.l1.Get(key); ok {
			log.Info("redirecting", "key", key, "url", url, "cache", "l1")
			return redirect(url), nil
		}
	}

	// L2: shared Valkey. A hit skips DynamoDB. Any error (miss, timeout, cache
	// stack gone) just falls through.
	if h.l2 != nil {
		url, err := h.l2.Get(ctx, key)
		switch {
		case err == nil:
			if h.l1 != nil {
				h.l1.Put(key, url)
			}
			log.Info("redirecting", "key", key, "url", url, "cache", "l2")
			return redirect(url), nil
		case errors.Is(err, errL2Miss):
			// fall through to DynamoDB
		default:
			log.Warn("l2 get failed; falling through to DynamoDB", "error", err, "key", key)
		}
	}

	out, err := h.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(h.table),
		Key: map[string]ddbtypes.AttributeValue{
			"key": &ddbtypes.AttributeValueMemberS{Value: key},
		},
	})
	if err != nil {
		log.Error("looking up key", "error", err, "key", key, "table", h.table)
		return errorResponse(500, "internal error"), nil
	}

	// No item means the key was never issued, or was deleted. Misses are not
	// cached — negative caching is a Step 5 decision.
	if len(out.Item) == 0 {
		log.Info("key not found", "key", key)
		return errorResponse(404, "not found"), nil
	}

	// Stored as a string attribute by url-shortener.
	urlAttr, ok := out.Item["url"].(*ddbtypes.AttributeValueMemberS)
	if !ok || urlAttr.Value == "" {
		log.Error("row has no usable url attribute", "key", key)
		return errorResponse(500, "internal error"), nil
	}

	// Populate both tiers on the way out.
	if h.l2 != nil {
		h.l2.Set(ctx, key, urlAttr.Value)
	}
	if h.l1 != nil {
		h.l1.Put(key, urlAttr.Value)
	}

	log.Info("redirecting", "key", key, "url", urlAttr.Value, "cache", "miss")
	return redirect(urlAttr.Value), nil
}

// cacheConfigFromEnv reads CACHE_TTL_SECONDS (default 60) and CACHE_MAX_ENTRIES
// (default 5000). Either at or below zero disables the L1 cache. The same TTL is
// used for L2.
func cacheConfigFromEnv() (time.Duration, int) {
	ttl := 60 * time.Second
	if v, err := strconv.Atoi(os.Getenv("CACHE_TTL_SECONDS")); err == nil {
		ttl = time.Duration(v) * time.Second
	}
	max := 5000
	if v, err := strconv.Atoi(os.Getenv("CACHE_MAX_ENTRIES")); err == nil {
		max = v
	}
	return ttl, max
}

func main() {
	// Built here rather than in init() so tests never touch the AWS SDK.
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}

	// JSON so CloudWatch Insights can filter on the fields directly.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ttl, max := cacheConfigFromEnv()
	h := &handler{
		ddb:   dynamodb.NewFromConfig(cfg),
		table: os.Getenv("URL_TABLE_NAME"),
		l1:    newL1Cache(ttl, max),
		l2:    newL2FromSSM(context.Background(), cfg, ttl),
	}
	lambda.Start(h.Redirect)
}
