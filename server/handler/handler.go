package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bfxavier/handoff/server/store"
	"github.com/redis/go-redis/v9"
)

type Config struct {
	RateLimitMax      int
	RateLimitWindowMs int64
}

func DefaultConfig() Config {
	return Config{
		RateLimitMax:      100,
		RateLimitWindowMs: 1000,
	}
}

type Server struct {
	store  *store.Store
	rdb    *redis.Client
	config Config
	mux    *http.ServeMux
}

func New(s *store.Store, rdb *redis.Client, cfg Config) *Server {
	srv := &Server{store: s, rdb: rdb, config: cfg}
	srv.mux = http.NewServeMux()
	srv.routes()
	return srv
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Track whether a registered handler was invoked
	wrapped := &routeTracker{ResponseWriter: w}
	s.mux.ServeHTTP(wrapped, r)
	if !wrapped.handlerCalled {
		// ServeMux found no matching route — return JSON 404
		apiError(w, 404, "NOT_FOUND", "Route not found")
	}
}

// routeTracker detects whether ServeMux dispatched to one of our handlers
// vs returning its default 404. It does this by checking if WriteHeader was
// called with anything other than 404, or if Write was called with non-default content.
// Since our handlers always call writeJSON which sets Content-Type to application/json,
// we check for that.
type routeTracker struct {
	http.ResponseWriter
	handlerCalled bool
	headerWritten bool
}

func (w *routeTracker) Header() http.Header {
	return w.ResponseWriter.Header()
}

func (w *routeTracker) WriteHeader(code int) {
	w.headerWritten = true
	// If Content-Type is application/json, one of our handlers set it
	if w.ResponseWriter.Header().Get("Content-Type") == "application/json" {
		w.handlerCalled = true
	}
	// If it's not a 404, definitely a handler
	if code != 404 {
		w.handlerCalled = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *routeTracker) Write(b []byte) (int, error) {
	if !w.headerWritten {
		// Implicit 200 — a handler is writing
		w.handlerCalled = true
	}
	if w.ResponseWriter.Header().Get("Content-Type") == "application/json" {
		w.handlerCalled = true
	}
	if !w.handlerCalled {
		// This is ServeMux's "404 page not found\n" — swallow it
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func (w *routeTracker) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

func readBody(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 65536))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return fmt.Errorf("empty body")
	}
	return json.Unmarshal(body, v)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---- Routes ----

func (s *Server) routes() {
	// Public (no auth)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /api", s.handleAPIInfo)
	s.mux.HandleFunc("POST /api/signup", s.handleSignup)

	// Authenticated
	s.mux.HandleFunc("POST /api/keys", s.auth(s.rateLimit(s.handleCreateKey)))
	s.mux.HandleFunc("GET /api/channels", s.auth(s.rateLimit(s.handleListChannels)))
	s.mux.HandleFunc("POST /api/channels", s.auth(s.rateLimit(s.handleCreateChannel)))
	s.mux.HandleFunc("DELETE /api/channels/{channel}", s.auth(s.rateLimit(s.handleDeleteChannel)))
	s.mux.HandleFunc("POST /api/channels/{channel}/messages", s.auth(s.rateLimit(s.handlePostMessage)))
	s.mux.HandleFunc("GET /api/channels/{channel}/messages", s.auth(s.rateLimit(s.handleReadMessages)))
	s.mux.HandleFunc("DELETE /api/channels/{channel}/messages/{id}", s.auth(s.rateLimit(s.handleDeleteMessage)))
	s.mux.HandleFunc("GET /api/channels/{channel}/threads/{id}", s.auth(s.rateLimit(s.handleReadThread)))
	s.mux.HandleFunc("GET /api/channels/{channel}/stream", s.auth(s.handleSSE))
	s.mux.HandleFunc("POST /api/channels/{channel}/ack", s.auth(s.rateLimit(s.handleAck)))
	s.mux.HandleFunc("GET /api/channels/{channel}/acks", s.auth(s.rateLimit(s.handleGetAcks)))
	s.mux.HandleFunc("GET /api/channels/{channel}/unread", s.auth(s.rateLimit(s.handleUnread)))
	s.mux.HandleFunc("PUT /api/channels/{channel}/status", s.auth(s.rateLimit(s.handleSetStatus)))
	s.mux.HandleFunc("POST /api/channels/{channel}/status", s.auth(s.rateLimit(s.handleSetStatus)))
	s.mux.HandleFunc("DELETE /api/channels/{channel}/status/{key}", s.auth(s.rateLimit(s.handleDeleteStatus)))
	s.mux.HandleFunc("GET /api/channels/{channel}/status", s.auth(s.rateLimit(s.handleGetStatus)))
	s.mux.HandleFunc("GET /api/channels/{channel}/status/changes", s.auth(s.rateLimit(s.handleGetStatusChanges)))
	s.mux.HandleFunc("GET /api/status", s.auth(s.rateLimit(s.handleGetStatusCrossChannel)))
}

// ---- Middleware ----

type contextKey string

const apiKeyCtx contextKey = "apiKey"

func getApiKey(r *http.Request) *store.ApiKey {
	if v := r.Context().Value(apiKeyCtx); v != nil {
		return v.(*store.ApiKey)
	}
	return nil
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var key string
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			key = auth[7:]
		} else if xk := r.Header.Get("X-Api-Key"); xk != "" {
			key = xk
		} else if tk := r.URL.Query().Get("token"); tk != "" {
			key = tk
		}

		if key == "" {
			apiError(w, 401, "MISSING_API_KEY", "Missing API key")
			return
		}

		ak, err := s.store.ValidateApiKey(r.Context(), key)
		if err != nil {
			apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
			return
		}
		if ak == nil {
			apiError(w, 401, "INVALID_API_KEY", "Invalid API key")
			return
		}

		ctx := context.WithValue(r.Context(), apiKeyCtx, ak)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) rateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ak := getApiKey(r)
		if ak == nil {
			next(w, r)
			return
		}
		allowed, remaining, _ := s.store.CheckRateLimit(r.Context(), ak.Key, s.config.RateLimitMax, s.config.RateLimitWindowMs)
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(s.config.RateLimitMax))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		if !allowed {
			apiError(w, 429, "RATE_LIMITED", "Too many requests. Slow down.")
			return
		}
		next(w, r)
	}
}

