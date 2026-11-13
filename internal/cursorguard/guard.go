// Package cursorguard caps Cursor on-demand spend per credential: once a credential's included
// quota is used up, at most the configured amount of on-demand spend is allowed per billing period.
//
// Overage is the credential's own usage events this period that are not "included" (Cursor's
// spendLimitUsage is a team pool and does not measure one credential). Between polls every Cursor
// request reserves an estimated cost, so a burst cannot run past the cap until the next poll.
package cursorguard

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Reason codes reported when a credential is blocked.
const (
	ReasonCapReached      = "cap_reached"
	ReasonEstimateReached = "estimate_reached"
	ReasonUsageUnknown    = "usage_unknown"
)

// Usage is one poll of a credential's Cursor usage. Amounts are in cents, as Cursor reports them.
type Usage struct {
	PeriodStart, PeriodEnd time.Time
	IncludedUsedCents      float64
	IncludedLimitCents     float64
	IncludedRemainingCents float64
	// OverageCents sums the credential's non-included usage events in the period.
	OverageCents float64
	// Complete reports that every usage event of the period was read.
	Complete bool
	// LatestEventAt is the newest usage event seen; reservations made before it are settled.
	LatestEventAt time.Time
	// ModelCostCents is a high (p95) per-request cost per upstream model, from the period's events.
	ModelCostCents map[string]float64
	// Account-side limit as Cursor reports it (a team pool on team plans).
	AccountLimitType  string
	AccountLimitCents *float64
	PooledUsedCents   *float64
	UsageBasedAllowed *bool
}

// IncludedExhausted reports that the included quota is used up.
func (u *Usage) IncludedExhausted() bool {
	return u != nil && u.IncludedLimitCents > 0 && u.IncludedRemainingCents <= 0
}

// Fetcher reads a credential's usage from Cursor.
type Fetcher func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (Usage, error)

// Reason explains a block.
type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BlockedError refuses a Cursor request. It is a credential-scoped 429 so the auth manager moves
// on to another credential and combos fall through to their next member.
type BlockedError struct {
	Reason     Reason
	retryAfter time.Duration
}

func (e *BlockedError) Error() string {
	return `{"error":{"message":"` + e.Reason.Message + `","type":"insufficient_quota","code":"cursor_spend_cap_reached"}}`
}

// StatusCode is 429.
func (e *BlockedError) StatusCode() int { return 429 }

// RetryAfter is one poll interval: the credential is checked again then.
func (e *BlockedError) RetryAfter() *time.Duration { d := e.retryAfter; return &d }

// IsCredentialScoped marks the refusal as specific to this credential.
func (e *BlockedError) IsCredentialScoped() bool { return true }

type reservation struct {
	cents float64
	at    time.Time
}

type state struct {
	pollMu       sync.Mutex
	usage        *Usage
	checkedAt    time.Time
	failures     int
	lastErr      string
	reservations map[uint64]reservation
	blocked      bool
	reason       *Reason
}

// Guard tracks every Cursor credential's spend. Use Default in production.
type Guard struct {
	mu      sync.Mutex
	states  map[string]*state
	nextID  uint64
	fetcher Fetcher
	now     func() time.Time
}

// New returns a guard reading usage through fetch.
func New(fetch Fetcher) *Guard {
	return &Guard{states: make(map[string]*state), fetcher: fetch, now: time.Now}
}

var defaultGuard = New(nil)

// Default is the process-wide guard.
func Default() *Guard { return defaultGuard }

// SetFetcher installs the Cursor usage reader.
func (g *Guard) SetFetcher(fetch Fetcher) {
	g.mu.Lock()
	g.fetcher = fetch
	g.mu.Unlock()
}

func (g *Guard) state(authID string) *state {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.states[authID]
	if !ok {
		st = &state{reservations: make(map[uint64]reservation)}
		g.states[authID] = st
	}
	return st
}

// Admit decides whether a Cursor request on auth may start, before it reaches Cursor. On success
// it reserves the request's estimated cost; release(false) drops the reservation when the request
// never reached Cursor, release(true) keeps it until a poll accounts for the request.
func (g *Guard) Admit(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, upstreamModel string) (func(reached bool), error) {
	noop := func(bool) {}
	if cfg == nil || auth == nil || !cfg.CursorSpendGuard.Enabled {
		return noop, nil
	}
	settings := cfg.CursorSpendGuard
	st := g.state(auth.ID)
	g.pollIfStale(ctx, cfg, auth, st, false)

	g.mu.Lock()
	defer g.mu.Unlock()
	reserve := settings.DefaultRequestReserveUSD * 100
	if st.usage != nil {
		if cost, ok := st.usage.ModelCostCents[normalizeModel(upstreamModel)]; ok && cost > 0 {
			reserve = cost
		}
	}
	reason := g.decideLocked(st, settings, reserve)
	g.recordLocked(auth, st, g.decideLocked(st, settings, settings.DefaultRequestReserveUSD*100))
	if reason != nil {
		return nil, &BlockedError{Reason: *reason, retryAfter: settings.PollEvery()}
	}
	g.nextID++
	id := g.nextID
	st.reservations[id] = reservation{cents: reserve, at: g.now()}
	return func(reached bool) {
		if reached {
			return
		}
		g.mu.Lock()
		delete(st.reservations, id)
		g.mu.Unlock()
	}, nil
}

