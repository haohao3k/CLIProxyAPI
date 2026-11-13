package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cursorguard"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Guard settings persist (explicit false included) and the status reports each Cursor credential
// in USD, with the label and never the token.
func TestCursorSpendGuardManagement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err = manager.Register(context.Background(), &coreauth.Auth{ID: "cursor-a", Provider: "cursor", Status: coreauth.StatusActive,
		Metadata: map[string]any{"email": "dev@example.com", "access_token": "secret-token"}}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path, authManager: manager}
	polls := 0
	cursorguard.Default().SetFetcher(func(context.Context, *config.Config, *coreauth.Auth) (cursorguard.Usage, error) {
		polls++
		now := time.Now()
		return cursorguard.Usage{PeriodStart: now.Add(-time.Hour), PeriodEnd: now.Add(time.Hour), IncludedUsedCents: 2000, IncludedLimitCents: 2000,
			OverageCents: 2100, Complete: true, AccountLimitType: "team"}, nil
	})
	t.Cleanup(func() { cursorguard.Default().SetFetcher(nil) })

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) })
	router.GET("/cursor-spend-guard", h.GetCursorSpendGuard)
	router.PUT("/cursor-spend-guard", h.PutCursorSpendGuard)
	router.GET("/cursor-spend-guard/status", h.GetCursorSpendGuardStatus)
	router.POST("/cursor-spend-guard/recheck", h.RecheckCursorSpendGuard)
	send := func(method, url, body string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, url, strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, url, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	if got := send(http.MethodGet, "/cursor-spend-guard", ""); !strings.Contains(got, `"max-overage-usd":20`) || !strings.Contains(got, `"fail-closed-when-unknown":true`) {
		t.Fatalf("defaults = %s", got)
	}
	send(http.MethodPut, "/cursor-spend-guard", `{"cursor-spend-guard":{"enabled":true,"max-overage-usd":20,"fail-closed-when-unknown":false}}`)
	loaded, err := config.LoadConfig(path)
	if err != nil || !loaded.CursorSpendGuard.Enabled || loaded.CursorSpendGuard.FailClosedWhenUnknown || loaded.CursorSpendGuard.PollInterval != "60s" {
		t.Fatalf("persisted = %+v err %v", loaded.CursorSpendGuard, err)
	}

	body := send(http.MethodPost, "/cursor-spend-guard/recheck", "")
	if strings.Contains(body, "secret-token") {
		t.Fatal("status leaks the credential token")
	}
	var status struct {
		Enabled     bool    `json:"enabled"`
		CapUSD      float64 `json:"cap_usd"`
		Credentials []struct {
			Label      string                 `json:"label"`
			Blocked    bool                   `json:"blocked"`
			Reason     *struct{ Code string } `json:"reason"`
			OverageUSD *float64               `json:"overage_usd"`
		} `json:"credentials"`
	}
	if err = json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.CapUSD != 20 || len(status.Credentials) != 1 {
		t.Fatalf("status = %s", body)
	}
	c := status.Credentials[0]
	if c.Label != "dev@example.com" || !c.Blocked || c.Reason == nil || c.Reason.Code != cursorguard.ReasonCapReached || c.OverageUSD == nil || *c.OverageUSD != 21 {
		t.Fatalf("credential = %s", body)
	}
	if polls != 1 {
		t.Fatalf("recheck polls = %d, want 1", polls)
	}
}
