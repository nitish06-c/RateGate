# How This Project Works — Complete Internals

This document explains every part of the codebase: what it does, why it exists, how
the pieces connect, and what you should be able to say about it in an interview.
Read this top to bottom once, then use it as a reference.

---

## The Problem This Solves

You have an API. You want to limit each client to 100 requests per minute.

The naive solution: keep a counter in memory on the server. When it hits 100, return 429.

This breaks the moment you run two server instances. Each instance has its own counter.
A client sends 60 requests to instance A and 60 requests to instance B. Both counters
say "60, still under 100." Both allow everything. The client got 120 requests through.

The real problem: **rate limit state must be shared across all nodes.**

---

## The Core Solution

Move the counter out of the application and into Redis. Every node reads from and writes
to the same Redis instance. Now there is one counter, shared globally.

But moving to Redis introduces a new problem: the read-modify-write race condition.

```
Node A: reads count = 99
Node B: reads count = 99
Node A: 99 < 100, increments → count = 100, returns ALLOWED
Node B: 99 < 100, increments → count = 101, returns ALLOWED  ← wrong
```

Both nodes read 99 before either wrote back. Both allowed. You exceeded the limit.

The solution: **run the entire check atomically as a Lua script on Redis.**

Redis executes Lua scripts in its single-threaded model. No other command runs while
a script is executing. The read and the write happen as one indivisible operation.
No interleaving is possible.

---

## The Algorithm: Sliding Window Log

There are several rate limiting algorithms. We use the sliding window log.

**How it works:**

Every request is stored as a timestamped entry in a Redis sorted set (ZSET).
A ZSET is a set where every member has a numeric score. We use the timestamp
as the score, which lets us do range queries by time.

When a request arrives:
1. Remove all entries with timestamps older than `now - window`
2. Count remaining entries
3. If count < limit: add this request, return ALLOWED
4. If count >= limit: return DENIED, tell the client when the oldest entry expires

**Why this algorithm:**

- **Precise.** Fixed window counters have a 2x burst problem at window boundaries.
  If the window resets at :00 and :60, a client can send 100 requests at :59 and
  100 more at :01 — 200 requests in 2 seconds with a "100 per minute" limit.
  Sliding window log has no boundary. The window is always "the last N seconds
  from right now."

- **Explainable.** You can draw the timeline on a whiteboard and trace exactly
  what happens for any sequence of requests.

- **Redis-native.** ZSETs with score-based range queries are exactly what Redis
  was built for. `ZREMRANGEBYSCORE` and `ZCARD` are O(log N) operations.

**The tradeoff:**

Memory is O(N) per key, where N is the number of requests in the window. If a key
has a limit of 10,000 requests per minute and is at capacity, you store 10,000 ZSET
entries. For most API rate limiting this is fine. At extreme scale you would switch
to a sliding window counter (two fixed buckets with interpolation), which is O(1)
memory but approximate.

---

## File-by-File Explanation

### `scripts/sliding_window.lua`

This is the most important file in the project. Everything else wraps it.

```lua
local key = KEYS[1]
local window = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local request_id = ARGV[3]
```

The script receives three arguments:
- `KEYS[1]`: the Redis key for this client's ZSET (e.g. `rl:default:1.2.3.4`)
- `ARGV[1]`: window size in microseconds (e.g. 60,000,000 for 60 seconds)
- `ARGV[2]`: the limit (e.g. 100)
- `ARGV[3]`: a unique ID for this request

```lua
local time = redis.call('TIME')
local now_us = tonumber(time[1]) * 1000000 + tonumber(time[2])
```

`redis.call('TIME')` returns the Redis server's current time as
`{seconds, microseconds}`. We combine them into a single microsecond timestamp.

**Why use Redis time instead of the application clock?**
If Node A's clock says 12:00:00.000 and Node B's clock says 12:00:00.003, they
disagree on which entries are inside the window. By using Redis's own clock,
all nodes see exactly the same time. Clock skew between application nodes is
eliminated entirely.

```lua
redis.call('ZREMRANGEBYSCORE', key, '-inf', window_start)
```

Remove all entries older than the window start. This is the "sliding" in sliding
window — expired entries are pruned on every request.

```lua
local count = redis.call('ZCARD', key)
```

Count remaining entries. This is the current number of requests in the window.

