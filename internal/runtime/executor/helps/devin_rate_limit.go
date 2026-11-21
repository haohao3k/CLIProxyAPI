package helps

import (
	"sync"
	"time"
)

// DevinRequestsPerMinute caps the requests one Devin credential sends upstream in any
// 60-second window. Devin starts refusing a credential at about 100 requests a minute;
// the margin keeps the credential clear of that limit.
const DevinRequestsPerMinute = 80

// DevinRequestLimiter is a sliding-window request limiter keyed by credential ID.
type DevinRequestLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	sent   map[string][]time.Time
}

// NewDevinRequestLimiter returns a limiter that admits limit requests a minute for each
// credential. A nil now uses the wall clock.
func NewDevinRequestLimiter(limit int, now func() time.Time) *DevinRequestLimiter {
	if now == nil {
		now = time.Now
	}
	return &DevinRequestLimiter{limit: limit, window: time.Minute, now: now, sent: make(map[string][]time.Time)}
}

// Admit records one request for authID and returns 0. When the credential's window is
// full it records nothing and returns how long until its oldest request leaves the window.
func (l *DevinRequestLimiter) Admit(authID string) time.Duration {
	if l == nil || l.limit <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	sent := l.sent[authID]
	expired := 0
	for expired < len(sent) && !sent[expired].After(cutoff) {
		expired++
	}
	sent = sent[expired:]
	if len(sent) >= l.limit {
		l.sent[authID] = sent
		return sent[0].Add(l.window).Sub(now)
	}
	l.sent[authID] = append(sent, now)
	return 0
}
