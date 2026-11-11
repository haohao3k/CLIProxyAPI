package executor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	codexAutocutTelemetryFileName = "codex-autocut.jsonl"
	codexAutocutTelemetryMaxBytes = 20 * 1024 * 1024 // 20 MB active + 20 MB .1 backup
	codexAutocutRingCapacity      = 1000
	codexQuotaDeltaRingCapacity   = 200
)

// CodexAutocutTurnRecord is the structured forensic record for one Codex turn,
// persisted to <log-dir>/codex-autocut.jsonl and indexed in memory for instant diagnostics.
type CodexAutocutTurnRecord struct {
	Timestamp            string  `json:"ts"`
	UnixMs               int64   `json:"unix_ms"`
	AuthID               string  `json:"auth_id,omitempty"`
	AuthLabel            string  `json:"auth_label,omitempty"`
	AuthEmail            string  `json:"auth_email,omitempty"`
	Model                string  `json:"model"`
	SessionID            string  `json:"session_id"`
	How                  string  `json:"how"`
	AbortReason          string  `json:"abort_reason,omitempty"`
	FreeCut              bool    `json:"free_cut"`
	LeakRisk             bool    `json:"leak_risk"`
	RaceLost             bool    `json:"race_lost"`
	EstInputTokens       int     `json:"est_input_tokens"`
	DeliveredChars       int     `json:"delivered_chars"`
	MaxToolArgsLen       int     `json:"max_tool_args_len,omitempty"`
	FinalizeLen          int     `json:"finalize_len,omitempty"`
	ToolCallCount        int     `json:"tool_call_count,omitempty"`
	LastToolName         string  `json:"last_tool_name,omitempty"`
	DurationMs           int64   `json:"duration_ms"`
	SessionStreak        int     `json:"session_streak"`
	HasQuota             bool    `json:"has_quota,omitempty"`
	UsedPct              float64 `json:"used_pct,omitempty"`
	RemainingPct         float64 `json:"remaining_pct,omitempty"`
	QuotaDeltaPct        float64 `json:"quota_delta_pct,omitempty"`
	UpstreamInputTokens  int     `json:"upstream_input_tokens,omitempty"`
	UpstreamOutputTokens int     `json:"upstream_output_tokens,omitempty"`
	UpstreamCachedTokens int     `json:"upstream_cached_tokens,omitempty"`
}

// CodexQuotaDeltaEvent records an observed tick in an account's 5h quota header (`X-Codex-Primary-Used-Percent`).
// Strictly observational (Iron Rule #4: NEVER mass-arms session violations).
type CodexQuotaDeltaEvent struct {
	Timestamp    string  `json:"ts"`
	UnixMs       int64   `json:"unix_ms"`
	AuthID       string  `json:"auth_id"`
	AuthLabel    string  `json:"auth_label,omitempty"`
	AuthEmail    string  `json:"auth_email,omitempty"`
	PrevUsedPct  float64 `json:"prev_used_pct"`
	NewUsedPct   float64 `json:"new_used_pct"`
	DeltaPct     float64 `json:"delta_pct"`
	RemainingPct float64 `json:"remaining_pct"`
	SessionID    string  `json:"session_id,omitempty"`
	Model        string  `json:"model,omitempty"`
	LastTurnHow  string  `json:"last_turn_how,omitempty"`
}

// CodexAccountTelemetrySummary summarizes per-account quota & autocut health.
type CodexAccountTelemetrySummary struct {
	AuthID                string  `json:"auth_id"`
	AuthLabel             string  `json:"auth_label,omitempty"`
	AuthEmail             string  `json:"auth_email,omitempty"`
	HasQuota              bool    `json:"has_quota"`
	LastUsedPct           float64 `json:"last_used_pct"`
	LastRemainingPct      float64 `json:"last_remaining_pct"`
	TotalPositiveDeltaPct float64 `json:"total_positive_delta_pct"`
	Turns                 int64   `json:"turns"`
	FreeCuts              int64   `json:"free_cuts"`
	LeakRiskTurns         int64   `json:"leak_risk_turns"`
	RaceLostTurns         int64   `json:"race_lost_turns"`
	CircuitBreakerTripped bool    `json:"circuit_breaker_tripped"`
	LastHow               string  `json:"last_how,omitempty"`
	LastSessionID         string  `json:"last_session_id,omitempty"`
	LastSeenAt            string  `json:"last_seen_at,omitempty"`
}

