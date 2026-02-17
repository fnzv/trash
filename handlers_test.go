package main

import (
	"sync"
	"testing"
)

func TestApprovalStore(t *testing.T) {
	s := NewApprovalStore()
	chatID := int64(123)

	if s.Has(chatID) {
		t.Error("New store should be empty")
	}

	turn := &PendingTurn{Commands: []string{"ls"}}
	s.Set(chatID, turn)

	if !s.Has(chatID) {
		t.Error("Store should have chatID after Set")
	}

	got := s.Get(chatID)
	if got != turn {
		t.Errorf("Get returned %v, want %v", got, turn)
	}

	s.Delete(chatID)
	if s.Has(chatID) {
		t.Error("Store should be empty after Delete")
	}
}

func TestSessionManager(t *testing.T) {
	sm := NewSessionManager()
	chatID := int64(123)
	sessionID := "sess-abc"

	if got := sm.Get(chatID); got != "" {
		t.Errorf("New manager should return empty string, got %q", got)
	}

	sm.Set(chatID, sessionID)
	if got := sm.Get(chatID); got != sessionID {
		t.Errorf("Get returned %q, want %q", got, sessionID)
	}

	sm.Delete(chatID)
	if got := sm.Get(chatID); got != "" {
		t.Errorf("Manager should clear session after Delete, got %q", got)
	}
}

func TestChatLocks(t *testing.T) {
	cl := NewChatLocks()
	chatID := int64(456)

	// Test basic locking/unlocking doesn't panic
	unlock1 := cl.Lock(chatID)
	unlock1()

	// Test concurrent access for mutual exclusion
	var wg sync.WaitGroup
	count := 0
	iterations := 100

	wg.Add(iterations)
	for i := 0; i < iterations; i++ {
		go func() {
			defer wg.Done()
			unlock := cl.Lock(chatID)
			defer unlock()
			count++
		}()
	}
	wg.Wait()

	if count != iterations {
		t.Errorf("Expected count %d, got %d", iterations, count)
	}
}