```lua
if count < limit then
    redis.call('ZADD', key, now_us, now_us .. ':' .. request_id)
    redis.call('PEXPIRE', key, math.ceil(window / 1000) + 1000)
    return {1, limit - count - 1, reset_at, 0}
```

Under the limit: add this request to the ZSET. The member is `timestamp:requestID`.
The score is the timestamp (used for range queries). The member must be unique —
two requests at the exact same microsecond would overwrite each other without the
random ID suffix.

`PEXPIRE` sets a TTL on the key in milliseconds. If a key has no traffic for
a full window, Redis automatically deletes it. This prevents unbounded memory growth.
We add 1000ms buffer so the key doesn't expire while it might still have valid entries.

```lua
else
    local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
    local retry_after = math.ceil((oldest_score + window - now_us) / 1000000)
    return {0, 0, reset_at, retry_after}
```

Over the limit: find the oldest entry. When it expires (oldest_score + window),
a slot opens. That's the `Retry-After` value — how many seconds until the client
can try again.

**Return values:** `{allowed (1/0), remaining, reset_at_unix, retry_after_seconds}`

**Why this is atomic:**
Redis runs Lua scripts in its single-threaded event loop. When this script starts,
nothing else executes on Redis until it finishes. The ZREMRANGEBYSCORE, ZCARD, and
ZADD happen as one unit. No other script from any other node can interleave.

---

### `internal/limiter/limiter.go`

Defines the core interface that everything depends on.

```go
type Rule struct {
    Name   string
    Limit  int64
    Window time.Duration
}

type Decision struct {
    Allowed   bool
    Limit     int64
    Remaining int64
    ResetAt   time.Time
    RetryAt   time.Duration
}

type Limiter interface {
    Allow(ctx context.Context, key string, rule Rule) (*Decision, error)
}
```

`Limiter` is the contract. Anything that implements `Allow` is a Limiter.
This is what enables the decorator pattern used in the metrics layer —
`InstrumentedLimiter` and `SlidingWindowLimiter` both satisfy this interface,
so the server can't tell them apart.

---

### `internal/redis/scripts.go`

Embeds the Lua script as a Go variable using `redis.NewScript()`.

```go
var slidingWindowScript = redis.NewScript(`...lua source...`)
```

`redis.NewScript()` is a go-redis utility that handles EVALSHA automatically.
On the first call it sends the full Lua source via `EVAL`. Redis caches it by
SHA1 hash. Every subsequent call uses `EVALSHA <hash>` — sending just 40
characters instead of the full script. If Redis restarts and loses the cache,
go-redis falls back to `EVAL` transparently and re-caches. You never have to
manage this yourself.

---

### `internal/redis/client.go`

Implements `Limiter` using the Lua script.

```go
func (s *SlidingWindowLimiter) Allow(ctx context.Context, key string, rule Rule) (*Decision, error) {
    redisKey := fmt.Sprintf("%s:%s", s.prefix, key)
    ...
    result, err := slidingWindowScript.Run(ctx, s.client,
        []string{redisKey},
        rule.Window.Microseconds(),
        rule.Limit,
        requestID,
    ).Int64Slice()
```

The Redis key format is `{prefix}:{key}`. The prefix (set to `"rl"` in production)
isolates this service's keys from anything else using the same Redis instance.
The key itself comes from the middleware (e.g. `"default:1.2.3.4"`).

`randomID()` generates 8 random bytes as hex. This is the unique request ID
appended to each ZSET member to prevent collisions at identical microsecond timestamps.

The Lua script returns `{allowed, remaining, reset_at_unix, retry_after_seconds}`.
We parse these into a `Decision` struct and return it.

---

### `internal/config/config.go`

Loads and validates `rules.yaml`.

```go
type Duration struct {
    time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
    dur, err := time.ParseDuration(value.Value)
    ...
}
```

Go's `time.Duration` doesn't unmarshal from YAML strings like `"60s"` by default.
We wrap it with a custom `UnmarshalYAML` method that calls `time.ParseDuration`.
This is a common Go pattern for extending standard types with custom serialization.

The validator checks: redis addr is set, all rules have names, limits > 0,
windows > 0, and key sources are valid (`"ip"` or `"header:<name>"`).

---

### `internal/middleware/http.go`

