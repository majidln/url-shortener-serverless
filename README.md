# A Serverless URL Shortener on AWS with SAM

![Abstract illustration: many long strands converging into a single short point surrounded by concentric cache rings](docs/hero.svg)

A URL shortener is one of the classic entry-level system design exercises. In this project we implement one step by step on AWS serverless, and add different layers of caching to make the system fast for the people who use it.

We use AWS SAM (the Serverless Application Model, a layer on top of CloudFormation) to declare the whole system in one template, so every resource below is code, reviewed in pull requests and reproducible. The functions are written in Go and run on `provided.al2023` on `arm64`.

This is the overall overview of the architecture, and we will go through each architecture decision.

![Architecture diagram: a user reaches CloudFront and then API Gateway, with Route 53 and a Certificate Manager and CloudWatch box alongside; POST /shorten goes to the url-shortener Lambda, which calls the key-generator Lambda, writes to DynamoDB and uses a Redis cache; GET /:short-url goes to the url-redirect Lambda, which reads DynamoDB and the Redis cache](docs/url-shortener.svg)

## Table of contents

- [1. The core service](#1-the-core-service)
  - [1.1 The HTTP API](#11-the-http-api)
  - [1.2 The table](#12-the-table)
  - [1.3 The key generator](#13-the-key-generator)
  - [1.4 The shortener](#14-the-shortener)
  - [1.5 The redirect](#15-the-redirect)
- [2. A short domain](#2-a-short-domain)
- [3. The cache](#3-the-cache)
  - [3.1 L1: the in-process cache](#31-l1-the-in-process-cache)
  - [3.2 L2: Valkey in its own stack](#32-l2-valkey-in-its-own-stack)
  - [3.3 The VPC without a NAT gateway](#33-the-vpc-without-a-nat-gateway)
  - [3.4 Degrading gracefully](#34-degrading-gracefully)
- [4. Observability and safe deploys](#4-observability-and-safe-deploys)
  - [4.1 Logs](#41-logs)
  - [4.2 Alarms on the symptom](#42-alarms-on-the-symptom)
  - [4.3 The canary](#43-the-canary)
  - [4.4 Canary deploys with automatic rollback](#44-canary-deploys-with-automatic-rollback)
- [5. The edge](#5-the-edge)
  - [5.1 CloudFront and the cache policy](#51-cloudfront-and-the-cache-policy)
  - [5.2 302, not 301](#52-302-not-301)
  - [5.3 The WAF](#53-the-waf)
- [Results](#results)
- [Good to know](#good-to-know)
- [Next steps](#next-steps)
- [How to run it](#how-to-run-it)
  - [1. Prerequisites](#1-prerequisites)
  - [2. Deploy the main stack](#2-deploy-the-main-stack)
  - [3. Check that it works](#3-check-that-it-works)
  - [4. Optional: the cache stack and the WAF](#4-optional-the-cache-stack-and-the-waf)
  - [5. Tests and the load test](#5-tests-and-the-load-test)
  - [6. Tear it down](#6-tear-it-down)

## 1. The core service

The first step is the smallest thing that works: `POST` a URL and get a key back, `GET` the key and get a redirect. It takes three Lambda functions, one HTTP API and one DynamoDB table. Each function is its own Go module under `cmd/`, with its own `go.mod`.

### 1.1 The HTTP API

Everything enters through one HTTP API, and it has two routes, one for each thing a user does:

| Route | Function | What it does | Success |
|---|---|---|---|
| `POST /shorten` | `url-shortener` | The user posts a long URL and receives the short key | `200` |
| `GET /{key}` | `url-redirect` | Redirects the short URL to the original one | `302` |

```bash
curl -sX POST "https://<domain>/shorten" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/some/long/path"}'
# => 200 {"key":"aB3xY7z"}

curl -si "https://<domain>/aB3xY7z"
# => 302
#    location: https://example.com/some/long/path
```

Each route is declared as an event on its function, and SAM wires the integration:

```yaml
UrlShortener:
  Type: HttpApi
  Properties:
    ApiId: !Ref HttpApi
    Method: POST
    Path: /shorten
# and on the redirect function:
UrlRedirect:
  Type: HttpApi
  Properties:
    ApiId: !Ref HttpApi
    Method: GET
    Path: /{key}
```

- `POST /shorten` answers `400` if the body is missing, not JSON, has no `url`, or the URL is not an absolute `http(s)` URL (see 1.4).
- `GET /{key}` answers `404` if the key was never issued.
- An HTTP API costs about a third of a REST API per request. The extras REST adds, such as caching, come from CloudFront here (section 5), which does them better.

### 1.2 The table

The whole data model is one table with a string partition key, `key`, and on-demand billing (`template.yaml`):

```yaml
UrlTable:
  Type: AWS::Serverless::SimpleTable
  DeletionPolicy: Delete
  UpdateReplacePolicy: Delete
  Properties:
    PrimaryKey:
      Name: key
      Type: String
```

- Each item is `{key, url, created_at}`.
- Access is pure key-value, so DynamoDB fits: nothing at rest costs money, and there is nothing to size.
- `SimpleTable` is SAM's shorthand. It creates a plain table with on-demand billing, which is what we want here.
- `DeletionPolicy: Delete` means deleting the stack deletes the data. That is right for a lab and wrong for anything you care about.

### 1.3 The key generator

A short key is 7 characters of base62 (`0-9a-zA-Z`), which gives 62^7, about 3.5 trillion, possible keys. The `key-generator` function draws a random number below that bound from `crypto/rand` and encodes it (`cmd/key-generator/main.go`):

```go
var maxKey = new(big.Int).Exp(big.NewInt(62), big.NewInt(keyLength), nil)

func generateKey() (string, error) {
	n, err := rand.Int(rand.Reader, maxKey)
	if err != nil {
		return "", fmt.Errorf("reading random source: %w", err)
	}
	key := toBase62(n.Uint64())
	// Left-pad so every key is exactly keyLength characters
	...
}
```

Random keys were chosen over hashing the URL. A hash gives free de-duplication, but it makes every key predictable from its URL and rules out a separate code per campaign. With random keys, a key tells you nothing about its URL.

This function is deliberately a separate Lambda, even though it does microseconds of work and costs a network hop. The idea is that other services can call it later, so the boundary is the deliverable. Four constraints keep it reusable:

- **It is pure.** It has no storage access. Checking uniqueness inside it would tie it to this table.
- **IAM enforces that.** Its role has zero DynamoDB permissions, so this is checked on the role, not just in the code.
- **It is invoked directly** with `lambda:InvokeFunction` scoped to its ARN. There is no API Gateway in front of it.
- **It knows nothing about URLs.** It is named `key-generator`, not `url-key-generator`, and it ignores the request body.

The tests for it use no AWS mocking at all. If they ever needed mocks, it would no longer be pure.

### 1.4 The shortener

`url-shortener` receives `POST /shorten`, asks the generator for a key, and writes the item. Two random keys can collide, so the write must fail rather than overwrite:

```go
_, err := h.ddb.PutItem(ctx, &dynamodb.PutItemInput{
	TableName: aws.String(h.tableName),
	Item: map[string]ddbtypes.AttributeValue{
		"key":        &ddbtypes.AttributeValueMemberS{Value: key},
		"url":        &ddbtypes.AttributeValueMemberS{Value: url},
		"created_at": &ddbtypes.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
	},
	// "key" is a DynamoDB reserved word, so reference it via a name placeholder.
	ConditionExpression:      aws.String("attribute_not_exists(#k)"),
	ExpressionAttributeNames: map[string]string{"#k": "key"},
})
```

- The condition makes the put atomic: DynamoDB checks and writes in one operation, so two concurrent requests can never both win the same key.
- On `ConditionalCheckFailedException` the handler asks the generator for a new key and tries again, up to `maxKeyAttempts = 5` times. Several collisions in a row mean the table is nearly full or the generator is broken, so it fails loudly instead of looping.
- Any other error is returned as a 500. The handler never swallows it.
- The URL is validated before anything is spent on it: it must be an absolute `http` or `https` URL with a host, at most 2,048 characters, with no spaces or control characters and no embedded credentials. Otherwise the request is a `400`, and the key generator is never called. This keeps `javascript:` and `file:` links, and host-spoofing links like `https://trusted.com@evil.com`, out of a redirect service, and keeps line breaks out of the `Location` header.
- The generator is called synchronously (`InvocationTypeRequestResponse`), and a `FunctionError` in its response is treated as a failure even though the invoke itself succeeded.
- The two functions are separate modules, so the response type is duplicated on purpose. The JSON is the contract between them.

The collision path was proven by shortening the key to one or two characters and hammering the endpoint.

### 1.5 The redirect

`url-redirect` receives `GET /{key}`, reads the item and answers `302` with a `Location` header, or `404` if the key was never issued. At this step it is a single `GetItem`. The next sections add layers in front of it, and every layer is a change to what this function does, so the handler keeps one structure: try each cheaper source first, and fall through to DynamoDB, the source of truth.

Its role has read-only access to the table (`DynamoDBReadPolicy`), and the shortener's has write-only (`DynamoDBWritePolicy`).

The first thing it checks is the key's shape. `key-generator` only ever issues 7 base62 characters, so anything else (`/favicon.ico`, a very long path, a probe) cannot be in the table. It is answered with a `404` straight away, without touching a cache or DynamoDB.

## 2. A short domain

The default API URL, `https://abc123.execute-api.us-east-1.amazonaws.com/Dev/xY7z`, is longer than most URLs worth shortening. So the next step is a real domain: an ACM certificate validated by DNS in your Route 53 hosted zone.

```yaml
Certificate:
  Type: AWS::CertificateManager::Certificate
  Properties:
    DomainName: !Ref DomainName
    ValidationMethod: DNS
    DomainValidationOptions:
      - DomainName: !Ref DomainName
        HostedZoneId: !Ref HostedZoneId
```

- CloudFormation writes the validation record into the hosted zone itself, so the first deploy blocks for a few minutes (up to about 30 if delegation of the domain is slow or missing).
- The domain name and the hosted zone id are not in the repository. They are stack parameters of type `AWS::SSM::Parameter::Value<String>`, resolved from Parameter Store under `/url-shortener/*` at deploy time. You create them once with `aws ssm put-parameter` (see [How to run it](#how-to-run-it)).
- The region is `us-east-1` on purpose. CloudFront needs its certificate there, and step 5 moves the domain to CloudFront.
- This step originally pointed the domain straight at API Gateway. Once the edge was added, the same domain became an alias to the CloudFront distribution, so the rework was small.

The domain choice matters more than it seems. Cheap TLDs that are heavily used for phishing through shorteners, such as `.xyz`, `.top` or `.click`, get filtered, and short links on them look broken.

## 3. The cache

Every redirect invoked a function and made a DynamoDB call for a value that practically never changes. So the next step puts caches in front, and measures each layer before adding the next.

### 3.1 L1: the in-process cache

The first layer is a map in the Lambda's own memory (`cmd/url-redirect/l1.go`):

```go
type l1Cache struct {
	mu         sync.Mutex
	m          map[string]entry
	ttl        time.Duration
	maxEntries int
	now        func() time.Time // swapped in tests
}
```

- It is deliberately not an LRU. When it fills up (5,000 entries by default), it drops everything and starts over. It is simple, and the hit rate depends on how concentrated the traffic is.
- Each execution environment has its own copy, and a new one starts empty.
- The TTL (60 seconds by default) is the only invalidation: it is the longest a repointed or deleted link can still be served from here.

### 3.2 L2: Valkey in its own stack

The second layer is a shared Valkey cache (ElastiCache), so a new execution environment does not start cold. The handler checks L1, then L2, then DynamoDB, and fills both caches on the way out.

ElastiCache is the first thing here that **bills while idle**, about $0.016 an hour for a `cache.t4g.micro`. So it lives in a separate CloudFormation stack, `cache/template.yaml`, created at the start of a session and deleted at the end. Deleting it never touches the VPC or the functions.

```yaml
CacheReplicationGroup:
  Type: AWS::ElastiCache::ReplicationGroup
  Properties:
    Engine: valkey
    CacheNodeType: !Ref NodeType     # cache.t4g.micro
    NumCacheClusters: 1              # one node, no replica
    CacheSubnetGroupName: !Ref CacheSubnetGroup
    SecurityGroupIds:
      - Fn::ImportValue: !Sub "${NetworkStackName}-cache-sg-id"
```

- The cache stack imports the private subnets and a security group from the main stack, through CloudFormation exports. That is why the main stack deploys first.
- The main stack has **no dependency** on the cache stack. The cache stack writes its address to the SSM parameter `/url-shortener/cache/endpoint`, and the redirect function reads that parameter at cold start. A missing parameter means L2 is simply off.
- The security group on the cache allows port 6379 only from the redirect function's security group.
- The node uses at-rest encryption but no transit encryption or authentication. Network isolation is the only protection here, which is fine for a lab and not for production.

### 3.3 The VPC without a NAT gateway

ElastiCache only lives inside a VPC, so the redirect function has to join one, and a VPC-attached Lambda loses its route to the internet. The naive fix is a NAT gateway, at about $32 a month, which is the most expensive mistake available in this project. It is not needed:

```yaml
DynamoDBEndpoint:
  Type: AWS::EC2::VPCEndpoint
  Properties:
    VpcId: !Ref Vpc
    ServiceName: !Sub "com.amazonaws.${AWS::Region}.dynamodb"
    VpcEndpointType: Gateway
    RouteTableIds:
      - !Ref PrivateRouteTable
```

| From a VPC Lambda | Through | Cost |
|---|---|---|
| DynamoDB, S3 | Gateway endpoint | free |
| SSM, Secrets Manager, STS, SNS | Interface endpoint | about $7 a month per subnet |
| The public internet | NAT gateway | about $32 a month |

- The private subnets have no route to the internet at all. The one gateway endpoint covers DynamoDB for free.
- The redirect function does call one other AWS service: SSM, to find the cache. That needs a paid interface endpoint, and one per subnet, because with a single endpoint about a third of cold starts landed in the other subnet and could not reach SSM.

### 3.4 Degrading gracefully

Every failure of L2 falls through to DynamoDB:

```go
case errors.Is(err, errL2Miss):
	// fall through to DynamoDB
default:
	log.Warn("l2 get failed; falling through to DynamoDB", "error", err, "key", key)
```

The client uses 75 ms timeouts and no retries. If the cache stack is deleted, the parameter disappears, new cold starts skip L2, and the service runs on L1 plus DynamoDB. This was verified by deleting the stack under load. See [Good to know](#good-to-know) for the one case where it is not clean.

The handler also logs which tier answered (`l1`, `l2` or `miss`), and metric filters turn that field into CloudWatch metrics with no call on the hot path.

## 4. Observability and safe deploys

With a cache, a VPC and an edge in the path, there are more places for redirects to break quietly, and the only way to find out would be to click a link. This section makes the system tell you.

### 4.1 Logs

Every function writes structured JSON with `log/slog`, tagged with the request id, so one request is greppable end to end and CloudWatch Logs Insights can filter on fields directly. The log groups are declared in the template, with 14-day retention:

```yaml
RedirectLogGroup:
  Type: AWS::Logs::LogGroup
  Properties:
    LogGroupName: !Sub "/aws/lambda/${AWS::StackName}-url-redirect"
    RetentionInDays: 14
```

Declaring them matters. A group that Lambda creates implicitly never expires its logs and outlives the stack.

### 4.2 Alarms on the symptom

The rule here is to **alarm on the symptom, not the implementation**. An alarm on the redirect Lambda's `Errors` metric is wrong twice:

- A `404` or a `500` that the handler returns cleanly is a successful invocation. A wrong table name shows zero Lambda errors while every redirect fails.
- An alarm tied to one function name goes blind, silently, once other layers start serving redirects.

So the page-worthy alarm is on the HTTP API's 5xx *rate*, and Lambda and DynamoDB metrics stay on the dashboard for diagnosis:

```yaml
Http5xxRateAlarm:
  Type: AWS::CloudWatch::Alarm
  Properties:
    Metrics:
      - Id: rate
        Expression: "IF(total >= 10, 100 * err / total, 0)"
        ReturnData: true
      # err and total are the API's 5xx and Count metrics, summed per minute
    Threshold: 5
    EvaluationPeriods: 5
    DatapointsToAlarm: 3
    TreatMissingData: notBreaching
```

- It is a rate, not a count, and it is floored: below 10 requests a minute it reports 0, so one bad request at 3 a.m. does not page anyone.
- Three bad minutes out of five trips it.
- `TreatMissingData: notBreaching` handles the quiet hours, when there are no data points at all.
- Alarms publish to an SNS topic, which fans out to a Slack channel through AWS Chatbot and to a backup email address. The Slack ids and the email are SSM parameters, like the domain.

The same logic produced a third alarm once CloudFront arrived: CloudFront's `5xxErrorRate` catches failures that never reach API Gateway (certificate, cache behaviour, a WAF misfire). It is a free metric, but only at 5-minute granularity, so it reacts slower than the API alarm by design.

### 4.3 The canary

Alarms need traffic. At 3 a.m. there is none, so a scheduled Lambda creates traffic. Every minute, `canary` runs `POST /shorten`, then `GET /{key}` against the public URL, checks for the `302` and a `Location`, and publishes a 1 or a 0 to `UrlShortener/Canary/Success`:

```yaml
CanaryAlarm:
  Type: AWS::CloudWatch::Alarm
  Properties:
    Namespace: UrlShortener/Canary
    MetricName: Success
    Statistic: Minimum
    Threshold: 1
    ComparisonOperator: LessThanThreshold
    DatapointsToAlarm: 3
    EvaluationPeriods: 5
    TreatMissingData: breaching
```

Here missing data means *breaching*, the opposite of the other alarm: a canary that goes silent is itself a failure. The metric write is allowed only for its own namespace, and a failed `PutMetricData` fails the function, so it never goes silent unnoticed.

### 4.4 Canary deploys with automatic rollback

Both alarms are also the gate for deploys. The redirect function publishes a new version on every deploy and shifts traffic gradually:

```yaml
AutoPublishAlias: live
DeploymentPreference:
  Type: Canary10Percent5Minutes
  Alarms:
    - !Sub "${AWS::StackName}-http-5xx-rate"
    - !Ref CanaryAlarm
```

CodeDeploy sends 10% of traffic to the new version, watches the alarms for five minutes, and rolls back by itself if either fires. For a system with no staging environment, this is the substitute for testing in a copy of production.

One detail: the 5xx alarm is referenced by name, not with `!Ref`. It depends on the HTTP API, the API depends on this function's alias, and the alias depends on this deployment group, so a `!Ref` would close a circular dependency. The canary alarm has no such dependency, so `!Ref` is fine there.

All three proofs have been run: forcing the alarm state and checking the notification path (alarm to SNS to Slack and email), breaking redirects for real and watching both alarms fire and recover, and shipping a deliberately broken redirect.

That last one is the one that matters. With about 10 requests a second of never-seen keys (so they miss the edge and reach the origin), the 5xx alarm fired 3 minutes 23 seconds after the traffic shift began. CodeDeploy stopped the deployment and finished rolling the alias back 3 seconds after that. CloudFormation then reported `UPDATE_ROLLBACK_COMPLETE`, so the deploy command exits with an error even though the service was healthy again within seconds.

## 5. The edge

Redirects still travelled to a region for immutable data, and the service was public enough to be abused. CloudFront went in after the cache on purpose: build the edge first and Redis has nothing left to do, and you never learn what it was worth.

### 5.1 CloudFront and the cache policy

The distribution answers for the custom domain, and its origin is the HTTP API. The cache policy is the important part:

```yaml
RedirectCachePolicy:
  Type: AWS::CloudFront::CachePolicy
  Properties:
    CachePolicyConfig:
      DefaultTTL: 0
      MinTTL: 0
      MaxTTL: 3600
      ParametersInCacheKeyAndForwardedToOrigin:
        CookiesConfig: { CookieBehavior: none }
        HeadersConfig: { HeaderBehavior: none }
        QueryStringsConfig: { QueryStringBehavior: none }
```

- The cache key is the path only: no cookies, headers or query strings.
- With a default TTL of 0, CloudFront trusts the origin's `Cache-Control`. A managed policy such as `CachingOptimized` would quietly cache for a day when the origin sends nothing, which is wrong for a service that must be able to kill a link.
- `/shorten` has its own behaviour with the managed `CachingDisabled` policy and all methods allowed, because CloudFront only offers three method sets and `POST` forces the full one.
- Edge 5xx responses are not cached (`ErrorCachingMinTTL: 0`), so an origin blip does not stay pinned after it recovers.
- API Gateway rejects the viewer's `Host` header, so the origin request policy is the managed `AllViewerExceptHostHeader`. Without it, every request is a 403.

### 5.2 302, not 301

The redirect status is the central decision of this step. A `301` is cached almost permanently by browsers: it is the fastest repeat redirect at zero cost, but you never see the click again, and the link can never be repointed or killed, because a browser cache cannot be invalidated.

The handler sends a `302` with an explicit `Cache-Control`:

```
cache-control: public, max-age=60, s-maxage=300
```

- `s-maxage=300` is for CloudFront, which can be purged with an invalidation.
- `max-age=60` is for browsers, which cannot be purged, so they get the shorter lifetime.
- A miss is cached too, for 30 seconds, so a key probed just before it is created does not stay a 404 for long.
- This is also the takedown story. A malware link has to die now. An invalidation clears the edge, and the browser TTL bounds the rest. The TTL argument and the takedown argument are the same argument.

All three values come from environment variables, so they can change without a code change.

### 5.3 The WAF

The WAF is **off by default**. It is the one piece that costs money for as long as it is attached, about $5 a month for the ACL and $1 for each rule. It is attached to CloudFront by `WebACLId`, because the regional association does not work for CloudFront.

```yaml
EnableWaf:
  Type: String
  Default: "false"
  AllowedValues: ["true", "false"]
```

When it is on, the rules run in this order:

1. **Allow-list.** CIDR ranges that skip everything else, so you can load-test without being blocked. They come from an SSM `StringList`, not from the template.
2. **`AmazonIpReputationList`** (managed).
3. **`KnownBadInputsRuleSet`** (managed).
4. **A rate limit** on `POST /shorten`: more than 100 requests in 5 minutes from one IP is blocked.

The rate limit exists because `/shorten` is the only expensive, write-path endpoint. Redirects are mostly absorbed by the cache.

**The bypass, and the fix.** The WAF only sees traffic that goes through CloudFront, and the raw `execute-api` URL is CloudFront's own origin, so it cannot be switched off. Anyone who found that URL could skip the rate limit. So CloudFront adds a secret header, `X-Origin-Verify`, to every request it forwards, and both functions answer `403` to a request without it (constant-time comparison, before any other work).

The secret is an SSM parameter, like the other settings. It is a plain `String`, so it is visible in the CloudFormation console and the function configuration: it is a speed bump for the origin URL, not a credential.

Turning it on takes two deploys. In the first (`EnforceOriginSecret=false`, the default), CloudFront starts sending the header and nothing is rejected. In the second (`EnforceOriginSecret=true`), the functions start checking it. Doing both at once would return `403` to some traffic while the new edge configuration is still propagating, and the canary deploy would read that as a failure and roll back.

## Results

Every layer is measured with the same frozen script (`scripts/loadtest.sh`), and each result goes into [RESULTS.md](RESULTS.md). The ones that tell the story:

| Layer | `GET /{key}` p50 | What changed |
|---|---|---|
| Core | 125 ms | One `GetItem` per redirect |
| Cache | 124 ms | One hot key, and L1 already answered, so there was nothing to gain |
| Edge | **6.9 ms** | CloudFront answers without touching a Lambda |

- At the edge, 86 to 88% of requests never reached the origin in the same load.
- The cache steps show their value on many-key traffic, not on one hot key. On a many-key workload the three tiers split about 66% L1, 26% L2 and 8% DynamoDB.
- After the edge took the easy traffic, the share reaching the Lambda was 12% L1, 40% L2 and 48% DynamoDB. The cache layers now serve the long tail that the edge misses.
- The `key-generator` hop costs about 2 ms of function time on the create path, which is a small price for a reusable boundary.
- Detection and recovery: with redirects deliberately broken, the canary alarm fired after about 3 minutes and recovered after about 3.5.

The create path has a p99 near 2 seconds in every run. It sits on about 1% of the traffic, so it was left alone.

## Good to know

- The snippets are shortened. The full code is in `template.yaml` and under `cmd/`.
- The numbers above are from a single account and region, with `hey`, not a benchmark.
- **A config-only change does not publish a new Lambda version by default.** SAM's `AutoPublishAlias` only publishes when the code changes. Changing an environment variable on the redirect function updated `$LATEST` while the `live` alias stayed on the old version, with no CodeDeploy deployment at all. The fix is `AutoPublishAliasAllProperties: true`, which also makes config changes go through the canary gate. We found it when the origin-secret rollout enforced on the shortener but not on the redirect.
- **Deleting the cache is clean, with one caveat.** After `make cache-down`, redirects kept answering `302` with no `5xx` and no latency bump, because new cold starts find no SSM parameter and skip L2. That test did not cover an execution environment that was already warm and already holding the old address. By reading the code, such an environment would keep dialing it and pay the 75 ms timeout on each L1 miss until it is recycled. That case has not been measured.
- **The default Lambda concurrency limit is low on a new account.** Ours was 10. Hammering the redirect at 50 concurrent requests gave 827 throttles and `503`s, because API Gateway turns a Lambda `429` into a `503`. `POST /shorten` is hit harder, because the synchronous call to `key-generator` holds two concurrency slots per request. Request an increase under Service Quotas ("Concurrent executions") before load testing. The frozen load test runs the create path at 5 concurrent requests and the redirect path at 10 for this reason.
- **Idle cost.** The cache stack, the WAF and the SSM interface endpoints are the parts that bill while idle. Everything else is close to free.
- **A deploy is not always a no-op on failure.** A failed rollback can strand a stack in `UPDATE_ROLLBACK_FAILED`.

## Next steps

These come out of reviewing the code, roughly in order of importance:

- **Rate-limit redirects.** A scan that sends well-formed random keys still reaches DynamoDB, because each key is its own cache entry and the 30-second negative cache does not help. The WAF only rate-limits `POST /shorten`, so a rate rule on `GET` would close that.
- **Never cache forever.** `CACHE_TTL_SECONDS=0` disables L1, but L2 would be created with a TTL of 0, which Redis clients treat as "no expiry". L2 should be skipped when the TTL is not positive.
- **Stop retrying a dead cache.** Add a small circuit breaker that turns L2 off for a while after a failure.
- **Expire the canary's rows.** The canary writes a row every minute and nothing removes them. A DynamoDB TTL attribute fixes that.
- **Log less.** The destination URL is logged at INFO and can contain tokens.
- **Analytics and a frontend.** Click counts from DynamoDB Streams (or CloudFront logs, since most hits never reach the origin), and a small page for creating links, are the two optional steps in [PLANNING.md](PLANNING.md).

## How to run it

The step-by-step guides are in `docs/` ([step 1](docs/STEP-1-GUIDE.md) to [step 5](docs/STEP-5-GUIDE.md)), and the roadmap and its reasoning are in [PLANNING.md](PLANNING.md).

### 1. Prerequisites

- An AWS account with a non-root identity, the AWS CLI configured with credentials, and the [SAM CLI](https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/serverless-sam-cli-install.html).
- Go 1.21 or newer, and Docker for `sam local`.
- A domain with a **Route 53 public hosted zone**, delegated from the registrar. `dig +short NS <domain> @1.1.1.1` must return the zone's `awsdns-*` servers.
- Everything lives in `us-east-1`.

The stack reads its settings from SSM Parameter Store. Create them once:

```bash
aws ssm put-parameter --name /url-shortener/domain-name        --type String --value <domain>
aws ssm put-parameter --name /url-shortener/hosted-zone-id     --type String --value <zone-id>
aws ssm put-parameter --name /url-shortener/notification-email --type String --value <email>
aws ssm put-parameter --name /url-shortener/slack-workspace-id --type String --value <workspace-id>
aws ssm put-parameter --name /url-shortener/slack-channel-id   --type String --value <channel-id>
aws ssm put-parameter --name /url-shortener/waf-allowed-ips    --type StringList --value "<your-ip>/32"
aws ssm put-parameter --name /url-shortener/origin-secret       --type String --value "$(openssl rand -hex 32)"
```

The `/32` suffix is required: the IP set rejects an address without a prefix and the deploy rolls back.

### 2. Deploy the main stack

```bash
make          # wraps `sam build`
sam deploy
```

`samconfig.toml` sets the stack name (`url-shortener`) and the region. The first deploy waits while ACM validates the certificate. The outputs are `CustomDomainUrl` (the HTTPS URL to use), `UrlShortenerApi` (the raw `execute-api` URL, which rejects requests once the origin secret is enforced), the dashboard URL and the alarm names.

Turn on the origin check in a second deploy, once the first has finished:

```bash
sam deploy --parameter-overrides EnforceOriginSecret=true
```

After that, the raw `execute-api` URL answers `403` and only the custom domain works. Test it with `curl -si https://<api-id>.execute-api.us-east-1.amazonaws.com/Dev/<key>`.

### 3. Check that it works

Run the two requests from [1.1](#11-the-http-api) against your domain. A healthy redirect also carries the cache header from section 5.2:

```bash
curl -si "https://<domain>/<key>"
# cache-control: public, max-age=60, s-maxage=300
```

Request the same key a second time. If CloudFront answered, the response has `x-cache: Hit from cloudfront`.

### 4. Optional: the cache stack and the WAF

The cache stack is created at the start of a session and deleted at the end:

```bash
make cache-up      # deploy ElastiCache and print the endpoint (5 to 10 minutes)
make cache-status  # StackStatus, or "not deployed"
make cache-down    # delete it before ending the session
```

Run `sam deploy` first, because the cache stack imports the VPC from it. The redirect function reads the endpoint at cold start, so L2 engages for containers that start after `make cache-up`, a few minutes later. To force it, redeploy.

To turn the WAF on or off:

```bash
sam deploy --parameter-overrides EnableWaf=true    # or false
```

CloudFormation remembers the value across later deploys. The allow-list resolves at deploy time, so after changing the SSM parameter you must run `sam deploy` again.

### 5. Tests and the load test

Each module is tested on its own:

```bash
for d in cmd/key-generator cmd/url-redirect cmd/url-shortener cmd/canary; do
  echo "=== $d ==="; (cd "$d" && go test ./...)
done
```

The redirect tests run against fakes and an in-memory Valkey (`miniredis`), so they need no AWS access. Useful flags: `-v`, `-cover`, `-run TestName`, `-count=1`.

The load test drives the create and redirect paths with [`hey`](https://github.com/rakyll/hey):

```bash
BASE_URL=https://<domain> ./scripts/loadtest.sh 100
```

Its default run sends 250 `POST /shorten` requests in well under a minute, which the WAF rate limit would block, so add your own IP to the allow-list first.

### 6. Tear it down

```bash
make cache-down    # if the cache stack is up
sam delete
```

Do this when you are done with a session. The table is deleted with the stack, and the cache node, the WAF and the interface endpoints are billed by the hour.