// ---- Health ----

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.rdb.Ping(r.Context()).Err(); err != nil {
		writeJSON(w, 503, map[string]string{"status": "unavailable", "redis": "disconnected"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "redis": "connected"})
}

func (s *Server) handleAPIInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"name":    "agent-relay",
		"version": "1.0.0",
		"endpoints": []map[string]interface{}{
			{"method": "POST", "path": "/api/signup", "description": "Create a team and get an API key", "auth": false},
			{"method": "POST", "path": "/api/keys", "description": "Create an additional API key for the team"},
			{"method": "GET", "path": "/api/channels", "description": "List all channels"},
			{"method": "POST", "path": "/api/channels", "description": "Create a channel"},
			{"method": "DELETE", "path": "/api/channels/{channel}", "description": "Delete a channel and all its data"},
			{"method": "GET", "path": "/api/channels/{channel}/messages", "description": "Read messages (query: after_id, limit, mention, sender, thread_id)"},
			{"method": "POST", "path": "/api/channels/{channel}/messages", "description": "Post a message (body: content, mention?, thread_id?)"},
			{"method": "DELETE", "path": "/api/channels/{channel}/messages/{id}", "description": "Delete a message"},
			{"method": "GET", "path": "/api/channels/{channel}/threads/{id}", "description": "Read a thread (parent + replies, query: after_id, limit)"},
			{"method": "GET", "path": "/api/channels/{channel}/stream", "description": "SSE stream of new messages (query: token, last_event_id)"},
			{"method": "POST", "path": "/api/channels/{channel}/ack", "description": "Acknowledge messages up to a given ID"},
			{"method": "GET", "path": "/api/channels/{channel}/acks", "description": "Get ack state for all agents"},
			{"method": "GET", "path": "/api/channels/{channel}/unread", "description": "Get unread messages (after your last ack)"},
			{"method": "GET", "path": "/api/channels/{channel}/status", "description": "Get status entries (query: key)"},
			{"method": "PUT", "path": "/api/channels/{channel}/status", "description": "Set a status entry"},
			{"method": "POST", "path": "/api/channels/{channel}/status", "description": "Set a status entry (alias for PUT)"},
			{"method": "DELETE", "path": "/api/channels/{channel}/status/{key}", "description": "Delete a status entry"},
			{"method": "GET", "path": "/api/channels/{channel}/status/changes", "description": "Read status change log (query: after_id, limit)"},
			{"method": "GET", "path": "/api/status", "description": "Cross-channel status query (query: channel, key)"},
			{"method": "GET", "path": "/healthz", "description": "Liveness probe", "auth": false},
			{"method": "GET", "path": "/readyz", "description": "Readiness probe (checks Redis)", "auth": false},
		},
		"limits": map[string]string{
			"message_content": fmt.Sprintf("%d bytes", store.MaxContentLength),
			"status_key":      fmt.Sprintf("%d chars", store.MaxStatusKeyLength),
			"status_value":    fmt.Sprintf("%d bytes", store.MaxStatusValueLength),
			"channel_name":    fmt.Sprintf("1-%d chars, alphanumeric/hyphens/underscores/dots", store.MaxChannelNameLength),
		},
	})
}