The HTTP middleware. This is where the limiter integrates with the HTTP layer.

**`RateLimit(lim Limiter, rules []RuleConfig)`**

Returns an `http.Handler` wrapper. Every request goes through this:

1. **Match a rule** — find the most specific path prefix rule, fall back to `default`
2. **Extract the key** — client IP or header value
3. **Scope the key** — prepend the rule name: `"auth:1.2.3.4"`
4. **Call the limiter** — `lim.Allow(ctx, scopedKey, rule)`
5. **Handle the result** — set headers, either call `next` or return 429

**`matchRule`** iterates all rules, finds those whose `path_prefix` matches the
request path, and picks the longest match. Longest match = most specific rule.
`/auth/login` beats `/auth` for a request to `/auth/login/reset`.

**`extractKey`** handles two cases:
- `"ip"`: reads `X-Forwarded-For` (first entry, leftmost = original client) or
  falls back to `RemoteAddr`
- `"header:<name>"`: reads the named header, falls back to IP if absent

**Why scope by rule name:** a client IP hitting `/auth/login` (limit: 5) and
`/api/search` (limit: 1000) would share one Redis ZSET without rule scoping.
Their counters would interfere. `auth:1.2.3.4` and `api:1.2.3.4` are independent.

**Fail open on error:** if `lim.Allow` returns an error (Redis unreachable),
the request is allowed through with an `X-RateLimit-Error` header. The rate
limiter should not be a single point of failure for the entire API.

**Response headers set on every request:**
- `X-RateLimit-Limit`: the configured limit
- `X-RateLimit-Remaining`: slots left in the current window
- `X-RateLimit-Reset`: unix timestamp when the window resets

**On 429, also:**
- `Retry-After`: seconds until a slot opens (computed from oldest ZSET entry)

---

### `internal/metrics/metrics.go`

The `InstrumentedLimiter` decorator.

```go
type InstrumentedLimiter struct {
    inner limiter.Limiter
}

func (l *InstrumentedLimiter) Allow(...) (*Decision, error) {
    start := time.Now()
    dec, err := l.inner.Allow(ctx, key, rule)
    duration := time.Since(start).Seconds()

    decisionDuration.WithLabelValues(rule.Name).Observe(duration)

    if err != nil {
        redisErrors.Inc()
        return dec, err
    }

    result := "allowed"
    if !dec.Allowed { result = "denied" }
    decisions.WithLabelValues(result, rule.Name).Inc()

    return dec, nil
}
```

The decorator pattern: `InstrumentedLimiter` implements `Limiter` by wrapping
another `Limiter`. It times the inner call, records the result, then returns
exactly what the inner limiter returned. The server never knows it's talking
to a wrapper.

**Three metrics:**

`ratelimit_decisions_total` is a **counter** — only goes up. You query it as a
rate: `rate(ratelimit_decisions_total[1m])`. Labels (`result`, `rule`) let you
slice: "deny rate on the auth rule specifically."

`ratelimit_decision_duration_seconds` is a **histogram** — records latency
distribution in pre-defined buckets. From this Prometheus can compute P50, P95,
P99 without storing every individual measurement. The query:
`histogram_quantile(0.99, rate(ratelimit_decision_duration_seconds_bucket[5m]))`

`ratelimit_redis_errors_total` is a **counter** — tracks Redis failures.
Alert on `rate(ratelimit_redis_errors_total[1m]) > 0`.

---

### `internal/server/server.go`

Sets up the HTTP server.

```go
mux.HandleFunc("/health", handleHealth)
mux.Handle("/metrics", promhttp.Handler())
mux.HandleFunc("/", handleEcho)

Handler: middleware.RateLimit(lim, cfg.Rules)(mux)
```

The middleware wraps the entire mux. Every request — including `/health` and
`/metrics` — goes through rate limiting. The `/health` path matches the
`default` rule.

Timeouts are set explicitly: `ReadTimeout: 5s`, `WriteTimeout: 10s`,
`IdleTimeout: 60s`. Without these, slow clients can hold connections open
indefinitely.

Graceful shutdown: `srv.Shutdown(ctx)` waits for in-flight requests to complete
(up to 10 seconds) before stopping. This means a `docker stop` or Ctrl+C won't
cut off requests mid-flight.

---

