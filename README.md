# URL Shortener

A serverless URL shortener on AWS, built with SAM. Go Lambdas behind CloudFront
+ an HTTP API with a custom domain over HTTPS, a DynamoDB table, CloudWatch
alarms + a synthetic canary, a two-tier read cache, and an optional WAF Web
ACL. See [PLANNING.md](PLANNING.md) for the step-by-step roadmap; the current
step guide is [STEP-5-GUIDE.md](STEP-5-GUIDE.md).

## Architecture

| Component | Role |
|---|---|
| `cmd/url-shortener` | `POST /shorten` — takes a URL, calls `key-generator`, writes `{key, url, created_at}` to DynamoDB with a conditional put + bounded retry so a collision never overwrites, returns the key |
| `cmd/url-redirect` | `GET /{key}` — `L1 (in-process) → L2 (Valkey) → DynamoDB`, filling both caches on the way out; `302` to the stored URL, `404` on miss. Runs in a private VPC subnet. |
| `cmd/key-generator` | Pure function: random 7-char base62 key from `crypto/rand`. No storage access, invoked directly (no API Gateway), IAM role has zero DynamoDB permissions |
| `cmd/canary` | Scheduled every minute; runs `POST /shorten` → `GET /{key}` against the public URL and emits a `UrlShortener/Canary/Success` metric |
| `UrlTable` | DynamoDB `SimpleTable`, partition key `key`, on-demand billing |
| `HttpApi` + `Certificate` | HTTP API with a custom domain; ACM cert, DNS-validated via Route 53, wired through SAM's `Domain` block |
| Alarms + SNS + Chatbot | HTTP API `5xx`-rate and canary alarms → SNS → Slack + backup email; the redirect function deploys via `Canary10Percent5Minutes` with these as the rollback gate |
| VPC + `DynamoDBEndpoint` | Private subnets, **no NAT** — the redirect Lambda reaches DynamoDB through a free gateway endpoint |
| **cache stack** (`cache/`) | ElastiCache Valkey — a **separate** CloudFormation stack, created and destroyed per session (see below) |

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

## Cache stack (Step 4)

The shared Valkey cache (L2) lives in **its own CloudFormation stack** under
`cache/`, driven by `make` targets. It is the first thing that bills at rest, so
it is created at the start of a work session and destroyed at the end.

```bash
make cache-up      # deploy the ElastiCache stack, print the endpoint  (~5–10 min)
make cache-status  # StackStatus, or "not deployed"
make cache-down    # delete it — run before ending the session
```

**Order:**

1. `sam deploy` — the main stack first. It creates the VPC and exports the
   subnet / security-group ids that the cache stack imports. The redirect
   function works immediately, running on **L1 + DynamoDB only** (no L2 yet).
2. `make cache-up` — deploys ElastiCache into those subnets and writes its
   endpoint to SSM at `/url-shortener/cache/endpoint`.
3. The redirect Lambda reads that SSM parameter at **cold start**, so L2 engages
   for containers that start after step 2 — a few minutes, or force it with
   `sam deploy` / a new function version.

If the cache stack is absent (never deployed, or `make cache-down`), the SSM
parameter doesn't exist, `l2` is disabled, and the service runs on L1 +
DynamoDB. Deleting it never breaks redirects.

## WAF (Step 5)

Off by default — it's the one piece of this stack that costs money for as
long as it's attached (~$5/mo for the ACL + ~$1/mo per rule). Two managed rule
groups (`AmazonIpReputationList`, `KnownBadInputsRuleSet`) plus a custom
rate-based rule: `POST /shorten` is blocked past 100 requests/5min from the
same IP.

**Turn it on:**

```bash
sam deploy --parameter-overrides EnableWaf=true
```

CloudFormation remembers this across future deploys — you don't need to keep
passing it every time, only when you want to flip it. `sam deploy
--parameter-overrides EnableWaf=false` turns it back off (a parameter flip,
not a teardown).

**Allow-list, for your own testing.** The rate limit would otherwise block
`scripts/loadtest.sh` itself — its default run sends 250 `POST /shorten`
calls in well under a minute. A `WAFv2::IPSet`, evaluated before the rate
limit with a terminating `Allow`, exempts specific CIDRs. Its content comes
from an SSM `StringList`, never hardcoded in the template:

```bash
aws ssm put-parameter --name /url-shortener/waf-allowed-ips \
  --type StringList --value "<your-ip>/32"
```

The `/32` suffix is required — `AWS::WAFv2::IPSet` rejects a bare IP with no
CIDR prefix and rolls the deploy back. To add more addresses later, overwrite
the parameter with the full comma-separated list (`"1.2.3.4/32,5.6.7.8/32"`)
and add `--overwrite`. This only takes effect on the **next** `sam deploy` —
unlike the redirect Lambda's own SSM reads at cold start, this value resolves
once, at deploy time, so updating SSM alone does nothing to the live `IPSet`.

**Known gap:** WAF only guards traffic that goes through CloudFront. The raw
`execute-api` origin URL is still public and unguarded — CloudFront needs it
as its origin, so it can't be disabled. Not fixed yet; see
[RESULTS.md](RESULTS.md) Step 5.

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
#    cache-control: public, max-age=60, s-maxage=300
```

`404` if the key was never issued.

## Tests

Each module is tested on its own:

```bash
cd cmd/key-generator && go test ./...
cd cmd/url-redirect  && go test ./...   # in-process + Valkey cache, via fakes and miniredis
cd cmd/url-shortener && go test ./...
cd cmd/canary        && go test ./...
```

All four at once, from this directory:

```bash
for d in cmd/key-generator cmd/url-redirect cmd/url-shortener cmd/canary; do
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
