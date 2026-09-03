# URL Shortener

A serverless URL shortener on AWS, built with SAM. Three Go Lambdas behind an
HTTP API, backed by a single on-demand DynamoDB table. See
[PLANNING.md](PLANNING.md) for the step-by-step roadmap and
[STEP-1-GUIDE.md](STEP-1-GUIDE.md) for the current step.

## Architecture

| Component | Role |
|---|---|
| `cmd/url-shortener` | `POST /shorten` — takes a URL, calls `key-generator`, writes `{key, url, created_at}` to DynamoDB, returns the key |
| `cmd/url-redirect` | `GET /{key}` — looks the key up, returns `302` to the stored URL (`404` on miss) |
| `cmd/key-generator` | Pure function: random 7-char base62 key from `crypto/rand`. No storage access, invoked directly (no API Gateway), IAM role has zero DynamoDB permissions |
| `UrlTable` | DynamoDB `SimpleTable`, partition key `key`, on-demand billing |

Each function under `cmd/` is its own Go module with its own `go.mod`.

Runtime: Go on `provided.al2023`, `arm64`.

## Requirements

- AWS CLI, configured with credentials
- [AWS SAM CLI](https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/serverless-sam-cli-install.html)
- Go 1.21+
- Docker (for `sam local`)

## Build

```bash
make          # wraps `sam build`
```

## Deploy

First time (prompts for stack name, region, etc. — answers are saved to `samconfig.toml`):

```bash
sam deploy --guided
```

Afterwards:

```bash
sam deploy
```

Defaults in `samconfig.toml`: stack `url-shortener`, region `us-east-1`. The API
base URL is printed as the `UrlShortenerApi` stack output.

## Endpoints

Base URL: `https://<api-id>.execute-api.us-east-1.amazonaws.com/<stage>`

### `POST /shorten`

Body: `{"url": "https://example.com/some/long/path"}`

```bash
curl -sX POST "$BASE_URL/shorten" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/some/long/path"}'
# => {"key":"aB3xY7z"}
```

`400` if the body is missing, not JSON, or has no `url`.

### `GET /{key}`

```bash
curl -si "$BASE_URL/aB3xY7z"
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