// ---- Auth endpoints ----

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TeamName   string `json:"team_name"`
		SenderName string `json:"sender_name"`
	}
	if err := readBody(r, &body); err != nil {
		apiError(w, 400, "INVALID_JSON", "Request body must be valid JSON")
		return
	}
	if body.TeamName == "" || body.SenderName == "" {
		apiError(w, 400, "MISSING_FIELDS", "team_name and sender_name are required")
		return
	}
	team, apiKey, err := s.store.CreateTeam(r.Context(), body.TeamName, body.SenderName)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 201, map[string]interface{}{"team": team, "api_key": apiKey})
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SenderName string `json:"sender_name"`
	}
	if err := readBody(r, &body); err != nil {
		apiError(w, 400, "INVALID_JSON", "Request body must be valid JSON")
		return
	}
	if body.SenderName == "" {
		apiError(w, 400, "MISSING_FIELDS", "sender_name is required")
		return
	}
	ak := getApiKey(r)
	key, err := s.store.CreateApiKey(r.Context(), ak.TeamID, body.SenderName)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 201, map[string]interface{}{"api_key": key, "sender": body.SenderName})
}

// ---- Channels ----

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string  `json:"name"`
		Description *string `json:"description"`
	}
	if err := readBody(r, &body); err != nil {
		apiError(w, 400, "INVALID_JSON", "Request body must be valid JSON")
		return
	}
	if body.Name == "" {
		apiError(w, 400, "MISSING_FIELDS", "name is required")
		return
	}
	if !store.IsValidChannelName(body.Name) {
		apiError(w, 400, "INVALID_CHANNEL_NAME", "Channel name must be 1-128 characters, alphanumeric with hyphens, underscores, and dots")
		return
	}
	ak := getApiKey(r)
	ch, created, err := s.store.CreateChannel(r.Context(), ak.TeamID, body.Name, body.Description)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, ch)
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	ak := getApiKey(r)
	channels, err := s.store.ListChannels(r.Context(), ak.TeamID)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 200, channels)
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	ak := getApiKey(r)
	ch := r.PathValue("channel")
	deleted, err := s.store.DeleteChannel(r.Context(), ak.TeamID, ch)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	if !deleted {
		apiError(w, 404, "CHANNEL_NOT_FOUND", "Channel not found")
		return
	}
	w.WriteHeader(204)
}

// ---- Messages ----

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content  string  `json:"content"`
		Mention  *string `json:"mention"`
		ThreadID *string `json:"thread_id"`
	}
	if err := readBody(r, &body); err != nil {
		apiError(w, 400, "INVALID_JSON", "Request body must be valid JSON")
		return
	}
	if body.Content == "" {
		apiError(w, 400, "MISSING_FIELDS", "content is required")
		return
	}
	if len(body.Content) > store.MaxContentLength {
		apiError(w, 400, "CONTENT_TOO_LARGE", fmt.Sprintf("content must be %d bytes or less", store.MaxContentLength))
		return
	}
	if body.ThreadID != nil && !store.IsValidCursor(*body.ThreadID) {
		apiError(w, 400, "INVALID_CURSOR", "Invalid thread_id format. Expected a message ID like '1234567890-0'")
		return
	}

	ak := getApiKey(r)
	ch := r.PathValue("channel")

	if body.ThreadID != nil {
		exists, _ := s.store.MessageExists(r.Context(), ak.TeamID, ch, *body.ThreadID)
		if !exists {
			apiError(w, 404, "THREAD_PARENT_NOT_FOUND", "The message you are replying to does not exist")
			return
		}
	}

	msg, err := s.store.PostMessage(r.Context(), ak.TeamID, ch, ak.Sender, body.Content, body.Mention, body.ThreadID)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 201, msg)
}

