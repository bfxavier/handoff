package store

import (
	"context"
	"testing"
	"time"

	"github.com/bfxavier/handoff/server/testutil"
	"github.com/redis/go-redis/v9"
)

var _ = redis.Nil // ensure import

func setup(t *testing.T) (*Store, context.Context) {
	t.Helper()
	rdb := testutil.RedisClient(t)
	testutil.FlushDB(t, rdb)
	return New(rdb), context.Background()
}

// ---- Validation ----

func TestIsValidCursor(t *testing.T) {
	tests := []struct{ in string; want bool }{
		{"1234567890-0", true},
		{"0-0", true},
		{"123-456", true},
		{"0", false},
		{"abc", false},
		{"123-", false},
		{"-123", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsValidCursor(tt.in); got != tt.want {
			t.Errorf("IsValidCursor(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIsValidChannelName(t *testing.T) {
	tests := []struct{ in string; want bool }{
		{"my-channel", true},
		{"deploy.prod", true},
		{"ch_1", true},
		{"A1", true},
		{"test/slash", false},
		{"-starts-dash", false},
		{".starts-dot", false},
		{"", false},
		{"a", true},
	}
	for _, tt := range tests {
		if got := IsValidChannelName(tt.in); got != tt.want {
			t.Errorf("IsValidChannelName(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// ---- Auth ----

func TestCreateTeamAndValidateKey(t *testing.T) {
	s, ctx := setup(t)

	team, apiKey, err := s.CreateTeam(ctx, "test-team", "agent-1")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if team.Name != "test-team" {
		t.Errorf("team name = %q, want %q", team.Name, "test-team")
	}
	if apiKey == "" || apiKey[:6] != "relay_" {
		t.Errorf("unexpected api key format: %q", apiKey)
	}

	// Validate
	ak, err := s.ValidateApiKey(ctx, apiKey)
	if err != nil {
		t.Fatalf("ValidateApiKey: %v", err)
	}
	if ak == nil {
		t.Fatal("expected api key, got nil")
	}
	if ak.Sender != "agent-1" {
		t.Errorf("sender = %q, want %q", ak.Sender, "agent-1")
	}
	if ak.TeamID != team.ID {
		t.Errorf("team_id = %q, want %q", ak.TeamID, team.ID)
	}
}

func TestValidateInvalidKey(t *testing.T) {
	s, ctx := setup(t)
	ak, err := s.ValidateApiKey(ctx, "relay_bogus")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ak != nil {
		t.Errorf("expected nil for invalid key, got %+v", ak)
	}
}

func TestCreateMultipleKeys(t *testing.T) {
	s, ctx := setup(t)
	team, _, err := s.CreateTeam(ctx, "multi-key", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	key2, err := s.CreateApiKey(ctx, team.ID, "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	ak, _ := s.ValidateApiKey(ctx, key2)
	if ak.Sender != "agent-2" {
		t.Errorf("sender = %q, want agent-2", ak.Sender)
	}
}

// ---- Channels ----

func TestCreateAndListChannels(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	desc := "test channel"

	ch, created, err := s.CreateChannel(ctx, teamID, "general", &desc)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("expected created=true")
	}
	if ch.Name != "general" || ch.Description == nil || *ch.Description != "test channel" {
		t.Errorf("unexpected channel: %+v", ch)
	}

	// Duplicate
	_, created2, _ := s.CreateChannel(ctx, teamID, "general", nil)
	if created2 {
		t.Error("expected created=false for duplicate")
	}

	// List
	channels, err := s.ListChannels(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || channels[0].Name != "general" {
		t.Errorf("unexpected channels: %+v", channels)
	}
}

func TestDeleteChannel(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	s.CreateChannel(ctx, teamID, "to-delete", nil)
	s.PostMessage(ctx, teamID, "to-delete", "sender", "msg", nil, nil)
	s.SetStatus(ctx, teamID, "to-delete", "k", "v", nil)

	deleted, err := s.DeleteChannel(ctx, teamID, "to-delete")
	if err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}

	channels, _ := s.ListChannels(ctx, teamID)
	if len(channels) != 0 {
		t.Errorf("expected 0 channels after delete, got %d", len(channels))
	}

	// Delete nonexistent
	deleted2, _ := s.DeleteChannel(ctx, teamID, "nonexistent")
	if deleted2 {
		t.Error("expected false for nonexistent")
	}
}

// ---- Messages ----

func TestPostAndReadMessages(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	mention := "agent-2"

	msg, err := s.PostMessage(ctx, teamID, "ch1", "agent-1", "hello", &mention, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Channel != "ch1" || msg.Sender != "agent-1" || msg.Content != "hello" {
		t.Errorf("unexpected msg: %+v", msg)
	}
	if msg.Mention == nil || *msg.Mention != "agent-2" {
		t.Error("expected mention")
	}
	if !IsValidCursor(msg.ID) {
		t.Errorf("invalid msg ID: %q", msg.ID)
	}

	// Read
	result, err := s.ReadMessages(ctx, teamID, "ch1", nil, 50, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
	if result.Messages[0].ID != msg.ID {
		t.Error("message ID mismatch")
	}
}

func TestReadMessagesWithCursor(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	// Post 5 messages
	for i := 0; i < 5; i++ {
		s.PostMessage(ctx, teamID, "ch1", "sender", "msg", nil, nil)
	}

	// Initial read (no cursor) returns latest N in chronological order
	r0, _ := s.ReadMessages(ctx, teamID, "ch1", nil, 2, nil, nil, nil)
	if len(r0.Messages) != 2 {
		t.Fatalf("initial read: expected 2, got %d", len(r0.Messages))
	}
	if !r0.HasMore {
		t.Error("expected has_more=true")
	}

	// Forward pagination: start from 0-0 to get all messages in order
	start := "0-0"
	r1, _ := s.ReadMessages(ctx, teamID, "ch1", &start, 2, nil, nil, nil)
	if len(r1.Messages) != 2 {
		t.Fatalf("page 1: expected 2, got %d", len(r1.Messages))
	}
	if !r1.HasMore {
		t.Error("expected has_more=true")
	}

	// Page 2
	r2, _ := s.ReadMessages(ctx, teamID, "ch1", &r1.NextAfterID, 2, nil, nil, nil)
	if len(r2.Messages) != 2 {
		t.Fatalf("page 2: expected 2, got %d", len(r2.Messages))
	}
	if !r2.HasMore {
		t.Error("expected has_more=true for page 2")
	}

	// Page 3 (last)
	r3, _ := s.ReadMessages(ctx, teamID, "ch1", &r2.NextAfterID, 2, nil, nil, nil)
	if len(r3.Messages) != 1 {
		t.Fatalf("page 3: expected 1, got %d", len(r3.Messages))
	}
	if r3.HasMore {
		t.Error("expected has_more=false")
	}
}

func TestReadMessagesFilterMentionAndSender(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	m := "bob"
	s.PostMessage(ctx, teamID, "ch1", "alice", "hi bob", &m, nil)
	s.PostMessage(ctx, teamID, "ch1", "bob", "hi alice", nil, nil)

	// Filter by mention
	r, _ := s.ReadMessages(ctx, teamID, "ch1", nil, 50, &m, nil, nil)
	if len(r.Messages) != 1 || r.Messages[0].Sender != "alice" {
		t.Errorf("mention filter: got %d msgs", len(r.Messages))
	}

	// Filter by sender
	bob := "bob"
	r2, _ := s.ReadMessages(ctx, teamID, "ch1", nil, 50, nil, &bob, nil)
	if len(r2.Messages) != 1 || r2.Messages[0].Sender != "bob" {
		t.Errorf("sender filter: got %d msgs", len(r2.Messages))
	}
}

func TestEmptyReadReturnsCursor(t *testing.T) {
	s, ctx := setup(t)
	r, err := s.ReadMessages(ctx, "team1", "empty", nil, 50, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.NextAfterID != "0-0" {
		t.Errorf("expected 0-0, got %q", r.NextAfterID)
	}
	if !IsValidCursor(r.NextAfterID) {
		t.Error("default cursor should be valid")
	}
}

func TestDeleteMessage(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	msg, _ := s.PostMessage(ctx, teamID, "ch1", "sender", "to delete", nil, nil)

	deleted, _ := s.DeleteMessage(ctx, teamID, "ch1", msg.ID)
	if !deleted {
		t.Error("expected deleted=true")
	}

	// Read back
	r, _ := s.ReadMessages(ctx, teamID, "ch1", nil, 50, nil, nil, nil)
	if len(r.Messages) != 0 {
		t.Errorf("expected 0 messages after delete, got %d", len(r.Messages))
	}

	// Delete nonexistent
	deleted2, _ := s.DeleteMessage(ctx, teamID, "ch1", "9999999-0")
	if deleted2 {
		t.Error("expected false for nonexistent")
	}
}

// ---- Threading ----

func TestThreading(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	parent, _ := s.PostMessage(ctx, teamID, "ch1", "alice", "parent msg", nil, nil)
	reply1, _ := s.PostMessage(ctx, teamID, "ch1", "bob", "reply 1", nil, &parent.ID)
	reply2, _ := s.PostMessage(ctx, teamID, "ch1", "alice", "reply 2", nil, &parent.ID)

	if reply1.ThreadID == nil || *reply1.ThreadID != parent.ID {
		t.Error("reply should have thread_id")
	}

	// Read thread
	thread, err := s.ReadThread(ctx, teamID, "ch1", parent.ID, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if thread.Parent.ID != parent.ID {
		t.Error("wrong parent")
	}
	if thread.Parent.ReplyCount != 2 {
		t.Errorf("reply_count = %d, want 2", thread.Parent.ReplyCount)
	}
	if len(thread.Replies) != 2 {
		t.Fatalf("expected 2 replies, got %d", len(thread.Replies))
	}
	if thread.Replies[0].ID != reply1.ID || thread.Replies[1].ID != reply2.ID {
		t.Error("replies out of order")
	}

	// Parent in main stream should have reply_count
	r, _ := s.ReadMessages(ctx, teamID, "ch1", nil, 50, nil, nil, nil)
	for _, m := range r.Messages {
		if m.ID == parent.ID && m.ReplyCount != 2 {
			t.Errorf("main stream reply_count = %d, want 2", m.ReplyCount)
		}
	}
}

func TestThreadPagination(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	parent, _ := s.PostMessage(ctx, teamID, "ch1", "alice", "parent", nil, nil)
	for i := 0; i < 5; i++ {
		s.PostMessage(ctx, teamID, "ch1", "bob", "reply", nil, &parent.ID)
	}

	// Page 1
	t1, _ := s.ReadThread(ctx, teamID, "ch1", parent.ID, nil, 2)
	if len(t1.Replies) != 2 || !t1.HasMore {
		t.Errorf("page 1: %d replies, has_more=%v", len(t1.Replies), t1.HasMore)
	}

	// Page 2
	t2, _ := s.ReadThread(ctx, teamID, "ch1", parent.ID, &t1.NextAfterID, 2)
	if len(t2.Replies) != 2 || !t2.HasMore {
		t.Errorf("page 2: %d replies, has_more=%v", len(t2.Replies), t2.HasMore)
	}

	// Page 3
	t3, _ := s.ReadThread(ctx, teamID, "ch1", parent.ID, &t2.NextAfterID, 2)
	if len(t3.Replies) != 1 || t3.HasMore {
		t.Errorf("page 3: %d replies, has_more=%v", len(t3.Replies), t3.HasMore)
	}
}

func TestThreadParentNotFound(t *testing.T) {
	s, ctx := setup(t)
	_, err := s.ReadThread(ctx, "team1", "ch1", "9999999-0", nil, 50)
	if _, ok := err.(ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestMessageExistsValidation(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	msg, _ := s.PostMessage(ctx, teamID, "ch1", "sender", "exists", nil, nil)

	exists, _ := s.MessageExists(ctx, teamID, "ch1", msg.ID)
	if !exists {
		t.Error("expected true")
	}

	exists2, _ := s.MessageExists(ctx, teamID, "ch1", "9999999-0")
	if exists2 {
		t.Error("expected false")
	}
}

// ---- Acks ----

func TestAckMessages(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	msg, _ := s.PostMessage(ctx, teamID, "ch1", "sender", "msg", nil, nil)

	ack, err := s.AckMessages(ctx, teamID, "ch1", "reader", msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ack.LastReadID != msg.ID {
		t.Errorf("last_read_id = %q, want %q", ack.LastReadID, msg.ID)
	}

	// Ack doesn't go backwards
	msg2, _ := s.PostMessage(ctx, teamID, "ch1", "sender", "msg2", nil, nil)
	s.AckMessages(ctx, teamID, "ch1", "reader", msg2.ID)
	ack3, _ := s.AckMessages(ctx, teamID, "ch1", "reader", msg.ID) // earlier ID
	if ack3.LastReadID != msg2.ID {
		t.Errorf("ack went backwards: %q", ack3.LastReadID)
	}

	// GetAcks
	acks, _ := s.GetAcks(ctx, teamID, "ch1")
	if len(acks) != 1 || acks[0].Sender != "reader" {
		t.Errorf("unexpected acks: %+v", acks)
	}
}

// ---- Status ----

func TestSetAndGetStatus(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	sender := "agent-1"

	st, err := s.SetStatus(ctx, teamID, "ch1", "stage", "building", &sender)
	if err != nil {
		t.Fatal(err)
	}
	if st.Value != "building" || st.UpdatedBy == nil || *st.UpdatedBy != "agent-1" {
		t.Errorf("unexpected status: %+v", st)
	}

	// Get
	ch := "ch1"
	key := "stage"
	statuses, _ := s.GetStatus(ctx, teamID, &ch, &key)
	if len(statuses) != 1 || statuses[0].Value != "building" {
		t.Errorf("get status: %+v", statuses)
	}
}

func TestStatusChanges(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	s.SetStatus(ctx, teamID, "ch1", "stage", "init", nil)
	s.SetStatus(ctx, teamID, "ch1", "stage", "building", nil)
	s.SetStatus(ctx, teamID, "ch1", "stage", "done", nil)

	result, err := s.GetStatusChanges(ctx, teamID, "ch1", nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 3 {
		t.Fatalf("expected 3 changes, got %d", len(result.Changes))
	}
	if result.Changes[0].Value != "init" || result.Changes[2].Value != "done" {
		t.Error("changes out of order")
	}
}

func TestCrossChannelStatus(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	s.SetStatus(ctx, teamID, "ch1", "stage", "build", nil)
	s.SetStatus(ctx, teamID, "ch2", "stage", "deploy", nil)

	key := "stage"
	statuses, _ := s.GetStatus(ctx, teamID, nil, &key)
	if len(statuses) != 2 {
		t.Fatalf("expected 2, got %d", len(statuses))
	}
}

// ---- Rate Limiting ----

func TestRateLimit(t *testing.T) {
	s, ctx := setup(t)

	// 3 requests per window
	for i := 0; i < 3; i++ {
		allowed, _, _ := s.CheckRateLimit(ctx, "testkey", 3, 10000)
		if !allowed {
			t.Errorf("request %d should be allowed", i+1)
		}
	}
	allowed, remaining, _ := s.CheckRateLimit(ctx, "testkey", 3, 10000)
	if allowed {
		t.Error("4th request should be rejected")
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0", remaining)
	}
}

// ---- Blocking Read (SSE) ----

func TestBlockingRead(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	// Post a message first
	msg, _ := s.PostMessage(ctx, teamID, "ch1", "sender", "hello", nil, nil)

	// Blocking read from before the message
	msgs, err := s.BlockingRead(ctx, teamID, "ch1", "0-0", 1*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != msg.ID {
		t.Errorf("expected 1 msg, got %d", len(msgs))
	}

	// Blocking read with no new messages (should timeout)
	msgs2, err := s.BlockingRead(ctx, teamID, "ch1", msg.ID, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 0 {
		t.Errorf("expected 0 msgs on timeout, got %d", len(msgs2))
	}
}

// ---- Additional coverage tests ----

func TestErrorTypes(t *testing.T) {
	e1 := ErrNotFound{What: "message"}
	if e1.Error() != "message not found" {
		t.Errorf("ErrNotFound.Error() = %q", e1.Error())
	}
	e2 := ErrValidation{Message: "bad input"}
	if e2.Error() != "bad input" {
		t.Errorf("ErrValidation.Error() = %q", e2.Error())
	}
}

func TestRedisAccessor(t *testing.T) {
	s, _ := setup(t)
	if s.Redis() == nil {
		t.Error("Redis() returned nil")
	}
}

func TestListApiKeys(t *testing.T) {
	s, ctx := setup(t)
	team, key1, _ := s.CreateTeam(ctx, "list-keys-team", "agent-1")
	key2, _ := s.CreateApiKey(ctx, team.ID, "agent-2")
	_ = key1
	_ = key2

	keys, err := s.ListApiKeys(ctx, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	// Keys should be masked
	for _, k := range keys {
		if len(k.Key) > 20 {
			t.Error("key should be masked but looks full length")
		}
		if k.TeamID != team.ID {
			t.Error("wrong team")
		}
	}
}

func TestGetUnread(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	msg1, _ := s.PostMessage(ctx, teamID, "ch1", "alice", "msg1", nil, nil)
	s.PostMessage(ctx, teamID, "ch1", "alice", "msg2", nil, nil)
	s.PostMessage(ctx, teamID, "ch1", "alice", "msg3", nil, nil)

	// Before any ack — all messages unread
	unread, err := s.GetUnread(ctx, teamID, "ch1", "bob", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(unread.Messages) != 3 {
		t.Fatalf("expected 3 unread, got %d", len(unread.Messages))
	}

	// Ack first message
	s.AckMessages(ctx, teamID, "ch1", "bob", msg1.ID)

	unread2, _ := s.GetUnread(ctx, teamID, "ch1", "bob", 50)
	if len(unread2.Messages) != 2 {
		t.Errorf("expected 2 unread after ack, got %d", len(unread2.Messages))
	}
}

func TestDeleteStatus(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	s.SetStatus(ctx, teamID, "ch1", "temp", "value", nil)
	deleted, err := s.DeleteStatus(ctx, teamID, "ch1", "temp")
	if err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}

	// Should be gone
	ch := "ch1"
	key := "temp"
	statuses, _ := s.GetStatus(ctx, teamID, &ch, &key)
	if len(statuses) != 0 {
		t.Errorf("expected 0 statuses after delete, got %d", len(statuses))
	}

	// Delete nonexistent
	deleted2, _ := s.DeleteStatus(ctx, teamID, "ch1", "nonexistent")
	if deleted2 {
		t.Error("expected false for nonexistent")
	}
}

func TestTrimMessages(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	for i := 0; i < 20; i++ {
		s.PostMessage(ctx, teamID, "ch1", "sender", "msg", nil, nil)
	}

	s.TrimMessages(ctx, teamID, "ch1", 10)

	r, _ := s.ReadMessages(ctx, teamID, "ch1", nil, 100, nil, nil, nil)
	if len(r.Messages) != 10 {
		t.Errorf("expected 10 after trim, got %d", len(r.Messages))
	}

	// Trim with 0 should be no-op
	s.TrimMessages(ctx, teamID, "ch1", 0)
	r2, _ := s.ReadMessages(ctx, teamID, "ch1", nil, 100, nil, nil, nil)
	if len(r2.Messages) != 10 {
		t.Errorf("expected 10 after no-op trim, got %d", len(r2.Messages))
	}
}

func TestGetStatusAllChannelsNoKey(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	s.SetStatus(ctx, teamID, "ch1", "a", "1", nil)
	s.SetStatus(ctx, teamID, "ch1", "b", "2", nil)
	s.SetStatus(ctx, teamID, "ch2", "c", "3", nil)

	// Cross-channel, no key filter
	statuses, err := s.GetStatus(ctx, teamID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 3 {
		t.Errorf("expected 3, got %d", len(statuses))
	}
}

func TestStatusChangesWithCursor(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	s.SetStatus(ctx, teamID, "ch1", "k", "v1", nil)
	s.SetStatus(ctx, teamID, "ch1", "k", "v2", nil)
	s.SetStatus(ctx, teamID, "ch1", "k", "v3", nil)

	// Page 1
	r1, _ := s.GetStatusChanges(ctx, teamID, "ch1", nil, 2)
	if len(r1.Changes) != 2 || !r1.HasMore {
		t.Errorf("page 1: %d changes, has_more=%v", len(r1.Changes), r1.HasMore)
	}

	// Page 2
	r2, _ := s.GetStatusChanges(ctx, teamID, "ch1", &r1.NextAfterID, 2)
	if len(r2.Changes) != 1 || r2.HasMore {
		t.Errorf("page 2: %d changes, has_more=%v", len(r2.Changes), r2.HasMore)
	}
}

func TestSetStatusNilUpdatedBy(t *testing.T) {
	s, ctx := setup(t)
	st, err := s.SetStatus(ctx, "team1", "ch1", "k", "v", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.UpdatedBy != nil {
		t.Errorf("expected nil updated_by, got %v", st.UpdatedBy)
	}
}

func TestReadMessagesDefaultLimit(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	for i := 0; i < 5; i++ {
		s.PostMessage(ctx, teamID, "ch1", "sender", "msg", nil, nil)
	}
	// Pass 0 limit — should default to 50
	r, _ := s.ReadMessages(ctx, teamID, "ch1", nil, 0, nil, nil, nil)
	if len(r.Messages) != 5 {
		t.Errorf("expected 5, got %d", len(r.Messages))
	}
}

func TestReadThreadDefaultLimit(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"
	parent, _ := s.PostMessage(ctx, teamID, "ch1", "alice", "parent", nil, nil)
	s.PostMessage(ctx, teamID, "ch1", "bob", "reply", nil, &parent.ID)

	// Pass 0 limit — should default to 50
	thread, _ := s.ReadThread(ctx, teamID, "ch1", parent.ID, nil, 0)
	if len(thread.Replies) != 1 {
		t.Errorf("expected 1, got %d", len(thread.Replies))
	}
}

// ---- compareStreamIDs ----

func TestCompareStreamIDs(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"10-1", "9-9", 1},    // numeric > lexicographic
		{"9-9", "10-1", -1},   // the old bug case
		{"100-0", "100-0", 0}, // equal
		{"100-1", "100-2", -1},
		{"100-2", "100-1", 1},
		{"1000000000000-0", "999999999999-0", 1},
	}
	for _, tt := range tests {
		got := compareStreamIDs(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("compareStreamIDs(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// ---- Rate limit key hashing ----

func TestCheckRateLimitKeyNotExposed(t *testing.T) {
	s, ctx := setup(t)
	secretKey := "relay_supersecretkey123456"
	s.CheckRateLimit(ctx, secretKey, 100, 1000)

	// SCAN for keys containing the raw secret — should find none
	var cursor uint64
	for {
		keys, next, err := s.Redis().Scan(ctx, cursor, "*"+secretKey+"*", 100).Result()
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(keys) > 0 {
			t.Errorf("found raw API key in Redis key names: %v", keys)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	// Verify rate limit key exists with hashed prefix
	keys, _, _ := s.Redis().Scan(ctx, 0, "rl:*", 100).Result()
	if len(keys) == 0 {
		t.Error("no rate limit keys found")
	}
}

// ---- Filtered pagination (scan-and-fill) ----

func TestReadMessagesFilteredPagination(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	// Post 20 messages: 10 from alice, 10 from bob, interleaved
	for i := 0; i < 20; i++ {
		sender := "alice"
		if i%2 == 1 {
			sender = "bob"
		}
		s.PostMessage(ctx, teamID, "ch1", sender, "msg", nil, nil)
	}

	// Read with sender filter, limit 5 — should get exactly 5 alice messages
	alice := "alice"
	r, err := s.ReadMessages(ctx, teamID, "ch1", nil, 5, nil, &alice, nil)
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(r.Messages) != 5 {
		t.Errorf("expected 5 alice messages, got %d", len(r.Messages))
	}
	for _, m := range r.Messages {
		if m.Sender != "alice" {
			t.Errorf("expected sender alice, got %s", m.Sender)
		}
	}

	// Paginate: read next 5 using cursor
	r2, err := s.ReadMessages(ctx, teamID, "ch1", &r.NextAfterID, 5, nil, &alice, nil)
	if err != nil {
		t.Fatalf("ReadMessages page 2: %v", err)
	}
	if len(r2.Messages) != 5 {
		t.Errorf("expected 5 alice messages on page 2, got %d", len(r2.Messages))
	}

	// No overlap between pages
	lastPage1 := r.Messages[len(r.Messages)-1].ID
	firstPage2 := r2.Messages[0].ID
	if compareStreamIDs(firstPage2, lastPage1) <= 0 {
		t.Errorf("page 2 first message (%s) should be after page 1 last (%s)", firstPage2, lastPage1)
	}
}

func TestReadMessagesFilteredMentionPagination(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	// Post 15 messages: only 3 mention "bob"
	bob := "bob"
	for i := 0; i < 15; i++ {
		var mention *string
		if i == 3 || i == 7 || i == 12 {
			mention = &bob
		}
		s.PostMessage(ctx, teamID, "ch1", "alice", "msg", mention, nil)
	}

	// Read with mention filter, limit 10 — should get all 3
	r, err := s.ReadMessages(ctx, teamID, "ch1", nil, 10, &bob, nil, nil)
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(r.Messages) != 3 {
		t.Errorf("expected 3 messages mentioning bob, got %d", len(r.Messages))
	}
}

// ---- Pipelined ListChannels ----

func TestListChannelsPipelined(t *testing.T) {
	s, ctx := setup(t)
	teamID := "team1"

	desc := "test desc"
	s.CreateChannel(ctx, teamID, "alpha", &desc)
	s.CreateChannel(ctx, teamID, "beta", nil)
	s.CreateChannel(ctx, teamID, "gamma", nil)

	channels, err := s.ListChannels(ctx, teamID)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(channels) != 3 {
		t.Fatalf("expected 3 channels, got %d", len(channels))
	}
	// Should be sorted
	if channels[0].Name != "alpha" || channels[1].Name != "beta" || channels[2].Name != "gamma" {
		t.Errorf("unexpected order: %v, %v, %v", channels[0].Name, channels[1].Name, channels[2].Name)
	}
	if channels[0].Description == nil || *channels[0].Description != "test desc" {
		t.Errorf("expected description 'test desc', got %v", channels[0].Description)
	}
}

func TestListChannelsEmpty(t *testing.T) {
	s, ctx := setup(t)
	channels, err := s.ListChannels(ctx, "no-such-team")
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(channels) != 0 {
		t.Errorf("expected 0 channels, got %d", len(channels))
	}
}
