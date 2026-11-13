package openai

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Single-model metadata lookups apply the caller key's policy: a restricted key gets 404 for a
// model it cannot call, on /v1/models/<id> and /v1beta/models/<name>.
func TestSingleModelLookupsHonourKeyPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("detail-access-auth", "codex", []*registry.ModelInfo{
		{ID: "detail-allowed", Object: "model", OwnedBy: "codex"},
		{ID: "detail-denied", Object: "model", OwnedBy: "codex"},
	})
	t.Cleanup(func() { reg.UnregisterClient("detail-access-auth") })
	cfg := &config.SDKConfig{APIKeyPolicies: []config.APIKeyPolicy{
		{Key: "detail-key", Restricted: true, Allow: []string{"codex/detail-allowed"}},
	}}
	base := handlers.NewBaseAPIHandlers(cfg, nil)
	openaiHandler := NewOpenAIAPIHandler(base)
	geminiHandler := gemini.NewGeminiAPIHandler(base)

	serve := func(path string, handle func(*gin.Context), detail string) int {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		c.Request = req.WithContext(coreusage.WithAPIKey(req.Context(), "detail-key"))
		if detail != "" {
			c.Set(handlers.ModelDetailIDContextKey, detail)
		} else {
			c.Params = gin.Params{{Key: "action", Value: "/" + path[len("/v1beta/models/"):]}}
		}
		handle(c)
		return rec.Code
	}
	for model, want := range map[string]int{"detail-allowed": http.StatusOK, "detail-denied": http.StatusNotFound} {
		if got := serve("/v1/models/"+model, openaiHandler.OpenAIModels, model); got != want {
			t.Errorf("/v1/models/%s = %d, want %d", model, got, want)
		}
		if got := serve("/v1beta/models/"+model, geminiHandler.GeminiGetHandler, ""); got != want {
			t.Errorf("/v1beta/models/%s = %d, want %d", model, got, want)
		}
	}
}
