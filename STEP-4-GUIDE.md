# Step 4 Guide — Paying to Run Code That Does a Key Lookup

A coaching guide, not a code drop. It names what to build and why; you write the
Go and the YAML.

**Goal (from [PLANNING.md](PLANNING.md)):** every redirect invokes a Lambda and
crosses a region for a value that never changes. Add caching — in-memory first,
then a shared cache — and measure what each layer actually buys.

**Exit:** hit rate and p99 recorded after each layer; **no NAT Gateway in the
account**; the cache stack deletes cleanly.

*Stops being free at rest. `sam delete` the cache stack before you close the
laptop.*

---

## 1. Check the dashboard first

Before building anything, look at the Step 3 dashboard:

- **redirect Lambda `Duration`** — p50/p99. From Step 1–3 this is ~15 ms
  server-side (one `GetItem`), tail ~30 ms warm.
- **DynamoDB `SuccessfulRequestLatency`** for `GetItem` — single-digit ms.
- **redirect `Invocations`** — every redirect is 1 invoke **+** 1 DynamoDB read.

Set expectations from that: a cache in front of a ~5 ms `GetItem` will barely
move p50. The wins are **cost** (fewer DynamoDB reads now, fewer Lambda invokes
at the edge in Step 5) and the **p99 tail** (no cross-AZ DynamoDB variance). If
your numbers show the cache doesn't move p50, that is itself a finding — record
it: "Go + on-demand DynamoDB is already fast; the cache earns its place on cost
and tail, and as prep for the edge."

The Go code changes this step; the load script does not.

---

## 2. Stack layout

Three concerns, split by lifetime:

| Stack | Holds | Lifetime |
|---|---|---|
| **main** (existing) | functions, API, table, alarms, dashboard | always up |
| **network** (new) | VPC, 2 private subnets, route tables, **DynamoDB gateway endpoint**, a Lambda SG and a cache SG | always up — cheap/free |
| **cache** (new) | the ElastiCache (Valkey) node | **created and destroyed per session** |

PLANNING.md says "VPC + ElastiCache go in a second stack." In practice the
redirect Lambda's VPC attachment can't blink in and out nightly without
stranding ENIs — so the VPC lives in a stack that stays up, and only the
hourly-billed cache node is disposable. Note the deviation in RESULTS.md; that is
the kind of SAM/architecture friction PLANNING asks you to write down.

The **network** and **cache** stacks publish what the main stack needs (subnet
ids, SG id, cache endpoint) via **SSM parameters** — same pattern as Step 3's
config — or `Fn::ImportValue`. SSM is easier to reason about when the cache stack
comes and goes.

---

## 3. Layer 1 — in-memory LRU in the redirect Lambda

Cheapest possible cache: a bounded map at package scope in `cmd/url-redirect`.

- On a request: check the map; hit → return, no DynamoDB call. Miss → `GetItem`,
  then store `{key → url}` with a short **TTL** (60–300 s).
- **Per execution environment.** Each concurrent container has its own copy;
  cold containers start empty. Hit rate tracks how concentrated your traffic is —
  a viral link caches beautifully, a uniform spread barely at all.
- **No invalidation.** A repointed or killed link stays served from memory until
  its TTL expires or the container recycles. The TTL *is* the invalidation
  window — keep it short. (This is the same 301-vs-302 argument as Step 5, one
  layer down.)
- Keep it small — a few thousand entries, bound by count. `container/list` + a
  map, or a tiny dependency; either is fine.

Emit a **hit/miss metric** (custom metric `UrlShortener/Cache/Hit` = 1/0, or a
structured log field you count in Logs Insights) so hit rate is measurable.

Deploy, re-run `scripts/loadtest.sh` against the domain, record hit rate + p50/p99.

---

## 4. Layer 2 — ElastiCache (Valkey), and the VPC lesson

A shared cache every container sees. **Valkey**, not Redis OSS — API-compatible,
cheaper. `cache.t4g.micro`, one node, no replica (this gets destroyed nightly).

