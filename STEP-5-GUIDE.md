# Step 5 Guide — The Edge

A coaching guide, not a code drop. It names what to build and why; you write the
YAML and the Go.

**Goal (from [PLANNING.md](PLANNING.md)):** redirects still round-trip to
`us-east-1` for a value that almost never changes, and the service is public
enough to be abused. Put CloudFront in front, cache redirects *deliberately*,
cache misses too, and put WAF on the front door.

**Exit:** share of requests that never reach the origin; L2 hit rate before and
after; a takedown demonstrated end to end; the fork (§8) resolved in writing.

*CloudFront is inside the always-free tier at this volume. WAF is the recurring
cost (~$5/month for the ACL + ~$1/month per rule) — switch it off when idle.*

---

## 1. Before and after

**Today:** `link123.cfd` → Route 53 → **API Gateway custom domain** → redirect
Lambda (L1 → Valkey → DynamoDB). Every redirect reaches the Lambda.

**After:** `link123.cfd` → Route 53 → **CloudFront** (+ WAF) → cached 302 served
from the edge, or on a miss → the `execute-api` origin → the same Lambda. The
Lambda only sees edge misses.

CloudFront comes *after* Redis on purpose: build the edge first and Redis has
nothing left to do, so you'd never learn what it was worth. Step 4's numbers are
your "before".

The API Gateway custom domain goes away. The ACM certificate stays — CloudFront
requires it in `us-east-1`, which is exactly where Step 2 put it.

---

## 2. The crux — 301 vs 302, and two TTLs

A **301** is cached by browsers near-permanently: the fastest possible repeat
redirect, for free — but you never see that click again, and you can never
repoint or kill the link, because nobody can purge a browser cache.

Decided: **302 with an explicit `Cache-Control`.** The handler sends `no-store`
today, which stops CloudFront caching anything — that's the line this step
changes.

There are two caches with different powers, so pick two numbers:

| Cache | Can you purge it? | Directive |
|---|---|---|
| CloudFront | yes — invalidation | `s-maxage` |
| browser | no | `max-age` |

The edge can safely hold longer than the browser, e.g.
`Cache-Control: public, max-age=60, s-maxage=300`. PLANNING's open question
suggests 300 s; argue your own numbers. **The browser `max-age` is your takedown
SLA** — anyone who already clicked a bad link keeps being redirected that long,
and nothing you do can stop it. That's why abuse handling lives in this step: the
TTL argument and the takedown argument are the same argument.

Read both TTLs from env vars. `TestRedirectIsNotCacheable` asserts `no-store` —
its premise is exactly what this step overturns, so rewrite it on purpose.

---

## 3. The distribution

`AWS::CloudFront::Distribution` — raw resource, no SAM shorthand.

**Origin** — the HTTP API's `execute-api` domain
(`${HttpApi}.execute-api.${AWS::Region}.amazonaws.com`), `https-only`, and
**`OriginPath: /Dev`**. Your stage is `Dev`, not `$default`; without the path
every request comes back as API Gateway's `{"message":"Not Found"}`.

**Two cache behaviors:**

| Path | Allowed methods | Cache policy | Origin request policy |
|---|---|---|---|
| `/shorten` | all seven | `CachingDisabled` | `AllViewerExceptHostHeader` |
| default | GET, HEAD | your own (below) | `AllViewerExceptHostHeader` |

- CloudFront only accepts three method sets — GET/HEAD, GET/HEAD/OPTIONS, or all
  seven. POST means all seven.
- **`AllViewerExceptHostHeader` is not optional.** Forward the viewer's
  `Host: link123.cfd` to an `execute-api` origin and API Gateway can't route it —
  403s. The most common CloudFront → API Gateway failure there is.
- Managed IDs: `CachingDisabled` `4135ea2d-6df8-44a3-9df3-4b5a84be39ad`,
  `AllViewerExceptHostHeader` `b689b0a8-53d0-40ab-baf2-68738e2966ac`.

**Your own cache policy** for redirects: cache key = **path only** (no query
strings, headers or cookies — the key *is* the path), and
`MinTTL 0 / DefaultTTL 0 / MaxTTL 3600`, so CloudFront caches exactly what the
origin's `Cache-Control` asks for and nothing when it's silent. The managed
`CachingOptimized` defaults to a *day* when the origin says nothing — wrong for a
service that has to be able to kill a link.

**Error caching** — CloudFront caches error responses for 10 s by default. Set
`ErrorCachingMinTTL: 0` for `500/502/503/504` so an origin blip isn't pinned at
the edge. 404s are §5.

**`ViewerProtocolPolicy: redirect-to-https`** — the `http://` → `https://`
redirect Step 2 deferred.

Quick check on any response: `X-Cache: Hit from cloudfront` vs
`Miss from cloudfront`.