// CodexAutocutTelemetrySummary provides aggregate counters across all observed turns.
type CodexAutocutTelemetrySummary struct {
	TotalTurns       int64            `json:"total_turns"`
	FreeCuts         int64            `json:"free_cuts"`
	LeakRiskTurns    int64            `json:"leak_risk_turns"`
	RaceLostTurns    int64            `json:"race_lost_turns"`
	FreeCutRatePct   float64          `json:"free_cut_rate_pct"`
	HowCounts        map[string]int64 `json:"how_counts"`
	SpecialCounts    map[string]int64 `json:"special_counts"`
	ActiveSemInUse   int              `json:"active_semaphore_in_use"`
	LogFilePath      string           `json:"log_file_path,omitempty"`
	LogFileSizeBytes int64            `json:"log_file_size_bytes,omitempty"`
}

// CodexAutocutTelemetrySnapshot is the JSON payload returned by GET /v0/management/codex-metering-bypass/telemetry.
type CodexAutocutTelemetrySnapshot struct {
	GeneratedAt string                         `json:"generated_at"`
	Summary     CodexAutocutTelemetrySummary   `json:"summary"`
	Accounts    []CodexAccountTelemetrySummary `json:"accounts"`
	QuotaDeltas []CodexQuotaDeltaEvent         `json:"quota_deltas"`
	RecentTurns []CodexAutocutTurnRecord       `json:"recent_turns"`
}

// CodexTelemetryQueryOptions filters SnapshotCodexAutocutTelemetry.
type CodexTelemetryQueryOptions struct {
	Limit     int
	LeaksOnly bool
	SessionID string
	AuthQuery string
}

type codexAutocutTelemetryStore struct {
	mu            sync.Mutex
	hydratedDir   string
	turns         []CodexAutocutTurnRecord
	quotaDeltas   []CodexQuotaDeltaEvent
	accounts      map[string]*CodexAccountTelemetrySummary
	howCounts     map[string]int64
	specialCounts map[string]int64
	totalTurns    int64
	freeCuts      int64
	leakRiskTurns int64
	raceLostTurns int64
}

var globalCodexTelemetryStore = &codexAutocutTelemetryStore{
	turns:         make([]CodexAutocutTurnRecord, 0, codexAutocutRingCapacity),
	quotaDeltas:   make([]CodexQuotaDeltaEvent, 0, codexQuotaDeltaRingCapacity),
	accounts:      make(map[string]*CodexAccountTelemetrySummary),
	howCounts:     make(map[string]int64),
	specialCounts: make(map[string]int64),
}

func resolveCodexTelemetryLogPath(cfg *config.Config, explicitLogDir string) string {
	dir := strings.TrimSpace(explicitLogDir)
	if dir == "" {
		dir = strings.TrimSpace(logging.ResolveLogDirectory(cfg))
	}
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, codexAutocutTelemetryFileName)
}

func extractAuthIdentity(auth *cliproxyauth.Auth) (authID, authLabel, authEmail string) {
	if auth == nil {
		return "", "", ""
	}
	authID = strings.TrimSpace(auth.ID)
	authLabel = strings.TrimSpace(auth.Label)
	_, accVal := auth.AccountInfo()
	authEmail = strings.TrimSpace(accVal)
	if authEmail == "" && auth.Metadata != nil {
		if em, ok := auth.Metadata["email"].(string); ok {
			authEmail = strings.TrimSpace(em)
		}
	}
	if authLabel == "" {
		if authEmail != "" {
			authLabel = authEmail
		} else {
			authLabel = authID
		}
	}
	return authID, authLabel, authEmail
}

// RecordCodexSpecialTelemetryEvent increments diagnostic counters for non-stream-cut events
// such as "titlegen_local", "autocompact_cache", "autocompact_external", "autocompact_window",
// "hard_cap", and "bypass_skipped".
func RecordCodexSpecialTelemetryEvent(kind string) {
	if strings.TrimSpace(kind) == "" {
		return
	}
	globalCodexTelemetryStore.mu.Lock()
	globalCodexTelemetryStore.specialCounts[kind]++
	globalCodexTelemetryStore.mu.Unlock()
}

