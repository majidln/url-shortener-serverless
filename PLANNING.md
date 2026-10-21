# URL Shortener — Execution Roadmap

A URL shortener built serverless-first on AWS with SAM, in steps where each one responds to a problem the previous step left behind.

One codebase, one stack that evolves. `git tag step-N-complete` at the end of each step — the diff between tags is the record of what changed.

One load script, written in Step 1 and never changed. p50/p99 and cost recorded in [RESULTS.md](RESULTS.md) every step.

Out of scope: EC2/containers, dev/prod environments, CI/CD pipeline.

## Status

| Step | What | Status | Tag | Cost at rest |
|---|---|---|---|---|
| 1 | Make it work | Done | `step-1-complete` | Free |
| 2 | Custom domain over HTTPS | Done | `step-2-complete` | $0.50/month |
| 3 | Observability and safe deploys | Done (all three proofs run) | not tagged | A few $/month |
| 4 | Caching the redirect lookup | Done | `step-4-complete` | Cache stack bills hourly |
| 5 | The edge | Fork decision still open | not tagged | WAF about $5/month |
| 6 | Click analytics | Optional, not started | | |
| 7 | Frontend | Optional, not started | | |

---

## 1. Prerequisites

- [ ] AWS account, non-root IAM identity, budget alarm. *(the budget alarm is not recorded anywhere in the repo, so left unchecked)*
- [x] AWS CLI + **SAM CLI** installed. *(the version was never recorded)*
- [x] **Go 1.21+** (for `log/slog`). The modules use Go 1.26.
- [ ] Docker for `sam local`. *(not recorded)*
- [x] **A domain with a Route 53 public hosted zone** (`link123.cfd`). Step 2 can't start without one, and it's the only prerequisite that takes real-world time. A subdomain of a domain you already own is easiest. Avoid `.xyz`/`.top`/`.click`/`.link` — heavily used for phishing via shorteners, so short links on them get filtered and Step 2 looks broken.
- [x] A load-testing tool (`hey`) and a script exercising create and redirect (`scripts/loadtest.sh`).

---

## 2. Decisions

| | |
|---|---|
| **Region** | `us-east-1` — CloudFront needs its ACM cert there, and Step 5 moves the domain to CloudFront |
| **Tooling** | SAM for everything, including Route 53/ACM/CloudFront/WAF. Raw `AWS::` resources where SAM has no shorthand. Note where it chafes rather than reaching for Terraform |
| **Store** | DynamoDB, on-demand. Pure key-value, nothing at rest |
| **Runtime** | Go on `provided.al2023`, arm64. `log/slog` gives structured JSON natively |
| **API** | HTTP API — ~$1/M vs REST's $3.50/M. REST's extras come from CloudFront instead |
| **Keys** | Random base62, 7 chars, from `crypto/rand`. Deterministic hashing rejected: free dedupe isn't worth losing per-campaign codes and unguessability |
| **Collisions** | `PutItem` conditional on `attribute_not_exists(key)`; on failure regenerate, bounded at ~5 tries |

**Three Lambdas:** `url-shortener` (URL in, short URL out), `url-redirect` (key in, redirect out), `key-generator` (string in, key out).

The generator is deliberate over-decomposition — a network hop for microseconds of CPU — because it's meant to be callable by other services later, so the boundary is the deliverable. It sits on the create path, ~1% of traffic in a read-heavy service, and Step 1 measures what the split costs.

Four constraints keep it reusable:
- **Pure** — no storage access. Uniqueness checking would weld it to this table.
- **Enforced by IAM**: zero DynamoDB permissions on its role.
- **Direct `lambda:InvokeFunction`**, scoped to its ARN. No API Gateway in front.
- **Named `key-generator`, not `url-key-generator`.** No URL validation inside. The response carries an `algorithm` field so the contract can version.

**Safe deploys instead of environments.** `DeploymentPreference: Canary10Percent5Minutes` with the redirect alarm attached — CodeDeploy shifts 10% of traffic, watches for five minutes, rolls back on its own. It's what Step 3's alarms are *for*, and the serverless substitute for testing in a copy of prod.

