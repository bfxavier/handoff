package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limits
const (
	MaxContentLength    = 32768
	MaxStatusKeyLength  = 256
	MaxStatusValueLength = 32768
	MaxChannelNameLength = 128
)

var (
	CursorRE      = regexp.MustCompile(`^\d+-\d+$`)
	ChannelNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
)

// Domain types

type Team struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type ApiKey struct {
	Key       string `json:"key"`
	TeamID    string `json:"team_id"`
	Sender    string `json:"sender"`
	CreatedAt string `json:"created_at"`
}

type Channel struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	CreatedAt   string  `json:"created_at"`
}

type Message struct {
	ID         string  `json:"id"`
	Channel    string  `json:"channel"`
	Sender     string  `json:"sender"`
	Content    string  `json:"content"`
	Mention    *string `json:"mention"`
	ThreadID   *string `json:"thread_id"`
	ReplyCount int     `json:"reply_count"`
	CreatedAt  string  `json:"created_at"`
}

type ReadResult struct {
	Messages   []Message `json:"messages"`
	NextAfterID string   `json:"next_after_id"`
	HasMore    bool      `json:"has_more"`
	Channel    string    `json:"channel"`
}

type ThreadResult struct {
	Parent      Message   `json:"parent"`
	Replies     []Message `json:"replies"`
	NextAfterID string    `json:"next_after_id"`
	HasMore     bool      `json:"has_more"`
}

type Status struct {
	Channel   string  `json:"channel"`
	Key       string  `json:"key"`
	Value     string  `json:"value"`
	UpdatedBy *string `json:"updated_by"`
	UpdatedAt string  `json:"updated_at"`
}

type StatusChange struct {
	ID        string  `json:"id"`
	Channel   string  `json:"channel"`
	Key       string  `json:"key"`
	Value     string  `json:"value"`
	ChangedBy *string `json:"changed_by"`
	ChangedAt string  `json:"changed_at"`
}

type StatusChangesResult struct {
	Changes     []StatusChange `json:"changes"`
	NextAfterID string         `json:"next_after_id"`
	HasMore     bool           `json:"has_more"`
}

type Ack struct {
	Channel    string `json:"channel"`
	Sender     string `json:"sender"`
	LastReadID string `json:"last_read_id"`
	AckedAt    string `json:"acked_at"`
}

// Error types

type ErrNotFound struct{ What string }
func (e ErrNotFound) Error() string { return e.What + " not found" }

type ErrValidation struct{ Message string }
func (e ErrValidation) Error() string { return e.Message }

// Validation helpers

func IsValidCursor(id string) bool {
	return CursorRE.MatchString(id)
}

func IsValidChannelName(name string) bool {
	return ChannelNameRE.MatchString(name)
}

// Store

type Store struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

func (s *Store) Redis() *redis.Client {
	return s.rdb
}

func (s *Store) k(teamID, suffix string) string {
	return fmt.Sprintf("t:%s:%s", teamID, suffix)
}

func generateID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateApiKeyStr() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "relay_" + hex.EncodeToString(b)
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// ---- Auth ----

func (s *Store) CreateTeam(ctx context.Context, name, senderName string) (*Team, string, error) {
	id := generateID()
	ts := now()
	team := &Team{ID: id, Name: name, CreatedAt: ts}

	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, "auth:team:"+id, "id", id, "name", name, "created_at", ts)
	pipe.SAdd(ctx, "auth:teams", id)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, "", err
	}

	apiKey, err := s.CreateApiKey(ctx, id, senderName)
	if err != nil {
		return nil, "", err
	}
	return team, apiKey, nil
}

func (s *Store) CreateApiKey(ctx context.Context, teamID, senderName string) (string, error) {
	key := generateApiKeyStr()
	ts := now()
	data := ApiKey{Key: key, TeamID: teamID, Sender: senderName, CreatedAt: ts}
	b, _ := json.Marshal(data)
	if err := s.rdb.HSet(ctx, "auth:keys", key, string(b)).Err(); err != nil {
		return "", err
	}
	return key, nil
}