func classifyCodexTurnOutcome(tel CodexAutocutTelemetry) (freeCut bool, leakRisk bool) {
	how := tel.How
	if how == "" {
		how = "eof"
	}
	// Note: On healthy toolpad/finalize cuts, StreamWithAutocut fires SetLinger(0) TCP RST
	// at the VERY START of "__pad9f3" / "const pad =", so MaxToolArgsLen / FinalizeLen is
	// simply the functional payload length before the pad! Actual leak-risk turns are:
	//   1) RaceLost (response.completed already arrived in rawBuffer — tiny/empty pad)
	//   2) terminal / eof* (stream finished without hitting any pad or finalize cut)
	//   3) *-unpadded (tool call or __finalize_9f3 closed without writing a pad)
	//   4) argcap (exceeded 8,000-char ceiling without pad, forced hard-cap close)
	// Any 1-RTT WAN post-RST leak is additionally caught by [CODEX-QUOTA-DELTA] on the next turn.
	isUnpadded := strings.Contains(how, "unpadded")
	isTerminalOrEOF := how == "terminal" || strings.HasPrefix(how, "eof")
	isError := how == "upstream-error"

	leakRisk = tel.RaceLost || isTerminalOrEOF || how == "argcap" || isUnpadded
	freeCut = !tel.RaceLost && !isTerminalOrEOF && !isError
	return freeCut, leakRisk
}

func (s *codexAutocutTelemetryStore) ensureAccountLocked(authID, label, email string) *CodexAccountTelemetrySummary {
	key := strings.TrimSpace(authID)
	if key == "" {
		key = "unknown"
	}
	acc, ok := s.accounts[key]
	if !ok {
		acc = &CodexAccountTelemetrySummary{
			AuthID: key,
		}
		s.accounts[key] = acc
	}
	if label != "" {
		acc.AuthLabel = label
	}
	if email != "" {
		acc.AuthEmail = email
	}
	return acc
}

func (s *codexAutocutTelemetryStore) ingestTurnLocked(rec CodexAutocutTurnRecord) {
	s.totalTurns++
	if rec.FreeCut {
		s.freeCuts++
	}
	if rec.LeakRisk {
		s.leakRiskTurns++
	}
	if rec.RaceLost {
		s.raceLostTurns++
	}
	how := rec.How
	if how == "" {
		how = "eof"
	}
	s.howCounts[how]++

	if len(s.turns) >= codexAutocutRingCapacity {
		copy(s.turns, s.turns[1:])
		s.turns[len(s.turns)-1] = rec
	} else {
		s.turns = append(s.turns, rec)
	}

	if rec.AuthID != "" || rec.AuthLabel != "" {
		acc := s.ensureAccountLocked(rec.AuthID, rec.AuthLabel, rec.AuthEmail)
		acc.Turns++
		if rec.FreeCut {
			acc.FreeCuts++
		}
		if rec.LeakRisk {
			acc.LeakRiskTurns++
		}
		if rec.RaceLost {
			acc.RaceLostTurns++
		}
		if rec.HasQuota {
			acc.HasQuota = true
			acc.LastUsedPct = rec.UsedPct
			acc.LastRemainingPct = rec.RemainingPct
		}
		acc.LastHow = how
		acc.LastSessionID = rec.SessionID
		acc.LastSeenAt = rec.Timestamp
		_, tripped := codexTrippedBreakers.Load(acc.AuthID)
		acc.CircuitBreakerTripped = tripped
	}
}

