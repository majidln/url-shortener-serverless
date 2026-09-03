#!/usr/bin/env bash
# URL-shortener load test.
#
# The WARM load below (seed + create burst + redirect burst) is the Step 1
# baseline. FROZEN at step-1-complete — every later step re-runs it unchanged
# and adds a row to RESULTS.md. The COLD and HOP helper modes are just
# measurement aids and may evolve.
#
# Usage:
#   BASE_URL=https://xxxx.execute-api.us-east-1.amazonaws.com/Dev ./loadtest.sh [seed_count]
#   SEED_ONLY=1 ./loadtest.sh 100    # reseed /tmp code list, skip the load
#   COLD=1     ./loadtest.sh         # cold sample: needs codes already seeded
#                                    # AND ~15 min of no traffic first
#   HOP=1      ./loadtest.sh         # measure the key-generator invoke cost
set -euo pipefail
: "${BASE_URL:?set BASE_URL to the API base, no trailing slash}"

REGION="${AWS_REGION:-us-east-1}"
STACK="${STACK:-url-shortener}"
CODES=/tmp/urlshort-codes.txt
SEED_COUNT="${1:-${SEED_COUNT:-50}}"

# Portable random line picker (macOS has no `shuf`).
pick_code() {
  awk 'BEGIN{srand()} {a[NR]=$0} END{if (NR) print a[int(rand()*NR)+1]}' "$CODES"
}

need_codes() {
  [ -s "$CODES" ] || { echo "no codes in $CODES — run without COLD= first to seed" >&2; exit 1; }
}

# ---------------------------------------------------------------------------
# HOP: what the synchronous Lambda->Lambda call to key-generator costs.
# Measured server-side from CloudWatch: run a WARM baseline first, then this
# within ~20 min. The shortener's Duration includes the sync invoke + the
# PutItem; the redirect's Duration is a comparable single DynamoDB round trip;
# the generator's own Duration is the actual work. hop ~= shortener - redirect.
# ---------------------------------------------------------------------------
if [ -n "${HOP:-}" ]; then
  start=$(python3 -c 'import datetime;print((datetime.datetime.now(datetime.UTC)-datetime.timedelta(minutes=25)).strftime("%Y-%m-%dT%H:%M:%S"))')
  end=$(python3 -c 'import datetime;print(datetime.datetime.now(datetime.UTC).strftime("%Y-%m-%dT%H:%M:%S"))')
  dur() {
    aws lambda list-functions --region "$REGION" \
      --query "Functions[?starts_with(FunctionName, 'url-shortener-$1')].FunctionName" --output text
  }
  avg_dur() {
    aws cloudwatch get-metric-statistics --region "$REGION" \
      --namespace AWS/Lambda --metric-name Duration \
      --dimensions Name=FunctionName,Value="$1" --start-time "$start" --end-time "$end" \
      --period 300 --statistics Average \
      --query "sort_by(Datapoints, &Timestamp)[-1].Average" --output text
  }
  s=$(avg_dur "$(dur UrlShortenerFunction)")
  r=$(avg_dur "$(dur UrlRedirectFunction)")
  g=$(avg_dur "$(dur KeyGeneratorFunction)")
  echo "### server-side Duration, last 20 min (avg ms)"
  echo "  url-shortener : $s   (sync key-generator invoke + PutItem)"
  echo "  url-redirect  : $r   (one DynamoDB GetItem, for comparison)"
  echo "  key-generator : $g   (the actual work)"
  python3 -c "print(f'  => key-generator hop ~= {float('$s') - float('$r'):.0f} ms server-side, for {float('$g'):.0f} ms of real work')"
  exit 0
fi

# ---------------------------------------------------------------------------
# COLD: small sequential bursts against idle functions. No seeding (that would
# warm the create path). Run this FIRST, after ~15 min of no traffic.
# ---------------------------------------------------------------------------
if [ -n "${COLD:-}" ]; then
  need_codes
  code=$(pick_code)
  echo "### cold: GET /{code}  (redirect path)"
  hey -n 20 -c 1 -disable-redirects "$BASE_URL/$code"
  echo
  echo "### cold: POST /shorten  (create path)"
  hey -n 20 -c 1 -m POST \
    -H 'Content-Type: application/json' \
    -d '{"url":"https://example.com/cold"}' \
    "$BASE_URL/shorten"
  exit 0
fi

# ---------------------------------------------------------------------------
# WARM baseline (the frozen part).
# ---------------------------------------------------------------------------
echo "### seeding $SEED_COUNT codes"
: > "$CODES"
for i in $(seq 1 "$SEED_COUNT"); do
  key=$(curl -sX POST "$BASE_URL/shorten" \
    -H 'Content-Type: application/json' \
    -d "{\"url\":\"https://example.com/page/$i\"}" | jq -r '.key')
  [ -n "$key" ] && [ "$key" != "null" ] || { echo "create failed at $i" >&2; exit 1; }
  echo "$key" >> "$CODES"
done

code=$(pick_code)
echo "sample code: $code"

if [ -n "${SEED_ONLY:-}" ]; then
  echo "seed only, done"
  exit 0
fi

# This account's Lambda concurrency limit is 10 (unlifted default). Concurrency
# is kept at/under that so the baseline measures latency, not throttling:
#   - create holds 2 slots per request (shortener + its sync key-generator call),
#     so -c 5 keeps it clear of the ceiling.
#   - redirect holds 1 slot, so -c 10 saturates without throttling.
# If the account limit is raised later, keep these values — the point is a fixed,
# reproducible load, not a maxed-out one.
CREATE_CONC="${CREATE_CONC:-5}"
REDIRECT_CONC="${REDIRECT_CONC:-10}"

echo
echo "### warm: POST /shorten  (create path)"
hey -n 200 -c "$CREATE_CONC" -m POST \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/loadtest"}' \
  "$BASE_URL/shorten"

echo
echo "### warm: GET /{code}  (redirect path — the real workload)"
# -disable-redirects: measure OUR 302, don't chase Location to the target site.
# Expect the status distribution to be all [302].
hey -n 2000 -c "$REDIRECT_CONC" -disable-redirects "$BASE_URL/$code"