func (s *Store) ValidateApiKey(ctx context.Context, key string) (*ApiKey, error) {
	raw, err := s.rdb.HGet(ctx, "auth:keys", key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ak ApiKey
	if err := json.Unmarshal([]byte(raw), &ak); err != nil {
		return nil, err
	}
	return &ak, nil
}

func (s *Store) ListApiKeys(ctx context.Context, teamID string) ([]ApiKey, error) {
	raw, err := s.rdb.HGetAll(ctx, "auth:keys").Result()
	if err != nil {
		return nil, err
	}
	var keys []ApiKey
	for _, v := range raw {
		var ak ApiKey
		if json.Unmarshal([]byte(v), &ak) == nil && ak.TeamID == teamID {
			// Mask the key for security — show prefix + last 4 chars
			if len(ak.Key) > 12 {
				ak.Key = ak.Key[:10] + "..." + ak.Key[len(ak.Key)-4:]
			}
			keys = append(keys, ak)
		}
	}
	return keys, nil
}

// ---- Channels ----

func (s *Store) ensureChannel(ctx context.Context, teamID, name string) error {
	added, err := s.rdb.SAdd(ctx, s.k(teamID, "channels"), name).Result()
	if err != nil {
		return err
	}
	if added > 0 {
		s.rdb.HSetNX(ctx, s.k(teamID, "ch:"+name), "created_at", now())
	}
	return nil
}

func (s *Store) CreateChannel(ctx context.Context, teamID, name string, description *string) (*Channel, bool, error) {
	added, err := s.rdb.SAdd(ctx, s.k(teamID, "channels"), name).Result()
	if err != nil {
		return nil, false, err
	}
	key := s.k(teamID, "ch:"+name)
	s.rdb.HSetNX(ctx, key, "created_at", now())
	if description != nil {
		s.rdb.HSet(ctx, key, "description", *description)
	}
	info, err := s.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, false, err
	}
	ch := &Channel{
		Name:      name,
		CreatedAt: info["created_at"],
	}
	if d, ok := info["description"]; ok && d != "" {
		ch.Description = &d
	}
	return ch, added > 0, nil
}

func (s *Store) ListChannels(ctx context.Context, teamID string) ([]Channel, error) {
	names, err := s.rdb.SMembers(ctx, s.k(teamID, "channels")).Result()
	if err != nil {
		return nil, err
	}
	// Sort
	sorted := make([]string, len(names))
	copy(sorted, names)
	sortStrings(sorted)

	channels := make([]Channel, 0, len(sorted))
	for _, name := range sorted {
		info, err := s.rdb.HGetAll(ctx, s.k(teamID, "ch:"+name)).Result()
		if err != nil {
			return nil, err
		}
		ch := Channel{Name: name, CreatedAt: info["created_at"]}
		if d, ok := info["description"]; ok && d != "" {
			ch.Description = &d
		}
		channels = append(channels, ch)
	}
	return channels, nil
}

func (s *Store) DeleteChannel(ctx context.Context, teamID, name string) (bool, error) {
	removed, err := s.rdb.SRem(ctx, s.k(teamID, "channels"), name).Result()
	if err != nil {
		return false, err
	}
	if removed == 0 {
		return false, nil
	}

	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, s.k(teamID, "ch:"+name))
	pipe.Del(ctx, s.k(teamID, "msg:"+name))
	pipe.Del(ctx, s.k(teamID, "acks:"+name))
	pipe.Del(ctx, s.k(teamID, "status:"+name))
	pipe.Del(ctx, s.k(teamID, "slog:"+name))
	pipe.Del(ctx, s.k(teamID, "thrc:"+name))
	pipe.Exec(ctx)

	// Clean up thread streams
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, s.k(teamID, "thr:"+name+":*"), 100).Result()
		if err != nil {
			break
		}
		if len(keys) > 0 {
			s.rdb.Del(ctx, keys...)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	return true, nil
}

// ---- Messages ----

