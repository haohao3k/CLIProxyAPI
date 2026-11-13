package cursorguard

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// AccountLimit is Cursor's own account-side on-demand limit (a team pool on team plans).
type AccountLimit struct {
	Type              string   `json:"type"`
	LimitUSD          *float64 `json:"limit_usd"`
	PooledUsedUSD     *float64 `json:"pooled_used_usd"`
	UsageBasedAllowed *bool    `json:"usage_based_allowed"`
}

// CredentialStatus is one Cursor credential as GET /cursor-spend-guard/status reports it.
type CredentialStatus struct {
	AuthIndex            string       `json:"auth_index"`
	Label                string       `json:"label"`
	Blocked              bool         `json:"blocked"`
	Reason               *Reason      `json:"reason"`
	CheckedAt            *time.Time   `json:"checked_at"`
	PeriodStart          *time.Time   `json:"period_start"`
	PeriodEnd            *time.Time   `json:"period_end"`
	IncludedUsedUSD      *float64     `json:"included_used_usd"`
	IncludedLimitUSD     *float64     `json:"included_limit_usd"`
	IncludedRemainingUSD *float64     `json:"included_remaining_usd"`
	IncludedExhausted    bool         `json:"included_exhausted"`
	OverageUSD           *float64     `json:"overage_usd"`
	EstimateUSD          float64      `json:"estimate_usd"`
	CapUSD               float64      `json:"cap_usd"`
	AccountLimit         AccountLimit `json:"account_limit"`
	ConsecutiveFailures  int          `json:"consecutive_failures"`
	LastError            *string      `json:"last_error"`
}

// Status reports every Cursor credential in auths. recheck polls each one now; otherwise a
// credential is polled only if it has never been read. Polls run even when the guard is disabled
// so the UI can show usage; blocks apply only when it is enabled.
func (g *Guard) Status(ctx context.Context, cfg *config.Config, auths []*coreauth.Auth, recheck bool) []CredentialStatus {
	out := make([]CredentialStatus, 0, len(auths))
	if cfg == nil {
		return out
	}
	settings := cfg.CursorSpendGuard
	for _, auth := range auths {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "cursor") {
			continue
		}
		st := g.state(auth.ID)
		g.mu.Lock()
		never := st.checkedAt.IsZero()
		g.mu.Unlock()
		if recheck || never {
			g.pollIfStale(ctx, cfg, auth, st, true)
		}
		g.mu.Lock()
		entry := CredentialStatus{
			AuthIndex:           auth.EnsureIndex(),
			Label:               credentialLabel(auth),
			CapUSD:              settings.MaxOverageUSD,
			EstimateUSD:         round2(sumReservations(st) / 100),
			ConsecutiveFailures: st.failures,
		}
		if !st.checkedAt.IsZero() {
			t := st.checkedAt
			entry.CheckedAt = &t
		}
		if st.lastErr != "" {
			e := st.lastErr
			entry.LastError = &e
		}
		if u := st.usage; u != nil {
			entry.PeriodStart, entry.PeriodEnd = timePtr(u.PeriodStart), timePtr(u.PeriodEnd)
			entry.IncludedUsedUSD = usd(u.IncludedUsedCents)
			entry.IncludedLimitUSD = usd(u.IncludedLimitCents)
			entry.IncludedRemainingUSD = usd(u.IncludedRemainingCents)
			entry.IncludedExhausted = u.IncludedExhausted()
			entry.OverageUSD = usd(u.OverageCents)
			entry.AccountLimit = AccountLimit{Type: u.AccountLimitType, UsageBasedAllowed: u.UsageBasedAllowed}
			if u.AccountLimitCents != nil {
				entry.AccountLimit.LimitUSD = usd(*u.AccountLimitCents)
			}
			if u.PooledUsedCents != nil {
				entry.AccountLimit.PooledUsedUSD = usd(*u.PooledUsedCents)
			}
		}
		if entry.AccountLimit.Type == "" {
			entry.AccountLimit.Type = "unknown"
		}
		if settings.Enabled {
			reason := g.decideLocked(st, settings, settings.DefaultRequestReserveUSD*100)
			g.recordLocked(auth, st, reason)
			entry.Blocked, entry.Reason = reason != nil, reason
		}
		g.mu.Unlock()
		out = append(out, entry)
	}
	return out
}

// credentialLabel is the account email or the auth file name, never a token.
func credentialLabel(auth *coreauth.Auth) string {
	if auth.Metadata != nil {
		if email, ok := auth.Metadata["email"].(string); ok && strings.TrimSpace(email) != "" {
			return strings.TrimSpace(email)
		}
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return filepath.Base(name)
	}
	return auth.EnsureIndex()
}

func usd(cents float64) *float64 {
	v := round2(cents / 100)
	return &v
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