func (s *Server) handleReadMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	afterID := strPtr(q.Get("after_id"))
	mention := strPtr(q.Get("mention"))
	sender := strPtr(q.Get("sender"))
	threadID := strPtr(q.Get("thread_id"))

	if afterID != nil && !store.IsValidCursor(*afterID) {
		apiError(w, 400, "INVALID_CURSOR", "Invalid after_id format. Expected a stream ID like '1234567890-0'")
		return
	}
	if threadID != nil && !store.IsValidCursor(*threadID) {
		apiError(w, 400, "INVALID_CURSOR", "Invalid thread_id format")
		return
	}

	limit := 50
	if ls := q.Get("limit"); ls != "" {
		l, err := strconv.Atoi(ls)
		if err != nil || l < 1 || l > 100 {
			apiError(w, 400, "INVALID_LIMIT", "limit must be an integer between 1 and 100")
			return
		}
		limit = l
	}

	ak := getApiKey(r)
	ch := r.PathValue("channel")
	result, err := s.store.ReadMessages(r.Context(), ak.TeamID, ch, afterID, limit, mention, sender, threadID)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	msgID := r.PathValue("id")
	if !store.IsValidCursor(msgID) {
		apiError(w, 400, "INVALID_CURSOR", "Invalid message ID format")
		return
	}
	ak := getApiKey(r)
	ch := r.PathValue("channel")
	deleted, err := s.store.DeleteMessage(r.Context(), ak.TeamID, ch, msgID)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	if !deleted {
		apiError(w, 404, "MESSAGE_NOT_FOUND", "Message not found")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handleReadThread(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("id")
	if !store.IsValidCursor(parentID) {
		apiError(w, 400, "INVALID_CURSOR", "Invalid thread ID format")
		return
	}
	q := r.URL.Query()
	afterID := strPtr(q.Get("after_id"))
	if afterID != nil && !store.IsValidCursor(*afterID) {
		apiError(w, 400, "INVALID_CURSOR", "Invalid after_id format")
		return
	}
	limit := 50
	if ls := q.Get("limit"); ls != "" {
		l, err := strconv.Atoi(ls)
		if err != nil || l < 1 || l > 100 {
			apiError(w, 400, "INVALID_LIMIT", "limit must be an integer between 1 and 100")
			return
		}
		limit = l
	}

	ak := getApiKey(r)
	ch := r.PathValue("channel")
	result, err := s.store.ReadThread(r.Context(), ak.TeamID, ch, parentID, afterID, limit)
	if err != nil {
		if _, ok := err.(store.ErrNotFound); ok {
			apiError(w, 404, "MESSAGE_NOT_FOUND", "Parent message not found")
			return
		}
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 200, result)
}

// ---- SSE ----

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		apiError(w, 500, "INTERNAL_ERROR", "Streaming not supported")
		return
	}

	ak := getApiKey(r)
	ch := r.PathValue("channel")
	lastEventID := r.Header.Get("Last-Event-ID")
	if lastEventID == "" {
		lastEventID = r.URL.Query().Get("last_event_id")
	}
	if lastEventID == "" {
		lastEventID = "$"
	}
	if lastEventID != "$" && !store.IsValidCursor(lastEventID) {
		apiError(w, 400, "INVALID_CURSOR", "Invalid last_event_id format")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	// Initial padding to push through reverse proxy buffers (Traefik needs ~4KB+)
	fmt.Fprintf(w, ":%s\n:ok\n\n", strings.Repeat("_", 8192))
	flusher.Flush()

	cursor := lastEventID
	ctx := r.Context()

	// Catch-up on reconnect
	if cursor != "$" {
		catchUp, err := s.store.ReadMessages(ctx, ak.TeamID, ch, &cursor, 100, nil, nil, nil)
		if err == nil {
			for _, msg := range catchUp.Messages {
				b, _ := json.Marshal(msg)
				fmt.Fprintf(w, "id: %s\nevent: message\ndata: %s\n\n", msg.ID, b)
				cursor = msg.ID
			}
			flusher.Flush()
		}
	}

	// Resolve $ to actual cursor
	if cursor == "$" {
		info, err := s.rdb.XInfoStream(ctx, fmt.Sprintf("t:%s:msg:%s", ak.TeamID, ch)).Result()
		if err == nil {
			cursor = info.LastGeneratedID
		}
		if cursor == "$" || cursor == "" {
			cursor = "0-0"
		}
	}

	// Dedicated Redis connection for blocking reads
	subRdb := redis.NewClient(s.rdb.Options())
	defer subRdb.Close()
	subStore := store.New(subRdb)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := subStore.BlockingRead(ctx, ak.TeamID, ch, cursor, 25*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(w, "event: error\ndata: {\"error\":\"stream error\"}\n\n")
			flusher.Flush()
			return
		}

		if len(msgs) > 0 {
			for _, msg := range msgs {
				b, _ := json.Marshal(msg)
				fmt.Fprintf(w, "id: %s\nevent: message\ndata: %s\n\n", msg.ID, b)
				cursor = msg.ID
			}
		} else {
			fmt.Fprintf(w, ":keepalive\n\n")
		}
		flusher.Flush()
	}
}

