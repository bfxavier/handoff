package main

import (
	"context"
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
	rateLimitMax := 100
	if v := os.Getenv("RATE_LIMIT_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("invalid RATE_LIMIT_MAX %q: %v", v, err)
		}
		rateLimitMax = n
	}
	var rateLimitWindowMs int64 = 1000
	if v := os.Getenv("RATE_LIMIT_WINDOW_MS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			log.Fatalf("invalid RATE_LIMIT_WINDOW_MS %q: %v", v, err)
		}
		rateLimitWindowMs = n
	}

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
	cfg := handler.DefaultConfig()
	cfg.RateLimitMax = rateLimitMax
	cfg.RateLimitWindowMs = rateLimitWindowMs
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

