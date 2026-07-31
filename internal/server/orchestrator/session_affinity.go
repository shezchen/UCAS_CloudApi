package orchestrator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

const (
	defaultSessionAffinityTTL        = 6 * time.Hour
	defaultSessionAffinityMaxEntries = 4096
	maxSessionAffinityPrefixBytes    = 8 * 1024
	// Keep this below TraceAwareStrategy's 1000-point boost so an explicit
	// client-provided trace always wins if both contexts are ever present.
	sessionAffinityScore = 900.0
)

type sessionAffinitySecretProvider interface {
	SecretKey(ctx context.Context) (string, error)
}

type sessionAffinityEntry struct {
	channelID int
	expiresAt time.Time
	lastUsed  time.Time
}

// SessionAffinityTracker keeps a privacy-preserving, bounded mapping from a
// stable conversation prefix to the last semantically successful channel.
// Keys are HMAC digests; raw prompt text is never retained.
type SessionAffinityTracker struct {
	mu sync.Mutex

	secretProvider sessionAffinitySecretProvider
	secret         []byte
	entries        map[string]sessionAffinityEntry
	ttl            time.Duration
	maxEntries     int
	now            func() time.Time
}

func NewSessionAffinityTracker(secretProvider sessionAffinitySecretProvider) *SessionAffinityTracker {
	return &SessionAffinityTracker{
		secretProvider: secretProvider,
		entries:        make(map[string]sessionAffinityEntry),
		ttl:            defaultSessionAffinityTTL,
		maxEntries:     defaultSessionAffinityMaxEntries,
		now:            time.Now,
	}
}

func newSessionAffinityTrackerWithSecret(secret string, ttl time.Duration, maxEntries int) *SessionAffinityTracker {
	tracker := NewSessionAffinityTracker(nil)
	tracker.secret = deriveSessionAffinitySecret(secret)
	tracker.ttl = ttl
	tracker.maxEntries = maxEntries

	return tracker
}

func deriveSessionAffinitySecret(secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("axonhub/session-affinity/v1"))

	return mac.Sum(nil)
}

func (t *SessionAffinityTracker) signingSecret(ctx context.Context) ([]byte, bool) {
	if t == nil {
		return nil, false
	}

	t.mu.Lock()
	if len(t.secret) > 0 {
		secret := t.secret
		t.mu.Unlock()

		return secret, true
	}
	provider := t.secretProvider
	t.mu.Unlock()

	if provider == nil {
		return nil, false
	}

	systemSecret, err := provider.SecretKey(ctx)
	if err != nil || systemSecret == "" {
		if err != nil {
			log.Debug(ctx, "session affinity unavailable: failed to load signing secret", log.Cause(err))
		}

		return nil, false
	}

	derived := deriveSessionAffinitySecret(systemSecret)
	t.mu.Lock()
	if len(t.secret) == 0 {
		t.secret = derived
	}
	secret := t.secret
	t.mu.Unlock()

	return secret, true
}

type sessionAffinityPrefixMessage struct {
	Role      string   `json:"role"`
	Text      []string `json:"text,omitempty"`
	PartTypes []string `json:"part_types,omitempty"`
}

func sessionAffinityPrefix(req *llm.Request) []byte {
	if req == nil {
		return nil
	}

	prefix := make([]sessionAffinityPrefixMessage, 0, 4)
	foundUser := false

	for _, message := range req.Messages {
		role := strings.ToLower(message.Role)
		if role != "system" && role != "developer" && role != "user" {
			continue
		}
		if foundUser {
			break
		}

		item := sessionAffinityPrefixMessage{Role: role}
		if message.Content.Content != nil && *message.Content.Content != "" {
			item.Text = append(item.Text, *message.Content.Content)
		}
		for _, part := range message.Content.MultipleContent {
			if part.Text != nil && *part.Text != "" {
				item.Text = append(item.Text, *part.Text)
			} else if part.Type != "" {
				// Keep only the type for non-text payloads. Signed URLs and binary
				// data are often volatile and must not be retained in the key input.
				item.PartTypes = append(item.PartTypes, part.Type)
			}
		}

		prefix = append(prefix, item)
		if role == "user" {
			foundUser = true
			break
		}
	}

	if len(prefix) == 0 && len(req.Messages) > 0 {
		message := req.Messages[0]
		item := sessionAffinityPrefixMessage{Role: strings.ToLower(message.Role)}
		if message.Content.Content != nil && *message.Content.Content != "" {
			item.Text = append(item.Text, *message.Content.Content)
		}
		prefix = append(prefix, item)
	}

	if len(prefix) == 0 {
		return nil
	}

	data, err := json.Marshal(prefix)
	if err != nil {
		return nil
	}
	if len(data) > maxSessionAffinityPrefixBytes {
		data = data[:maxSessionAffinityPrefixBytes]
	}

	return data
}

func (t *SessionAffinityTracker) RequestKey(ctx context.Context, req *llm.Request, apiKey *ent.APIKey) (string, bool) {
	prefix := sessionAffinityPrefix(req)
	if len(prefix) == 0 {
		return "", false
	}

	secret, ok := t.signingSecret(ctx)
	if !ok {
		return "", false
	}

	projectID, userID := 0, 0
	if apiKey != nil {
		projectID = apiKey.ProjectID
		userID = apiKey.UserID
	}

	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("project:"))
	_, _ = mac.Write([]byte(strconv.Itoa(projectID)))
	_, _ = mac.Write([]byte("\x00user:"))
	_, _ = mac.Write([]byte(strconv.Itoa(userID)))
	_, _ = mac.Write([]byte("\x00prefix:"))
	_, _ = mac.Write(prefix)

	return hex.EncodeToString(mac.Sum(nil)), true
}

