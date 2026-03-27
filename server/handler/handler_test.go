package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bfxavier/handoff/server/store"
	"github.com/bfxavier/handoff/server/testutil"
)

func setup(t *testing.T) (*Server, string) {
	t.Helper()
	rdb := testutil.RedisClient(t)
	testutil.FlushDB(t, rdb)
	s := store.New(rdb)
	srv := New(s, rdb, DefaultConfig())

	// Create team and get key
	team, apiKey, err := s.CreateTeam(t.Context(), "test-team", "test-sender")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	_ = team
	return srv, apiKey
}

func doReq(srv http.Handler, method, path, body, apiKey string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func parseJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("parse json: %v\nbody: %s", err, rec.Body.String())
	}
	return m
}

// ---- Health ----

func TestHealthz(t *testing.T) {
	srv, _ := setup(t)
	rec := doReq(srv, "GET", "/healthz", "", "")
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["status"] != "ok" {
		t.Errorf("status = %v", m["status"])
	}
}

func TestReadyz(t *testing.T) {
	srv, _ := setup(t)
	rec := doReq(srv, "GET", "/readyz", "", "")
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["redis"] != "connected" {
		t.Errorf("redis = %v", m["redis"])
	}
}

func TestAPIInfo(t *testing.T) {
	srv, _ := setup(t)
	rec := doReq(srv, "GET", "/api", "", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["version"] != "1.0.0" {
		t.Errorf("version = %v", m["version"])
	}
	endpoints := m["endpoints"].([]interface{})
	if len(endpoints) < 15 {
		t.Errorf("expected 15+ endpoints, got %d", len(endpoints))
	}
}

// ---- Auth ----

func TestSignup(t *testing.T) {
	srv, _ := setup(t)
	rec := doReq(srv, "POST", "/api/signup", `{"team_name":"new","sender_name":"bot"}`, "")
	if rec.Code != 201 {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	m := parseJSON(t, rec)
	if m["api_key"] == nil || m["team"] == nil {
		t.Error("missing api_key or team")
	}
}

func TestSignupMissingFields(t *testing.T) {
	srv, _ := setup(t)
	rec := doReq(srv, "POST", "/api/signup", `{"team_name":"x"}`, "")
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "MISSING_FIELDS" {
		t.Errorf("code = %v", m["code"])
	}
}

func TestAuthRequired(t *testing.T) {
	srv, _ := setup(t)
	rec := doReq(srv, "GET", "/api/channels", "", "")
	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "MISSING_API_KEY" {
		t.Errorf("code = %v", m["code"])
	}
}

func TestAuthInvalidKey(t *testing.T) {
	srv, _ := setup(t)
	rec := doReq(srv, "GET", "/api/channels", "", "relay_bogus")
	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "INVALID_API_KEY" {
		t.Errorf("code = %v", m["code"])
	}
}

func TestAuthTokenQueryParam(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/channels?token="+key, "", "")
	if rec.Code != 200 {
		t.Errorf("token auth: status = %d, want 200", rec.Code)
	}
}

// ---- Channels ----

func TestCreateAndListChannels(t *testing.T) {
	srv, key := setup(t)

	// Create
	rec := doReq(srv, "POST", "/api/channels", `{"name":"test-ch","description":"desc"}`, key)
	if rec.Code != 201 {
		t.Fatalf("create: status = %d, body: %s", rec.Code, rec.Body.String())
	}

	// Duplicate returns 200
	rec2 := doReq(srv, "POST", "/api/channels", `{"name":"test-ch"}`, key)
	if rec2.Code != 200 {
		t.Errorf("duplicate: status = %d, want 200", rec2.Code)
	}

	// List
	rec3 := doReq(srv, "GET", "/api/channels", "", key)
	if rec3.Code != 200 {
		t.Fatalf("list: status = %d", rec3.Code)
	}
	var channels []map[string]interface{}
	json.Unmarshal(rec3.Body.Bytes(), &channels)
	if len(channels) != 1 || channels[0]["name"] != "test-ch" {
		t.Errorf("unexpected channels: %v", channels)
	}
}

func TestInvalidChannelName(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels", `{"name":"bad/slash"}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "INVALID_CHANNEL_NAME" {
		t.Errorf("code = %v", m["code"])
	}
}

func TestDeleteChannel(t *testing.T) {
	srv, key := setup(t)
	doReq(srv, "POST", "/api/channels", `{"name":"del-me"}`, key)
	rec := doReq(srv, "DELETE", "/api/channels/del-me", "", key)
	if rec.Code != 204 {
		t.Errorf("delete: status = %d", rec.Code)
	}

	// 404 on second delete
	rec2 := doReq(srv, "DELETE", "/api/channels/del-me", "", key)
	if rec2.Code != 404 {
		t.Errorf("re-delete: status = %d, want 404", rec2.Code)
	}
}

// ---- Messages ----

func TestPostAndReadMessages(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"hello","mention":"bob"}`, key)
	if rec.Code != 201 {
		t.Fatalf("post: status = %d, body: %s", rec.Code, rec.Body.String())
	}
	m := parseJSON(t, rec)
	if m["content"] != "hello" || m["mention"] != "bob" {
		t.Errorf("unexpected msg: %v", m)
	}

	// Read
	rec2 := doReq(srv, "GET", "/api/channels/ch1/messages", "", key)
	if rec2.Code != 200 {
		t.Fatalf("read: status = %d", rec2.Code)
	}
	var result map[string]interface{}
	json.Unmarshal(rec2.Body.Bytes(), &result)
	msgs := result["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Errorf("expected 1 message, got %d", len(msgs))
	}
}

func TestContentTooLarge(t *testing.T) {
	srv, key := setup(t)
	big := strings.Repeat("x", store.MaxContentLength+1)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"`+big+`"}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "CONTENT_TOO_LARGE" {
		t.Errorf("code = %v", m["code"])
	}
}

func TestInvalidCursor(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/channels/ch1/messages?after_id=bad", "", key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "INVALID_CURSOR" {
		t.Errorf("code = %v", m["code"])
	}
}

func TestInvalidLimit(t *testing.T) {
	srv, key := setup(t)
	for _, limit := range []string{"0", "-1", "101", "abc"} {
		rec := doReq(srv, "GET", "/api/channels/ch1/messages?limit="+limit, "", key)
		if rec.Code != 400 {
			t.Errorf("limit=%s: status = %d, want 400", limit, rec.Code)
		}
	}
}

func TestCursorZeroZeroValid(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/channels/ch1/messages?after_id=0-0", "", key)
	if rec.Code != 200 {
		t.Errorf("0-0 cursor: status = %d, want 200", rec.Code)
	}
}

// ---- Threading ----

func TestThreading(t *testing.T) {
	srv, key := setup(t)

	// Post parent
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"parent"}`, key)
	parent := parseJSON(t, rec)
	parentID := parent["id"].(string)

	// Reply
	rec2 := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"reply","thread_id":"`+parentID+`"}`, key)
	if rec2.Code != 201 {
		t.Fatalf("reply: status = %d", rec2.Code)
	}
	reply := parseJSON(t, rec2)
	if reply["thread_id"] != parentID {
		t.Errorf("thread_id = %v, want %s", reply["thread_id"], parentID)
	}

	// Read thread
	rec3 := doReq(srv, "GET", "/api/channels/ch1/threads/"+parentID, "", key)
	if rec3.Code != 200 {
		t.Fatalf("read thread: status = %d, body: %s", rec3.Code, rec3.Body.String())
	}
	var thread map[string]interface{}
	json.Unmarshal(rec3.Body.Bytes(), &thread)
	parentObj := thread["parent"].(map[string]interface{})
	if parentObj["reply_count"].(float64) != 1 {
		t.Errorf("reply_count = %v, want 1", parentObj["reply_count"])
	}
	replies := thread["replies"].([]interface{})
	if len(replies) != 1 {
		t.Errorf("expected 1 reply, got %d", len(replies))
	}
}

func TestOrphanThread(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"orphan","thread_id":"9999999999999-0"}`, key)
	if rec.Code != 404 {
		t.Errorf("orphan: status = %d, want 404", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "THREAD_PARENT_NOT_FOUND" {
		t.Errorf("code = %v", m["code"])
	}
}

func TestThreadNotFound(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/channels/ch1/threads/9999999999999-0", "", key)
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ---- Acks ----

func TestAck(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"msg"}`, key)
	msg := parseJSON(t, rec)
	msgID := msg["id"].(string)

	rec2 := doReq(srv, "POST", "/api/channels/ch1/ack", `{"last_read_id":"`+msgID+`"}`, key)
	if rec2.Code != 200 {
		t.Fatalf("ack: status = %d", rec2.Code)
	}

	rec3 := doReq(srv, "GET", "/api/channels/ch1/acks", "", key)
	if rec3.Code != 200 {
		t.Fatalf("get acks: status = %d", rec3.Code)
	}
	var acks []map[string]interface{}
	json.Unmarshal(rec3.Body.Bytes(), &acks)
	if len(acks) != 1 {
		t.Errorf("expected 1 ack, got %d", len(acks))
	}
}

// ---- Status ----

func TestSetAndGetStatus(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "PUT", "/api/channels/ch1/status", `{"key":"stage","value":"build"}`, key)
	if rec.Code != 200 {
		t.Fatalf("set: status = %d, body: %s", rec.Code, rec.Body.String())
	}

	rec2 := doReq(srv, "GET", "/api/channels/ch1/status?key=stage", "", key)
	if rec2.Code != 200 {
		t.Fatalf("get: status = %d", rec2.Code)
	}
	var statuses []map[string]interface{}
	json.Unmarshal(rec2.Body.Bytes(), &statuses)
	if len(statuses) != 1 || statuses[0]["value"] != "build" {
		t.Errorf("unexpected: %v", statuses)
	}
}

func TestStatusChanges(t *testing.T) {
	srv, key := setup(t)
	doReq(srv, "PUT", "/api/channels/ch1/status", `{"key":"stage","value":"init"}`, key)
	doReq(srv, "PUT", "/api/channels/ch1/status", `{"key":"stage","value":"done"}`, key)

	rec := doReq(srv, "GET", "/api/channels/ch1/status/changes", "", key)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var result map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &result)
	changes := result["changes"].([]interface{})
	if len(changes) != 2 {
		t.Errorf("expected 2 changes, got %d", len(changes))
	}
}

func TestStatusKeyTooLarge(t *testing.T) {
	srv, key := setup(t)
	bigKey := strings.Repeat("k", store.MaxStatusKeyLength+1)
	rec := doReq(srv, "PUT", "/api/channels/ch1/status", `{"key":"`+bigKey+`","value":"v"}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestCrossChannelStatus(t *testing.T) {
	srv, key := setup(t)
	doReq(srv, "PUT", "/api/channels/ch1/status", `{"key":"stage","value":"a"}`, key)
	doReq(srv, "PUT", "/api/channels/ch2/status", `{"key":"stage","value":"b"}`, key)

	rec := doReq(srv, "GET", "/api/status?key=stage", "", key)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var statuses []map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &statuses)
	if len(statuses) != 2 {
		t.Errorf("expected 2, got %d", len(statuses))
	}
}

// ---- Rate Limiting ----

func TestRateLimitHeaders(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/channels", "", key)
	if rec.Header().Get("X-RateLimit-Limit") == "" {
		t.Error("missing X-RateLimit-Limit header")
	}
	if rec.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("missing X-RateLimit-Remaining header")
	}
}

// ---- Error handling ----

func TestInvalidJSON(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{bad json`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "INVALID_JSON" {
		t.Errorf("code = %v", m["code"])
	}
}
