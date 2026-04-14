//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitish/ratelimiter/internal/config"
	redislimiter "github.com/nitish/ratelimiter/internal/redis"
	"github.com/nitish/ratelimiter/internal/server"
	"github.com/redis/go-redis/v9"
)

const (
	limit      = 100
	totalReqs  = 200
	nodeCount  = 3
	testWindow = 30 * time.Second
)

func TestDistributed_ExactCount(t *testing.T) {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not available at %s: %v", redisAddr, err)
	}

	// Flush Redis so previous test runs don't interfere.
	client.FlushDB(ctx)

	nodes := startNodes(t, client, nodeCount)

	var (
		allowed atomic.Int64
		denied  atomic.Int64
		wg      sync.WaitGroup
	)

	for i := 0; i < totalReqs; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Distribute requests round-robin across nodes.
			node := nodes[n%nodeCount]
			resp, err := http.Get(fmt.Sprintf("%s/", node))
			if err != nil {
				t.Errorf("request %d: %v", n, err)
				return
			}
			resp.Body.Close()

			switch resp.StatusCode {
			case http.StatusOK:
				allowed.Add(1)
			case http.StatusTooManyRequests:
				denied.Add(1)
			default:
				t.Errorf("request %d: unexpected status %d", n, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()

	got := allowed.Load()
	if got != limit {
		t.Errorf("expected exactly %d allowed across %d nodes, got %d (denied=%d)",
			limit, nodeCount, got, denied.Load())
	}
	t.Logf("nodes=%d total=%d allowed=%d denied=%d", nodeCount, totalReqs, got, denied.Load())
}

func TestDistributed_KeysAreIndependent(t *testing.T) {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not available at %s: %v", redisAddr, err)
	}
	client.FlushDB(ctx)

	nodes := startNodes(t, client, 2)

	// Two different API keys, each should get the full limit independently.
	type result struct {
		allowed int64
		denied  int64
	}
	results := make([]result, 2)
	var wg sync.WaitGroup

	for keyIdx := 0; keyIdx < 2; keyIdx++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			apiKey := fmt.Sprintf("user-%d", k)
			var a, d atomic.Int64
			var inner sync.WaitGroup
			for i := 0; i < totalReqs; i++ {
				inner.Add(1)
				go func(n int) {
					defer inner.Done()
					node := nodes[n%2]
					req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/", node), nil)
					req.Header.Set("X-API-Key", apiKey)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						return
					}
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						a.Add(1)
					} else {
						d.Add(1)
					}
				}(i)
			}
			inner.Wait()
			results[k] = result{a.Load(), d.Load()}
		}(keyIdx)
	}
	wg.Wait()

	for k, r := range results {
		if r.allowed != limit {
			t.Errorf("key %d: expected %d allowed, got %d", k, limit, r.allowed)
		}
		t.Logf("key %d: allowed=%d denied=%d", k, r.allowed, r.denied)
	}
}

// startNodes starts n in-process limiter servers all sharing the same Redis client.
// Each server listens on a random available port. Servers are shut down when the
// test completes.
func startNodes(t *testing.T, client *redis.Client, n int) []string {
	t.Helper()

	cfg := &config.Config{
		Redis:  config.RedisConfig{Addr: client.Options().Addr},
		Server: config.ServerConfig{Addr: ":0"},
		Rules: []config.RuleConfig{
			{
				Name:      "default",
				Limit:     limit,
				Window:    config.Duration{Duration: testWindow},
				KeySource: "header:X-API-Key",
			},
		},
	}

	addrs := make([]string, n)
	for i := 0; i < n; i++ {
		lim := redislimiter.NewSlidingWindowLimiter(client, "inttest")
		srv := server.New(fmt.Sprintf(":%d", 19000+i), lim, cfg)

		go srv.Start()
		time.Sleep(20 * time.Millisecond) // let it bind

		addrs[i] = fmt.Sprintf("http://localhost:%d", 19000+i)

		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			srv.Shutdown(ctx)
		})
	}

	return addrs
}