func (t *SessionAffinityTracker) Lookup(key string) (int, bool) {
	if t == nil || key == "" {
		return 0, false
	}

	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.entries[key]
	if !ok {
		return 0, false
	}
	if !entry.expiresAt.After(now) {
		delete(t.entries, key)

		return 0, false
	}

	entry.lastUsed = now
	entry.expiresAt = now.Add(t.ttl)
	t.entries[key] = entry

	return entry.channelID, true
}

func (t *SessionAffinityTracker) Bind(key string, channelID int) {
	if t == nil || key == "" || channelID <= 0 || t.maxEntries <= 0 {
		return
	}

	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, exists := t.entries[key]; !exists && len(t.entries) >= t.maxEntries {
		t.evictOneLocked(now)
	}
	t.entries[key] = sessionAffinityEntry{
		channelID: channelID,
		expiresAt: now.Add(t.ttl),
		lastUsed:  now,
	}
}

func (t *SessionAffinityTracker) evictOneLocked(now time.Time) {
	var oldestKey string
	var oldestTime time.Time

	for key, entry := range t.entries {
		if !entry.expiresAt.After(now) {
			delete(t.entries, key)
			return
		}
		if oldestKey == "" || entry.lastUsed.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.lastUsed
		}
	}

	if oldestKey != "" {
		delete(t.entries, oldestKey)
	}
}

type sessionAffinityContextKey struct{}

func contextWithSessionAffinityChannel(ctx context.Context, channelID int) context.Context {
	return context.WithValue(ctx, sessionAffinityContextKey{}, channelID)
}

func sessionAffinityChannelFromContext(ctx context.Context) int {
	channelID, _ := ctx.Value(sessionAffinityContextKey{}).(int)

	return channelID
}

// SessionAffinityStrategy makes a previously successful fallback channel win
// within its normal candidate/health pool. Explicit trace affinity is resolved
// separately and takes precedence because fallback keys are not created for
// requests carrying a trace.
type SessionAffinityStrategy struct{}

func NewSessionAffinityStrategy() *SessionAffinityStrategy {
	return &SessionAffinityStrategy{}
}

func (s *SessionAffinityStrategy) Score(ctx context.Context, channel *biz.Channel) float64 {
	if channel != nil && channel.ID == sessionAffinityChannelFromContext(ctx) {
		return sessionAffinityScore
	}

	return 0
}

func (s *SessionAffinityStrategy) ScoreWithDebug(ctx context.Context, channel *biz.Channel) (float64, StrategyScore) {
	preferredID := sessionAffinityChannelFromContext(ctx)
	score := s.Score(ctx, channel)

	return score, StrategyScore{
		StrategyName: s.Name(),
		Score:        score,
		Details: map[string]any{
			"preferred_channel_id": preferredID,
			"matched":              channel != nil && channel.ID == preferredID,
		},
	}
}

func (s *SessionAffinityStrategy) Name() string {
	return "SessionAffinity"
}

func withSessionAffinity(outbound *PersistentOutboundTransformer, tracker *SessionAffinityTracker) pipeline.Middleware {
	return &sessionAffinityRecording{
		outbound: outbound,
		tracker:  tracker,
	}
}

type sessionAffinityRecording struct {
	pipeline.DummyMiddleware

	outbound *PersistentOutboundTransformer
	tracker  *SessionAffinityTracker
}

func (m *sessionAffinityRecording) Name() string {
	return "session-affinity"
}

func (m *sessionAffinityRecording) OnOutboundLlmResponse(ctx context.Context, response *llm.Response) (*llm.Response, error) {
	terminal, successful := llmTerminalOutcome(response)
	if pipeline.HasResponseContent(response) && (!terminal || successful) {
		m.bindCurrent()
	}

	return response, nil
}

func (m *sessionAffinityRecording) OnOutboundLlmStream(ctx context.Context, stream streams.Stream[*llm.Response]) (streams.Stream[*llm.Response], error) {
	return &sessionAffinityBindingStream{
		stream: stream,
		state:  m.outbound.state,
		bind:   m.bindCurrent,
	}, nil
}

func (m *sessionAffinityRecording) bindCurrent() {
	if m == nil || m.tracker == nil || m.outbound == nil || m.outbound.state == nil {
		return
	}

	state := m.outbound.state
	channel := m.outbound.GetCurrentChannel()
	if state.SessionAffinityKey == "" || channel == nil {
		return
	}

	m.tracker.Bind(state.SessionAffinityKey, channel.ID)
}

type sessionAffinityBindingStream struct {
	stream streams.Stream[*llm.Response]
	state  *PersistenceState
	bind   func()

	semanticOutput  bool
	bound           bool
	terminalFailure bool
}

func (s *sessionAffinityBindingStream) Next() bool {
	return s.stream.Next()
}

func (s *sessionAffinityBindingStream) Current() *llm.Response {
	event := s.stream.Current()
	if pipeline.HasResponseContent(event) {
		s.semanticOutput = true
	}
	if terminal, successful := llmTerminalOutcome(event); terminal && !successful {
		s.terminalFailure = true
	} else if !s.bound && successful && s.semanticOutput && s.state != nil && s.state.AttemptAccepted {
		s.bindOnce()
	}

	return event
}

func (s *sessionAffinityBindingStream) Err() error {
	return s.stream.Err()
}

func (s *sessionAffinityBindingStream) Close() error {
	if !s.bound && !s.terminalFailure && s.semanticOutput && s.state != nil && s.state.AttemptAccepted && s.state.OutboundStreamCompleted {
		s.bindOnce()
	}

	return s.stream.Close()
}

func (s *sessionAffinityBindingStream) bindOnce() {
	if s.bound || s.bind == nil {
		return
	}
	s.bind()
	s.bound = true
}
