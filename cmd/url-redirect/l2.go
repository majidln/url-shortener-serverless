package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/redis/go-redis/v9"
)

// errL2Miss means the key is not in the shared cache. Any other error from Get
// is a transport/timeout problem — the handler treats both the same way (fall
// through to DynamoDB), so a missing or unreachable cache stack is harmless.
var errL2Miss = errors.New("l2: miss")

// l2Cache is the shared (Valkey) tier.
type l2Cache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, url string)
}

type redisL2 struct {
	rdb *redis.Client
	ttl time.Duration
}

// newRedisL2 builds a client for host:port. It does not connect here — the
// first Get/Set does, bounded by the timeouts below so a dead cache costs a
// redirect ~75 ms once, not forever.
func newRedisL2(addr string, ttl time.Duration) *redisL2 {
	return &redisL2{
		rdb: redis.NewClient(&redis.Options{
			Addr:                  addr,
			DialTimeout:           75 * time.Millisecond,
			ReadTimeout:           75 * time.Millisecond,
			WriteTimeout:          75 * time.Millisecond,
			MaxRetries:            0,
			ContextTimeoutEnabled: true,
		}),
		ttl: ttl,
	}
}

func (r *redisL2) Get(ctx context.Context, key string) (string, error) {
	v, err := r.rdb.Get(ctx, key).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return "", errL2Miss
	case err != nil:
		return "", err
	default:
		return v, nil
	}
}

// Set is best-effort — a failed write just means the next request re-reads the
// origin and tries again.
func (r *redisL2) Set(ctx context.Context, key, url string) {
	if err := r.rdb.Set(ctx, key, url, r.ttl).Err(); err != nil {
		slog.Warn("l2 set failed", "error", err, "key", key)
	}
}

// ssmGetter is the slice of the SSM client newL2 uses — small so tests can fake it.
type ssmGetter interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// newL2 resolves the cache endpoint from SSM (written by the cache stack) and
// returns an l2Cache, or nil if there is no usable endpoint — missing
// parameter, empty value, or any read error. nil means L2 is off and the
// service runs on L1 + DynamoDB alone.
func newL2(ctx context.Context, ssmc ssmGetter, paramName string, ttl time.Duration) l2Cache {
	// 3s: the first call through a fresh interface endpoint (DNS + TLS + SigV4 +
	// GetParameter) occasionally runs long on a cold start.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := ssmc.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(paramName)})
	if err != nil {
		slog.Warn("cache endpoint not available; L2 disabled", "param", paramName, "error", err)
		return nil
	}

	addr := aws.ToString(out.Parameter.Value)
	if addr == "" {
		slog.Warn("cache endpoint is empty; L2 disabled", "param", paramName)
		return nil
	}

	slog.Info("L2 cache enabled", "addr", addr, "ttl", ttl)
	return newRedisL2(addr, ttl)
}

// newL2FromSSM is the production wiring: build the real SSM client and take the
// parameter name from the environment.
func newL2FromSSM(ctx context.Context, cfg aws.Config, ttl time.Duration) l2Cache {
	name := os.Getenv("CACHE_ENDPOINT_PARAM")
	if name == "" {
		name = "/url-shortener/cache/endpoint"
	}
	return newL2(ctx, ssm.NewFromConfig(cfg), name, ttl)
}
