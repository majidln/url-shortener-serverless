# Step 2 Guide — The Short URL Isn't Short

A coaching guide, not a code drop. It names what to build and why; you write the
YAML.

**Goal (from [PLANNING.md](PLANNING.md)):**
`https://hoaqoopsjd.execute-api.us-east-1.amazonaws.com/Dev/xY7z` is longer than
most URLs worth shortening. Put a real domain in front of it, over HTTPS.

**Exit:** `https://<your-domain>/<code>` returns the `302`. Nothing else changes.

*Cost: ~$0.50/month — the hosted zone. ACM DNS certs and an HTTP API custom
domain are free. Leave it running.*

---

## 0. Prerequisite that takes real-world time

You need a **Route 53 public hosted zone** for a domain or subdomain you control,
and its nameservers actually delegated. This is the one thing that can't be
rushed — do it first.

- A subdomain of a domain you already own is easiest: create a hosted zone for
  `short.example.com`, then copy its four `NS` records into the parent zone as a
  delegation.
- Avoid `.xyz` / `.top` / `.click` / `.link` — heavily abused by real shorteners,
  so short links on them get filtered and Step 2 will look broken when it isn't.
- **Verify delegation before you touch the template:**
  ```
  dig +short NS short.example.com
  ```
  It must return the Route 53 nameservers for your zone. If it returns nothing or
  the parent's nameservers, the ACM certificate will never validate and the stack
  will sit in `CREATE_IN_PROGRESS` for ~30 minutes and then roll back.

---

## 1. Working directory

Same as Step 1 — one evolving codebase, steps marked by git tags. Everything
stays in `url-shortener/`. No `step-2/` folder. The Go code does not change at
all this step; this is a `template.yaml` change plus two new parameters.

---

## 2. What to add to the template

Three things, and SAM has shorthand for two of them.

### a. The certificate — raw resource (this is where SAM chafes)

SAM has no shorthand for ACM, so declare it directly:

- `AWS::CertificateManager::Certificate`, `ValidationMethod: DNS`.
- Give it `DomainValidationOptions` with your `HostedZoneId`. That makes
  CloudFormation create the validation `CNAME` in the zone for you and wait for
  issuance. Without it you're back to creating validation records by hand.
- It must be in the **same region as the API** (`us-east-1`). That also happens
  to be where Step 5's CloudFront certificate has to live, so there's no conflict
  and nothing to redo.

Jot one line somewhere (RESULTS.md is fine) about having to hand-roll this —
PLANNING.md §2 asks you to note SAM friction rather than reach for Terraform.

### b. The custom domain + mapping + DNS — SAM shorthand

The existing `HttpApi` resource takes a `Domain` property. Point it at the
certificate and the hosted zone and SAM creates the `ApiGatewayV2::DomainName`,
the `ApiMapping` to your stage, and the Route 53 alias records (A and AAAA):

```
HttpApi:
  Type: AWS::Serverless::HttpApi
  Properties:
    StageName: !Ref StageName
    Domain:
      DomainName: !Ref DomainName
      CertificateArn: !Ref Certificate
      SecurityPolicy: TLS_1_2
      Route53:
        HostedZoneId: !Ref HostedZoneId
```

No base path — the domain root maps to the stage, so `https://<domain>/<code>`
lands on `GET /{key}`.

### c. Parameters

Add `DomainName` and `HostedZoneId` as `Parameters`. Do not hardcode them. Then
put them in `samconfig.toml` under `[default.deploy.parameters]` as
`parameter_overrides` so `sam deploy` stays parameterless.

Add the custom-domain URL to `Outputs`.

---

## 3. What NOT to do

Step 5 moves this whole thing behind CloudFront and PLANNING.md promises "the
rework is small" — keep it that way:

- No `www.` handling, no apex-domain juggling. One hostname.
- Don't touch the redirect code or its `Cache-Control`. The 302 and `no-store`
  are already right; edge caching is a Step 5 decision.
- Leave the `execute-api` endpoint enabled. `DisableExecuteApiEndpoint: true` is
  tempting but it's a Step 5-shaped concern and it makes debugging harder now.
- `http://` won't work (API Gateway custom domains are HTTPS-only, no listener on
  80). That's fine for Step 2 — the http→https redirect is something CloudFront
  does in Step 5.

---

## 4. Suggested order

1. Hosted zone exists and `dig NS` confirms delegation (§0).
2. Add the two parameters, the `Certificate` resource, and the `Domain` block.
3. `sam validate --lint`.
4. `sam deploy` — the first deploy blocks on certificate issuance. Expect a few
   minutes; up to ~30 if DNS is slow. If it's still going at 30 minutes, the
   delegation is wrong — cancel, fix §0, retry.
5. Verify by hand (§5).
6. Point the frozen load script at the new domain and record a row in
   RESULTS.md.
7. `git tag step-2-complete`.

---

## 5. Checkpoints

- **Certificate:**
  ```
  echo | openssl s_client -connect <domain>:443 -servername <domain> 2>/dev/null \
    | openssl x509 -noout -subject -dates
  ```
  Subject matches your domain, dates are valid.
- **Redirect over the real domain:**
  ```
  curl -sI https://<domain>/<a-known-code>
  ```
  `HTTP/2 302` with a `location:` header. Then open it in a browser and confirm
  it actually lands on the target.
- **A miss still 404s:** `curl -sI https://<domain>/nope1234` → `404`.
- **Latency:** run `BASE_URL=https://<domain> scripts/loadtest.sh` and compare
  p50/p99 to the Step 1 row. A regional custom domain adds one DNS lookup and
  essentially no server-side latency — if p50 jumped, something's wrong (wrong
  stage mapping, cert renegotiation, following redirects to the target site).

---

## 6. Definition of done

- `https://<domain>/<code>` returns the `302` over HTTPS — verified with `curl`
  and in a browser.
- Certificate issued by ACM and left with its DNS validation record in place, so
  it auto-renews.
- The `execute-api` URL still works — Step 2 added a front door, it didn't break
  the old one.
- RESULTS.md has a `step-2` row: redirect p50/p99 via the custom domain, next to
  the Step 1 numbers, plus your one line on SAM/ACM friction.
- Tagged `step-2-complete`. ~$0.50/month. Leave it running.
