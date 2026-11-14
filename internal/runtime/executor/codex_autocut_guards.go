package executor

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// ============================================================================
// 1. Per-Session Violation Streak Tracking (:9gl-med, Iron Rule #4)
// ============================================================================

type codexSessionViolationEntry struct {
	streak int
	lastAt time.Time
}

const codexSessionStreakTTL = 6 * time.Hour

var (
	codexViolationMu       sync.Mutex
	codexSessionViolations = make(map[string]*codexSessionViolationEntry)
)

func recordCodexAutocutViolation(sessionKey string, weight int) int {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return 0
	}
	if weight <= 0 {
		weight = 1
	}
	now := time.Now()
	codexViolationMu.Lock()
	defer codexViolationMu.Unlock()

	entry := codexSessionViolations[sessionKey]
	if entry == nil || now.Sub(entry.lastAt) > codexSessionStreakTTL {
		entry = &codexSessionViolationEntry{}
		codexSessionViolations[sessionKey] = entry
	}
	entry.streak += weight
	if entry.streak > 10 {
		entry.streak = 10
	}
	entry.lastAt = now

	if len(codexSessionViolations) > 512 {
		for k, v := range codexSessionViolations {
			if now.Sub(v.lastAt) > codexSessionStreakTTL {
				delete(codexSessionViolations, k)
			}
		}
	}
	return entry.streak
}

func decayCodexAutocutViolations(sessionKey string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}
	codexViolationMu.Lock()
	defer codexViolationMu.Unlock()
	entry := codexSessionViolations[sessionKey]
	if entry == nil {
		return
	}
	entry.streak--
	if entry.streak <= 0 {
		delete(codexSessionViolations, sessionKey)
	} else {
		entry.lastAt = time.Now()
	}
}

func clearCodexAutocutViolations(sessionKey string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}
	codexViolationMu.Lock()
	delete(codexSessionViolations, sessionKey)
	codexViolationMu.Unlock()
}

func getCodexAutocutStreak(sessionKey string) int {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return 0
	}
	codexViolationMu.Lock()
	defer codexViolationMu.Unlock()
	entry := codexSessionViolations[sessionKey]
	if entry == nil {
		return 0
	}
	if time.Since(entry.lastAt) > codexSessionStreakTTL {
		delete(codexSessionViolations, sessionKey)
		return 0
	}
	return entry.streak
}

// ============================================================================
// 2. Codex Concurrency Semaphore (FIFO + ctx.Done() + 300s Watchdog, Iron Rules #9 & #11)
// ============================================================================

type codexSemaphoreWaiter struct {
	ready    chan struct{}
	canceled bool
}

type codexConcurrencySemaphore struct {
	mu      sync.Mutex
	max     int
	active  int
	waiters []*codexSemaphoreWaiter
}

var globalCodexSemaphore = &codexConcurrencySemaphore{max: 2}

func (s *codexConcurrencySemaphore) setMaxLocked(max int) {
	if max <= 0 {
		max = 2
	}
	s.max = max
	for s.active < s.max && len(s.waiters) > 0 {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		if w.canceled {
			continue
		}
		s.active++
		close(w.ready)
	}
}

func (s *codexConcurrencySemaphore) Acquire(ctx context.Context, maxConcurrent int) (func(reason string), error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var waiter *codexSemaphoreWaiter
	s.mu.Lock()
	s.setMaxLocked(maxConcurrent)
	if s.active < s.max {
		s.active++
		s.mu.Unlock()
	} else {
		waiter = &codexSemaphoreWaiter{ready: make(chan struct{})}
		s.waiters = append(s.waiters, waiter)
		qLen := len(s.waiters)
		active := s.active
		maxVal := s.max
		s.mu.Unlock()
		log.Debugf("[CODEX-QUEUE] queued | active=%d/%d | queueLen=%d", active, maxVal, qLen)

		var doneCh <-chan struct{}
		if ctx != nil {
			doneCh = ctx.Done()
		}
		select {
		case <-waiter.ready:
		case <-doneCh:
			s.mu.Lock()
			alreadyGranted := false
			select {
			case <-waiter.ready:
				alreadyGranted = true
			default:
				waiter.canceled = true
				for i, w := range s.waiters {
					if w == waiter {
						s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
						break
					}
				}
			}
			s.mu.Unlock()
			if alreadyGranted {
				s.releaseOne()
			}
			return nil, ctx.Err()
		}
	}

	var once sync.Once
	var watchdog *time.Timer
	releaseFn := func(reason string) {
		once.Do(func() {
			if watchdog != nil {
				watchdog.Stop()
			}
			s.releaseOne()
			if reason != "" {
				log.Debugf("[CODEX-QUEUE] released (%s)", reason)
			}
		})
	}
	watchdog = time.AfterFunc(300*time.Second, func() {
		log.Warnf("[CODEX-QUEUE] [WATCHDOG:TIMEOUT] forcibly releasing slot after 300s")
		releaseFn("watchdog-timeout")
	})
	return releaseFn, nil
}

