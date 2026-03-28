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

// ---- Additional coverage tests ----

func TestCreateAndListKeys(t *testing.T) {
	srv, key := setup(t)
	// Create a second key
	rec := doReq(srv, "POST", "/api/keys", `{"sender_name":"agent-2"}`, key)
	if rec.Code != 201 {
		t.Fatalf("create key: status = %d, body: %s", rec.Code, rec.Body.String())
	}

	// List keys
	rec2 := doReq(srv, "GET", "/api/keys", "", key)
	if rec2.Code != 200 {
		t.Fatalf("list keys: status = %d", rec2.Code)
	}
	var keys []map[string]interface{}
	json.Unmarshal(rec2.Body.Bytes(), &keys)
	if len(keys) < 2 {
		t.Errorf("expected >= 2 keys, got %d", len(keys))
	}
}

func TestCreateKeyMissingFields(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/keys", `{}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestDeleteMessage(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"to-delete"}`, key)
	msg := parseJSON(t, rec)
	msgID := msg["id"].(string)

	rec2 := doReq(srv, "DELETE", "/api/channels/ch1/messages/"+msgID, "", key)
	if rec2.Code != 204 {
		t.Errorf("delete: status = %d", rec2.Code)
	}

	// Delete nonexistent
	rec3 := doReq(srv, "DELETE", "/api/channels/ch1/messages/9999999999-0", "", key)
	if rec3.Code != 404 {
		t.Errorf("delete nonexistent: status = %d, want 404", rec3.Code)
	}

	// Invalid ID format
	rec4 := doReq(srv, "DELETE", "/api/channels/ch1/messages/badid", "", key)
	if rec4.Code != 400 {
		t.Errorf("bad id: status = %d, want 400", rec4.Code)
	}
}

func TestUnread(t *testing.T) {
	srv, key := setup(t)
	doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"msg1"}`, key)
	doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"msg2"}`, key)

	rec := doReq(srv, "GET", "/api/channels/ch1/unread", "", key)
	if rec.Code != 200 {
		t.Fatalf("unread: status = %d", rec.Code)
	}
	var result map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &result)
	msgs := result["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Errorf("expected 2 unread, got %d", len(msgs))
	}
}

func TestUnreadBadLimit(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/channels/ch1/unread?limit=0", "", key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestDeleteStatus(t *testing.T) {
	srv, key := setup(t)
	doReq(srv, "PUT", "/api/channels/ch1/status", `{"key":"temp","value":"x"}`, key)

	rec := doReq(srv, "DELETE", "/api/channels/ch1/status/temp", "", key)
	if rec.Code != 204 {
		t.Errorf("delete: status = %d", rec.Code)
	}

	// Delete nonexistent
	rec2 := doReq(srv, "DELETE", "/api/channels/ch1/status/nope", "", key)
	if rec2.Code != 404 {
		t.Errorf("nonexistent: status = %d, want 404", rec2.Code)
	}
}

func TestStatusMissingKey(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "PUT", "/api/channels/ch1/status", `{"value":"v"}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestStatusValueTooLarge(t *testing.T) {
	srv, key := setup(t)
	big := strings.Repeat("v", store.MaxStatusValueLength+1)
	rec := doReq(srv, "PUT", "/api/channels/ch1/status", `{"key":"k","value":"`+big+`"}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestAckBadCursor(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/ack", `{"last_read_id":"bad"}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "INVALID_CURSOR" {
		t.Errorf("code = %v", m["code"])
	}
}

func TestAckMissingField(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/ack", `{}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestStatusChangesWithCursor(t *testing.T) {
	srv, key := setup(t)
	doReq(srv, "PUT", "/api/channels/ch1/status", `{"key":"k","value":"1"}`, key)
	doReq(srv, "PUT", "/api/channels/ch1/status", `{"key":"k","value":"2"}`, key)
	doReq(srv, "PUT", "/api/channels/ch1/status", `{"key":"k","value":"3"}`, key)

	// Forward pagination from 0-0
	rec := doReq(srv, "GET", "/api/channels/ch1/status/changes?after_id=0-0&limit=2", "", key)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var r1 map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &r1)
	changes1 := r1["changes"].([]interface{})
	if len(changes1) != 2 {
		t.Fatalf("page 1: expected 2, got %d", len(changes1))
	}
	if r1["has_more"] != true {
		t.Error("expected has_more=true")
	}

	// Page 2
	cursor := r1["next_after_id"].(string)
	rec2 := doReq(srv, "GET", "/api/channels/ch1/status/changes?after_id="+cursor+"&limit=10", "", key)
	if rec2.Code != 200 {
		t.Fatalf("status = %d", rec2.Code)
	}
	var r2 map[string]interface{}
	json.Unmarshal(rec2.Body.Bytes(), &r2)
	changes2 := r2["changes"].([]interface{})
	if len(changes2) != 1 {
		t.Errorf("page 2: expected 1, got %d", len(changes2))
	}
}

