# URL Shortener

A serverless URL shortener on AWS, built with SAM. Three Go Lambdas behind an
HTTP API with a custom domain over HTTPS, backed by a single on-demand DynamoDB
table. See [PLANNING.md](PLANNING.md) for the step-by-step roadmap; the current
step guide is [STEP-2-GUIDE.md](STEP-2-GUIDE.md).

## Architecture

| Component | Role |
|---|---|
| `cmd/url-shortener` | `POST /shorten` — takes a URL, calls `key-generator`, writes `{key, url, created_at}` to DynamoDB with a conditional put + bounded retry so a collision never overwrites, returns the key |
| `cmd/url-redirect` | `GET /{key}` — looks the key up, returns `302` to the stored URL (`404` on miss) |
| `cmd/key-generator` | Pure function: random 7-char base62 key from `crypto/rand`. No storage access, invoked directly (no API Gateway), IAM role has zero DynamoDB permissions |
| `UrlTable` | DynamoDB `SimpleTable`, partition key `key`, on-demand billing |
| `HttpApi` + `Certificate` | HTTP API with a custom domain; ACM cert, DNS-validated via Route 53, wired through SAM's `Domain` block |

Each function under `cmd/` is its own Go module with its own `go.mod`.

Runtime: Go on `provided.al2023`, `arm64`.

## Requirements

- AWS CLI, configured with credentials
- [AWS SAM CLI](https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/serverless-sam-cli-install.html)
- Go 1.21+
- Docker (for `sam local`)
- A domain with a **Route 53 public hosted zone**, delegated from the registrar
  (`dig +short NS <domain> @1.1.1.1` must return the zone's `awsdns-*` servers)

## Build

```bash
make          # wraps `sam build`
```

## Deploy

The stack needs two parameters — the custom domain and its hosted zone id. They
live in `samconfig.toml` under `[default.deploy.parameters]` as
`parameter_overrides`, so a plain deploy is parameterless:

```bash
sam deploy
```

First time, or to (re)enter the parameters interactively:

```bash
sam deploy --guided
```

Defaults in `samconfig.toml`: stack `url-shortener`, region `us-east-1`. The
first deploy blocks while ACM validates the certificate via a DNS record it
writes into the hosted zone — a few minutes; up to ~30 if delegation is slow or
missing.

Outputs: `CustomDomainUrl` (the HTTPS URL to use) and `UrlShortenerApi` (the raw
`execute-api` URL, still live).

## Endpoints

Base URL: `https://<your-domain>` (the `CustomDomainUrl` output). The
`execute-api` URL works too.

### `POST /shorten`

Body: `{"url": "https://example.com/some/long/path"}`

```bash
curl -sX POST "https://<domain>/shorten" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/some/long/path"}'
# => {"key":"aB3xY7z"}
```

`400` if the body is missing, not JSON, or has no `url`.

### `GET /{key}`

```bash
curl -si "https://<domain>/aB3xY7z"
# => HTTP/2 302
#    location: https://example.com/some/long/path
#    cache-control: no-store
```

`404` if the key was never issued.

## Tests

Each module is tested on its own:

```bash
cd cmd/key-generator && go test ./...
cd cmd/url-redirect  && go test ./...
cd cmd/url-shortener && go test ./...
```

All three at once, from this directory:

```bash
for d in cmd/key-generator cmd/url-redirect cmd/url-shortener; do
  echo "=== $d ==="; (cd "$d" && go test ./...)
done
```

Useful flags: `go test -v ./...` (per-test output), `-cover`, `-run TestName`,
`-count=1` (skip the cache).

The `key-generator` tests run with no AWS mocking — that is the check that it
stays pure.

## Load test

`scripts/loadtest.sh` drives the create and redirect paths with `hey` and is the
frozen baseline for every roadmap step. Results in [RESULTS.md](RESULTS.md).

```bash
BASE_URL=https://<domain> ./scripts/loadtest.sh 100
```