func (s *codexConcurrencySemaphore) releaseOne() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active > 0 {
		s.active--
	}
	for s.active < s.max && len(s.waiters) > 0 {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		if w.canceled {
			continue
		}
		s.active++
		close(w.ready)
	}
}

// ============================================================================
// 3. Per-Session Race-Loss Guard (3 losses in 60s -> 30s 429 cooldown)
// ============================================================================

type codexSessionGuardRecord struct {
	lossTimes   []time.Time
	pausedUntil time.Time
}

const (
	codexGuardWindow    = 60 * time.Second
	codexGuardCooldown  = 30 * time.Second
	codexGuardMaxLosses = 3
)

var (
	codexSessionGuardMu  sync.Mutex
	codexSessionGuardMap = make(map[string]*codexSessionGuardRecord)
)

func checkCodexSessionGuard(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	now := time.Now()
	codexSessionGuardMu.Lock()
	rec := codexSessionGuardMap[sessionID]
	var retrySec int
	var riskCount int
	if rec != nil && now.Before(rec.pausedUntil) {
		retrySec = int(math.Ceil(rec.pausedUntil.Sub(now).Seconds()))
		if retrySec < 1 {
			retrySec = 1
		}
		riskCount = len(rec.lossTimes)
	}
	codexSessionGuardMu.Unlock()

	if retrySec > 0 {
		log.Warnf("[SESSION-GUARD] %s | riskCount=%d | retryAfterSec=%d", sessionID, riskCount, retrySec)
		return statusErr{
			code: http.StatusTooManyRequests,
			msg:  fmt.Sprintf("[SESSION-GUARD] Paused session after repeated upstream race losses; retry in %ds.", retrySec),
		}
	}
	return nil
}

func recordCodexSessionRisk(sessionID, how string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	isRaceLoss := how == "terminal" || how == "eof" || strings.HasSuffix(how, "-racelost")
	codexSessionGuardMu.Lock()
	defer codexSessionGuardMu.Unlock()

	now := time.Now()
	rec := codexSessionGuardMap[sessionID]
	if !isRaceLoss {
		// Healthy clean cut clears consecutive loss pressure if not currently paused
		if rec != nil && now.After(rec.pausedUntil) && len(rec.lossTimes) == 0 {
			delete(codexSessionGuardMap, sessionID)
		}
		return
	}
	if rec == nil {
		rec = &codexSessionGuardRecord{}
		codexSessionGuardMap[sessionID] = rec
	}
	kept := rec.lossTimes[:0]
	for _, t := range rec.lossTimes {
		if now.Sub(t) <= codexGuardWindow {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	rec.lossTimes = kept
	if len(rec.lossTimes) >= codexGuardMaxLosses {
		rec.pausedUntil = now.Add(codexGuardCooldown)
		log.Warnf("[SESSION-GUARD] tripped for session=%s (%d race losses in 60s -> 30s cooldown)", sessionID, len(rec.lossTimes))
	}

	if len(codexSessionGuardMap) > 512 {
		for k, v := range codexSessionGuardMap {
			if now.After(v.pausedUntil) && (len(v.lossTimes) == 0 || now.Sub(v.lossTimes[len(v.lossTimes)-1]) > codexGuardWindow*2) {
				delete(codexSessionGuardMap, k)
			}
		}
	}
}

// ============================================================================
// 4. 5h Quota Emergency Circuit Breaker (Iron Rule #9)
// ============================================================================

// codexCircuitBreakerHost is the auth manager: the breaker disables the
// credential there, so the selector, the management UI and the auth file all
// see one state, and the manager re-enables it when the 5h window resets.
type codexCircuitBreakerHost interface {
	GetByID(id string) (*cliproxyauth.Auth, bool)
	TripCircuitBreaker(ctx context.Context, id, reason string, reenableAt time.Time) (bool, error)
}

var (
	codexBreakerHostMu sync.RWMutex
	codexBreakerHost   codexCircuitBreakerHost
)

// SetCodexCircuitBreakerHost wires the auth manager the breaker disables credentials in.
func SetCodexCircuitBreakerHost(host codexCircuitBreakerHost) {
	codexBreakerHostMu.Lock()
	codexBreakerHost = host
	codexBreakerHostMu.Unlock()
}

func currentCodexCircuitBreakerHost() codexCircuitBreakerHost {
	codexBreakerHostMu.RLock()
	defer codexBreakerHostMu.RUnlock()
	return codexBreakerHost
}

// codexBreakerFallbackWindow bounds the disable when upstream sends no reset time.
const codexBreakerFallbackWindow = 5 * time.Hour

// codexCircuitBreakerActive reports whether authID is currently disabled by the breaker.
func codexCircuitBreakerActive(authID string) bool {
	host := currentCodexCircuitBreakerHost()
	if host == nil || authID == "" {
		return false
	}
	current, ok := host.GetByID(authID)
	if !ok || !(current.Disabled || current.Status == cliproxyauth.StatusDisabled) {
		return false
	}
	_, marked := cliproxyauth.CircuitBreakerReenableAt(current)
	return marked
}

// checkCodexCircuitBreakerTripped rejects a request whose credential was
// disabled after it was selected (e.g. while it waited for a codex slot).
func checkCodexCircuitBreakerTripped(auth *cliproxyauth.Auth) error {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return nil
	}
	current := auth
	if host := currentCodexCircuitBreakerHost(); host != nil {
		if latest, ok := host.GetByID(auth.ID); ok {
			current = latest
		}
	}
	if !current.Disabled && current.Status != cliproxyauth.StatusDisabled {
		return nil
	}
	msg := fmt.Sprintf("[CIRCUIT-BREAKER] Codex credential %q is disabled.", auth.ID)
	if at, ok := cliproxyauth.CircuitBreakerReenableAt(current); ok {
		msg = fmt.Sprintf("[CIRCUIT-BREAKER] Codex credential %q is disabled until %s (5h quota below threshold).", auth.ID, at.Format(time.RFC3339))
	}
	return statusErr{code: http.StatusServiceUnavailable, msg: msg}
}

func codexPrimaryResetAt(headers http.Header, now time.Time) time.Time {
	if raw := strings.TrimSpace(headers.Get("X-Codex-Primary-Reset-After-Seconds")); raw != "" {
		if secs, err := strconv.ParseFloat(raw, 64); err == nil && secs >= 0 && !math.IsNaN(secs) && !math.IsInf(secs, 0) {
			return now.Add(time.Duration(secs * float64(time.Second)))
		}
	}
	if raw := strings.TrimSpace(headers.Get("X-Codex-Primary-Reset-At")); raw != "" {
		if unix, err := strconv.ParseInt(raw, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0)
		}
	}
	return now.Add(codexBreakerFallbackWindow)
}