### `cmd/limiter/main.go`

The entry point. Wires everything together in order:

1. Parse flags (`-config`)
2. Load and validate config
3. Connect to Redis, verify with `PING`
4. Create `SlidingWindowLimiter`
5. Wrap it with `InstrumentedLimiter`
6. Create and start the HTTP server
7. Wait for SIGINT or SIGTERM
8. Graceful shutdown

Nothing in this file is novel — it's all wiring. The interesting code is in
the packages it calls.

---

### `integration/distributed_test.go`

The most important test in the project.

**`TestDistributed_ExactCount`**

Starts 3 in-process HTTP servers on ports 19000-19002, all sharing one Redis client.
Fires 200 concurrent requests round-robin across all 3 servers. Asserts exactly
100 are allowed.

Why this test proves the claim: if the Lua script had a race condition (e.g. using
separate GET/SET instead of atomic Lua), concurrent requests from different nodes
could both read count=99, both allow, and you'd see 105 or 108 allowed — different
every run. Exactly 100, deterministically, every run, is only possible with atomic
execution.

**`TestDistributed_KeysAreIndependent`**

Fires 200 requests for each of two different API keys across 2 nodes. Both keys
get exactly 100 allowed. Verifies that separate keys use separate Redis ZSETs
and don't interfere.

**`//go:build integration`**

Build tag at the top of the file. `go test ./...` skips this file. Only runs with
`-tags=integration`. This is the Go convention for tests that require real
infrastructure (Redis). Unit tests should run anywhere with no dependencies.

**`client.FlushDB(ctx)` before each test**

Wipes all Redis keys. Without this, leftover keys from a previous test run make
the count wrong. Test 2 would inherit the state from Test 1 and fail non-deterministically.
Tests that share infrastructure must clean up before they run.

---

### `bench/bench_test.go`

Three Go benchmarks using `testing.B`.

**`BenchmarkAllow`** — sequential, one goroutine. Measures raw Redis round-trip
latency. Result: ~0.11ms on loopback. This is the floor.

**`BenchmarkAllowParallel`** — `b.RunParallel` with GOMAXPROCS goroutines all
hitting the same key. Result: ~0.033ms per op. Faster than sequential because
goroutines pipeline — while one waits for Redis, others are already sending.
Throughput: ~30K decisions/sec.

**`BenchmarkAllowDistinctKeys`** — parallel but each goroutine uses its own key.
Slightly faster than parallel same-key because there's no ZSET-level contention
in Redis — each script operates on a different sorted set.

**`712 B/op, 27 allocs/op`** — every decision allocates 712 bytes across 27
heap allocations. These come from go-redis creating request/response objects and
the Lua result parsing. At 30K/sec that's ~21MB/sec of allocation, manageable
but worth knowing.

**Loopback caveat:** these numbers use Redis via Docker on the same machine.
Add 1-5ms of network RTT for a real deployment. Sequential latency would be
1-5ms, not 0.11ms.

---

### `bench/loadtest/main.go`

A CLI load tester that fires real HTTP requests at the running service.

```
go run ./bench/loadtest/main.go -url http://localhost:8081/ -n 2000 -c 50
```

Spawns `-c` goroutines, each pulling work from a shared channel of `-n` total
requests. Records latency for each request. After all requests complete, sorts
latencies and computes percentiles.

Results through Docker Compose (~8K req/s, P50 4.66ms, P99 64ms) include the
full cost: Docker networking, HTTP parsing, middleware, Redis over virtual network.
The P99 spike to 64ms is Docker-on-Mac scheduler noise. On Linux this would be
under 5ms.

---

## How a Request Flows End to End