// observeCodexAccountQuotaTelemetry tracks per-account 5h quota changes strictly for
// forensic logging & telemetry (Iron Rule #4: NEVER touches session violation streaks!).
func observeCodexAccountQuotaTelemetry(auth *cliproxyauth.Auth, usedPct, remainingPct float64, minRemaining int, sessionID, model string) float64 {
	authID, authLabel, authEmail := extractAuthIdentity(auth)
	if authID == "" {
		return 0
	}

	now := time.Now().UTC()
	ts := now.Format(time.RFC3339Nano)

	globalCodexTelemetryStore.mu.Lock()
	acc := globalCodexTelemetryStore.ensureAccountLocked(authID, authLabel, authEmail)
	hadPrev := acc.HasQuota
	prevUsed := acc.LastUsedPct
	delta := 0.0
	if hadPrev {
		delta = math.Round((usedPct-prevUsed)*100) / 100
	}
	acc.HasQuota = true
	acc.LastUsedPct = usedPct
	acc.LastRemainingPct = remainingPct
	acc.LastSeenAt = ts
	lastTurnHow := acc.LastHow
	if delta > 0.01 {
		acc.TotalPositiveDeltaPct = math.Round((acc.TotalPositiveDeltaPct+delta)*100) / 100
		ev := CodexQuotaDeltaEvent{
			Timestamp:    ts,
			UnixMs:       now.UnixMilli(),
			AuthID:       authID,
			AuthLabel:    authLabel,
			AuthEmail:    authEmail,
			PrevUsedPct:  prevUsed,
			NewUsedPct:   usedPct,
			DeltaPct:     delta,
			RemainingPct: remainingPct,
			SessionID:    sessionID,
			Model:        model,
			LastTurnHow:  lastTurnHow,
		}
		if len(globalCodexTelemetryStore.quotaDeltas) >= codexQuotaDeltaRingCapacity {
			copy(globalCodexTelemetryStore.quotaDeltas, globalCodexTelemetryStore.quotaDeltas[1:])
			globalCodexTelemetryStore.quotaDeltas[len(globalCodexTelemetryStore.quotaDeltas)-1] = ev
		} else {
			globalCodexTelemetryStore.quotaDeltas = append(globalCodexTelemetryStore.quotaDeltas, ev)
		}
	}
	globalCodexTelemetryStore.mu.Unlock()

	if !hadPrev {
		log.Infof("📊 [CODEX-QUOTA] auth=%s (%s) 5h used=%.1f%% remaining=%.1f%% minRemaining=%d%% session=%s",
			authLabel, authID, usedPct, remainingPct, minRemaining, sessionID)
	} else if delta > 0.01 {
		log.Warnf("📈 [CODEX-QUOTA-DELTA] auth=%s (%s) 5h used %.1f%% -> %.1f%% (%+.1f%%, remaining=%.1f%%) model=%s session=%s prevHow=%s",
			authLabel, authID, prevUsed, usedPct, delta, remainingPct, model, sessionID, lastTurnHow)
	} else if delta < -0.5 {
		log.Infof("🔄 [CODEX-QUOTA-RESET] auth=%s (%s) 5h window refreshed %.1f%% -> %.1f%% (remaining=%.1f%%)",
			authLabel, authID, prevUsed, usedPct, remainingPct)
	}
	return delta
}

func appendCodexAutocutJSONL(logFilePath string, rec CodexAutocutTurnRecord) {
	if strings.TrimSpace(logFilePath) == "" {
		return
	}
	lineBytes, err := json.Marshal(rec)
	if err != nil {
		return
	}
	lineBytes = append(lineBytes, '\n')

	dir := filepath.Dir(logFilePath)
	if errMk := os.MkdirAll(dir, 0o755); errMk != nil {
		return
	}
	if info, errStat := os.Stat(logFilePath); errStat == nil && info.Size()+int64(len(lineBytes)) > codexAutocutTelemetryMaxBytes {
		backupPath := logFilePath + ".1"
		_ = os.Remove(backupPath)
		_ = os.Rename(logFilePath, backupPath)
	}
	f, errOpen := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if errOpen != nil {
		return
	}
	_, _ = f.Write(lineBytes)
	_ = f.Close()
}