---

## 4. Moving the domain

Four deploys. The split is forced, not caution:

1. **Distribution with no alternate domain.** Test through its
   `dxxxx.cloudfront.net` name — a create, a redirect, a 404, `http` → `https`,
   `X-Cache` on a repeat.
2. **Add `Aliases: [link123.cfd]` and `ViewerCertificate`** (the existing
   `Certificate`, `sni-only`, `TLSv1.2_2021`). DNS still points at API Gateway,
   so nothing changes for users yet.
3. **Remove the `Domain` block from `HttpApi`.** `link123.cfd` stops resolving.
4. **Add a Route 53 `A` + `AAAA` alias to the distribution** (CloudFront's alias
   hosted-zone id is `Z2FDTNDATAQYW2` for every distribution). Service is back.

Why 3 and 4 can't be one deploy: SAM's `Domain.Route53` owns the `link123.cfd`
record, and CloudFormation creates new resources *before* deleting removed ones.
Your new record collides with the old one ("already exists") and the whole update
rolls back. Run 3 and 4 back to back; expect a few minutes of outage, and the
canary alarm firing and clearing — a free live re-run of Step 3.

Every CloudFront change takes 5+ minutes to deploy; `sam deploy` waits on it.

---

## 5. Negative caching

A scan of random keys (`/aaaaaaa`, `/aaaaaab`, …) is a Lambda invoke plus a
DynamoDB read per guess today. Cache the `404` briefly at the edge: have the
handler send `Cache-Control: public, max-age=30` on 404s — explicit, and unit
testable — instead of leaning on `ErrorCachingMinTTL`.

The cost: a key probed as a 404 just before it's created stays a 404 at that
edge location for up to 30 s. Keys are random and unguessable, so the race is
rare but real — keep the TTL short. Leave L1 and Valkey positive-only; this
resolves the "negative caching is a Step 5 decision" comment in `main.go`.

---

## 6. WAF

`AWS::WAFv2::WebACL` with **`Scope: CLOUDFRONT`**. CloudFront-scoped WAF must
live in `us-east-1` — already true here.

Attach it via the distribution's **`WebACLId`** (the Web ACL's ARN).
`AWS::WAFv2::WebACLAssociation` does **not** work for CloudFront; it's for
regional resources only.

Cheap and sufficient:

- `AWSManagedRulesAmazonIpReputationList`, `AWSManagedRulesKnownBadInputsRuleSet`.
- A **rate-based rule scoped to `POST /shorten`**, aggregated by IP over a
  5-minute window. Link spam is the abuse that costs you money and domain
  reputation.

**The trap:** your load script sends ~300 creates in seconds from one IP. A
sensible limit blocks it, and `hey` shows 403s that look like an outage. Allow
your IP with an `IPSet` rule evaluated first, or set the limit above the script
and write down why. The canary is one create a minute — no problem.

Make it switchable: a plain `EnableWaf` parameter (default `false`), a
`Condition` on the Web ACL, and
`WebACLId: !If [WafOn, !GetAtt WebAcl.Arn, !Ref AWS::NoValue]`. Off-when-idle is
a parameter flip, not a teardown.

**Gap to write down:** WAF only guards traffic that goes *through* CloudFront.
The `execute-api` URL is still public and unguarded. You can't set
`DisableExecuteApiEndpoint` — it *is* CloudFront's origin. The usual fix is a
secret header CloudFront adds to origin requests and the handler checks (403
otherwise). Cheap; decide it deliberately.

---

## 7. What the edge breaks

**The paging alarm goes half-blind.** `Http5xxRateAlarm` watches API Gateway,
which now sees only edge misses: its `Count` denominator collapses, and anything
that fails *at* the edge — certificate, WAF misfire, a broken behavior — never
reaches API Gateway at all. Step 3's principle again: alarm on the front door,
and the front door just moved.

Page on **`AWS/CloudFront` `5xxErrorRate`** instead — dimensions `DistributionId`
and `Region = Global`, already a percentage so no metric math, and published only
in `us-east-1`. Keep the API Gateway alarm on the dashboard. Watch
`4xxErrorRate` after WAF goes on: a misfire looks like a 403 spike.

**The deploy gate changes shape.** During a bad redirect deploy most users get
the *good* 302s already cached at the edge; only misses reach the new version.
The canary creates a fresh key every run, so it always misses the edge and always
exercises the new code — it's now the gate that matters.

**Step 6's data source moves.** Edge hits never reach the Lambda, so click
analytics has to come from CloudFront logs.

Add dashboard panels for CloudFront `Requests`, `4xxErrorRate`, `5xxErrorRate`.

---

## 8. The fork

PLANNING asks you to pick one and keep the loser documented:

- **Keep Lambda + VPC + Valkey behind the edge.** The edge takes hot keys; L1 and
  Valkey soak up the misses that still reach origin.
- **No Lambda on the hot path.** A VPC-attached Lambda can't collapse into a
  managed integration, so this removes the VPC path. Know the constraint: an
  **HTTP API can't integrate directly with DynamoDB** — its AWS service
  integrations cover EventBridge, SQS, Kinesis, Step Functions and AppConfig. A
  direct `GetItem` means a **REST API with VTL mapping templates**. The modern
  variant skips the origin for hits altogether: a **CloudFront Function reading
  CloudFront KeyValueStore** (keys ≤ 512 B, values ≤ 1 KB) that returns the 302
  at the edge.

Decide with the §9 numbers, not taste. If the edge absorbs the hot keys, Valkey's
share of the residual traffic collapses — and you'd be paying for a cache node,
~$14/month of SSM interface endpoints, and VPC cold starts to accelerate traffic
that barely exists. Write the decision and the evidence in RESULTS.md. If you
drop VPC + Valkey, that's its own change, made *after* the decision is written.

---

## 9. Measure

`make cache-up` first — L2 has to exist to measure it.

**Origin share.** Run the frozen `loadtest.sh` against `https://link123.cfd` and
compare over the same window: `origin share = API Gateway Count ÷ CloudFront
Requests`; edge share is one minus that. (`CacheHitRate` exists too, but only
with CloudFront's paid additional metrics — the ratio is free.)

**L2 hit rate after.** Step 4's many-key burst was 66% L1 / 26% L2 / 8% origin,
all at the Lambda. Re-run the same burst through CloudFront; record how many
requests still reach the handler, and their tier mix.

Caveat — same shape as Step 4's single-key one: everything from your laptop hits
**one** CloudFront edge location, and each has its own cache. Real traffic spans
many, so your edge share is optimistic. Say so next to the number.

**Takedown, end to end.** Pick a created link:

1. Delete its DynamoDB item.
2. **Wait out the origin tiers** — L1 and Valkey hold it for their 60 s TTL.
   Invalidate before that and the next edge miss re-fetches the stale 302 from a
   warm L1 or Valkey and re-caches it at the edge for a full `s-maxage`.
3. `aws cloudfront create-invalidation --distribution-id … --paths "/<key>"`.
4. Time until `curl -sI https://link123.cfd/<key>` returns `404` — which is now
   negative-cached, as it should be.
5. In a browser that already followed the link, it keeps redirecting for
   `max-age`. Confirm it, and record the end-to-end time as the takedown SLA.

The first 1,000 invalidation paths a month are free.

---

## 10. Traps — pre-deploy checklist

- `OriginPath: /Dev` (§3)
- `AllViewerExceptHostHeader` on **both** behaviors (§3)
- `/shorten` allows all seven methods (§3)
- Own cache policy, not `CachingOptimized` (§3)
- `ErrorCachingMinTTL: 0` for 5xx (§3)
- Domain move is two separate deploys for steps 3 and 4 (§4)
- `Scope: CLOUDFRONT`, attached by `WebACLId` (§6)
- WAF rate limit vs your own load test (§6)
- Paging alarm moved to CloudFront metrics (§7)
- Invalidate *after* the origin TTL (§9)

---

## 11. Order

1. Handler: configurable `max-age` / `s-maxage` on the 302, `max-age=30` on the
   404; update the tests. Deploy — still behind API Gateway directly.
2. Distribution on its `cloudfront.net` name (§3); test.
3. Move the domain (§4).
4. CloudFront `5xxErrorRate` alarm + dashboard panels (§7).
5. Measure origin share and L2 after (§9).
6. WAF on (§6); show a rate-limit block; confirm the load script still passes.
7. Takedown end to end (§9).
8. Write the fork decision (§8).
9. `git tag step-5-complete`.

---

## 12. Definition of done

- `https://link123.cfd/<code>` served through CloudFront; `http://` redirects to
  `https://`; a repeat request shows `X-Cache: Hit from cloudfront`.
- A repeat 404 also comes back as an edge hit.
- Origin share recorded, with the one-edge-location caveat.
- L2 hit rate after, next to Step 4's.
- WAF: a rate-limit block demonstrated; `EnableWaf=false` removes it.
- Paging alarm on CloudFront `5xxErrorRate`.
- Takedown timed end to end; browser `max-age` recorded as the SLA.
- Fork decided in writing, loser documented.
- Tagged `step-5-complete`.

---

## 13. Session discipline

| Resource | Rule |
|---|---|
| CloudFront distribution | leave running — free tier |
| WAF | `EnableWaf=false` when idle |
| cache stack | `make cache-down`, as in Step 4 |
| SSM interface endpoints | ~$14/month while the VPC design stands — the fork may remove them |