---

## 3. Steps

### Step 1 — Make it work
Three Lambdas, HTTP API, DynamoDB table. See [STEP-1-GUIDE.md](docs/STEP-1-GUIDE.md).

**Exit:**
- [x] POST a URL, get a code; GET the code, get a redirect.
- [x] Collision path forced and proven. *(covered by `TestShortenRegeneratesOnCollision`; a live run with a 1–2 character key is not recorded in RESULTS.md)*
- [x] Generator's role has no DynamoDB permissions — check the role, not the code. *(the function declares no `Policies` in `template.yaml`)*
- [x] Unit tests for `key-generator` pass with no AWS mocking; if mocking is needed, it isn't pure.
- [x] Baseline recorded, cold and warm ([RESULTS.md](RESULTS.md)).

*Free. Leave it running.*

### Step 2 — The short URL isn't short
`https://abc123.execute-api.us-east-1.amazonaws.com/prod/xY7z` is longer than most URLs worth shortening.

Hosted zone, ACM cert, API Gateway custom domain, alias record.

Don't build for Step 5 — it moves this to CloudFront, and the rework is small.

**Exit:**
- [x] `https://<domain>/<code>` redirects over HTTPS.
- [x] A `step-2` row (redirect p50/p99 via the domain) in [RESULTS.md](RESULTS.md).

*$0.50/month. Leave it running.*

### Step 3 — Nobody told us it broke
A bad deploy broke redirects and you found out by clicking a link yourself.

Dashboard, alarms, SNS + email, structured logs, canary, and the auto-rollback from §2.

**The point: alarm on the symptom, not the implementation.** The redirect Lambda's `Errors` metric is wrong twice — a 404 is a successful invocation, so a bad table name shows zero errors while every redirect fails; and an alarm bound to `FunctionName=url-redirect` goes blind, silently, once Steps 4–5 change what serves redirects. Page on API Gateway metrics; keep Lambda and DynamoDB metrics on the dashboard for diagnosis.

Traps: `TreatMissingData` (no traffic at 3am means no datapoints); rate not count, floored by a composite alarm so one bad request doesn't page; log retention defaults to never-expire.

**Exit:** three proofs.
- [x] Force the alarm state and confirm the email arrives. *(effectively covered by the transitions in proof 2)*
- [x] Break redirects for real and watch it fire and recover. *(canary alarm: 3m02s to fire, 3m26s to recover)*
- [x] Deploy a broken redirect Lambda and watch CodeDeploy roll it back while the alarm fires. *(rolled back 3m44s after the traffic shift began; see RESULTS.md)*

*A few $/month — first thing that bills at rest. Still fine to leave running.*

### Step 4 — Paying to run code that does a key lookup
Every redirect invokes a function and crosses a region for an immutable value. (Not "cold starts dominate p99" — Go may not have that problem. Check the dashboard first.)

In-memory cache in the redirect Lambda, measured (it ended up a TTL map, not an LRU); then ElastiCache on `cache.t4g.micro` (Valkey, the Redis-compatible engine) (~$0.016/hr — priced hourly because this gets destroyed nightly), measured again.

**The VPC is the real lesson.** ElastiCache is VPC-only, so the Lambda goes in a VPC and loses its internet route. The naive fix is a NAT Gateway at ~$32/month — the most expensive mistake available here. You don't need one: private subnets plus the **free DynamoDB gateway endpoint**. Zero NAT is the takeaway, and most tutorials get it wrong.

DAX rejected (VPC-bound, ~$30/month minimum, solves a microsecond problem). API Gateway caching rejected (REST only, ~$14/month; CloudFront does it better).

**Stack split:** VPC + ElastiCache go in a second stack, so the hourly-billed half can be deleted nightly without taking the alarms with it.

**Exit:**
- [x] Hit rate and p99 recorded after each cache layer.
- [x] No NAT Gateway in the account (`aws ec2 describe-nat-gateways` returned empty).
- [x] Cache stack deletes cleanly, and redirects keep working without it.

*Stops being free at rest. Delete the cache stack before closing the laptop.*

