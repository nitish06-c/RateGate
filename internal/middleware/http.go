package middleware

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nitish/ratelimiter/internal/config"
	"github.com/nitish/ratelimiter/internal/limiter"
)

func RateLimit(lim limiter.Limiter, rules []config.RuleConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rule, ok := matchRule(r.URL.Path, rules)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			key, err := extractKey(r, rule.KeySource)
			if err != nil {
				log.Printf("WARN: failed to extract rate limit key path=%s error=%v", r.URL.Path, err)
				next.ServeHTTP(w, r)
				return
			}

			scopedKey := fmt.Sprintf("%s:%s", rule.Name, key)

			dec, err := lim.Allow(r.Context(), scopedKey, limiter.Rule{
				Name:   rule.Name,
				Limit:  rule.Limit,
				Window: rule.Window.Duration,
			})
			if err != nil {
				log.Printf("ERROR: rate limiter error key=%s error=%v", scopedKey, err)
				w.Header().Set("X-RateLimit-Error", "limiter unavailable")
				next.ServeHTTP(w, r)
				return
			}

			setHeaders(w, dec)

			if !dec.Allowed {
				if dec.RetryAt > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(int(dec.RetryAt.Seconds())))
				}
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// matchRule returns the most specific matching rule for path (longest prefix wins),
// falling back to the rule named "default".
func matchRule(path string, rules []config.RuleConfig) (config.RuleConfig, bool) {
	var best *config.RuleConfig
	bestLen := -1

	for i := range rules {
		r := &rules[i]
		if r.Match.PathPrefix != "" && strings.HasPrefix(path, r.Match.PathPrefix) {
			if len(r.Match.PathPrefix) > bestLen {
				best = r
				bestLen = len(r.Match.PathPrefix)
			}
		}
	}
	if best != nil {
		return *best, true
	}

	for i := range rules {
		if rules[i].Name == "default" {
			return rules[i], true
		}
	}

	return config.RuleConfig{}, false
}

func extractKey(r *http.Request, keySource string) (string, error) {
	if keySource == "" || keySource == "ip" {
		return clientIP(r), nil
	}
	if strings.HasPrefix(keySource, "header:") {
		name := strings.TrimPrefix(keySource, "header:")
		if val := r.Header.Get(name); val != "" {
			return val, nil
		}
		return clientIP(r), nil
	}
	return "", fmt.Errorf("unsupported key_source %q", keySource)
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if ip, _, err := net.SplitHostPort(strings.TrimSpace(strings.Split(fwd, ",")[0])); err == nil {
			return ip
		}
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func setHeaders(w http.ResponseWriter, dec *limiter.Decision) {
	w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(dec.Limit, 10))
	w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(dec.Remaining, 10))
	if !dec.ResetAt.IsZero() {
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(dec.ResetAt.Unix(), 10))
	}
}

func retryAtTime(dec *limiter.Decision) time.Time {
	if dec.RetryAt == 0 {
		return time.Time{}
	}
	return time.Now().Add(dec.RetryAt)
}
