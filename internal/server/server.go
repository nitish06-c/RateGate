package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/nitish/ratelimiter/internal/config"
	"github.com/nitish/ratelimiter/internal/limiter"
	"github.com/nitish/ratelimiter/internal/middleware"
)

type Server struct {
	http *http.Server
}

func New(addr string, lim limiter.Limiter, cfg *config.Config) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/", handleEcho)

	return &Server{
		http: &http.Server{
			Addr:         addr,
			Handler:      middleware.RateLimit(lim, cfg.Rules)(mux),
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
	}
}

func (s *Server) Start() error {
	log.Printf("INFO: server starting addr=%s", s.http.Addr)
	return s.http.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
