package main

import (
	"context"
	"sync"
	"time"
)

// CommandResult stores the outcome of one approved/denied command.
type CommandResult struct {
	Command  string
	Approved bool
	Output   string
}

// PendingTurn holds all pending commands for a single AI response.
type PendingTurn struct {
	Commands   []string
	CurrentIdx int
	Results    []CommandResult
	SessionID  string
	Provider   string // "claude" or "gemini"
}

// ApprovalStore is a thread-safe map of chatID → pending turn.
type ApprovalStore struct {
	mu      sync.RWMutex
	pending map[int64]*PendingTurn
}

func NewApprovalStore() *ApprovalStore {
	return &ApprovalStore{pending: make(map[int64]*PendingTurn)}
}

func (s *ApprovalStore) Get(chatID int64) *PendingTurn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pending[chatID]
}

func (s *ApprovalStore) Set(chatID int64, turn *PendingTurn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[chatID] = turn
}

func (s *ApprovalStore) Delete(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, chatID)
}

func (s *ApprovalStore) Has(chatID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.pending[chatID]
	return ok
}

// PendingLogin holds state for an in-progress login.
// For Claude this is an OAuth PTY flow; for Gemini it's an API key prompt.
type PendingLogin struct {
	FeedCode        func(code string) error
	Cancel          context.CancelFunc
	OriginalMessage string
	Provider        string // "claude" or "gemini"
}

// LoginStore is a thread-safe map of chatID → pending login.
type LoginStore struct {
	mu      sync.RWMutex
	pending map[int64]*PendingLogin
}

func NewLoginStore() *LoginStore {
	return &LoginStore{pending: make(map[int64]*PendingLogin)}
}

func (s *LoginStore) Get(chatID int64) *PendingLogin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pending[chatID]
}

func (s *LoginStore) Set(chatID int64, login *PendingLogin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[chatID] = login
}

func (s *LoginStore) Delete(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, chatID)
}

func (s *LoginStore) Has(chatID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.pending[chatID]
	return ok
}

// ChatUsage accumulates usage stats for a single chat session.
type ChatUsage struct {
	TotalCostUSD  float64
	InputTokens   int64
	OutputTokens  int64
	CacheRead     int64
	CacheCreate   int64
	NumCalls      int
	TotalDuration time.Duration
	LastCallTime  time.Time
}

// UsageTracker is a thread-safe map of chatID → accumulated usage.
type UsageTracker struct {
	mu    sync.RWMutex
	stats map[int64]*ChatUsage
}

func NewUsageTracker() *UsageTracker {
	return &UsageTracker{stats: make(map[int64]*ChatUsage)}
}

// Record adds a Claude response's usage data to the running totals.
func (t *UsageTracker) Record(chatID int64, resp *ClaudeResponse) {
	if resp == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.stats[chatID]
	if s == nil {
		s = &ChatUsage{}
		t.stats[chatID] = s
	}
	s.TotalCostUSD += resp.CostUSD
	s.InputTokens += resp.Usage.InputTokens
	s.OutputTokens += resp.Usage.OutputTokens
	s.CacheRead += resp.Usage.CacheReadInputTokens
	s.CacheCreate += resp.Usage.CacheCreationInputTokens
	s.NumCalls++
	s.TotalDuration += time.Duration(resp.DurationMs) * time.Millisecond
	s.LastCallTime = time.Now()
}

// Get returns the accumulated usage for a chat, or nil if none.
func (t *UsageTracker) Get(chatID int64) *ChatUsage {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.stats[chatID]
}

// Reset clears usage stats for a chat.
func (t *UsageTracker) Reset(chatID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.stats, chatID)
}
