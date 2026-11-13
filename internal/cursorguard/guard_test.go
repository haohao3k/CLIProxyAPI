package cursorguard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type fakeCursor struct {
	usage Usage
	err   error
	polls int
}

func (f *fakeCursor) fetch(context.Context, *config.Config, *coreauth.Auth) (Usage, error) {
	f.polls++
	return f.usage, f.err
}

type guardFixture struct {
	t      *testing.T
	guard  *Guard
	cursor *fakeCursor
	clock  time.Time
	cfg    *config.Config
	auth   *coreauth.Auth
}

func newGuardFixture(t *testing.T) *guardFixture {
	f := &guardFixture{t: t, cursor: &fakeCursor{}, clock: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	f.guard = New(f.cursor.fetch)
	f.guard.now = func() time.Time { return f.clock }
	settings := config.DefaultCursorSpendGuard()
	settings.Enabled = true
	f.cfg = &config.Config{SDKConfig: config.SDKConfig{CursorSpendGuard: settings}}
	f.auth = &coreauth.Auth{ID: "cursor-a", Provider: "cursor"}
	// Included quota used up, $19 of on-demand spent, period ends in a week.
	f.cursor.usage = Usage{
		PeriodStart: f.clock.Add(-7 * 24 * time.Hour), PeriodEnd: f.clock.Add(7 * 24 * time.Hour),
		IncludedUsedCents: 2000, IncludedLimitCents: 2000, IncludedRemainingCents: 0,
		OverageCents: 1900, Complete: true, LatestEventAt: f.clock.Add(-time.Minute),
	}
	return f
}

// admit returns the refusal reason code, or "" when admitted; admitted requests reach Cursor.
func (f *guardFixture) admit() string {
	f.t.Helper()
	release, err := f.guard.Admit(context.Background(), f.cfg, f.auth, "deepseek-v41-flash-1017")
	if err != nil {
		var blocked *BlockedError
		if !errors.As(err, &blocked) || blocked.StatusCode() != 429 || !blocked.IsCredentialScoped() {
			f.t.Fatalf("refusal is not a credential-scoped 429: %v", err)
		}
		return blocked.Reason.Code
	}
	release(true)
	return ""
}

func TestGuardAllowsUnderCapAndBlocksAtCap(t *testing.T) {
	f := newGuardFixture(t)
	f.cursor.usage.OverageCents = 1000
	if code := f.admit(); code != "" {
		t.Fatalf("$10 of a $20 cap refused: %s", code)
	}
	f.cursor.usage.OverageCents = 2000
	f.clock = f.clock.Add(time.Minute) // next poll
	if code := f.admit(); code != ReasonCapReached {
		t.Fatalf("at the cap: %q, want %s", code, ReasonCapReached)
	}
	// Raising the cap in config is the only override, and it applies at once.
	f.cfg.CursorSpendGuard.MaxOverageUSD = 30
	if code := f.admit(); code != "" {
		t.Fatalf("after raising the cap: %s", code)
	}
}

func TestGuardReservationsStopABurstBetweenPolls(t *testing.T) {
	f := newGuardFixture(t) // $19 spent, $0.50 reserved per request
	if f.admit() != "" || f.admit() != "" {
		t.Fatal("the first two requests ($20 total) must pass")
	}
	if code := f.admit(); code != ReasonEstimateReached {
		t.Fatalf("third request in the same poll window: %q, want %s", code, ReasonEstimateReached)
	}
	if f.cursor.polls != 1 {
		t.Fatalf("polls = %d, want 1 within one interval", f.cursor.polls)
	}

	// A request that never reached Cursor gives its reservation back.
	g := newGuardFixture(t)
	release, err := g.guard.Admit(context.Background(), g.cfg, g.auth, "m")
	if err != nil {
		t.Fatal(err)
	}
	release(false)
	if g.admit() != "" || g.admit() != "" {
		t.Fatal("a released reservation still counts")
	}

	// Remaining included quota absorbs reservations first.
	h := newGuardFixture(t)
	h.cursor.usage.IncludedRemainingCents = 500
	h.cursor.usage.IncludedUsedCents = 1500
	h.cursor.usage.OverageCents = 1990
	for i := 0; i < 10; i++ {
		if code := h.admit(); code != "" {
			t.Fatalf("request %d with $5 included left refused: %s", i, code)
		}
	}
}

func TestGuardPeriodResetUnblocks(t *testing.T) {
	f := newGuardFixture(t)
	f.cursor.usage.OverageCents = 2500
	if code := f.admit(); code != ReasonCapReached {
		t.Fatalf("over the cap: %q", code)
	}
	f.clock = f.cursor.usage.PeriodEnd.Add(time.Second)
	f.cursor.usage = Usage{PeriodStart: f.clock, PeriodEnd: f.clock.Add(30 * 24 * time.Hour), IncludedLimitCents: 2000, IncludedRemainingCents: 2000, Complete: true}
	if code := f.admit(); code != "" {
		t.Fatalf("new billing period: %s", code)
	}
}

func TestGuardFailsClosedWhenUsageIsUnknown(t *testing.T) {
	// Never read, polls failing: allowed for the grace polls, then blocked.
	f := newGuardFixture(t)
	f.cursor.err = errors.New("dashboard unavailable")
	for i := 1; i < f.cfg.CursorSpendGuard.UnknownGracePolls; i++ {
		if code := f.admit(); code != "" {
			t.Fatalf("failed poll %d within grace: %s", i, code)
		}
		f.clock = f.clock.Add(time.Minute)
	}
	if code := f.admit(); code != ReasonUsageUnknown {
		t.Fatalf("after %d failed polls: %q", f.cfg.CursorSpendGuard.UnknownGracePolls, code)
	}

	// Included quota known to be used up: the first failed poll blocks.
	g := newGuardFixture(t)
	g.cursor.usage.OverageCents = 0
	if code := g.admit(); code != "" {
		t.Fatalf("good poll: %s", code)
	}
	g.cursor.err = errors.New("dashboard unavailable")
	g.clock = g.clock.Add(time.Minute)
	if code := g.admit(); code != ReasonUsageUnknown {
		t.Fatalf("failed poll with included exhausted: %q", code)
	}

	// Not all usage events of the period could be read: unknown.
	h := newGuardFixture(t)
	h.cursor.usage.Complete = false
	if code := h.admit(); code != ReasonUsageUnknown {
		t.Fatalf("incomplete events: %q", code)
	}
	h.cfg.CursorSpendGuard.FailClosedWhenUnknown = false
	if code := h.admit(); code != "" {
		t.Fatalf("incomplete events with fail-closed off: %s", code)
	}
}

// Cursor's spendLimitUsage on a team plan is the whole team's pool: it must not count as ours.
func TestGuardIgnoresTeamPooledSpend(t *testing.T) {
	f := newGuardFixture(t)
	pooled := 116317.0
	f.cursor.usage.PooledUsedCents = &pooled
	f.cursor.usage.AccountLimitType = "team"
	f.cursor.usage.OverageCents = 0
	if code := f.admit(); code != "" {
		t.Fatalf("team pool spend blocked the credential: %s", code)
	}
}

func TestGuardDisabledNeverPolls(t *testing.T) {
	f := newGuardFixture(t)
	f.cfg.CursorSpendGuard.Enabled = false
	f.cursor.usage.OverageCents = 99999
	if code := f.admit(); code != "" || f.cursor.polls != 0 {
		t.Fatalf("disabled guard: code %q polls %d", code, f.cursor.polls)
	}
}