### Step 5 — The edge
Redirects still round-trip to a region for immutable data, and the service is public enough to be abused.

CloudFront in front, deliberate `Cache-Control`, negative caching, WAF.

**CloudFront comes after Redis, deliberately.** Build the edge first and Redis has nothing left to do, so you never learn what it was worth.

**The crux is 301 vs 302.** A 301 is cached near-permanently by browsers — fastest repeat redirect at zero cost, but the click is never seen again and the link can never be repointed or killed, because a browser cache isn't invalidatable. Decided: **302 with an explicit `Cache-Control: max-age`**. That's also why abuse handling lives here: a malware link needs to die *now*, invalidation purges the edge but not browsers, so the TTL argument and takedown are the same argument.

**What the edge breaks:** API Gateway now sees only cache misses, so the error-rate denominator collapses and the symptom moves to CloudFront's metrics — Step 3's principle applied again. Edge hits never reach Lambda, so Step 6's data source changes too.

**The fork to resolve in writing:** VPC+Redis and "no Lambda on the hot path at all" are incompatible — a VPC-attached Lambda can't collapse into an API Gateway → DynamoDB direct integration. Pick one, say why, keep the loser documented.

**Exit:**
- [x] Share of requests never reaching the origin (86–88% at the edge).
- [x] Cache hit rate before and after (the tier mix changed from 66/26/8 to 12/40/48 at the origin).
- [x] A takedown demonstrated end to end (about 6 seconds when invalidated in the right order).
- [ ] The fork resolved in writing. *(the Step 5 guide §8 decision is still pending)*
- [x] The raw `execute-api` URL guarded. *(`X-Origin-Verify` header check, rolled out in two deploys and verified live: `403` without the secret)*

*WAF is the biggest recurring cost (~$5/month). Destroy after each session.*

### Step 6 — Which links are popular? *(optional)*
No operational trigger. Click analytics from DynamoDB Streams if traffic reaches the origin, CloudFront logs if it doesn't. If it's never built, the project is complete.

### Step 7 — Nobody wants to curl a URL shortener *(optional)*
No operational trigger either. Everything so far is an API — a real user needs
a page: create a link, see the short URL, maybe a list of links already made.
A minimal frontend (static site on S3 behind the existing CloudFront
distribution, or its own) turns this from a backend exercise into something a
non-technical person could actually use. Deliberately vague until it's
picked back up — worth deciding then whether it's a static form calling
`/shorten` directly, or something with its own login/ownership model, which
is a much bigger scope change. If it's never built, the project is still
complete.

---

## 4. Session discipline

Deploy → verify the exit criteria by hand → record numbers in [RESULTS.md](RESULTS.md) → `git tag step-N-complete` → tear down what the step requires.

| Stack | Rule |
|---|---|
| Main application | Leave running |
| Cache (ElastiCache), Step 4 | **`make cache-down` before ending the session** |
| WAF, Step 5 | Delete if idle |

A failed deploy is not a no-op — rollback can itself fail and strand a stack in `UPDATE_ROLLBACK_FAILED`. And log groups Lambda creates implicitly can outlive their stack, retaining logs forever; declare them with retention in Step 3.

---

## 5. Open questions, and what was decided

| Question | Recommendation | Decision |
|---|---|---|
| Synthetic canary (Step 3) | Yes, 5-minute interval (~$1/month). No traffic at 3am means the alarm has nothing to evaluate, and the auto-rollback depends on it | Yes, but every **1 minute** (`rate(1 minute)` in `template.yaml`), so the alarm fires in about 3 minutes |
| `max-age` (Step 5) | 300s, revisited against takedown speed | Split: **`max-age=60`** for browsers (cannot be purged) and **`s-maxage=300`** for CloudFront (can be) |
| Valkey vs Redis OSS (Step 4) | Valkey — API-compatible, cheaper | Valkey |
| Fork: VPC + cache vs no Lambda on the hot path (Step 5) | Pick one, say why | **Still open** |

*Costs are approximate `us-east-1` list prices. Confirm before relying on them.*
