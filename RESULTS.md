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

---

## Step 2 — custom domain over HTTPS

`https://link123.cfd/<code>` in front of the `execute-api` URL: ACM certificate
(DNS-validated via Route 53), HTTP API custom domain, `A`/`AAAA` alias records.
Same frozen `loadtest.sh`, both endpoints run back to back.

| Path | endpoint | p50 | p95 | p99 | status |
|---|---|---|---|---|---|
| `POST /shorten` | `link123.cfd` | 154 ms | 204 ms | 1887 ms | 200× `200` |
| `POST /shorten` | `execute-api` | 150 ms | 179 ms | 406 ms | 200× `200` |
| `GET /{code}` | `link123.cfd` | 129 ms | 147 ms | 179 ms | 2000× `302` |
| `GET /{code}` | `execute-api` | 127 ms | 143 ms | 174 ms | 2000× `302` |

**The custom domain adds no measurable latency.** Redirect p50 differs by 2 ms —
noise. It's the same regional API Gateway endpoint; the alias record resolves to
the same IPs and `hey` reuses connections, so the extra DNS lookup is one-time.
The `POST` p99 gap (1887 vs 406 ms) is which run caught more cold containers, not
the domain.

### Notes

- **Post-deploy propagation blip.** For a minute or two after `sam deploy`
  finished, some `GET /{code}` requests returned API Gateway's `{"message":"Not
  Found"}` (a routing miss, not the redirect Lambda's 404) on both the new domain
  *and* `execute-api` — the redeployed stage and the new domain mapping settling.
  Cleared on its own; verify a few minutes after deploy, not immediately.
- **SAM/ACM friction.** SAM has no shorthand for ACM, so the certificate is a
  raw `AWS::CertificateManager::Certificate`. Its `DomainValidationOptions`
  `HostedZoneId` is what lets CloudFormation write the validation `CNAME` itself
  and block until issued — miss it and the stack hangs waiting on a record that
  never appears. The `Domain` block also has to be nested under the `HttpApi`
  `Properties` (not at resource level); put it in the wrong place and the deploy
  silently no-ops the custom domain.
- `http://link123.cfd` does not connect — API Gateway custom domains are
  HTTPS-only. The `http`→`https` redirect is a Step 5 (CloudFront) job.

---

## Step 3 — observability & safe deploys

What this step added:

- **Alarms** on the HTTP API `5xx` **rate** (`5xx ÷ Count`, floored below
  10 req/min) and on a **1/minute synthetic canary** that runs the real user
  path (`POST /shorten` → `GET /{key}`) against `https://link123.cfd` and emits
  a custom metric `UrlShortener/Canary/Success` (1/0).
- Both alarms → one **SNS topic** → Slack (AWS Chatbot) + a backup email.
- Redirect function on `AutoPublishAlias: live` +
  `DeploymentPreference: Canary10Percent5Minutes`, both alarms as the
  auto-rollback gate.
- Config (domain, hosted zone id, email, Slack ids) moved out of the repo into
  **SSM Parameter Store**; four **log groups** declared with 14-day retention.
- A CloudWatch **dashboard** (API / Lambda / DynamoDB / canary panels + alarm
  tiles) as the diagnosis surface.

### Warm baseline — frozen `loadtest.sh` via `link123.cfd`

| Path | n / conc | p50 | p95 | p99 | status |
|---|---|---|---|---|---|
| `POST /shorten` | 200 / 5 | 144 ms | 168 ms | 1957 ms | 200× `200` |
| `GET /{code}` | 2000 / 10 | 132 ms | 149 ms | 171 ms | 2000× `302` |

Redirect p50 across the three steps: **125 → 129 → 132 ms** — all noise. The
`AutoPublishAlias` alias indirection and the once-a-minute canary traffic add
**no measurable latency**; alias routing is resolved in the API Gateway
integration config, not per request. `POST` p99 (1957 ms) is the usual handful
of cold containers in the opening requests.

### Proof 2 — real outage, detected and recovered

Redirect broken by `aws lambda put-function-concurrency
--reserved-concurrent-executions 0` (every redirect throttles → API Gateway
`503`), ~1 req/s of synthetic traffic during the window, then
`delete-function-concurrency` to restore. Timings from CloudWatch alarm history.

| Transition | `canary-redirect` | `http-5xx-rate` |
|---|---|---|
| break → `ALARM` | **3m02s** | **3m18s** |
| restore → `OK` | **3m26s** | **4m42s** |

- Both alarms use `DatapointsToAlarm: 3` over 1-minute periods, so ~3 min to
  fire is the design floor — matched exactly. The `5xx` alarm reason recorded
  `[97.1%, 97.7%, 92.6%] > 5.0` — the `5xx ÷ Count` metric-math and the
  10-req/min floor both behaved as intended.
- `5xx` recovers slower than it fires (4m42s) because after the fix API Gateway
  needs a fresh clean minute published before non-breaching datapoints
  accumulate; the canary clears faster (fewer datapoints, one per minute).
- Each transition fired `AlarmActions` / `OKActions` → SNS → Slack + email
  (2 alert + 2 recovery messages per run).

