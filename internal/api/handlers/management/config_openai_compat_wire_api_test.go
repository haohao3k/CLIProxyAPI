package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func patchOpenAICompatForTest(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/openai-compatibility", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchOpenAICompat(ctx)
	return rec
}

func TestPatchOpenAICompatWireAPI(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "compat",
			BaseURL: "https://api.example.com/v1",
		}},
	}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	if rec := patchOpenAICompatForTest(t, h, `{"index":0,"value":{"wire-api":" Responses "}}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := cfg.OpenAICompatibility[0].WireAPI; got != config.OpenAICompatWireAPIResponses {
		t.Fatalf("WireAPI = %q, want responses", got)
	}

	rec := patchOpenAICompatForTest(t, h, `{"index":0,"value":{"wire-api":"grpc"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid wire-api status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := cfg.OpenAICompatibility[0].WireAPI; got != config.OpenAICompatWireAPIResponses {
		t.Fatalf("rejected update changed WireAPI to %q", got)
	}

	getRec := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRec)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)
	h.GetOpenAICompat(getCtx)
	var body struct {
		OpenAICompatibility []struct {
			WireAPI string `json:"wire-api"`
		} `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if len(body.OpenAICompatibility) != 1 || body.OpenAICompatibility[0].WireAPI != config.OpenAICompatWireAPIResponses {
		t.Fatalf("GET wire-api = %+v, want responses", body.OpenAICompatibility)
	}

	if rec = patchOpenAICompatForTest(t, h, `{"index":0,"value":{"wire-api":"chat"}}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := cfg.OpenAICompatibility[0].WireAPI; got != "" {
		t.Fatalf("WireAPI = %q, want default chat (empty)", got)
	}
}