func (s *Store) MessageExists(ctx context.Context, teamID, channel, msgID string) (bool, error) {
	result, err := s.rdb.XRangeN(ctx, s.k(teamID, "msg:"+channel), msgID, msgID, 1).Result()
	if err != nil {
		return false, err
	}
	return len(result) > 0, nil
}

func (s *Store) PostMessage(ctx context.Context, teamID, channel, sender, content string, mention, threadID *string) (*Message, error) {
	if err := s.ensureChannel(ctx, teamID, channel); err != nil {
		return nil, err
	}
	ts := now()
	fields := map[string]interface{}{
		"sender": sender, "content": content, "created_at": ts,
	}
	if mention != nil {
		fields["mention"] = *mention
	}
	if threadID != nil {
		fields["thread_id"] = *threadID
	}

	id, err := s.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: s.k(teamID, "msg:"+channel),
		Values: fields,
	}).Result()
	if err != nil {
		return nil, err
	}

	if threadID != nil {
		thrKey := s.k(teamID, "thr:"+channel+":"+*threadID)
		s.rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: thrKey,
			ID:     id,
			Values: fields,
		})
		s.rdb.HIncrBy(ctx, s.k(teamID, "thrc:"+channel), *threadID, 1)
	}

	msg := &Message{
		ID: id, Channel: channel, Sender: sender, Content: content,
		Mention: mention, ThreadID: threadID, ReplyCount: 0, CreatedAt: ts,
	}
	return msg, nil
}

// TrimMessages caps the number of messages in a channel stream.
func (s *Store) TrimMessages(ctx context.Context, teamID, channel string, maxLen int64) {
	if maxLen <= 0 {
		return
	}
	s.rdb.XTrimMaxLen(ctx, s.k(teamID, "msg:"+channel), maxLen)
}

func (s *Store) DeleteMessage(ctx context.Context, teamID, channel, msgID string) (bool, error) {
	n, err := s.rdb.XDel(ctx, s.k(teamID, "msg:"+channel), msgID).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) ReadMessages(ctx context.Context, teamID, channel string, afterID *string, limit int, mention, sender, threadID *string) (*ReadResult, error) {
	if limit <= 0 {
		limit = 50
	}

	var streamKey string
	if threadID != nil {
		streamKey = s.k(teamID, "thr:"+channel+":"+*threadID)
	} else {
		streamKey = s.k(teamID, "msg:"+channel)
	}

	var entries []redis.XMessage
	var err error

	if afterID != nil {
		// Use XRANGE with exclusive start (afterID is exclusive in Redis when using "(")
		// But XRANGE doesn't support exclusive, so we use the ID and skip it.
		// Actually, use XRANGE from afterID exclusive by appending \x00
		// Simpler: use XRANGE and filter. But easiest: just use XRANGE with the next possible ID.
		entries, err = s.rdb.XRangeN(ctx, streamKey, "("+*afterID, "+", int64(limit+1)).Result()
		if err != nil {
			return nil, err
		}
	} else {
		entries, err = s.rdb.XRevRangeN(ctx, streamKey, "+", "-", int64(limit+1)).Result()
		if err != nil {
			return nil, err
		}
		// Reverse to chronological order
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}

	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}

	messages := make([]Message, 0, len(entries))
	for _, e := range entries {
		messages = append(messages, parseMessage(channel, e))
	}

	// Enrich with reply counts for top-level reads
	if threadID == nil && len(messages) > 0 {
		pipe := s.rdb.Pipeline()
		cmds := make([]*redis.StringCmd, len(messages))
		for i, m := range messages {
			cmds[i] = pipe.HGet(ctx, s.k(teamID, "thrc:"+channel), m.ID)
		}
		pipe.Exec(ctx)
		for i, cmd := range cmds {
			if v, err := cmd.Result(); err == nil {
				if n, err := strconv.Atoi(v); err == nil {
					messages[i].ReplyCount = n
				}
			}
		}
	}

	// Apply filters
	if mention != nil {
		filtered := messages[:0]
		for _, m := range messages {
			if m.Mention != nil && *m.Mention == *mention {
				filtered = append(filtered, m)
			}
		}
		messages = filtered
	}
	if sender != nil {
		filtered := messages[:0]
		for _, m := range messages {
			if m.Sender == *sender {
				filtered = append(filtered, m)
			}
		}
		messages = filtered
	}

	nextAfterID := "0-0"
	if afterID != nil {
		nextAfterID = *afterID
	}
	if len(messages) > 0 {
		nextAfterID = messages[len(messages)-1].ID
	}

	return &ReadResult{
		Messages: messages, NextAfterID: nextAfterID, HasMore: hasMore, Channel: channel,
	}, nil
}

