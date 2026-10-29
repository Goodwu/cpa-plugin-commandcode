package main

import (
	"sync"
	"time"
)

const (
	// rollingWindow matches CommandCode's five-hour plan window.
	rollingWindow = 5 * time.Hour
	// requestHistory is the number of per-request records kept for the status page.
	requestHistory = 50
	// authRequestTimes bounds the per-auth timestamp ring used for local counting.
	authRequestTimes = 200
)

// requestRecord is one completed execution attempt, kept in memory only.
type requestRecord struct {
	At               time.Time `json:"at"`
	AuthID           string    `json:"auth_id"`
	Model            string    `json:"model"`
	Stream           bool      `json:"stream"`
	Status           int       `json:"status"` // 0 = success
	Error            string    `json:"error,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	ReasoningTokens  int       `json:"reasoning_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	DurationMs       int64     `json:"duration_ms"`
}

// rateLimitSnapshot is what the upstream reported on a 429 today.
type rateLimitSnapshot struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"reset_at"`
	Window    string    `json:"window"`
	SeenAt    time.Time `json:"seen_at"`
}

// authStats is the per-credential view used by the status page and the local
// quota fallback.
type authStats struct {
	AuthID        string
	Label         string
	Prefix        string
	Disabled      bool
	LastRequestAt time.Time
	Requests      []time.Time
	RateLimit     *rateLimitSnapshot
}

type stateTotals struct {
	Requests         int
	Failures         int
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	CachedTokens     int
}

type stateStore struct {
	mu        sync.RWMutex
	startedAt time.Time
	records   []requestRecord
	auths     map[string]*authStats
	totals    stateTotals
}

var pluginState = newStateStore()

func newStateStore() *stateStore {
	return &stateStore{
		startedAt: time.Now(),
		auths:     map[string]*authStats{},
	}
}

// noteAuthRecords credential metadata learned during auth.parse.
func (s *stateStore) noteAuth(authID, label, prefix string, disabled bool) {
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.authEntryLocked(authID)
	if label != "" {
		entry.Label = label
	}
	if prefix != "" {
		entry.Prefix = prefix
	}
	entry.Disabled = disabled
}

func (s *stateStore) authEntryLocked(authID string) *authStats {
	if entry, ok := s.auths[authID]; ok {
		return entry
	}
	entry := &authStats{AuthID: authID}
	s.auths[authID] = entry
	return entry
}

// recordRequest stores one finished attempt and updates counters.
func (s *stateStore) recordRequest(rec requestRecord) {
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.records = append(s.records, rec)
	if len(s.records) > requestHistory {
		s.records = s.records[len(s.records)-requestHistory:]
	}

	s.totals.Requests++
	if rec.Status != 0 {
		s.totals.Failures++
	}
	s.totals.PromptTokens += rec.PromptTokens
	s.totals.CompletionTokens += rec.CompletionTokens
	s.totals.ReasoningTokens += rec.ReasoningTokens
	s.totals.CachedTokens += rec.CachedTokens

	if rec.AuthID == "" {
		return
	}
	entry := s.authEntryLocked(rec.AuthID)
	entry.LastRequestAt = rec.At
	entry.Requests = append(entry.Requests, rec.At)
	if len(entry.Requests) > authRequestTimes {
		entry.Requests = entry.Requests[len(entry.Requests)-authRequestTimes:]
	}
}

// recordRateLimit stores the 429 calibration for one credential.
func (s *stateStore) recordRateLimit(authID, window string, limit, remaining int, resetAt time.Time) {
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.authEntryLocked(authID)
	entry.RateLimit = &rateLimitSnapshot{
		Limit:     limit,
		Remaining: remaining,
		ResetAt:   resetAt,
		Window:    window,
		SeenAt:    time.Now(),
	}
}

// requestsSince counts locally observed requests inside the rolling window.
func (entry *authStats) requestsSince(since time.Time) int {
	count := 0
	for i := len(entry.Requests) - 1; i >= 0; i-- {
		if entry.Requests[i].Before(since) {
			break
		}
		count++
	}
	return count
}

// authSnapshot is the read-only projection used by JSON rendering.
type authSnapshot struct {
	AuthID        string             `json:"auth_id"`
	Label         string             `json:"label,omitempty"`
	Prefix        string             `json:"prefix,omitempty"`
	Disabled      bool               `json:"disabled"`
	LastRequestAt string             `json:"last_request_at,omitempty"`
	Requests5h    int                `json:"requests_5h"`
	RateLimit     *rateLimitSnapshot `json:"rate_limit,omitempty"`
}

// stateSnapshot is the status payload served to the management page.
type stateSnapshot struct {
	StartedAt string          `json:"started_at"`
	UptimeSec int64           `json:"uptime_sec"`
	Totals    stateTotals     `json:"totals"`
	Auths     []authSnapshot  `json:"auths"`
	Recent    []requestRecord `json:"recent"`
}

func (s *stateStore) snapshot() stateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	since := now.Add(-rollingWindow)
	out := stateSnapshot{
		StartedAt: s.startedAt.UTC().Format(time.RFC3339),
		UptimeSec: int64(now.Sub(s.startedAt).Seconds()),
		Totals:    s.totals,
		Recent:    make([]requestRecord, 0, len(s.records)),
		Auths:     make([]authSnapshot, 0, len(s.auths)),
	}
	for i := len(s.records) - 1; i >= 0 && len(out.Recent) < 20; i-- {
		out.Recent = append(out.Recent, s.records[i])
	}
	for _, entry := range s.auths {
		snap := authSnapshot{
			AuthID:     entry.AuthID,
			Label:      entry.Label,
			Prefix:     entry.Prefix,
			Disabled:   entry.Disabled,
			Requests5h: entry.requestsSince(since),
			RateLimit:  entry.RateLimit,
		}
		if !entry.LastRequestAt.IsZero() {
			snap.LastRequestAt = entry.LastRequestAt.UTC().Format(time.RFC3339)
		}
		out.Auths = append(out.Auths, snap)
	}
	return out
}

// reset clears counters and history but keeps learned credential metadata.
func (s *stateStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = nil
	s.totals = stateTotals{}
	for _, entry := range s.auths {
		entry.Requests = nil
		entry.LastRequestAt = time.Time{}
	}
}