```
1. Client sends GET /api/search
   Header: X-API-Key: user-abc

2. Load balancer routes to any Limiter node (say, Node 2)

3. HTTP server receives request
   Passes to middleware.RateLimit

4. matchRule("/api/search", rules)
   → finds rule "api" (path_prefix: /api)
   → rule: limit=1000, window=60s, key_source="header:X-API-Key"

5. extractKey(r, "header:X-API-Key")
   → r.Header.Get("X-API-Key") = "user-abc"
   → returns "user-abc"

6. scopedKey = "api:user-abc"

7. InstrumentedLimiter.Allow(ctx, "api:user-abc", rule)
   → starts timer
   → calls SlidingWindowLimiter.Allow(...)

8. SlidingWindowLimiter.Allow
   → redisKey = "rl:api:user-abc"
   → generates requestID = "a3f8c2d1..."
   → runs Lua script on Redis:
       KEYS[1] = "rl:api:user-abc"
       ARGV[1] = 60000000  (60s in microseconds)
       ARGV[2] = 1000
       ARGV[3] = "a3f8c2d1..."

9. Lua script (atomic on Redis):
   → TIME → now_us = 1714000123456789
   → ZREMRANGEBYSCORE "rl:api:user-abc" -inf 1714000063456789
   → ZCARD → count = 47
   → 47 < 1000 → ZADD score=now_us member="1714000123456789:a3f8c2d1..."
   → PEXPIRE 61000ms
   → return {1, 952, 1714000183, 0}

10. Decision{Allowed: true, Remaining: 952, ResetAt: ..., RetryAt: 0}

11. InstrumentedLimiter records:
    → decisions_total{result="allowed", rule="api"} + 1
    → decision_duration_seconds{rule="api"}.Observe(0.00084)
    → stops timer

12. Middleware receives Decision
    → sets X-RateLimit-Limit: 1000
    → sets X-RateLimit-Remaining: 952
    → sets X-RateLimit-Reset: 1714000183
    → calls next handler

13. Response: 200 OK with rate limit headers
```

---

## Distributed Systems Concepts in This Project

**Atomicity** — the Lua script is the atomic unit. Read + write cannot be observed
in a partial state by any other operation.

**Consistency** — all nodes share one Redis instance. Every node sees the same
counter. There is no eventual consistency here; decisions are globally consistent
within Redis's single-node consistency model.

**Clock skew elimination** — using Redis server time inside the Lua script means
all nodes agree on what "now" means. Application node clocks are irrelevant.

**Stateless nodes** — limiter nodes hold no state. All state is in Redis. A node
crash loses nothing. Adding nodes requires no coordination — just point them at
the same Redis.

**Fail open** — the deliberate choice to allow traffic when Redis is down rather
than rejecting everything. Rate limiting is a protective layer, not the primary
path. Availability of the API is more important than perfect rate limit enforcement
during an outage.

**Key namespacing** — the three-level key format `{prefix}:{rule}:{identity}` provides
environment isolation (prefix), per-rule isolation (rule), and per-client isolation
(identity). Each level prevents a different category of interference.

---

## What You Should Be Able to Say in an Interview

**"Walk me through how your rate limiter works."**

"A client request hits any limiter node. The middleware extracts the client identity
from the IP or a header, scopes it by rule name, and calls the limiter. The limiter
runs a Lua script on Redis that atomically removes expired entries from a sorted set,
counts remaining entries, and either records the request and returns allowed, or
returns denied with a retry-after value. The node sets standard rate limit headers
and either calls the next handler or returns 429."

**"Why Lua scripts?"**

"Rate limiting is a read-modify-write operation. Without atomicity, two concurrent
requests from different nodes can both read the counter below the limit and both be
allowed, exceeding it. Redis executes Lua scripts in its single-threaded model — the
entire check-and-increment is one indivisible operation. No two scripts can interleave."

**"What happens if Redis goes down?"**

"The limiter fails open — requests are allowed through with an X-RateLimit-Error
header. I chose this because the rate limiter should not be a single point of failure
for the API. During a Redis outage, slightly exceeding a rate limit is a better
outcome than rejecting all traffic. This is observable via the redis_errors_total
Prometheus counter."

**"How do you know it actually works across multiple nodes?"**

"There's an integration test that starts 3 in-process HTTP servers sharing one Redis,
fires 200 concurrent requests round-robin, and asserts exactly 100 are allowed. Not
approximately 100 — exactly 100, every run. That determinism is only possible because
the Lua script is atomic. A race condition would produce inconsistent counts."

**"What's the memory tradeoff of sliding window log?"**

"O(N) per key, where N is the number of requests in the window. For a key with limit
10,000 and at capacity, you're storing 10,000 ZSET entries. The alternative is the
sliding window counter — two fixed buckets with linear interpolation — which is O(1)
memory but approximate. I chose the log because precision matters more than memory
for this project scope, and because it's more interesting to explain."