func TestStatusChangesBadCursor(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/channels/ch1/status/changes?after_id=bad", "", key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestStatusChangesBadLimit(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/channels/ch1/status/changes?limit=abc", "", key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestThreadBadCursor(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/channels/ch1/threads/badid", "", key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestThreadBadAfterID(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"p"}`, key)
	pid := parseJSON(t, rec)["id"].(string)
	rec2 := doReq(srv, "GET", "/api/channels/ch1/threads/"+pid+"?after_id=bad", "", key)
	if rec2.Code != 400 {
		t.Errorf("status = %d, want 400", rec2.Code)
	}
}

func TestThreadBadLimit(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"p"}`, key)
	pid := parseJSON(t, rec)["id"].(string)
	rec2 := doReq(srv, "GET", "/api/channels/ch1/threads/"+pid+"?limit=999", "", key)
	if rec2.Code != 400 {
		t.Errorf("status = %d, want 400", rec2.Code)
	}
}

func TestSignupBadJSON(t *testing.T) {
	srv, _ := setup(t)
	rec := doReq(srv, "POST", "/api/signup", `{bad`, "")
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestContentTooLargeExact(t *testing.T) {
	srv, key := setup(t)
	// Exactly at limit should pass
	exact := strings.Repeat("x", store.MaxContentLength)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"`+exact+`"}`, key)
	if rec.Code != 201 {
		t.Errorf("exact limit: status = %d, want 201", rec.Code)
	}
}

func TestMissingContent(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"mention":"x"}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestBadThreadID(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels/ch1/messages", `{"content":"x","thread_id":"bad"}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestReadMessagesBadThreadID(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/channels/ch1/messages?thread_id=bad", "", key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestJSON404(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "GET", "/api/nonexistent", "", key)
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "NOT_FOUND" {
		t.Errorf("code = %v", m["code"])
	}
}