func (s *Store) ReadThread(ctx context.Context, teamID, channel, parentID string, afterID *string, limit int) (*ThreadResult, error) {
	if limit <= 0 {
		limit = 50
	}

	// Get parent
	raw, err := s.rdb.XRangeN(ctx, s.k(teamID, "msg:"+channel), parentID, parentID, 1).Result()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, ErrNotFound{What: "parent message"}
	}
	parent := parseMessage(channel, raw[0])
	if v, err := s.rdb.HGet(ctx, s.k(teamID, "thrc:"+channel), parentID).Result(); err == nil {
		if n, err := strconv.Atoi(v); err == nil {
			parent.ReplyCount = n
		}
	}

	// Get replies
	thrKey := s.k(teamID, "thr:"+channel+":"+parentID)
	var entries []redis.XMessage

	if afterID != nil {
		entries, err = s.rdb.XRangeN(ctx, thrKey, "("+*afterID, "+", int64(limit+1)).Result()
		if err != nil {
			return nil, err
		}
	} else {
		entries, err = s.rdb.XRangeN(ctx, thrKey, "-", "+", int64(limit+1)).Result()
		if err != nil {
			return nil, err
		}
	}

	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}

	replies := make([]Message, 0, len(entries))
	for _, e := range entries {
		replies = append(replies, parseMessage(channel, e))
	}

	nextAfterID := "0-0"
	if afterID != nil {
		nextAfterID = *afterID
	}
	if len(replies) > 0 {
		nextAfterID = replies[len(replies)-1].ID
	}

	return &ThreadResult{
		Parent: parent, Replies: replies, NextAfterID: nextAfterID, HasMore: hasMore,
	}, nil
}

// ---- Acks ----

func (s *Store) AckMessages(ctx context.Context, teamID, channel, sender, lastReadID string) (*Ack, error) {
	key := s.k(teamID, "acks:"+channel)
	existing, err := s.rdb.HGet(ctx, key, sender).Result()

	effectiveID := lastReadID
	if err == nil && existing != "" {
		var prev Ack
		if json.Unmarshal([]byte(existing), &prev) == nil {
			if prev.LastReadID > lastReadID {
				effectiveID = prev.LastReadID
			}
		}
	}

	ts := now()
	ack := &Ack{Channel: channel, Sender: sender, LastReadID: effectiveID, AckedAt: ts}
	b, _ := json.Marshal(ack)
	s.rdb.HSet(ctx, key, sender, string(b))
	return ack, nil
}

func (s *Store) GetAcks(ctx context.Context, teamID, channel string) ([]Ack, error) {
	raw, err := s.rdb.HGetAll(ctx, s.k(teamID, "acks:"+channel)).Result()
	if err != nil {
		return nil, err
	}
	acks := make([]Ack, 0, len(raw))
	for _, v := range raw {
		var a Ack
		if json.Unmarshal([]byte(v), &a) == nil {
			acks = append(acks, a)
		}
	}
	sortAcks(acks)
	return acks, nil
}