### Finding: leftover manual test levers persist silently

The first proof attempt found `ReservedConcurrentExecutions: 0` **already set**
on the redirect function from an earlier manual test that was never cleaned up —
redirects had been fully down (225 throttles / 5 min, every request `503`) for
an unknown period. The `canary-redirect` alarm was correctly in `ALARM` the
whole time; the observability added in this step is exactly what surfaced it.
Lesson: CLI test levers (`put-function-concurrency`, env-var overrides on
`$LATEST`) don't show up in `sam deploy` diffs and must be reverted explicitly —
`update-function-configuration` on `$LATEST` also can't be used to break the
deployed service now that `AutoPublishAlias` pins traffic to a published
version.

### Not yet run

Proof 1 (force alarm state) is trivial and effectively covered by the transitions
above. Proof 3 (ship a deliberately broken redirect, watch CodeDeploy roll it
back inside the 5-minute bake) still pending — needs a redirect code change plus
load during the bake.

---

## Step 4 — caching the redirect lookup

Two tiers in front of DynamoDB, in `cmd/url-redirect`:

- **L1** — in-process TTL map, 60 s TTL, 5000 entries, one per execution
  environment.
- **L2** — shared ElastiCache **Valkey** (`cache.t4g.micro`, 1 node) in a
  private VPC subnet. Endpoint read from SSM at cold start; a missing/unreachable
  endpoint disables L2 silently.

Flow: `L1 → L2 → DynamoDB`, populating both tiers on the way out; any L2 error
falls straight through.

### Latency — frozen `loadtest.sh` (single hot key)

| Path | p50 | p95 | p99 |
|---|---|---|---|
| `POST /shorten` | 142 ms | 172 ms | 1828 ms |
| `GET /{code}` | 124 ms | 140 ms | 158 ms |

Redirect p50 across the four steps: **125 → 129 → 132 → 124 ms** — flat. The
cache buys **no latency**; the path is API Gateway + TLS + network bound and the
`GetItem` it removes is ~5 ms of that. Exactly what the guide predicted.

### Tier mix — needs a many-key workload

The frozen script hammers **one** key, so each container misses once (fills its
own L1) then serves `l1` forever; L2 is never consulted. A **many-key burst**
(3000 requests spread over 100 codes, `-P 8`):

| tier | count | share |
|---|---|---|
| `l1` (in-process) | 1995 | 66% |
| `l2` (Valkey) | 767 | 26% |
| `miss` (DynamoDB) | 241 | 8% |

Overall hit rate **92%**; only **8% of redirects reached DynamoDB**. Without L2,
those 767 `l2` hits would have been origin reads — **L2 cut DynamoDB reads
~76%** for this workload (~1008 → 241). Confirmed table-side:
`ConsumedReadCapacityUnits` over the burst ≈ 117 RCU ≈ 234 eventually-consistent
`GetItem`s, matching the `miss` count. Redirect Lambda `Duration` averaged
**~2.5 ms** during the burst (Step 3 was ~15 ms) — most requests skip the
DynamoDB SDK call entirely.

### The VPC — what it actually cost

1. `VpcConfig` on the redirect function → no route to the internet or AWS public
   endpoints.
2. **DynamoDB** — free **Gateway** endpoint, a route-table entry. Worked first
   try. Zero NAT.
3. **SSM** (handler reads the cache endpoint from Parameter Store at cold start)
   — needs an **Interface** endpoint, ~$7/mo per ENI. Gateway endpoints exist
   only for S3 and DynamoDB.
4. **An interface endpoint must be in every subnet the Lambda runs in.** First
   attempt put it in one of the two private subnets → ~1/3 of cold starts (ENI
   in the other subnet) still timed out on SSM and fell back to L1 + DynamoDB,
   silently. Fixed by placing it in both subnets (~$14/mo for the pair). The
   2 s SSM timeout was bumped to 3 s — the first call through a fresh endpoint
   occasionally runs long.
5. `aws ec2 describe-nat-gateways` → empty.

Reachability rule, recorded for later:

| From a VPC Lambda | via | cost |
|---|---|---|
| S3, DynamoDB | Gateway endpoint | free |
| SSM, Secrets Manager, STS, SNS… | Interface endpoint | ~$7/mo per ENI |
| public internet | NAT Gateway | ~$32/mo |

### Graceful degradation — verified

`make cache-down` (cache stack deleted, SSM parameter gone), then redirects:
still `302`, no `5xx`, no per-request latency bump — L2 is disabled once at cold
start (`h.l2 == nil`), not failing per request, so there's no 75 ms timeout to
pay. Tier field is only `l1` / `miss`. `make cache-up` again and `l2` reappears
in the mix. Also covered by unit tests and, earlier, by the single-subnet
misconfiguration (SSM unreachable → same clean fallback).

### Cost at rest

Cache stack (`cache.t4g.micro`, ~$0.016/hr) — `make cache-down` before ending a
session. SSM interface endpoint (~$14/mo, two ENIs) lives in the permanent
network resources.
