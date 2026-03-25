package auth

import (
	"testing"
	"time"
)

func TestManagerPrunesExpiredSessionsAndEnforcesLimit(t *testing.T) {
	manager := NewManager(2, 15*time.Minute)
	now := time.Date(2026, 3, 25, 1, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }

	first, err := manager.Create(1, "admin", "admin")
	if err != nil {
		t.Fatalf("create first session: %v", err)
	}
	if _, err := manager.Create(2, "user", "user"); err != nil {
		t.Fatalf("create second session: %v", err)
	}
	if _, err := manager.Create(3, "guest", "guest"); err != ErrSessionLimit {
		t.Fatalf("expected session limit error, got %v", err)
	}

	now = now.Add(16 * time.Minute)
	if _, ok := manager.Get(first.Token); ok {
		t.Fatal("expected expired session to be pruned")
	}
	if manager.Count() != 0 {
		t.Fatalf("expected all sessions pruned, got %d", manager.Count())
	}

	if _, err := manager.Create(4, "new-admin", "admin"); err != nil {
		t.Fatalf("create session after prune: %v", err)
	}
}