// GetUnread returns messages after the sender's last ack watermark.
func (s *Store) GetUnread(ctx context.Context, teamID, channel, sender string, limit int) (*ReadResult, error) {
	if limit <= 0 {
		limit = 50
	}
	// Get sender's last ack
	ackKey := s.k(teamID, "acks:"+channel)
	afterID := "0-0"
	raw, err := s.rdb.HGet(ctx, ackKey, sender).Result()
	if err == nil && raw != "" {
		var ack Ack
		if json.Unmarshal([]byte(raw), &ack) == nil {
			afterID = ack.LastReadID
		}
	}
	return s.ReadMessages(ctx, teamID, channel, &afterID, limit, nil, nil, nil)
}

func (s *Store) DeleteStatus(ctx context.Context, teamID, channel, key string) (bool, error) {
	n, err := s.rdb.HDel(ctx, s.k(teamID, "status:"+channel), key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---- Status ----

func (s *Store) SetStatus(ctx context.Context, teamID, channel, key, value string, updatedBy *string) (*Status, error) {
	if err := s.ensureChannel(ctx, teamID, channel); err != nil {
		return nil, err
	}
	ts := now()
	status := &Status{Channel: channel, Key: key, Value: value, UpdatedBy: updatedBy, UpdatedAt: ts}
	b, _ := json.Marshal(status)

	changedBy := ""
	if updatedBy != nil {
		changedBy = *updatedBy
	}

	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, s.k(teamID, "status:"+channel), key, string(b))
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: s.k(teamID, "slog:"+channel),
		Values: map[string]interface{}{
			"key": key, "value": value, "changed_by": changedBy, "changed_at": ts,
		},
	})
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	return status, nil
}

func (s *Store) GetStatus(ctx context.Context, teamID string, channel, key *string) ([]Status, error) {
	if channel != nil && key != nil {
		raw, err := s.rdb.HGet(ctx, s.k(teamID, "status:"+*channel), *key).Result()
		if err == redis.Nil {
			return []Status{}, nil
		}
		if err != nil {
			return nil, err
		}
		var st Status
		json.Unmarshal([]byte(raw), &st)
		return []Status{st}, nil
	}

	if channel != nil {
		raw, err := s.rdb.HGetAll(ctx, s.k(teamID, "status:"+*channel)).Result()
		if err != nil {
			return nil, err
		}
		statuses := make([]Status, 0, len(raw))
		for _, v := range raw {
			var st Status
			json.Unmarshal([]byte(v), &st)
			statuses = append(statuses, st)
		}
		sortStatuses(statuses)
		return statuses, nil
	}

	// Cross-channel
	channels, err := s.rdb.SMembers(ctx, s.k(teamID, "channels")).Result()
	if err != nil {
		return nil, err
	}
	var results []Status
	for _, ch := range channels {
		if key != nil {
			raw, err := s.rdb.HGet(ctx, s.k(teamID, "status:"+ch), *key).Result()
			if err == redis.Nil {
				continue
			}
			if err != nil {
				return nil, err
			}
			var st Status
			json.Unmarshal([]byte(raw), &st)
			results = append(results, st)
		} else {
			raw, err := s.rdb.HGetAll(ctx, s.k(teamID, "status:"+ch)).Result()
			if err != nil {
				return nil, err
			}
			for _, v := range raw {
				var st Status
				json.Unmarshal([]byte(v), &st)
				results = append(results, st)
			}
		}
	}
	sortStatuses(results)
	return results, nil
}

func (s *Store) GetStatusChanges(ctx context.Context, teamID, channel string, afterID *string, limit int) (*StatusChangesResult, error) {
	if limit <= 0 {
		limit = 50
	}
	logKey := s.k(teamID, "slog:"+channel)
	var entries []redis.XMessage
	var err error

	if afterID != nil {
		entries, err = s.rdb.XRangeN(ctx, logKey, "("+*afterID, "+", int64(limit+1)).Result()
		if err != nil {
			return nil, err
		}
	} else {
		entries, err = s.rdb.XRevRangeN(ctx, logKey, "+", "-", int64(limit+1)).Result()
		if err != nil {
			return nil, err
		}
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}

	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}

	changes := make([]StatusChange, 0, len(entries))
	for _, e := range entries {
		changes = append(changes, parseStatusChange(channel, e))
	}

	nextAfterID := "0-0"
	if afterID != nil {
		nextAfterID = *afterID
	}
	if len(changes) > 0 {
		nextAfterID = changes[len(changes)-1].ID
	}

	return &StatusChangesResult{
		Changes: changes, NextAfterID: nextAfterID, HasMore: hasMore,
	}, nil
}

