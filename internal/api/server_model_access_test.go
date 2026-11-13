package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"net/http/httptest"
	"strings"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// Routes that take a model outside the shared execution path (Codex alpha search, live and
// realtime, the Grok model list, Home mode) apply exposure and the caller key's policy too.
func TestModelAccessOnRoutesOutsideTheExecutionPath(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("route-access-codex", "codex", []*registry.ModelInfo{
		{ID: "route-visible", DisplayName: "Visible"},
		{ID: "route-hidden", DisplayName: "Hidden"},
	})
	t.Cleanup(func() { reg.UnregisterClient("route-access-codex") })

	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{
		APIKeys: []string{"test-key", "limited-key"},
		APIKeyPolicies: []sdkconfig.APIKeyPolicy{
			{Key: "limited-key", Restricted: true, Allow: []string{"codex/route-visible"}},
		},
	}}
	server := newTestServerWithConfig(t, cfg)
	do := func(method, path, key, body string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		return rec
	}
	ws := map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}

	for _, tc := range []struct {
		name, method, path, body string
		headers                  map[string]string
	}{
		{"alpha search", http.MethodPost, "/v1/alpha/search", `{"model":"route-hidden"}`, nil},
		{"alpha search without model", http.MethodPost, "/v1/alpha/search", `{}`, nil},
		{"live call", http.MethodPost, "/v1/live", `{"model":"gpt-live-1-codex"}`, nil},
		{"realtime call default model", http.MethodPost, "/v1/realtime", `{}`, nil},
		{"realtime websocket", http.MethodGet, "/v1/realtime?model=gpt-realtime", "", ws},
		{"realtime client secret", http.MethodPost, "/v1/realtime/client_secrets", `{"session":{"type":"realtime","model":"gpt-realtime"}}`, nil},
	} {
		if rec := do(tc.method, tc.path, "limited-key", tc.body, tc.headers); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403; body=%s", tc.name, rec.Code, rec.Body.String())
		}
	}

	grokUA := map[string]string{"User-Agent": "grok-shell/0.2.119 (macos; aarch64)"}
	body := do(http.MethodGet, "/v1/models", "limited-key", "", grokUA).Body.String()
	if !strings.Contains(body, "route-visible") || strings.Contains(body, "route-hidden") {
		t.Errorf("grok model list for a restricted key = %s", body)
	}
	if rec := do(http.MethodGet, "/v1/models/route-hidden", "limited-key", "", grokUA); rec.Code != http.StatusNotFound {
		t.Errorf("grok model detail of a disallowed model: status %d, want 404", rec.Code)
	}

	// Home mode cannot resolve a model's provider locally, so with filters active its model lists
	// refuse explicitly instead of skipping the filters.
	server.cfg.Home.Enabled = true
	engine := gin.New()
	engine.GET("/v1/models", func(c *gin.Context) {
		c.Request = c.Request.WithContext(coreusage.WithAPIKey(c.Request.Context(), "limited-key"))
		server.unifiedModelsHandler(nil, nil)(c)
	})
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not supported in Home mode") {
		t.Errorf("Home model list with key policies: status %d body %s, want explicit 503", rec.Code, rec.Body.String())
	}
}
