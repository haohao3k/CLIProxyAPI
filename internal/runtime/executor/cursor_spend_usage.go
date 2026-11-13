package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cursorwire "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cursorguard"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	cursorDashboardService   = cursorwire.APIHost + "/aiserver.v1.DashboardService/"
	cursorUsageEventPageSize = 100
	cursorUsageEventMaxPages = 50
)

func init() {
	cursorguard.Default().SetFetcher(fetchCursorSpendUsage)
}

// cursorDashboardBaseURL is overridden in tests.
var cursorDashboardBaseURL = cursorDashboardService

// fetchCursorSpendUsage reads a credential's billing-period usage from Cursor's dashboard API:
// included quota and account limit from GetCurrentPeriodUsage, whether usage-based pricing is
// allowed from GetHardLimit, and the credential's own usage events (paged over the whole period)
// for overage and per-model costs.
func fetchCursorSpendUsage(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (cursorguard.Usage, error) {
	var usage cursorguard.Usage
	exec := &CursorExecutor{cfg: cfg}
	client := helps.NewDevinHTTPClient(ctx, cfg, auth, 30*time.Second)
	call := func(method string, body any) ([]byte, error) {
		payload, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorDashboardBaseURL+method, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		if err = exec.PrepareRequest(req, auth); err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: status %d", method, resp.StatusCode)
		}
		return data, nil
	}

	period, err := call("GetCurrentPeriodUsage", map[string]any{})
	if err != nil {
		return usage, err
	}
	start := ojson.GetBytes(period, "billingCycleStart").String()
	usage.PeriodStart = msTime(start)
	usage.PeriodEnd = msTime(ojson.GetBytes(period, "billingCycleEnd").String())
	if usage.PeriodStart.IsZero() {
		return usage, fmt.Errorf("GetCurrentPeriodUsage: no billing period")
	}
	plan := ojson.GetBytes(period, "planUsage")
	usage.IncludedUsedCents = firstNumber(plan, "includedSpend", "totalSpend")
	usage.IncludedLimitCents = firstNumber(plan, "limit")
	if remaining := plan.Get("remaining"); remaining.Exists() {
		usage.IncludedRemainingCents = remaining.Float()
	} else {
		usage.IncludedRemainingCents = usage.IncludedLimitCents - usage.IncludedUsedCents
	}
	spend := ojson.GetBytes(period, "spendLimitUsage")
	usage.AccountLimitType = strings.TrimSpace(spend.Get("limitType").String())
	for _, key := range []string{"pooledLimit", "individualLimit", "limit"} {
		if v := spend.Get(key); v.Exists() {
			f := v.Float()
			usage.AccountLimitCents = &f
			break
		}
	}
	for _, key := range []string{"pooledUsed", "totalSpend"} {
		if v := spend.Get(key); v.Exists() {
			f := v.Float()
			usage.PooledUsedCents = &f
			break
		}
	}

	if hard, errHard := call("GetHardLimit", map[string]any{}); errHard == nil {
		if v := ojson.GetBytes(hard, "noUsageBasedAllowed"); v.Exists() {
			allowed := !v.Bool()
			usage.UsageBasedAllowed = &allowed
		}
	}

	end := strconv.FormatInt(time.Now().UnixMilli(), 10)
	costs := make(map[string][]float64)
	read, total := 0, int64(-1)
	for page := 1; page <= cursorUsageEventMaxPages; page++ {
		events, errEvents := call("GetFilteredUsageEvents", map[string]any{"startDate": start, "endDate": end, "page": page, "pageSize": cursorUsageEventPageSize})
		if errEvents != nil {
			return usage, errEvents
		}
		if total < 0 {
			total = ojson.GetBytes(events, "totalUsageEventsCount").Int()
		}
		list := ojson.GetBytes(events, "usageEventsDisplay").Array()
		for _, event := range list {
			cents := event.Get("chargedCents").Float()
			// Anything not explicitly included in the plan is on-demand spend.
			if !strings.EqualFold(strings.TrimSpace(event.Get("usageType").String()), "included") {
				usage.OverageCents += cents
			}
			if model := strings.ToLower(strings.TrimSpace(event.Get("model").String())); model != "" {
				costs[model] = append(costs[model], cents)
			}
			if at := msTime(event.Get("timestamp").String()); at.After(usage.LatestEventAt) {
				usage.LatestEventAt = at
			}
		}
		read += len(list)
		if len(list) < cursorUsageEventPageSize || int64(read) >= total {
			break
		}
	}
	usage.Complete = total >= 0 && int64(read) >= total
	usage.ModelCostCents = make(map[string]float64, len(costs))
	for model, values := range costs {
		usage.ModelCostCents[model] = p95(values)
	}
	return usage, nil
}

func firstNumber(obj ojson.Result, keys ...string) float64 {
	for _, key := range keys {
		if v := obj.Get(key); v.Exists() {
			return v.Float()
		}
	}
	return 0
}

func msTime(raw string) time.Time {
	ms, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func p95(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[int(float64(len(sorted)-1)*0.95)]
}