// ---- Rate Limiting ----

func (s *Store) CheckRateLimit(ctx context.Context, apiKeyStr string, maxReqs int, windowMs int64) (bool, int, error) {
	nowMs := time.Now().UnixMilli()
	windowKey := fmt.Sprintf("rl:%s:%d", apiKeyStr, nowMs/windowMs)
	count, err := s.rdb.Incr(ctx, windowKey).Result()
	if err != nil {
		return true, maxReqs, err
	}
	if count == 1 {
		s.rdb.PExpire(ctx, windowKey, time.Duration(windowMs)*time.Millisecond)
	}
	remaining := maxReqs - int(count)
	if remaining < 0 {
		remaining = 0
	}
	return int(count) <= maxReqs, remaining, nil
}

// ---- SSE support ----

func (s *Store) BlockingRead(ctx context.Context, teamID, channel, afterID string, timeout time.Duration) ([]Message, error) {
	streamKey := s.k(teamID, "msg:"+channel)
	streams, err := s.rdb.XRead(ctx, &redis.XReadArgs{
		Streams: []string{streamKey, afterID},
		Count:   100,
		Block:   timeout,
	}).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(streams) == 0 {
		return nil, nil
	}

	messages := make([]Message, 0, len(streams[0].Messages))
	for _, e := range streams[0].Messages {
		messages = append(messages, parseMessage(channel, e))
	}

	// Enrich reply counts
	if len(messages) > 0 {
		pipe := s.rdb.Pipeline()
		cmds := make([]*redis.StringCmd, len(messages))
		for i, m := range messages {
			cmds[i] = pipe.HGet(ctx, s.k(teamID, "thrc:"+channel), m.ID)
		}
		pipe.Exec(ctx)
		for i, cmd := range cmds {
			if v, err := cmd.Result(); err == nil {
				if n, err := strconv.Atoi(v); err == nil {
					messages[i].ReplyCount = n
				}
			}
		}
	}

	return messages, nil
}

// ---- Helpers ----

func parseMessage(channel string, e redis.XMessage) Message {
	m := Message{
		ID:      e.ID,
		Channel: channel,
		Sender:  strOr(e.Values, "sender", "unknown"),
		Content: strOr(e.Values, "content", ""),
		CreatedAt: strOr(e.Values, "created_at", ""),
	}
	if v, ok := e.Values["mention"]; ok {
		s := v.(string)
		if s != "" {
			m.Mention = &s
		}
	}
	if v, ok := e.Values["thread_id"]; ok {
		s := v.(string)
		if s != "" {
			m.ThreadID = &s
		}
	}
	return m
}

func parseStatusChange(channel string, e redis.XMessage) StatusChange {
	sc := StatusChange{
		ID:        e.ID,
		Channel:   channel,
		Key:       strOr(e.Values, "key", ""),
		Value:     strOr(e.Values, "value", ""),
		ChangedAt: strOr(e.Values, "changed_at", ""),
	}
	if v, ok := e.Values["changed_by"]; ok {
		s := v.(string)
		if s != "" {
			sc.ChangedBy = &s
		}
	}
	return sc
}

func strOr(m map[string]interface{}, key, fallback string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func sortAcks(a []Ack) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j].Sender < a[j-1].Sender; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func sortStatuses(s []Status) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && statusLess(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func statusLess(a, b Status) bool {
	if a.Channel != b.Channel {
		return a.Channel < b.Channel
	}
	return a.Key < b.Key
}

// Ptr helpers for tests and handlers
func Ptr(s string) *string { return &s }

func init() {
	// Ensure strings package is used
	_ = strings.TrimSpace
}
