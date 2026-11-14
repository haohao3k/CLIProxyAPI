package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// MetadataKeyCircuitBreakerReenableAt marks an auth that the quota circuit
// breaker disabled, with the RFC3339 time at which it may be re-enabled.
// A manual enable/disable through the management API removes it.
const MetadataKeyCircuitBreakerReenableAt = "circuit_breaker_reenable_at"

const circuitBreakerSweepInterval = 30 * time.Second

var errCircuitBreakerNoop = errors.New("circuit breaker: no change")

// TripCircuitBreaker disables auth id and records when it may be re-enabled.
// An auth that is already disabled (manually or by an earlier trip) is left
// untouched, so a manual disable is never turned into an automatic one.
func (m *Manager) TripCircuitBreaker(ctx context.Context, id, reason string, reenableAt time.Time) (bool, error) {
	if m == nil {
		return false, nil
	}
	if current, ok := m.GetByID(id); !ok || current.Disabled || current.Status == StatusDisabled {
		return false, nil
	}
	_, committed, err := m.Modify(ctx, id, 0, func(a *Auth) error {
		if a.Disabled || a.Status == StatusDisabled {
			return errCircuitBreakerNoop
		}
		a.Disabled = true
		a.Status = StatusDisabled
		a.StatusMessage = reason
		a.UpdatedAt = time.Now()
		if a.Metadata == nil {
			a.Metadata = make(map[string]any)
		}
		a.Metadata["disabled"] = true
		a.Metadata[MetadataKeyCircuitBreakerReenableAt] = reenableAt.UTC().Format(time.RFC3339)
		return nil
	})
	if errors.Is(err, errCircuitBreakerNoop) {
		return false, nil
	}
	return committed, err
}

// CircuitBreakerReenableAt reports the re-enable time of an auth the circuit
// breaker disabled.
func CircuitBreakerReenableAt(a *Auth) (time.Time, bool) {
	if a == nil || a.Metadata == nil {
		return time.Time{}, false
	}
	raw, ok := a.Metadata[MetadataKeyCircuitBreakerReenableAt].(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ClearCircuitBreakerMark drops the breaker marker; callers use it when a
// person sets the disabled state by hand.
func ClearCircuitBreakerMark(a *Auth) {
	if a != nil && a.Metadata != nil {
		delete(a.Metadata, MetadataKeyCircuitBreakerReenableAt)
	}
}

// ReenableExpiredCircuitBreakers re-enables auths whose breaker window has
// passed, and drops stale markers from auths someone already re-enabled.
func (m *Manager) ReenableExpiredCircuitBreakers(ctx context.Context, now time.Time) int {
	if m == nil {
		return 0
	}
	reenabled := 0
	for _, a := range m.List() {
		at, marked := CircuitBreakerReenableAt(a)
		if !marked {
			if _, present := a.Metadata[MetadataKeyCircuitBreakerReenableAt]; !present {
				continue
			}
		}
		disabled := a.Disabled || a.Status == StatusDisabled
		if disabled && marked && now.Before(at) {
			continue
		}
		_, committed, err := m.Modify(ctx, a.ID, 0, func(cur *Auth) error {
			if _, present := cur.Metadata[MetadataKeyCircuitBreakerReenableAt]; !present {
				return errCircuitBreakerNoop
			}
			curAt, curMarked := CircuitBreakerReenableAt(cur)
			curDisabled := cur.Disabled || cur.Status == StatusDisabled
			if curDisabled && curMarked && now.Before(curAt) {
				return errCircuitBreakerNoop
			}
			ClearCircuitBreakerMark(cur)
			if curDisabled {
				cur.Disabled = false
				cur.Status = StatusActive
				cur.StatusMessage = ""
				cur.Metadata["disabled"] = false
			}
			cur.UpdatedAt = now
			return nil
		})
		if err != nil && !errors.Is(err, errCircuitBreakerNoop) {
			log.Warnf("[CIRCUIT-BREAKER] failed to re-enable %s: %v", a.ID, err)
			continue
		}
		if committed && disabled {
			reenabled++
			log.Infof("[CIRCUIT-BREAKER] re-enabled %s (%s): quota window reset at %s", a.Label, a.ID, at.Format(time.RFC3339))
		}
	}
	return reenabled
}

func (m *Manager) runCircuitBreakerSweep(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	m.ReenableExpiredCircuitBreakers(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.ReenableExpiredCircuitBreakers(ctx, now)
		}
	}
}