### The lesson: a VPC Lambda loses its internet route

ElastiCache has no public endpoint — it is reachable only from inside a VPC. So
the redirect Lambda gets a `VpcConfig` (private subnets + the Lambda SG). The
moment it does:

- It can reach things **inside** the VPC (the cache).
- It **cannot** reach the public internet or AWS public API endpoints —
  including DynamoDB. Every redirect that misses the cache now fails.

**The wrong fix:** a NAT Gateway. ~$32/month plus per-GB processing — the most
expensive mistake available in this whole project.

**The right fix:** a **DynamoDB gateway VPC endpoint** (`AWS::EC2::VPCEndpoint`,
type `Gateway`, service `com.amazonaws.<region>.dynamodb`). It adds a route to
DynamoDB over the AWS backbone in your subnet route tables. **Free.** No NAT.
Zero NAT Gateways in the account is the takeaway, and most tutorials get it
wrong.

CloudWatch Logs still work from a VPC Lambda without an endpoint — Lambda
delivers logs through its own service path, not the function's ENI. The redirect
handler only talks to DynamoDB and the cache, so the gateway endpoint is the
only one you need.

### Security groups

- Lambda SG: outbound to the cache SG on the Valkey port (6379).
- Cache SG: inbound from the Lambda SG on 6379. Nothing else.

### Handler logic

`in-memory LRU → Valkey → DynamoDB`, populating backwards on the way out. Treat
**cache unreachable** (connection refused / timeout — e.g. the cache stack is
deleted) as a miss and fall through to DynamoDB. The service must run fine with
the cache stack absent.

Deploy, re-run the load script, record hit rate + p50/p99 again.

---

## 5. Rejected — say why in one line each

- **DAX** — VPC-bound, ~$30/month floor, solves a microsecond problem.
- **API Gateway caching** — REST API only (you're on HTTP API), ~$14/month, and
  CloudFront does it better in Step 5.

---

## 6. Traps

- **NAT Gateway** — the whole point. If `aws ec2 describe-nat-gateways` returns
  anything, you took the wrong path.
- **Subnets in one AZ** — ElastiCache wants two; put the private subnets in two
  AZs from the start.
- **Public subnets by habit** — these must be *private* (no route to an internet
  gateway). The DynamoDB endpoint is what gives them AWS access.
- **Lambda ENI cold starts** — a VPC Lambda attaches an ENI on cold start. Modern
  accounts share a pre-provisioned pool so it's fast now, but check the
  dashboard's redirect `Duration` p99 didn't regress after attaching the VPC.
- **Leaving the cache stack up** — ~$0.016/hr is ~$11/month if you forget. The
  discipline is: `sam delete` it at the end of every session.
- **Cross-stack coupling** — if the main stack hard-`ImportValue`s something from
  the cache stack, you can't delete the cache stack. Reference cache things
  loosely (SSM param the handler reads at runtime), not as CloudFormation
  dependencies.

---

## 7. Definition of done

- Hit rate + p50/p99 in RESULTS.md **after the in-memory LRU** and **after
  Valkey** — two rows, same frozen `loadtest.sh`.
- `aws ec2 describe-nat-gateways --filter Name=state,Values=available` → empty.
- The redirect service works with the **cache stack deleted** (falls through to
  DynamoDB).
- `sam delete` on the cache stack completes with no leftovers
  (`aws elasticache describe-cache-clusters` empty, ENIs released).
- One line in RESULTS.md on the stack-split friction (§2).
- Tagged `step-4-complete`.

---

## 8. Session discipline

| Stack | Rule |
|---|---|
| main, network | leave running |
| **cache** | **`sam delete` before ending the session** |

A half-deleted VPC stack strands on ENIs that Lambda hasn't released yet — if
`sam delete` on the network stack ever hangs, wait a few minutes for ENI cleanup
and retry rather than forcing it.
