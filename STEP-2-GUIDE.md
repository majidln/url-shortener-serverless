# Step 2 Guide — The Short URL Isn't Short

A coaching guide, not a code drop. It names what to build and why; you write the
YAML.

**Goal (from [PLANNING.md](PLANNING.md)):** put a real domain in front of the
`execute-api` URL, over HTTPS.

**Exit:** `https://<your-domain>/<code>` returns the `302`. Nothing else changes.

*~$0.50/month (the hosted zone). Leave it running.*

---

## 1. Prerequisite — delegation

You need a Route 53 public hosted zone whose nameservers are actually delegated
from the registrar. This is the one part that costs real-world time, and a broken
delegation makes the ACM certificate hang unvalidated until the deploy times out.

Check before touching the template:

```
dig +short NS <your-domain> @1.1.1.1
```

Must return the `awsdns-*` names from your hosted zone. If it returns the
registrar's nameservers, fix that at the registrar first.

The Go code does not change this step — it's `template.yaml` plus two parameters.

---

## 2. What to add to the template

**Parameters:** `DomainName` and `HostedZoneId`. Don't hardcode them; put the
values in `samconfig.toml` as `parameter_overrides` so `sam deploy` stays
parameterless. Add the custom-domain URL to `Outputs`.

**Certificate — raw resource.** SAM has no shorthand for ACM, so declare
`AWS::CertificateManager::Certificate` directly: `ValidationMethod: DNS`, plus
`DomainValidationOptions` with your `HostedZoneId` so CloudFormation writes the
validation `CNAME` and waits. Same region as the API (`us-east-1`) — which is
also where Step 5's CloudFront cert must live, so nothing to redo. Note this bit
of SAM friction in RESULTS.md, per PLANNING.md §2.

**Custom domain — SAM shorthand.** The existing `HttpApi` resource takes a
`Domain` property. Point it at the certificate and the hosted zone and SAM
creates the domain name, the stage mapping, and the Route 53 alias records:

```
Domain:
  DomainName: !Ref DomainName
  CertificateArn: !Ref Certificate
  SecurityPolicy: TLS_1_2
  Route53:
    HostedZoneId: !Ref HostedZoneId
```

No base path — the domain root maps to the stage, so `https://<domain>/<code>`
hits `GET /{key}`.

---

## 3. Keep it minimal

Step 5 moves this behind CloudFront and the rework should be small:

- One hostname. No `www`, no apex juggling.
- Don't touch the redirect code or its `Cache-Control` — edge caching is a Step 5
  call.
- Leave the `execute-api` endpoint enabled.
- `http://` won't work (custom domains are HTTPS-only). Fine — the http→https
  redirect is a Step 5 CloudFront job.

---

## 4. Order

1. Delegation confirmed (§1).
2. Add the parameters, the `Certificate`, the `Domain` block.
3. `sam validate --lint`, then `sam deploy`. First deploy blocks on cert
   issuance — minutes, up to ~30 if DNS is slow. Still going at 30? Delegation is
   wrong.
4. Verify by hand (§5).
5. Re-run the frozen load script with `BASE_URL=https://<domain>`; add a `step-2`
   row to RESULTS.md.
6. `git tag step-2-complete`.

---

## 5. Definition of done

- `curl -sI https://<domain>/<code>` → `HTTP/2 302` with a `location:` header;
  confirmed in a browser too.
- `https://<domain>/nope1234` → `404`.
- Certificate issued by ACM, validation record left in place so it auto-renews.
- `execute-api` URL still works.
- RESULTS.md has a `step-2` row (redirect p50/p99 via the domain) and a line on
  the ACM/SAM friction.
- Tagged `step-2-complete`. Leave it running.