func (s *codexAutocutTelemetryStore) hydrateFromDiskIfNeededLocked(logFilePath string) {
	if strings.TrimSpace(logFilePath) == "" {
		return
	}
	cleanPath := filepath.Clean(logFilePath)
	if s.hydratedDir == cleanPath || s.totalTurns > 0 {
		if s.hydratedDir == "" {
			s.hydratedDir = cleanPath
		}
		return
	}
	s.hydratedDir = cleanPath

	f, err := os.Open(cleanPath)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	// Read up to tail 2MB from existing JSONL on startup so restarts keep history.
	if info, errStat := f.Stat(); errStat == nil && info.Size() > 2*1024*1024 {
		_, _ = f.Seek(info.Size()-2*1024*1024, 0)
		rd := bufio.NewReader(f)
		_, _ = rd.ReadString('\n') // discard partial line
		scanner := bufio.NewScanner(rd)
		scanner.Buffer(make([]byte, 64*1024), 512*1024)
		for scanner.Scan() {
			var rec CodexAutocutTurnRecord
			if json.Unmarshal(scanner.Bytes(), &rec) == nil && rec.How != "" {
				s.ingestTurnLocked(rec)
			}
		}
		return
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 512*1024)
	for scanner.Scan() {
		var rec CodexAutocutTurnRecord
		if json.Unmarshal(scanner.Bytes(), &rec) == nil && rec.How != "" {
			s.ingestTurnLocked(rec)
		}
	}
}

func recordCodexAutocutTurnTelemetry(opts CodexAutocutOptions, tel CodexAutocutTelemetry, streak int) {
	how := tel.How
	if how == "" {
		how = "eof"
	}
	freeCut, leakRisk := classifyCodexTurnOutcome(tel)
	now := time.Now().UTC()

	rec := CodexAutocutTurnRecord{
		Timestamp:            now.Format(time.RFC3339Nano),
		UnixMs:               now.UnixMilli(),
		AuthID:               opts.AuthID,
		AuthLabel:            opts.AuthLabel,
		AuthEmail:            opts.AuthEmail,
		Model:                opts.Model,
		SessionID:            opts.SessionID,
		How:                  how,
		AbortReason:          tel.AbortReason,
		FreeCut:              freeCut,
		LeakRisk:             leakRisk,
		RaceLost:             tel.RaceLost,
		EstInputTokens:       opts.EstimateInputTokens,
		DeliveredChars:       tel.DeliveredChars,
		MaxToolArgsLen:       tel.MaxToolArgsLen,
		FinalizeLen:          tel.FinalizeLen,
		ToolCallCount:        tel.ToolCallCount,
		LastToolName:         tel.LastToolName,
		DurationMs:           tel.DurationMs,
		SessionStreak:        streak,
		HasQuota:             opts.HasQuota,
		UsedPct:              opts.UsedPct,
		RemainingPct:         opts.RemainingPct,
		QuotaDeltaPct:        opts.QuotaDeltaPct,
		UpstreamInputTokens:  tel.UpstreamInputTokens,
		UpstreamOutputTokens: tel.UpstreamOutputTokens,
		UpstreamCachedTokens: tel.UpstreamCachedTokens,
	}

	logFilePath := strings.TrimSpace(opts.TelemetryLogPath)
	globalCodexTelemetryStore.mu.Lock()
	globalCodexTelemetryStore.hydrateFromDiskIfNeededLocked(logFilePath)
	globalCodexTelemetryStore.ingestTurnLocked(rec)
	globalCodexTelemetryStore.mu.Unlock()

	if logFilePath != "" {
		appendCodexAutocutJSONL(logFilePath, rec)
	}

	authDisp := opts.AuthLabel
	if authDisp == "" {
		authDisp = opts.AuthID
	}
	if authDisp == "" {
		authDisp = "-"
	}
	abortDisp := tel.AbortReason
	if abortDisp == "" {
		abortDisp = "-"
	}
	toolDisp := tel.LastToolName
	if toolDisp == "" {
		toolDisp = "-"
	}
	quotaSuffix := ""
	if opts.HasQuota {
		if math.Abs(opts.QuotaDeltaPct) >= 0.05 {
			quotaSuffix = fmt.Sprintf(" used=%.1f%%(%+.1f%%) rem=%.1f%%", opts.UsedPct, opts.QuotaDeltaPct, opts.RemainingPct)
		} else {
			quotaSuffix = fmt.Sprintf(" used=%.1f%% rem=%.1f%%", opts.UsedPct, opts.RemainingPct)
		}
	}
	upUsageSuffix := ""
	if tel.UpstreamInputTokens > 0 || tel.UpstreamOutputTokens > 0 {
		upUsageSuffix = fmt.Sprintf(" upUsage=%d/%d/%d", tel.UpstreamInputTokens, tel.UpstreamOutputTokens, tel.UpstreamCachedTokens)
	}

	if tel.RaceLost {
		log.Warnf("🚨 [AC:RACE-LOST] how=%s abort=%s auth=%s model=%s session=%s inEst=%d delivered=%d maxArg=%d finLen=%d tool=%s(%d) dur=%dms streak=%d%s%s",
			how, abortDisp, authDisp, opts.Model, opts.SessionID, opts.EstimateInputTokens,
			tel.DeliveredChars, tel.MaxToolArgsLen, tel.FinalizeLen, toolDisp, tel.ToolCallCount,
			tel.DurationMs, streak, quotaSuffix, upUsageSuffix)
	} else if leakRisk {
		log.Warnf("⚠️ ✂ AUTOCUT [LEAK-RISK] · how=%s abort=%s auth=%s model=%s session=%s inEst=%d delivered=%d maxArg=%d finLen=%d tool=%s(%d) dur=%dms streak=%d%s%s",
			how, abortDisp, authDisp, opts.Model, opts.SessionID, opts.EstimateInputTokens,
			tel.DeliveredChars, tel.MaxToolArgsLen, tel.FinalizeLen, toolDisp, tel.ToolCallCount,
			tel.DurationMs, streak, quotaSuffix, upUsageSuffix)
	} else {
		log.Infof("✂ AUTOCUT · how=%s abort=%s auth=%s model=%s session=%s inEst=%d delivered=%d maxArg=%d finLen=%d tool=%s(%d) dur=%dms streak=%d%s",
			how, abortDisp, authDisp, opts.Model, opts.SessionID, opts.EstimateInputTokens,
			tel.DeliveredChars, tel.MaxToolArgsLen, tel.FinalizeLen, toolDisp, tel.ToolCallCount,
			tel.DurationMs, streak, quotaSuffix)
	}
}

