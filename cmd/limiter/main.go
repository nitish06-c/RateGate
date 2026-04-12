package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nitish/ratelimiter/internal/config"
	redislimiter "github.com/nitish/ratelimiter/internal/redis"
	"github.com/nitish/ratelimiter/internal/server"
	"github.com/redis/go-redis/v9"
)

func main() {
	configPath := flag.String("config", "configs/rules.yaml", "path to rules config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("ERROR: failed to load config: %v", err)
	}
	log.Printf("INFO: config loaded rules=%d redis=%s", len(cfg.Rules), cfg.Redis.Addr)

	redisClient := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Fatalf("ERROR: redis connection failed addr=%s error=%v", cfg.Redis.Addr, err)
	}
	log.Printf("INFO: redis connected addr=%s", cfg.Redis.Addr)

	lim := redislimiter.NewSlidingWindowLimiter(redisClient, "rl")

	srv := server.New(cfg.Server.Addr, lim, cfg)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		log.Printf("INFO: shutdown signal received signal=%v", sig)
	case err := <-errCh:
		log.Fatalf("ERROR: server error: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("ERROR: shutdown error: %v", err)
	}
	log.Println("INFO: server stopped cleanly")
}
