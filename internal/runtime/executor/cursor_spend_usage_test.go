package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// The usage reader pages over the whole billing period, counts only the credential's non-included
// events as overage (not the team pool), and reports a period it could not read fully as incomplete.
func TestFetchCursorSpendUsageFromDashboard(t *testing.T) {
	events := func(n int, usageType string, cents float64) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"timestamp": fmt.Sprint(1794590000000 + int64(i)), "model": "deepseek-v41-flash-1017-max", "usageType": usageType, "chargedCents": cents}
		}
		return out
	}
	all := append(events(150, "included", 0.4), events(30, "usage_based", 5)...)
	total := len(all)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Connect-Protocol-Version") != "1" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/GetCurrentPeriodUsage"):
			_, _ = w.Write([]byte(`{"billingCycleStart":"1794205230000","billingCycleEnd":"1796797230000",
				"planUsage":{"includedSpend":2000,"limit":2000,"remaining":0},
				"spendLimitUsage":{"totalSpend":116317,"pooledUsed":116317,"limitType":"team"}}`))
		case strings.HasSuffix(r.URL.Path, "/GetHardLimit"):
			_, _ = w.Write([]byte(`{"noUsageBasedAllowed":true}`))
		case strings.HasSuffix(r.URL.Path, "/GetFilteredUsageEvents"):
			var body struct{ Page, PageSize int }
			_ = json.NewDecoder(r.Body).Decode(&body)
			from := (body.Page - 1) * body.PageSize
			to := min(from+body.PageSize, len(all))
			_ = json.NewEncoder(w).Encode(map[string]any{"totalUsageEventsCount": total, "usageEventsDisplay": all[from:to]})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	previous := cursorDashboardBaseURL
	cursorDashboardBaseURL = server.URL + "/aiserver.v1.DashboardService/"
	t.Cleanup(func() { cursorDashboardBaseURL = previous })
	auth := &cliproxyauth.Auth{ID: "cursor-a", Provider: "cursor", Metadata: map[string]any{"access_token": "test-token"}}

	usage, err := fetchCursorSpendUsage(context.Background(), &config.Config{}, auth)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !usage.Complete || usage.OverageCents != 150 || usage.IncludedRemainingCents != 0 || usage.IncludedLimitCents != 2000 {
		t.Fatalf("usage = complete %v overage %v remaining %v limit %v", usage.Complete, usage.OverageCents, usage.IncludedRemainingCents, usage.IncludedLimitCents)
	}
	if usage.PooledUsedCents == nil || *usage.PooledUsedCents != 116317 || usage.AccountLimitType != "team" || usage.UsageBasedAllowed == nil || *usage.UsageBasedAllowed {
		t.Fatalf("account limit = %+v", usage)
	}
	if usage.ModelCostCents["deepseek-v41-flash-1017-max"] != 5 {
		t.Fatalf("p95 model cost = %v", usage.ModelCostCents)
	}

	// More events than the page cap can read: incomplete, so the guard treats usage as unknown.
	total = cursorUsageEventPageSize*cursorUsageEventMaxPages + 1
	all = events(total, "included", 0.1)
	usage, err = fetchCursorSpendUsage(context.Background(), &config.Config{}, auth)
	if err != nil || usage.Complete {
		t.Fatalf("over the page cap: complete %v err %v", usage.Complete, err)
	}
}
