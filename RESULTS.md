# Results

Load: `scripts/loadtest.sh` + `hey`. Region `us-east-1`, Go on `provided.al2023`
arm64, DynamoDB on-demand, HTTP API.

The warm section of `loadtest.sh` (seed + create burst + redirect burst) is the
frozen baseline — later steps re-run it unchanged and add a row here.

---

## Step 1 — baseline

**Environment**

- Stack deleted and redeployed clean before this run; collision handling
  (conditional `PutItem` + bounded retry) is live.
- **Account Lambda concurrency limit is 10** (unlifted default,
  `aws lambda get-account-settings`). Load is kept under it: `-c 5` for create
  (each create holds 2 slots — shortener + its synchronous call to
  key-generator), `-c 10` for redirect. See the throttling finding below.
- ~321 test rows left in the table after the run. Free at rest; left in place.

### Warm (concurrency under the ceiling)

| Path | n / conc | p50 | p90 | p95 | p99 | avg | status |
|---|---|---|---|---|---|---|---|
| `POST /shorten` | 200 / 5 | 149 ms | 165 ms | 204 ms | 2258 ms | 193 ms | 200× `200` |
| `GET /{code}`   | 2000 / 10 | 125 ms | 138 ms | 142 ms | 158 ms | 134 ms | 2000× `302` |

- `GET` throughput ~72 rps — capped by `-c 10`, not the service.
- `POST` p99 (2.26 s) is a handful of cold containers in the opening requests;
  p95 (204 ms) is the honest warm tail.
- Zero throttles on all three functions during this run.

### Cold (fresh deploy)

| Path | n / conc | p50 | first request | status | note |
|---|---|---|---|---|---|
| `GET /{code}` | 20 / 1 | 134 ms | **2093 ms** | 20× `302` | function never invoked before — a true cold start |
| `POST /shorten` | 20 / 1 | 161 ms | 402 ms | 20× `200` | path was warmed by seeding first — indicative only |

The redirect cold start is ~**1.9 s on the very first request** (`resp_wait`
1.87 s), then straight back to warm p50. Higher than the create cold bump seen
here only because create was pre-warmed; the redirect handler also builds the
AWS SDK client on first init. One request per idle period — not worth engineering
around at Step 1, but a candidate for provisioned concurrency / the Step 4 cache.

### key-generator hop cost (server-side, CloudWatch `Duration`)

| Function | avg Duration | does |
|---|---|---|
| `key-generator` | **2.0 ms** | `crypto/rand` + base62 — the actual work |
| `url-redirect`  | 15.5 ms | one DynamoDB `GetItem` (comparison baseline) |
| `url-shortener` | 44.6 ms | sync `key-generator` invoke + DynamoDB `PutItem` |

**The synchronous invoke to `key-generator` costs ≈ 29 ms server-side for ≈ 2 ms
of real work** over this window (which includes the fresh-deploy cold starts; a
warm-only window puts it closer to ~15 ms). Either way it is a small slice of the
149 ms end-to-end create p50, which is dominated by API Gateway + TLS + network.
This is the number the "separate Lambda for the key" decision trades against.

---

## Findings

### 1. Redirect path throttles above 10 concurrent

`GET /{code}` at `-c 50`:

- `500` × `302`, `1500` × `503`
- CloudWatch `Throttles` on `url-redirect` = **827** in the burst window

Cause: the account's Lambda concurrency limit of 10. API Gateway turns a Lambda
`429` into a `503`. Not a code defect — but real under load until a Service
Quotas increase ("Concurrent executions" → 1000). The `-c 10` numbers above are
the clean, like-for-like series to carry forward.

### 2. Create path saturates the ceiling at low concurrency

Each `POST /shorten` occupies **two** concurrency slots for the duration of the
synchronous key-generator invoke, so `-c 10` alone would saturate a limit-10
account. The frozen baseline runs create at `-c 5`, where it is clean:
`200/200`, zero throttles. A bounded retry with backoff on the key-generator
`Invoke` (for `TooManyRequestsException`) would make the create path resilient to
transient throttles regardless of the account limit — noted, not done at Step 1.

### 3. Collision handling adds no measurable latency

Warm p50 with the conditional write + retry in place (create 149 ms, redirect
125 ms) is within noise of the earlier plain-`PutItem` run (143 ms / 127 ms).
The uniqueness guarantee is effectively free on the happy path.
