# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build          # Compile to bin/ratelimiter
make run            # Build and run with configs/rules.yaml (requires Redis on localhost:6379)
make test           # go test -v -race ./...
make lint           # go vet ./...
make bench          # go test -bench=. -benchmem ./bench/...
make integration    # go test -v -race -tags=integration -timeout=60s ./integration/...
make docker-up      # Start 3 limiter nodes + Redis + Prometheus via Docker Compose
make docker-down    # Stop and remove all Docker Compose resources
```

Run a single test package:
```bash
go test -race ./internal/middleware/...
go test -race -run TestRuleMatching ./internal/middleware/...
```

Run the load test CLI (requires a running service):
```bash
go run ./bench/loadtest/main.go -url http://localhost:8081/ -n 2000 -c 50
```

Integration tests require Redis and the `-tags=integration` build flag. Unit tests under `internal/` do not require Redis (they use stub limiters injected via the `Limiter` interface).

## Architecture

RateGate is a distributed, Redis-backed HTTP rate limiter service. Multiple stateless nodes share a single Redis instance, which is the sole source of truth for rate limit state.

### Request flow

1. HTTP request arrives at a node
2. `internal/middleware/http.go` matches the request path to a rule (longest prefix wins, falls back to `default`)
3. Middleware extracts the client key (IP or header value, scoped by rule name: `rl:<rule>:<key>`)
4. `internal/redis/client.go` executes a Lua script atomically on Redis (sliding window log in a ZSET)
5. The Lua script prunes expired entries, counts remaining, and either admits or denies — returning remaining quota and retry-after
6. Middleware sets `X-RateLimit-*` headers; returns 429 with `Retry-After` on deny
7. `internal/metrics/metrics.go` wraps the limiter as a decorator, recording Prometheus counters and latency histograms

### Key components

| Package | Role |
|---|---|
| `cmd/limiter/main.go` | Wires config → Redis client → metrics decorator → HTTP server |
| `internal/limiter/limiter.go` | Core interface: `Allow(ctx, key, rule) → Decision` |
| `internal/redis/client.go` | `SlidingWindowLimiter` — Redis executor + Decision builder |
| `internal/redis/scripts.go` | Embedded Lua script (also readable at `scripts/sliding_window.lua`) |
| `internal/middleware/http.go` | Rule matching, key extraction, header injection |
| `internal/metrics/metrics.go` | `InstrumentedLimiter` — Prometheus decorator over `Limiter` |
| `internal/config/config.go` | YAML parsing with custom `Duration` type (parses "60s" strings) |
| `internal/server/server.go` | HTTP mux, `/health`, `/metrics`, graceful shutdown |

### The Lua script (atomicity guarantee)

Redis executes Lua scripts single-threaded, making the check-and-write a single indivisible unit across all nodes. The script uses `REDIS TIME` (not app clock) to eliminate clock skew. Entries are stored in a ZSET scored by microsecond timestamp; pruning is `ZREMRANGEBYSCORE` + `ZCARD`. A failing Redis causes **fail-open** behavior — requests are allowed when the limiter cannot be reached.

### Configuration

Rules are defined in YAML (`configs/rules.yaml`). Key fields per rule:

```yaml
rules:
  - name: auth               # scopes the Redis key
    match:
      path_prefix: /auth/login   # longest prefix wins; omit for catch-all
    limit: 5
    window: 300s             # custom Duration type
    key_source: ip           # "ip" or "header:<Header-Name>"
```

`configs/rules.docker.yaml` is used by the Docker Compose stack (Redis hostname: `redis`).

### Deployment topology

The Docker Compose stack (`deployments/docker-compose.yml`) runs three limiter nodes on ports 8081–8083, Redis 7-alpine, and Prometheus scraping all three nodes. Prometheus config is in `deployments/prometheus.yml`.

## Testing notes

- Benchmarks in `bench/bench_test.go` expect Redis on localhost; typical throughput is ~30K decisions/sec per node at sub-millisecond latency.
- The integration test (`integration/distributed_test.go`) fires 200 concurrent requests across 3 in-process nodes and asserts exactly the configured limit are allowed — this is the correctness proof for distributed atomicity.
- Prometheus metrics are exported at `/metrics` on each node; the relevant metric prefix is `ratelimit_`.