// SnapshotCodexAutocutTelemetry returns a thread-safe snapshot of all Codex metering-bypass
// forensic metrics, per-account quota states, quota deltas, and recent turns.
func SnapshotCodexAutocutTelemetry(cfg *config.Config, explicitLogDir string, q CodexTelemetryQueryOptions) CodexAutocutTelemetrySnapshot {
	logFilePath := resolveCodexTelemetryLogPath(cfg, explicitLogDir)
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > codexAutocutRingCapacity {
		limit = codexAutocutRingCapacity
	}
	sessFilter := strings.TrimSpace(q.SessionID)
	authFilter := strings.ToLower(strings.TrimSpace(q.AuthQuery))

	globalCodexTelemetryStore.mu.Lock()
	globalCodexTelemetryStore.hydrateFromDiskIfNeededLocked(logFilePath)

	howCopy := make(map[string]int64, len(globalCodexTelemetryStore.howCounts))
	for k, v := range globalCodexTelemetryStore.howCounts {
		howCopy[k] = v
	}
	specCopy := make(map[string]int64, len(globalCodexTelemetryStore.specialCounts)+1)
	for k, v := range globalCodexTelemetryStore.specialCounts {
		specCopy[k] = v
	}
	specCopy["compact_hijack"] = helps.CodexCompactHijackCount()

	total := globalCodexTelemetryStore.totalTurns
	free := globalCodexTelemetryStore.freeCuts
	leak := globalCodexTelemetryStore.leakRiskTurns
	race := globalCodexTelemetryStore.raceLostTurns
	rate := 100.0
	if total > 0 {
		rate = math.Round((float64(free)/float64(total))*10000) / 100
	}

	accounts := make([]CodexAccountTelemetrySummary, 0, len(globalCodexTelemetryStore.accounts))
	for _, acc := range globalCodexTelemetryStore.accounts {
		cp := *acc
		_, tripped := codexTrippedBreakers.Load(cp.AuthID)
		cp.CircuitBreakerTripped = tripped
		if authFilter != "" {
			if !strings.Contains(strings.ToLower(cp.AuthID), authFilter) &&
				!strings.Contains(strings.ToLower(cp.AuthLabel), authFilter) &&
				!strings.Contains(strings.ToLower(cp.AuthEmail), authFilter) {
				continue
			}
		}
		accounts = append(accounts, cp)
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].LastUsedPct != accounts[j].LastUsedPct {
			return accounts[i].LastUsedPct > accounts[j].LastUsedPct
		}
		return accounts[i].AuthID < accounts[j].AuthID
	})

	deltas := make([]CodexQuotaDeltaEvent, 0, len(globalCodexTelemetryStore.quotaDeltas))
	for i := len(globalCodexTelemetryStore.quotaDeltas) - 1; i >= 0; i-- {
		ev := globalCodexTelemetryStore.quotaDeltas[i]
		if authFilter != "" &&
			!strings.Contains(strings.ToLower(ev.AuthID), authFilter) &&
			!strings.Contains(strings.ToLower(ev.AuthLabel), authFilter) &&
			!strings.Contains(strings.ToLower(ev.AuthEmail), authFilter) {
			continue
		}
		if sessFilter != "" && ev.SessionID != sessFilter {
			continue
		}
		deltas = append(deltas, ev)
		if len(deltas) >= 100 {
			break
		}
	}

	recent := make([]CodexAutocutTurnRecord, 0, limit)
	for i := len(globalCodexTelemetryStore.turns) - 1; i >= 0; i-- {
		rec := globalCodexTelemetryStore.turns[i]
		if q.LeaksOnly && !rec.LeakRisk {
			continue
		}
		if sessFilter != "" && rec.SessionID != sessFilter {
			continue
		}
		if authFilter != "" &&
			!strings.Contains(strings.ToLower(rec.AuthID), authFilter) &&
			!strings.Contains(strings.ToLower(rec.AuthLabel), authFilter) &&
			!strings.Contains(strings.ToLower(rec.AuthEmail), authFilter) {
			continue
		}
		recent = append(recent, rec)
		if len(recent) >= limit {
			break
		}
	}
	globalCodexTelemetryStore.mu.Unlock()

	var logSize int64
	if logFilePath != "" {
		if info, errStat := os.Stat(logFilePath); errStat == nil {
			logSize = info.Size()
		}
	}

	globalCodexSemaphore.mu.Lock()
	activeSem := globalCodexSemaphore.active
	globalCodexSemaphore.mu.Unlock()

	return CodexAutocutTelemetrySnapshot{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Summary: CodexAutocutTelemetrySummary{
			TotalTurns:       total,
			FreeCuts:         free,
			LeakRiskTurns:    leak,
			RaceLostTurns:    race,
			FreeCutRatePct:   rate,
			HowCounts:        howCopy,
			SpecialCounts:    specCopy,
			ActiveSemInUse:   activeSem,
			LogFilePath:      logFilePath,
			LogFileSizeBytes: logSize,
		},
		Accounts:    accounts,
		QuotaDeltas: deltas,
		RecentTurns: recent,
	}
}

