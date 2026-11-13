package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The management writes persist to config.yaml in the v8 layout and survive a reload, and api-keys
// edits carry key policies along (rotation keeps the policy, removal drops it).
func TestModelExposureManagementRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\naccess:\n  api-keys:\n    - key-a\n    - key-b\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) })
	router.PUT("/model-exposure", h.PutModelExposure)
	router.GET("/model-exposure", h.GetModelExposure)
	router.PUT("/cursor-model-aliases", h.PutCursorModelAliases)
	router.PUT("/api-key-policies", h.PutAPIKeyPolicies)
	router.PATCH("/api-key-policies", h.PatchAPIKeyPolicy)
	router.PATCH("/api-keys", h.PatchAPIKeys)
	router.DELETE("/api-keys", h.DeleteAPIKeys)
	send := func(method, url, body string, want int) string {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, url, strings.NewReader(body)))
		if rec.Code != want {
			t.Fatalf("%s %s: status=%d body=%s", method, url, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	send(http.MethodPut, "/model-exposure", `{"model-exposure":{"enabled":true,"list-prefixed-only":true,"accept-bare-ids":false,
		"providers":{"cursor":{"prefix":"cu","mode":"allowlist","allow":["claude-*"],"deny":["*-fast"]}}}}`, http.StatusOK)
	send(http.MethodPut, "/cursor-model-aliases", `{"cursor-model-aliases":[{"alias":"ds","default-effort":"medium",
		"variants":{"low":"cursor-ds-low","high":"cursor-ds-high"}}]}`, http.StatusOK)
	send(http.MethodPut, "/api-key-policies", `{"key-policies":[{"key":"key-a","restricted":true,"allow":["cu/*"]}]}`, http.StatusOK)
	send(http.MethodPatch, "/api-key-policies", `{"key":"key-b","restricted":true,"allow":["combo:c1"]}`, http.StatusOK)
	send(http.MethodPatch, "/api-key-policies", `{"key":"not-a-client-key","restricted":false}`, http.StatusBadRequest)

	if got := send(http.MethodGet, "/model-exposure", "", http.StatusOK); !strings.Contains(got, `"prefix":"cu"`) || !strings.Contains(got, `"accept-bare-ids":false`) {
		t.Fatalf("GET model-exposure = %s", got)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "key-policies:") || strings.Contains(string(data), "api-key-policies:") {
		t.Fatalf("policies not saved under access.key-policies:\n%s", data)
	}

	send(http.MethodPatch, "/api-keys", `{"old":"key-a","new":"key-a2"}`, http.StatusOK)
	send(http.MethodDelete, "/api-keys?value=key-b", "", http.StatusOK)

	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	cursor := loaded.ModelExposure.Providers["cursor"]
	if !loaded.ModelExposure.Enabled || !loaded.ModelExposure.ListPrefixedOnly || loaded.ModelExposure.AcceptBareIDs ||
		cursor.Prefix != "cu" || cursor.Mode != "allowlist" || len(cursor.Allow) != 1 || len(cursor.Deny) != 1 {
		t.Fatalf("model-exposure after reload = %+v", loaded.ModelExposure)
	}
	if len(loaded.CursorModelAliases) != 1 || loaded.CursorModelAliases[0].Variants["high"] != "cursor-ds-high" {
		t.Fatalf("cursor-model-aliases after reload = %+v", loaded.CursorModelAliases)
	}
	if len(loaded.APIKeyPolicies) != 1 || loaded.APIKeyPolicies[0].Key != "key-a2" || !loaded.APIKeyPolicies[0].Restricted {
		t.Fatalf("key policies after rotate+delete = %+v, want only key-a2", loaded.APIKeyPolicies)
	}
}
