package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	mistralauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/mistral"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type fakeMistralOAuthService struct {
	startErr error
	waitErr  error
	apiKey   string
}

func (f *fakeMistralOAuthService) StartSignIn(context.Context) (*mistralauth.SignInSession, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &mistralauth.SignInSession{
		ProcessID:    "proc-1",
		SignInURL:    "https://console.mistral.ai/codestral/cli/authenticate?process_id=proc-1",
		PollURL:      "https://console.mistral.ai/api/vibe/sign-in/poll/proc-1",
		ExpiresAt:    time.Now().Add(10 * time.Minute),
		Interval:     time.Millisecond,
		CodeVerifier: "verifier",
	}, nil
}

func (f *fakeMistralOAuthService) WaitForAPIKey(context.Context, *mistralauth.SignInSession) (*mistralauth.AuthBundle, error) {
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	return &mistralauth.AuthBundle{
		APIKey:     f.apiKey,
		SignedInAt: time.Now(),
		Account:    &mistralauth.AccountInfo{PlanName: "Pro"},
	}, nil
}

func newMistralOAuthTestRouter(t *testing.T, svc mistralOAuthService) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	orig := newMistralOAuthService
	newMistralOAuthService = func(*config.Config, string) mistralOAuthService { return svc }
	t.Cleanup(func() { newMistralOAuthService = orig })

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, Port: 8317}, coreauth.NewManager(nil, nil, nil))
	router := gin.New()
	router.GET("/v0/management/mistral-auth-url", h.RequestMistralToken)
	router.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	router.GET("/v8/management/oauth/status", h.GetAuthStatus)
	return router, authDir
}

func startMistralOAuth(t *testing.T, router *gin.Engine, path string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d: %s", path, w.Code, w.Body.String())
	}
	var start map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	return start
}

func TestRequestMistralTokenSavesMistralAuthFile(t *testing.T) {
	router, authDir := newMistralOAuthTestRouter(t, &fakeMistralOAuthService{apiKey: "minted-key"})

	for _, path := range []string{"/v0/management/mistral-auth-url", "/v8/management/oauth/auth-url?provider=mistral"} {
		start := startMistralOAuth(t, router, path)
		if start["url"] != "https://console.mistral.ai/codestral/cli/authenticate?process_id=proc-1" || start["flow"] != "device" {
			t.Fatalf("%s start = %#v", path, start)
		}
		state, _ := start["state"].(string)
		if st := pollOAuthStatusUntilDone(t, router, state); st["status"] != "ok" {
			t.Fatalf("%s status = %#v, want ok", path, st)
		}
	}

	raw, err := os.ReadFile(filepath.Join(authDir, mistralauth.CredentialFileName("minted-key")))
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	var saved map[string]any
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["type"] != "mistral" || saved["api_key"] != "minted-key" || saved["auth_method"] != mistralauth.AuthMethodBrowserSignIn || saved["plan_name"] != "Pro" {
		t.Fatalf("saved auth = %#v", saved)
	}
}

func TestRequestMistralTokenReportsFailures(t *testing.T) {
	router, _ := newMistralOAuthTestRouter(t, &fakeMistralOAuthService{waitErr: mistralauth.ErrAccessDenied})
	start := startMistralOAuth(t, router, "/v0/management/mistral-auth-url")
	state, _ := start["state"].(string)
	if st := pollOAuthStatusUntilDone(t, router, state); st["status"] != "error" {
		t.Fatalf("status = %#v, want error", st)
	}

	routerStartFail, _ := newMistralOAuthTestRouter(t, &fakeMistralOAuthService{startErr: errors.New("boom")})
	w := httptest.NewRecorder()
	routerStartFail.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/management/mistral-auth-url", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("start failure status = %d", w.Code)
	}
}

func TestNormalizeOAuthProviderMistral(t *testing.T) {
	for _, in := range []string{"mistral", " Mistral ", "mistral-vibe"} {
		if got, err := NormalizeOAuthProvider(in); err != nil || got != "mistral" {
			t.Fatalf("NormalizeOAuthProvider(%q) = %q, %v", in, got, err)
		}
	}
}