// extractUpstreamUsageFromResponseMap extracts OpenAI's (input_tokens, output_tokens, cached_tokens)
// from a parsed response.completed / response.done event map when available.
func extractUpstreamUsageFromResponseMap(data map[string]any) (inTok, outTok, cachedTok int) {
	if data == nil {
		return 0, 0, 0
	}
	resp, _ := data["response"].(map[string]any)
	if resp == nil {
		resp = data
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		return 0, 0, 0
	}
	inTok = autocutIntField(usage, "input_tokens", 0)
	outTok = autocutIntField(usage, "output_tokens", 0)
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		cachedTok = autocutIntField(details, "cached_tokens", 0)
	}
	return inTok, outTok, cachedTok
}

// extractUpstreamUsageFromRawSSE scans rawBuffer on RACE-LOST cuts to extract
// upstream usage if response.completed arrived in the socket buffer before cut.
func extractUpstreamUsageFromRawSSE(raw string) (inTok, outTok, cachedTok int) {
	if raw == "" || !strings.Contains(raw, `"usage"`) {
		return 0, 0, 0
	}
	for _, block := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(block)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if !strings.Contains(payload, `"response.completed"`) && !strings.Contains(payload, `"response.done"`) {
			continue
		}
		var parsed map[string]any
		if json.Unmarshal([]byte(payload), &parsed) == nil {
			if i, o, c := extractUpstreamUsageFromResponseMap(parsed); i > 0 || o > 0 {
				return i, o, c
			}
		}
	}
	return 0, 0, 0
}
