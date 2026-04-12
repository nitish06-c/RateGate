package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nitish/ratelimiter/internal/config"
	"github.com/nitish/ratelimiter/internal/limiter"
	"github.com/nitish/ratelimiter/internal/middleware"
)

// stubLimiter lets tests control Allow outcomes without needing Redis.
type stubLimiter struct {
	allow bool
	err   error
}

func (s *stubLimiter) Allow(_ context.Context, _ string, rule limiter.Rule) (*limiter.Decision, error) {
	if s.err != nil {
		return nil, s.err
	}
	remaining := int64(0)
	if s.allow {
		remaining = rule.Limit - 1
	}
	return &limiter.Decision{
		Allowed:   s.allow,
		Limit:     rule.Limit,
		Remaining: remaining,
		ResetAt:   time.Now().Add(rule.Window),
		RetryAt:   2 * time.Second,
	}, nil
}

var testRules = []config.RuleConfig{
	{
		Name:      "auth",
		Limit:     5,
		Window:    config.Duration{Duration: 300 * time.Second},
		KeySource: "ip",
		Match:     config.MatchConfig{PathPrefix: "/auth/login"},
	},
	{
		Name:      "default",
		Limit:     100,
		Window:    config.Duration{Duration: 60 * time.Second},
		KeySource: "ip",
	},
}

func TestMiddleware_Allowed(t *testing.T) {
	lim := &stubLimiter{allow: true}
	handler := middleware.RateLimit(lim, testRules)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	req.RemoteAddr = "1.2.3.4:5000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Limit") == "" {
		t.Error("expected X-RateLimit-Limit header")
	}
}

func TestMiddleware_Denied(t *testing.T) {
	lim := &stubLimiter{allow: false}
	handler := middleware.RateLimit(lim, testRules)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	req.RemoteAddr = "1.2.3.4:5000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}
}

func TestMiddleware_PathPrefixRouting(t *testing.T) {
	var capturedRule string
	lim := &stubLimiter{allow: true}

	// We'll verify routing by checking X-RateLimit-Limit:
	// /auth/login should hit the "auth" rule (limit=5)
	// /api/data should hit the "default" rule (limit=100)
	handler := middleware.RateLimit(lim, testRules)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRule = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		path          string
		expectedLimit string
	}{
		{"/auth/login", "5"},
		{"/auth/login/extra", "5"},   // longer path still matches prefix
		{"/api/data", "100"},
		{"/other", "100"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.RemoteAddr = "1.2.3.4:5000"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			_ = capturedRule
			got := rec.Header().Get("X-RateLimit-Limit")
			if got != tt.expectedLimit {
				t.Errorf("path %q: expected X-RateLimit-Limit=%s, got %s", tt.path, tt.expectedLimit, got)
			}
		})
	}
}

func TestMiddleware_LimiterError_FailOpen(t *testing.T) {
	lim := &stubLimiter{err: context.DeadlineExceeded}
	reached := false
	handler := middleware.RateLimit(lim, testRules)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	req.RemoteAddr = "1.2.3.4:5000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !reached {
		t.Error("expected request to pass through on limiter error (fail open)")
	}
	if rec.Header().Get("X-RateLimit-Error") == "" {
		t.Error("expected X-RateLimit-Error header when limiter errors")
	}
}

func TestMiddleware_NoMatchingRule_PassThrough(t *testing.T) {
	// Rules with only a path prefix rule and no default -- requests to
	// other paths should pass through untouched.
	rules := []config.RuleConfig{
		{
			Name:      "auth",
			Limit:     5,
			Window:    config.Duration{Duration: 60 * time.Second},
			KeySource: "ip",
			Match:     config.MatchConfig{PathPrefix: "/auth"},
		},
	}
	lim := &stubLimiter{allow: false} // would deny if reached
	handler := middleware.RateLimit(lim, rules)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/public/page", nil)
	req.RemoteAddr = "1.2.3.4:5000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 pass-through, got %d", rec.Code)
	}
}
