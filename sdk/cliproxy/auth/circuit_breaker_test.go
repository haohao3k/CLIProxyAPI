package auth

import (
	"context"
	"testing"
	"time"
)

func registerBreakerTestAuth(t *testing.T, m *Manager, id string, disabled bool) {
	t.Helper()
	a := &Auth{ID: id, Provider: "codex", Label: id, Disabled: disabled, Metadata: map[string]any{"email": id}}
	if disabled {
		a.Status = StatusDisabled
	}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func TestTripCircuitBreakerDisablesAndReenablesAtReset(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	registerBreakerTestAuth(t, m, "acct-a", false)
	resetAt := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	tripped, err := m.TripCircuitBreaker(ctx, "acct-a", "quota low", resetAt)
	if err != nil || !tripped {
		t.Fatalf("trip = %v, %v; want true, nil", tripped, err)
	}
	got, _ := m.GetByID("acct-a")
	if !got.Disabled || got.Status != StatusDisabled || got.Metadata["disabled"] != true {
		t.Fatalf("auth not disabled after trip: disabled=%v status=%v meta=%v", got.Disabled, got.Status, got.Metadata["disabled"])
	}
	if at, ok := CircuitBreakerReenableAt(got); !ok || !at.Equal(resetAt) {
		t.Fatalf("reenable-at = %v, %v; want %v", at, ok, resetAt)
	}

	if again, _ := m.TripCircuitBreaker(ctx, "acct-a", "quota low", resetAt.Add(time.Hour)); again {
		t.Fatal("second trip on a disabled auth must be a no-op")
	}

	if n := m.ReenableExpiredCircuitBreakers(ctx, resetAt.Add(-time.Minute)); n != 0 {
		t.Fatalf("re-enabled %d before reset", n)
	}
	if n := m.ReenableExpiredCircuitBreakers(ctx, resetAt.Add(time.Second)); n != 1 {
		t.Fatalf("re-enabled %d after reset; want 1", n)
	}
	got, _ = m.GetByID("acct-a")
	if got.Disabled || got.Status == StatusDisabled || got.Metadata["disabled"] != false {
		t.Fatalf("auth still disabled after reset: disabled=%v status=%v", got.Disabled, got.Status)
	}
	if _, ok := got.Metadata[MetadataKeyCircuitBreakerReenableAt]; ok {
		t.Fatal("marker must be cleared after re-enable")
	}
}

func TestTripCircuitBreakerLeavesManualDisableAlone(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	registerBreakerTestAuth(t, m, "acct-manual", true)

	if tripped, _ := m.TripCircuitBreaker(ctx, "acct-manual", "quota low", time.Now()); tripped {
		t.Fatal("trip must not take over a manually disabled auth")
	}
	m.ReenableExpiredCircuitBreakers(ctx, time.Now().Add(24*time.Hour))
	if got, _ := m.GetByID("acct-manual"); !got.Disabled {
		t.Fatal("sweep re-enabled a manually disabled auth")
	}
}

func TestReenableSweepDropsMarkerFromManuallyEnabledAuth(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	registerBreakerTestAuth(t, m, "acct-b", false)
	resetAt := time.Now().Add(3 * time.Hour)
	if _, err := m.TripCircuitBreaker(ctx, "acct-b", "quota low", resetAt); err != nil {
		t.Fatal(err)
	}
	// Person re-enables in the UI before the reset; the management handler clears the marker,
	// but an enable that left it behind must not later be fought by the sweep.
	if _, _, err := m.Modify(ctx, "acct-b", 0, func(a *Auth) error {
		a.Disabled = false
		a.Status = StatusActive
		a.Metadata["disabled"] = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := m.ReenableExpiredCircuitBreakers(ctx, time.Now()); n != 0 {
		t.Fatalf("counted %d re-enables for an already enabled auth", n)
	}
	got, _ := m.GetByID("acct-b")
	if got.Disabled {
		t.Fatal("auth must stay enabled")
	}
	if _, ok := got.Metadata[MetadataKeyCircuitBreakerReenableAt]; ok {
		t.Fatal("stale marker must be dropped")
	}
}
