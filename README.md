# RateGate

Rate limiting across multiple nodes is harder than it looks. A local counter per node breaks as soon as you run two instances — both see the counter below the limit, both allow, and you've exceeded it. RateGate enforces shared quotas across multiple nodes using Redis as the coordination layer, with all counter operations running atomically via Lua scripts.

## How it works

```
                    ┌─────────────┐
          ┌────────▶│  Limiter 1  │──────┐
          │         └─────────────┘      │
Client ───┤         ┌─────────────┐      ├──▶ Redis
          ├────────▶│  Limiter 2  │──────┤
          │         └─────────────┘      │
          └────────▶│  Limiter 3  │──────┘
                    └─────────────┘
                           │
                      Prometheus
```

Each request hits any limiter node. The node runs a Lua script on Redis that atomically prunes expired entries, counts the window, and either records the request or denies it. Because Redis runs Lua scripts single-threaded, concurrent decisions from different nodes are serialized — the read-modify-write is indivisible.

Nodes are completely stateless. All state lives in Redis. A node crash loses nothing.

## Design decisions

| Decision | Choice | Why | Tradeoff |
|---|---|---|---|
| Algorithm | Sliding window log | No boundary burst artifacts like fixed window; precise per-window counting | O(N) memory per key vs O(1) for approximate counters |
| Atomicity | Redis Lua scripts | Single-threaded execution eliminates race conditions without distributed locks | Blocks other Redis commands while running |
| Clock | `redis.call('TIME')` | All nodes share Redis's clock; application node clock skew is irrelevant | Depends on Redis clock accuracy |
| Redis failure | Fail open | Rate limiter shouldn't take down the whole API | May over-allow during Redis outage |

## Quick start

Requires Docker and Go 1.20+.

```bash
make docker-up
```

This starts 3 limiter nodes, Redis, and Prometheus. Once healthy:

```bash
# See rate limiting in action — default rule is 10 req/60s
for i in $(seq 1 12); do
  printf "Request $i: "
  curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8081/
done

# Auth endpoint has a tighter limit (3 req/60s)
for i in $(seq 1 5); do
  printf "Request $i: "
  curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8081/auth/login
done

# Check metrics
curl -s http://localhost:8081/metrics | grep ratelimit

# Prometheus UI
open http://localhost:9090

make docker-down
```

## API

Allowed and denied requests both get these headers:

```
X-RateLimit-Limit:     100
X-RateLimit-Remaining: 43
X-RateLimit-Reset:     1714000000
```

429 responses also include:

```
Retry-After: 12
```

Endpoints: `GET /health`, `GET /metrics`

## Configuration

```yaml
redis:
  addr: "localhost:6379"

server:
  addr: ":8080"

rules:
  - name: default
    limit: 100
    window: 60s
    key_source: ip

  - name: auth
    match:
      path_prefix: /auth/login
    limit: 5
    window: 300s
    key_source: ip

  - name: api
    match:
      path_prefix: /api/v1
    limit: 1000
    window: 60s
    key_source: "header:X-API-Key"
```

Rules match by path prefix; longest match wins. If nothing matches, the `default` rule is used. `key_source` can be `ip` or `header:<name>` — header rules fall back to IP if the header is absent.

## Tests

```bash
# Unit tests, no infrastructure needed
go test -race ./internal/...

# The important one: 3 nodes, 1 Redis, limit 100, 200 requests → exactly 100 allowed
go test -race -tags=integration -timeout=60s ./integration/...

# Benchmarks
go test -bench=. -benchmem -benchtime=5s ./bench/...

# Load test against Docker Compose
go run ./bench/loadtest/main.go -url http://localhost:8081/ -n 2000 -c 50
```

## Performance

Measured on Apple M2, Redis over Docker loopback. Add 1-5ms for a real network hop to Redis.

| | Latency | Throughput |
|---|---|---|
| Sequential (1 goroutine) | ~0.11ms | ~9K/s |
| Parallel, same key (8 goroutines) | ~0.033ms | ~30K/s |
| Parallel, distinct keys (8 goroutines) | ~0.032ms | ~31K/s |

Through Docker Compose (full HTTP stack):

| P50 | P95 | P99 | Throughput |
|---|---|---|---|
| 4.66ms | 8.69ms | 64ms* | ~8K req/s |

*The P99 spike is Docker-on-Mac VM scheduler noise. On Linux it'd be under 5ms.

## Failure modes

**Node crash** — no impact. Nodes are stateless; the load balancer routes around it.

**Redis unreachable** — requests fail open with an `X-RateLimit-Error` header. The limiter shouldn't take down the whole API. Observable via `ratelimit_redis_errors_total` in Prometheus.

**Clock skew** — not an issue. All timestamps come from `redis.call('TIME')` inside the Lua script. Application node clocks don't matter.

## What I'd change for production

- Redis Sentinel or Cluster for HA — a single Redis instance is a SPOF
- Local token bucket fallback per node during Redis outages, using `global_limit / node_count`
- Hot key mitigation for high-traffic identities via local caching with periodic Redis sync
- Sliding window counter instead of log for very high-volume keys where O(N) ZSET memory is a concern

## Structure

```
cmd/limiter/       entry point
internal/
  config/          YAML loading and validation
  limiter/         core interface and types
  redis/           Lua script execution
  metrics/         Prometheus instrumentation (decorator over Limiter)
  middleware/      HTTP middleware, rule matching, key extraction
  server/          HTTP server, graceful shutdown
integration/       multi-node correctness tests
bench/             benchmarks and load test CLI
deployments/       Docker Compose, Prometheus config
scripts/           sliding_window.lua
```
