package testutil

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

// RedisClient returns a Redis client for testing.
// Uses REDIS_TEST_URL env var, defaults to localhost:6379 db 15.
func RedisClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		url = "redis://localhost:6379/15"
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("invalid REDIS_TEST_URL: %v", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not available: %v", err)
	}
	return client
}

// FlushDB clears the test Redis database.
func FlushDB(t *testing.T, client *redis.Client) {
	t.Helper()
	if err := client.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flush db: %v", err)
	}
}

// UniquePrefix returns a unique key prefix for test isolation.
func UniquePrefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test:%s:", t.Name())
}

// DoRequest makes an HTTP request to the test server and returns the response recorder.
func DoRequest(handler http.Handler, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	var bodyReader *stringReader
	if body != "" {
		bodyReader = &stringReader{s: body, i: 0}
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (n int, err error) {
	if r.i >= len(r.s) {
		return 0, fmt.Errorf("EOF")
	}
	n = copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