// pollIfStale reads usage when the last read is older than the poll interval, the billing period
// has ended, or force is set. Polls of one credential are serialized.
func (g *Guard) pollIfStale(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, st *state, force bool) {
	settings := cfg.CursorSpendGuard
	stale := func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		now := g.now()
		return force || st.checkedAt.IsZero() || now.Sub(st.checkedAt) >= settings.PollEvery() ||
			(st.usage != nil && !st.usage.PeriodEnd.IsZero() && !now.Before(st.usage.PeriodEnd))
	}
	if !stale() {
		return
	}
	st.pollMu.Lock()
	defer st.pollMu.Unlock()
	if !force && !stale() {
		return
	}
	g.mu.Lock()
	fetch := g.fetcher
	g.mu.Unlock()
	var usage Usage
	err := fmt.Errorf("cursor usage reader unavailable")
	if fetch != nil {
		usage, err = fetch(ctx, cfg, auth)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	st.checkedAt = now
	if err != nil {
		st.failures++
		st.lastErr = err.Error()
		return
	}
	st.failures = 0
	st.lastErr = ""
	st.usage = &usage
	// Requests started before the newest event are in the polled usage; old ones expire.
	for id, r := range st.reservations {
		if !r.at.After(usage.LatestEventAt) || now.Sub(r.at) > 2*settings.PollEvery() {
			delete(st.reservations, id)
		}
	}
}

// decideLocked returns why a request reserving extraCents must be refused, or nil.
func (g *Guard) decideLocked(st *state, settings config.CursorSpendGuardConfig, extraCents float64) *Reason {
	usage := st.usage
	if settings.FailClosedWhenUnknown {
		switch {
		case usage != nil && !usage.Complete:
			return &Reason{Code: ReasonUsageUnknown, Message: "Cursor usage events for the billing period could not all be read"}
		case st.failures > 0 && usage.IncludedExhausted():
			return &Reason{Code: ReasonUsageUnknown, Message: "Cursor usage could not be read and the included quota is used up"}
		case st.failures >= settings.UnknownGracePolls:
			return &Reason{Code: ReasonUsageUnknown, Message: fmt.Sprintf("Cursor usage could not be read for %d polls", st.failures)}
		}
	}
	if usage == nil {
		return nil
	}
	capCents := settings.MaxOverageUSD * 100
	if usage.OverageCents >= capCents && (capCents > 0 || usage.IncludedExhausted()) {
		return &Reason{Code: ReasonCapReached, Message: fmt.Sprintf("Cursor on-demand spend reached the $%.2f cap", settings.MaxOverageUSD)}
	}
	if usage.OverageCents+spill(usage, sumReservations(st)+extraCents) > capCents {
		return &Reason{Code: ReasonEstimateReached, Message: fmt.Sprintf("Cursor on-demand spend plus in-flight requests would pass the $%.2f cap", settings.MaxOverageUSD)}
	}
	return nil
}

// spill is the part of reserved cost the remaining included quota cannot absorb.
func spill(usage *Usage, reservedCents float64) float64 {
	return math.Max(0, reservedCents-math.Max(0, usage.IncludedRemainingCents))
}

func sumReservations(st *state) float64 {
	total := 0.0
	for _, r := range st.reservations {
		total += r.cents
	}
	return total
}

// recordLocked stores the current block state and logs transitions (never secrets).
func (g *Guard) recordLocked(auth *coreauth.Auth, st *state, reason *Reason) {
	blocked := reason != nil
	if blocked != st.blocked || (blocked && st.reason != nil && st.reason.Code != reason.Code) {
		if blocked {
			log.Warnf("cursor spend guard: credential %s blocked (%s): %s", auth.EnsureIndex(), reason.Code, reason.Message)
		} else {
			log.Infof("cursor spend guard: credential %s unblocked", auth.EnsureIndex())
		}
	}
	st.blocked = blocked
	st.reason = reason
}

func normalizeModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}
