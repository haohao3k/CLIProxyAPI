package helps

import (
	"testing"
	"time"
)

func TestDevinRequestLimiterSlidingWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := NewDevinRequestLimiter(3, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if wait := limiter.Admit("a"); wait != 0 {
			t.Fatalf("request %d refused, wait=%v", i+1, wait)
		}
		now = now.Add(10 * time.Second)
	}
	// Requests went out at +0s, +10s and +20s; the clock reads +30s.
	if wait := limiter.Admit("a"); wait != 30*time.Second {
		t.Fatalf("wait = %v, want 30s until the +0s request leaves the window", wait)
	}
	if wait := limiter.Admit("b"); wait != 0 {
		t.Fatalf("another credential was refused, wait=%v", wait)
	}

	now = now.Add(30 * time.Second)
	if wait := limiter.Admit("a"); wait != 0 {
		t.Fatalf("refused after the oldest request left the window, wait=%v", wait)
	}
	// A refusal records nothing, so the window now holds +10s, +20s and +60s.
	if wait := limiter.Admit("a"); wait != 10*time.Second {
		t.Fatalf("wait = %v, want 10s until the +10s request leaves the window", wait)
	}
}
