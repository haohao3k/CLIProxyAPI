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
// edits on v0 and v8 carry key policies along (rotation keeps the policy, removal drops it).
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
	router.PUT("/v8/management/config/*path", h.ConfigV8)
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
	send(http.MethodPut, "/v8/management/config/access/api-keys", `["key-a2"]`, http.StatusOK) // no change: no sync
	send(http.MethodPut, "/v8/management/config/access/api-keys", `["key-a3"]`, http.StatusOK)

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
	if len(loaded.APIKeyPolicies) != 1 || loaded.APIKeyPolicies[0].Key != "key-a3" || !loaded.APIKeyPolicies[0].Restricted {
		t.Fatalf("key policies after rotate+delete = %+v, want only key-a3", loaded.APIKeyPolicies)
	}
}

// Emptying a list, a map entry or a whole section through the management API must reach the file:
// the merge-on-save keeps nodes the generated document omits, so these would otherwise come back
// on the next reload.
func TestManagementWritesOfEmptiedValuesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `config-version: 8
access:
  api-keys: [key-a]
  key-policies:
    - {key: key-a, restricted: true, allow: [combo:c1]}
model-exposure:
  enabled: true
  providers:
    zcode-proxy: {prefix: zp, mode: all, deny: [kimi-k3-fast]}
    cursor: {prefix: cu, mode: all}
cursor-model-aliases:
  - {alias: ds, variants: {high: cursor-ds-high}}
combos:
  - {name: c1, models: [m1]}
`
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
	router.PUT("/cursor-model-aliases", h.PutCursorModelAliases)
	router.PUT("/api-key-policies", h.PutAPIKeyPolicies)
	router.PUT("/combos", h.PutCombos)
	for url, body := range map[string]string{
		"/model-exposure":       `{"model-exposure":{"enabled":true,"providers":{"zcode-proxy":{"prefix":"zp","mode":"all","deny":[]}}}}`,
		"/cursor-model-aliases": `{"cursor-model-aliases":[]}`,
		"/api-key-policies":     `{"key-policies":[{"key":"key-a","restricted":true,"allow":[]}]}`,
		"/combos":               `[]`,
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, url, strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", url, rec.Code, rec.Body.String())
		}
	}

	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	data, _ := os.ReadFile(path)
	if p := loaded.ModelExposure.Providers; len(p) != 1 || len(p["zcode-proxy"].Deny) != 0 {
		t.Fatalf("providers after emptying deny and removing cursor = %+v\n%s", p, data)
	}
	if len(loaded.CursorModelAliases) != 0 || len(loaded.Combos) != 0 {
		t.Fatalf("aliases=%+v combos=%+v, want none\n%s", loaded.CursorModelAliases, loaded.Combos, data)
	}
	if len(loaded.APIKeyPolicies) != 1 || len(loaded.APIKeyPolicies[0].Allow) != 0 || !loaded.APIKeyPolicies[0].Restricted {
		t.Fatalf("policies = %+v, want key-a restricted with empty allow\n%s", loaded.APIKeyPolicies, data)
	}

	// Whole lists and sections emptied: a fresh load of the file must see none of them.
	for url, body := range map[string]string{
		"/model-exposure":   `{"model-exposure":{"enabled":false,"providers":{}}}`,
		"/api-key-policies": `{"key-policies":[]}`,
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, url, strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", url, rec.Code, rec.Body.String())
		}
	}
	loaded, err = config.LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	data, _ = os.ReadFile(path)
	if loaded.ModelExposure.Enabled || len(loaded.ModelExposure.Providers) != 0 || len(loaded.APIKeyPolicies) != 0 {
		t.Fatalf("after emptying whole sections: exposure=%+v policies=%+v\n%s", loaded.ModelExposure, loaded.APIKeyPolicies, data)
	}
}
