package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/bfxavier/handoff/server/handler"
	"github.com/bfxavier/handoff/server/store"
	"github.com/redis/go-redis/v9"
)

func main() {
	port := envOr("PORT", "3000")
	redisURL := envOr("REDIS_URL", "redis://localhost:6379")
	rateLimitMax, _ := strconv.Atoi(envOr("RATE_LIMIT_MAX", "100"))
	rateLimitWindowMs, _ := strconv.ParseInt(envOr("RATE_LIMIT_WINDOW_MS", "1000"), 10, 64)

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("invalid REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis connect: %v", err)
	}
	log.Printf("Redis connected: %s", redisURL)

	s := store.New(rdb)
	cfg := handler.Config{
		RateLimitMax:      rateLimitMax,
		RateLimitWindowMs: rateLimitWindowMs,
	}
	srv := handler.New(s, rdb, cfg)

	// Serve static files from public/ if it exists
	publicDir := filepath.Join(".", "public")
	if info, err := os.Stat(publicDir); err == nil && info.IsDir() {
		fs := http.FileServer(http.Dir(publicDir))
		mux := http.NewServeMux()
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// API and health routes go to the handler
			if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/api" ||
				r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				srv.ServeHTTP(w, r)
				return
			}
			// Everything else tries static files, falls back to handler
			fs.ServeHTTP(w, r)
		}))
		http.Handle("/", mux)
	} else {
		http.Handle("/", srv)
	}

	server := &http.Server{
		Addr:         ":" + port,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // SSE needs unlimited write timeout
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
		rdb.Close()
	}()

	log.Printf("Agent Relay listening on port %s", port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func init() {
	_ = fmt.Sprintf // ensure fmt is used
}
