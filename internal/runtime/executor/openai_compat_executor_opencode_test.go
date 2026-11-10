package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOpenAICompatExecutor_OpencodeWireRouting(t *testing.T) {
	var hitPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPaths = append(hitPaths, r.URL.Path)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/responses") {
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","model":"gpt-6-luna","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok-responses"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok-chat"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("opencode", &config.Config{})
	auth := &coreauth.Auth{
		ID:       "opencode-test",
		Provider: "opencode",
		Attributes: map[string]string{
			"base_url": server.URL + "/zen/go/v1",
			"api_key":  "oc-test-key",
		},
	}

	// 1. Standard OpenCode chat model (glm-5.3) -> /chat/completions
	_, err := exec.Execute(context.Background(), auth, coreexecutor.Request{
		Model:   "glm-5.3",
		Payload: []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`),
	}, coreexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
	})
	if err != nil {
		t.Fatalf("Execute(glm-5.3) error: %v", err)
	}

	// 2. Responses-only OpenCode model (gpt-6-luna) -> /responses even when client sends Chat Completions
	_, err = exec.Execute(context.Background(), auth, coreexecutor.Request{
		Model:   "gpt-6-luna",
		Payload: []byte(`{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`),
	}, coreexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
	})
	if err != nil {
		t.Fatalf("Execute(gpt-6-luna) error: %v", err)
	}

	// 3. Responses-only OpenCode model (grok-4.7) -> /responses
	_, err = exec.Execute(context.Background(), auth, coreexecutor.Request{
		Model:   "grok-4.7",
		Payload: []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hi"}]}`),
	}, coreexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
	})
	if err != nil {
		t.Fatalf("Execute(grok-4.7) error: %v", err)
	}

	// 4. Dual-protocol OpenCode model (deepseek-v4-pro) from FormatOpenAIResponse -> /responses
	_, err = exec.Execute(context.Background(), auth, coreexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","input":"hi"}`),
	}, coreexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
	})
	if err != nil {
		t.Fatalf("Execute(deepseek-v4-pro responses) error: %v", err)
	}

	wantPaths := []string{
		"/zen/go/v1/chat/completions",
		"/zen/go/v1/responses",
		"/zen/go/v1/responses",
		"/zen/go/v1/responses",
	}
	if len(hitPaths) != len(wantPaths) {
		t.Fatalf("hitPaths = %v, want %v", hitPaths, wantPaths)
	}
	for i, want := range wantPaths {
		if hitPaths[i] != want {
			t.Fatalf("hitPaths[%d] = %q, want %q", i, hitPaths[i], want)
		}
	}
}
