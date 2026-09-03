# Step 1 Guide — Make It Work

A coaching guide, not a code drop. It names what to build and why; you write the Go and the YAML.

**Goal (from [PLANNING.md](PLANNING.md)):** POST a URL, get a code. GET the code, get a redirect. Three Lambdas, one HTTP API, one DynamoDB table.

---

## 0. Working directory

One codebase that evolves — steps are marked by git tags, not by directories. So there is no `step-1/` folder; everything lives in `url-shortener/app/` from here to Step 6.

---

## 1. The SAM template

`template.yaml` holds the whole stack: three functions, one table, one HTTP API. SAM's shorthand covers all of it at this step — no raw `AWS::` resources needed yet.

For Go on `provided.al2023`/arm64, four things have to line up or the build silently produces something that won't start: `Runtime: provided.al2023`, `Architectures: [arm64]`, `Handler: bootstrap`, and a `Metadata: BuildMethod: go1.x` on each function. The binary **must** be named `bootstrap` — that's the contract for the custom runtime.

Set `Globals` for the shared runtime settings rather than repeating them three times.

---

## 2. The three functions

**`key-generator`** — string in, key out. Random base62, 7 chars, from `crypto/rand`. Return an `algorithm` field alongside the key so the contract can version later.

Four constraints make it reusable, and Step 1 is where they get established:
- No storage access, no URL validation. It only makes keys.
- Its IAM role gets **zero** DynamoDB permissions.
- Invoked directly via `lambda:InvokeFunction`, scoped to its ARN — no API Gateway in front.
- Named `key-generator`, not `url-key-generator`.

**`url-shortener`** — POST handler. Calls `key-generator`, then writes to DynamoDB with `PutItem` conditional on `attribute_not_exists(key)`.

**`url-redirect`** — GET handler. Look up the key, return a 302 with a `Location` header. Miss returns 404. Nothing clever here yet; Step 5 revisits the redirect code and caching.

---

## 3. The table

DynamoDB, on-demand billing, partition key on the short code. No sort key, no indexes, no TTL — pure key-value. Grant each function only what it needs: write for the shortener, read for the redirect, nothing for the generator.

---

## 4. The load script

Write it now and **never change it** — every step compares against it, so a script that drifts makes the numbers meaningless. `brew install hey` if you haven't.

It needs to exercise both paths: create some URLs, then hammer the redirects with the codes it got back. Read-heavy, since that's the real traffic shape.

Create `RESULTS.md` and record p50/p99 for both paths, cold and warm, plus what the extra hop to `key-generator` costs on the create path. That last number is the whole reason the generator is a separate function.

---

## 5. Suggested order

1. Clear the old scaffold, `sam init` into `url-shortener/app/`
2. `key-generator` first — it has no dependencies
3. Unit tests for it, then `sam build && sam local invoke`
4. Table, then `url-shortener`, then `url-redirect`
5. `sam deploy --guided` (region `us-east-1`)
6. Verify create and redirect by hand with curl
7. Load script → `RESULTS.md`
8. `git tag step-1-complete`

---

## 6. Checkpoints

- Before writing the generator's tests: if you need to mock AWS to test it, it isn't pure. What would you have to remove?
- After deploying: force a collision. Shorten the key to 1–2 characters and hammer create until the conditional write fails. Does the retry hold, and does it give up cleanly at the bound?
- Check the generator's IAM role in the console, not your code. Does it have any DynamoDB permission at all?

---

## 7. Definition of done

- POST a URL, get a code. GET the code, get a redirect.
- Collision path forced and proven, not assumed.
- Generator's role verified to have no DynamoDB permissions.
- Unit tests for `key-generator` pass with no AWS mocking.
- Baseline in `RESULTS.md`, cold and warm.
- Tagged `step-1-complete`. Free at rest — leave it running.