func TestChannelMissingName(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "POST", "/api/channels", `{"description":"x"}`, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestDeleteChannelNotFound(t *testing.T) {
	srv, key := setup(t)
	rec := doReq(srv, "DELETE", "/api/channels/nonexistent", "", key)
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestXApiKeyAuth(t *testing.T) {
	srv, key := setup(t)
	req := httptest.NewRequest("GET", "/api/channels", nil)
	req.Header.Set("X-Api-Key", key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("X-Api-Key auth: status = %d, want 200", rec.Code)
	}
}

func TestSignupIPRateLimit(t *testing.T) {
	rdb := testutil.RedisClient(t)
	testutil.FlushDB(t, rdb)
	s := store.New(rdb)
	cfg := DefaultConfig()
	cfg.SignupPerIPMax = 2 // low limit for testing
	srv := New(s, rdb, cfg)

	doReq(srv, "POST", "/api/signup", `{"team_name":"t1","sender_name":"a"}`, "")
	doReq(srv, "POST", "/api/signup", `{"team_name":"t2","sender_name":"a"}`, "")
	rec := doReq(srv, "POST", "/api/signup", `{"team_name":"t3","sender_name":"a"}`, "")
	if rec.Code != 429 {
		t.Errorf("3rd signup: status = %d, want 429", rec.Code)
	}
}

func TestKeyLimitReached(t *testing.T) {
	rdb := testutil.RedisClient(t)
	testutil.FlushDB(t, rdb)
	s := store.New(rdb)
	cfg := DefaultConfig()
	cfg.MaxKeysPerTeam = 2 // team + 1 extra = 2 total
	srv := New(s, rdb, cfg)

	// Signup creates first key
	rec := doReq(srv, "POST", "/api/signup", `{"team_name":"limited","sender_name":"a1"}`, "")
	key := parseJSON(t, rec)["api_key"].(string)

	// Second key OK
	doReq(srv, "POST", "/api/keys", `{"sender_name":"a2"}`, key)

	// Third key should fail
	rec3 := doReq(srv, "POST", "/api/keys", `{"sender_name":"a3"}`, key)
	if rec3.Code != 403 {
		t.Errorf("3rd key: status = %d, want 403", rec3.Code)
	}
}

func TestChannelLimitReached(t *testing.T) {
	rdb := testutil.RedisClient(t)
	testutil.FlushDB(t, rdb)
	s := store.New(rdb)
	cfg := DefaultConfig()
	cfg.MaxChannelsPerTeam = 2
	srv := New(s, rdb, cfg)

	rec := doReq(srv, "POST", "/api/signup", `{"team_name":"limited","sender_name":"a"}`, "")
	key := parseJSON(t, rec)["api_key"].(string)

	doReq(srv, "POST", "/api/channels", `{"name":"ch1"}`, key)
	doReq(srv, "POST", "/api/channels", `{"name":"ch2"}`, key)
	rec3 := doReq(srv, "POST", "/api/channels", `{"name":"ch3"}`, key)
	if rec3.Code != 403 {
		t.Errorf("3rd channel: status = %d, want 403", rec3.Code)
	}
}

// ---- Signup field length validation ----

func TestSignupTeamNameTooLong(t *testing.T) {
	srv, _ := setup(t)
	longName := strings.Repeat("a", 129)
	body := `{"team_name":"` + longName + `","sender_name":"agent"}`
	rec := doReq(srv, "POST", "/api/signup", body, "")
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "FIELD_TOO_LARGE" {
		t.Errorf("code = %v, want FIELD_TOO_LARGE", m["code"])
	}
}

func TestSignupSenderNameTooLong(t *testing.T) {
	srv, _ := setup(t)
	longName := strings.Repeat("b", 129)
	body := `{"team_name":"team","sender_name":"` + longName + `"}`
	rec := doReq(srv, "POST", "/api/signup", body, "")
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "FIELD_TOO_LARGE" {
		t.Errorf("code = %v, want FIELD_TOO_LARGE", m["code"])
	}
}

func TestSignupFieldLengthAtLimit(t *testing.T) {
	srv, _ := setup(t)
	name128 := strings.Repeat("c", 128)
	body := `{"team_name":"` + name128 + `","sender_name":"agent"}`
	rec := doReq(srv, "POST", "/api/signup", body, "")
	if rec.Code != 201 {
		t.Errorf("status = %d, want 201 (128 chars should be ok)", rec.Code)
	}
}

func TestCreateKeySenderNameTooLong(t *testing.T) {
	srv, key := setup(t)
	longName := strings.Repeat("d", 129)
	body := `{"sender_name":"` + longName + `"}`
	rec := doReq(srv, "POST", "/api/keys", body, key)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	m := parseJSON(t, rec)
	if m["code"] != "FIELD_TOO_LARGE" {
		t.Errorf("code = %v, want FIELD_TOO_LARGE", m["code"])
	}
}

// ---- DefaultConfig ensures quotas work ----

// ---- Channel permissions ----

func setupWithScopedKey(t *testing.T, perms map[string]store.Permission) (*Server, string, string) {
	t.Helper()
	rdb := testutil.RedisClient(t)
	testutil.FlushDB(t, rdb)
	s := store.New(rdb)
	srv := New(s, rdb, DefaultConfig())

	team, adminKey, err := s.CreateTeam(t.Context(), "perm-team", "admin-agent")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	_ = team

	// Create a scoped key
	scopedKey, err := s.CreateApiKey(t.Context(), team.ID, "scoped-agent", perms)
	if err != nil {
		t.Fatalf("setup scoped key: %v", err)
	}
	return srv, adminKey, scopedKey
}

func TestReadOnlyKeyCannotPost(t *testing.T) {
	srv, _, scopedKey := setupWithScopedKey(t, map[string]store.Permission{"deploy": store.PermRead})

	// Create channel with admin key first isn't needed — ensureChannel auto-creates
	rec := doReq(srv, "POST", "/api/channels/deploy/messages", `{"content":"hello"}`, scopedKey)
	if rec.Code != 403 {
		t.Errorf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

func TestReadOnlyKeyCanRead(t *testing.T) {
	srv, adminKey, scopedKey := setupWithScopedKey(t, map[string]store.Permission{"deploy": store.PermRead})

	// Admin posts a message
	doReq(srv, "POST", "/api/channels/deploy/messages", `{"content":"hello"}`, adminKey)

	// Scoped key can read
	rec := doReq(srv, "GET", "/api/channels/deploy/messages", "", scopedKey)
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestWriteKeyCanPost(t *testing.T) {
	srv, _, scopedKey := setupWithScopedKey(t, map[string]store.Permission{"deploy": store.PermWrite})

	rec := doReq(srv, "POST", "/api/channels/deploy/messages", `{"content":"hello"}`, scopedKey)
	if rec.Code != 201 {
		t.Errorf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
}

func TestWriteKeyCannotDeleteChannel(t *testing.T) {
	srv, adminKey, scopedKey := setupWithScopedKey(t, map[string]store.Permission{"deploy": store.PermWrite})

	// Admin creates channel
	doReq(srv, "POST", "/api/channels", `{"name":"deploy"}`, adminKey)

	// Write key cannot delete
	rec := doReq(srv, "DELETE", "/api/channels/deploy", "", scopedKey)
	if rec.Code != 403 {
		t.Errorf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminKeyCanDeleteChannel(t *testing.T) {
	srv, adminKey, _ := setupWithScopedKey(t, map[string]store.Permission{"deploy": store.PermRead})

	doReq(srv, "POST", "/api/channels", `{"name":"deploy"}`, adminKey)
	rec := doReq(srv, "DELETE", "/api/channels/deploy", "", adminKey)
	if rec.Code != 204 {
		t.Errorf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
}

func TestLegacyKeyFullAccess(t *testing.T) {
	// setup() creates a key via CreateTeam which defaults to {"*": "admin"}
	srv, key := setup(t)

	rec := doReq(srv, "POST", "/api/channels", `{"name":"legacy-test"}`, key)
	if rec.Code != 201 {
		t.Errorf("create channel: status = %d, want 201", rec.Code)
	}
	rec2 := doReq(srv, "POST", "/api/channels/legacy-test/messages", `{"content":"hello"}`, key)
	if rec2.Code != 201 {
		t.Errorf("post message: status = %d, want 201", rec2.Code)
	}
	rec3 := doReq(srv, "DELETE", "/api/channels/legacy-test", "", key)
	if rec3.Code != 204 {
		t.Errorf("delete channel: status = %d, want 204", rec3.Code)
	}
}

func TestScopedKeyDeniedOnOtherChannel(t *testing.T) {
	srv, _, scopedKey := setupWithScopedKey(t, map[string]store.Permission{"deploy": store.PermWrite})

	// Can post to deploy
	rec := doReq(srv, "POST", "/api/channels/deploy/messages", `{"content":"ok"}`, scopedKey)
	if rec.Code != 201 {
		t.Errorf("deploy post: status = %d, want 201", rec.Code)
	}

	// Cannot post to other channel (no wildcard, no match)
	rec2 := doReq(srv, "POST", "/api/channels/production/messages", `{"content":"nope"}`, scopedKey)
	if rec2.Code != 403 {
		t.Errorf("production post: status = %d, want 403", rec2.Code)
	}
}

func TestCreateKeyWithPermissions(t *testing.T) {
	srv, key := setup(t)
	body := `{"sender_name":"junior","permissions":{"deploy":"read","dev":"write"}}`
	rec := doReq(srv, "POST", "/api/keys", body, key)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	m := parseJSON(t, rec)
	perms := m["permissions"].(map[string]interface{})
	if perms["deploy"] != "read" {
		t.Errorf("deploy = %v, want read", perms["deploy"])
	}
	if perms["dev"] != "write" {
		t.Errorf("dev = %v, want write", perms["dev"])
	}
}

func TestUpdatePermissionsEndpoint(t *testing.T) {
	rdb := testutil.RedisClient(t)
	testutil.FlushDB(t, rdb)
	s := store.New(rdb)
	srv := New(s, rdb, DefaultConfig())

	team, adminKey, _ := s.CreateTeam(t.Context(), "perm-crud-team", "admin")
	scopedKey, _ := s.CreateApiKey(t.Context(), team.ID, "agent", map[string]store.Permission{"*": store.PermRead})

	// Get the key hash by validating
	ak, _ := s.ValidateApiKey(t.Context(), scopedKey)

	// Update permissions
	body := `{"permissions":{"deploy":"admin","*":"write"}}`
	rec := doReq(srv, "PUT", "/api/keys/"+ak.Key+"/permissions", body, adminKey)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Verify updated perms
	ak2, _ := s.ValidateApiKey(t.Context(), scopedKey)
	if ak2.Permissions["deploy"] != store.PermAdmin {
		t.Errorf("deploy = %v, want admin", ak2.Permissions["deploy"])
	}
	if ak2.Permissions["*"] != store.PermWrite {
		t.Errorf("* = %v, want write", ak2.Permissions["*"])
	}
}

func TestDefaultConfigAllowsSignupAndChannels(t *testing.T) {
	// Verify DefaultConfig() has non-zero quotas (the old bug was Config{} with zero values)
	rdb := testutil.RedisClient(t)
	testutil.FlushDB(t, rdb)
	s := store.New(rdb)
	srv := New(s, rdb, DefaultConfig())

	// Signup should work
	rec := doReq(srv, "POST", "/api/signup", `{"team_name":"test","sender_name":"agent"}`, "")
	if rec.Code != 201 {
		t.Fatalf("signup status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	key := parseJSON(t, rec)["api_key"].(string)

	// Channel creation should work
	rec2 := doReq(srv, "POST", "/api/channels", `{"name":"general"}`, key)
	if rec2.Code != 201 {
		t.Errorf("create channel status = %d, want 201; body: %s", rec2.Code, rec2.Body.String())
	}

	// Key creation should work
	rec3 := doReq(srv, "POST", "/api/keys", `{"sender_name":"agent2"}`, key)
	if rec3.Code != 201 {
		t.Errorf("create key status = %d, want 201; body: %s", rec3.Code, rec3.Body.String())
	}
}