func observeCodexQuotaCircuitBreaker(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, headers http.Header) {
	_, _, _, _ = observeCodexQuotaCircuitBreakerWithMeta(ctx, cfg, auth, headers, "", "")
}

func observeCodexQuotaCircuitBreakerWithMeta(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, headers http.Header, sessionID, model string) (usedPct, remainingPct, quotaDeltaPct float64, hasQuota bool) {
	if auth == nil || headers == nil {
		return 0, 0, 0, false
	}
	rawPct := strings.TrimSpace(headers.Get("X-Codex-Primary-Used-Percent"))
	if rawPct == "" {
		return 0, 0, 0, false
	}
	parsedPct, err := strconv.ParseFloat(rawPct, 64)
	if err != nil || math.IsNaN(parsedPct) {
		return 0, 0, 0, false
	}
	usedPct = parsedPct
	var cxCfg *config.CodexConfig
	if cfg != nil {
		cxCfg = &cfg.Codex
	}
	minRemaining := cxCfg.EffectiveQuotaCircuitBreakerMinRemaining()
	remainingPct = math.Max(0, 100.0-usedPct)
	quotaDeltaPct = observeCodexAccountQuotaTelemetry(auth, usedPct, remainingPct, minRemaining, sessionID, model)
	hasQuota = true
	if minRemaining <= 0 || remainingPct >= float64(minRemaining) {
		return usedPct, remainingPct, quotaDeltaPct, true
	}
	label := strings.TrimSpace(auth.Label)
	if label == "" {
		label = auth.ID
	}
	host := currentCodexCircuitBreakerHost()
	if host == nil {
		log.Warnf("[CIRCUIT-BREAKER] %q below threshold (remaining %.1f%% < %d%%) but no auth manager is wired; not disabling", label, remainingPct, minRemaining)
		return usedPct, remainingPct, quotaDeltaPct, true
	}
	reenableAt := codexPrimaryResetAt(headers, time.Now())
	reason := fmt.Sprintf("[CIRCUIT-BREAKER] Auto-disabled: 5h quota remaining %.1f%% < %d%%, re-enable at %s", remainingPct, minRemaining, reenableAt.Format(time.RFC3339))
	if ctx == nil {
		ctx = context.Background()
	}
	tripCtx, cancelTrip := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	tripped, errTrip := host.TripCircuitBreaker(tripCtx, auth.ID, reason, reenableAt)
	cancelTrip()
	if errTrip != nil {
		log.Warnf("[CIRCUIT-BREAKER] failed to disable %q (%s): %v", label, auth.ID, errTrip)
		return usedPct, remainingPct, quotaDeltaPct, true
	}
	if tripped {
		log.Errorf(
			"🚨 [CIRCUIT-BREAKER] TRIPPED for Codex account %q (%s) — 5h used %.1f%% (remaining %.1f%% < %d%%)! Disabled until %s.",
			label, auth.ID, usedPct, remainingPct, minRemaining, reenableAt.Format(time.RFC3339),
		)
	}
	return usedPct, remainingPct, quotaDeltaPct, true
}