// ---- Acks ----

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LastReadID string `json:"last_read_id"`
	}
	if err := readBody(r, &body); err != nil {
		apiError(w, 400, "INVALID_JSON", "Request body must be valid JSON")
		return
	}
	if body.LastReadID == "" {
		apiError(w, 400, "MISSING_FIELDS", "last_read_id is required")
		return
	}
	if !store.IsValidCursor(body.LastReadID) {
		apiError(w, 400, "INVALID_CURSOR", "Invalid last_read_id format. Expected a stream ID like '1234567890-0'")
		return
	}
	ak := getApiKey(r)
	ch := r.PathValue("channel")
	ack, err := s.store.AckMessages(r.Context(), ak.TeamID, ch, ak.Sender, body.LastReadID)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 200, ack)
}

func (s *Server) handleGetAcks(w http.ResponseWriter, r *http.Request) {
	ak := getApiKey(r)
	ch := r.PathValue("channel")
	acks, err := s.store.GetAcks(r.Context(), ak.TeamID, ch)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 200, acks)
}

// GET /api/channels/{channel}/unread — messages after sender's last ack
func (s *Server) handleUnread(w http.ResponseWriter, r *http.Request) {
	ak := getApiKey(r)
	ch := r.PathValue("channel")
	limit := 50
	if ls := r.URL.Query().Get("limit"); ls != "" {
		l, err := strconv.Atoi(ls)
		if err != nil || l < 1 || l > 100 {
			apiError(w, 400, "INVALID_LIMIT", "limit must be an integer between 1 and 100")
			return
		}
		limit = l
	}
	result, err := s.store.GetUnread(r.Context(), ak.TeamID, ch, ak.Sender, limit)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 200, result)
}

// ---- Status ----

func (s *Server) handleSetStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := readBody(r, &body); err != nil {
		apiError(w, 400, "INVALID_JSON", "Request body must be valid JSON")
		return
	}
	if body.Key == "" {
		apiError(w, 400, "MISSING_FIELDS", "key and value are required")
		return
	}
	if len(body.Key) > store.MaxStatusKeyLength {
		apiError(w, 400, "KEY_TOO_LARGE", fmt.Sprintf("Status key must be %d characters or less", store.MaxStatusKeyLength))
		return
	}
	if len(body.Value) > store.MaxStatusValueLength {
		apiError(w, 400, "VALUE_TOO_LARGE", fmt.Sprintf("Status value must be %d bytes or less", store.MaxStatusValueLength))
		return
	}

	ak := getApiKey(r)
	ch := r.PathValue("channel")
	st, err := s.store.SetStatus(r.Context(), ak.TeamID, ch, body.Key, body.Value, &ak.Sender)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	ak := getApiKey(r)
	ch := r.PathValue("channel")
	key := strPtr(r.URL.Query().Get("key"))
	statuses, err := s.store.GetStatus(r.Context(), ak.TeamID, &ch, key)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 200, statuses)
}

func (s *Server) handleDeleteStatus(w http.ResponseWriter, r *http.Request) {
	ak := getApiKey(r)
	ch := r.PathValue("channel")
	key := r.PathValue("key")
	deleted, err := s.store.DeleteStatus(r.Context(), ak.TeamID, ch, key)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	if !deleted {
		apiError(w, 404, "STATUS_NOT_FOUND", "Status key not found")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handleGetStatusCrossChannel(w http.ResponseWriter, r *http.Request) {
	ak := getApiKey(r)
	channel := strPtr(r.URL.Query().Get("channel"))
	key := strPtr(r.URL.Query().Get("key"))
	statuses, err := s.store.GetStatus(r.Context(), ak.TeamID, channel, key)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 200, statuses)
}

func (s *Server) handleGetStatusChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	afterID := strPtr(q.Get("after_id"))
	if afterID != nil && !store.IsValidCursor(*afterID) {
		apiError(w, 400, "INVALID_CURSOR", "Invalid after_id format. Expected a stream ID like '1234567890-0'")
		return
	}
	limit := 50
	if ls := q.Get("limit"); ls != "" {
		l, err := strconv.Atoi(ls)
		if err != nil || l < 1 || l > 100 {
			apiError(w, 400, "INVALID_LIMIT", "limit must be an integer between 1 and 100")
			return
		}
		limit = l
	}

	ak := getApiKey(r)
	ch := r.PathValue("channel")
	result, err := s.store.GetStatusChanges(r.Context(), ak.TeamID, ch, afterID, limit)
	if err != nil {
		apiError(w, 500, "INTERNAL_ERROR", "Internal server error")
		return
	}
	writeJSON(w, 200, result)
}
