package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func newMultiModelComboTestHandler(
	t *testing.T,
	models []string,
	executor *modelExecutionCaptureExecutor,
	cfg *sdkconfig.SDKConfig,
) *BaseAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "combo-test-auth",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": "combo@example.com"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("manager.Register(): %v", errRegister)
	}
	modelInfos := make([]*registry.ModelInfo, 0, len(models))
	for _, m := range models {
		modelInfos = append(modelInfos, &registry.ModelInfo{ID: m})
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, modelInfos)
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	return NewBaseAPIHandlers(cfg, manager)
}

func TestComboExecution_FallbackPreservesPrimaryAndFallsBackOnError(t *testing.T) {
	models := []string{"combo-primary-model", "combo-secondary-model", "combo-tertiary-model"}
	var mu sync.Mutex
	var attemptedModels []string
	var payloadModels []string
	failPrimary := false

	executor := &modelExecutionCaptureExecutor{
		provider: "codex",
		execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			mu.Lock()
			attemptedModels = append(attemptedModels, req.Model)
			payloadModels = append(payloadModels, ojson.GetBytesNoCopy(req.Payload, "model").String())
			shouldFail := failPrimary && req.Model == "combo-primary-model"
			mu.Unlock()
			if shouldFail {
				return coreexecutor.Response{}, &coreauth.Error{
					Code:       "rate_limit_exceeded",
					Message:    "primary quota exhausted",
					HTTPStatus: http.StatusTooManyRequests,
				}
			}
			return coreexecutor.Response{
				Payload: []byte(fmt.Sprintf(`{"model":%q,"ok":true}`, req.Model)),
			}, nil
		},
	}

	cfg := &sdkconfig.SDKConfig{
		Combos: []sdkconfig.ComboConfig{
			{
				Name:     "coding-combo",
				Strategy: "fallback",
				Models:   models,
			},
		},
	}
	handler := newMultiModelComboTestHandler(t, models, executor, cfg)

	// 1. When healthy, 3 consecutive turns MUST all hit combo-primary-model (0 rotation, 100% prompt cache preservation).
	for i := 0; i < 3; i++ {
		body, _, errMsg := handler.ExecuteWithAuthManager(
			context.Background(),
			"openai",
			"coding-combo",
			[]byte(`{"model":"coding-combo","messages":[{"role":"user","content":"hello"}]}`),
			"",
		)
		if errMsg != nil {
			t.Fatalf("turn %d unexpected error: %+v", i, errMsg)
		}
		if got := ojson.GetBytesNoCopy(body, "model").String(); got != "combo-primary-model" {
			t.Fatalf("turn %d response model = %q, want combo-primary-model", i, got)
		}
	}

	mu.Lock()
	if len(attemptedModels) != 3 {
		t.Fatalf("attemptedModels = %v, want 3 calls to combo-primary-model", attemptedModels)
	}
	for i, m := range attemptedModels {
		if m != "combo-primary-model" || payloadModels[i] != "combo-primary-model" {
			t.Fatalf("turn %d attempted=%q payloadModel=%q, want combo-primary-model", i, m, payloadModels[i])
		}
	}
	attemptedModels = nil
	payloadModels = nil
	failPrimary = true
	mu.Unlock()

	// 2. When primary fails with 429, fallback tries primary (#1) then succeeds on secondary (#2).
	body, _, errMsg := handler.ExecuteWithAuthManager(
		context.Background(),
		"openai",
		"CODING-COMBO", // case-insensitive match
		[]byte(`{"model":"CODING-COMBO","messages":[{"role":"user","content":"hello"}]}`),
		"",
	)
	if errMsg != nil {
		t.Fatalf("fallback turn unexpected error: %+v", errMsg)
	}
	if got := ojson.GetBytesNoCopy(body, "model").String(); got != "combo-secondary-model" {
		t.Fatalf("fallback response model = %q, want combo-secondary-model", got)
	}

	mu.Lock()
	wantAttempted := []string{"combo-primary-model", "combo-secondary-model"}
	if len(attemptedModels) != len(wantAttempted) {
		t.Fatalf("attemptedModels = %v, want %v", attemptedModels, wantAttempted)
	}
	for i, want := range wantAttempted {
		if attemptedModels[i] != want || payloadModels[i] != want {
			t.Fatalf("step %d attempted=%q payload=%q, want %q", i, attemptedModels[i], payloadModels[i], want)
		}
	}
	mu.Unlock()
}

func TestComboExecution_StreamFallbackWhenCandidateFailsToOpen(t *testing.T) {
	models := []string{"stream-model-1", "stream-model-2"}
	var mu sync.Mutex
	var attempted []string

	executor := &modelExecutionCaptureExecutor{
		provider: "codex",
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			mu.Lock()
			attempted = append(attempted, ojson.GetBytesNoCopy(req.Payload, "model").String())
			mu.Unlock()
			if req.Model == "stream-model-1" {
				return nil, &coreauth.Error{
					Code:       "service_unavailable",
					Message:    "upstream overloaded",
					HTTPStatus: http.StatusServiceUnavailable,
				}
			}
			ch := make(chan coreexecutor.StreamChunk, 1)
			ch <- coreexecutor.StreamChunk{Payload: []byte(`data: {"id":"1","model":"stream-model-2"}` + "\n\n")}
			close(ch)
			return &coreexecutor.StreamResult{
				Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
				Chunks:  ch,
			}, nil
		},
	}

	cfg := &sdkconfig.SDKConfig{
		Combos: []sdkconfig.ComboConfig{
			{
				Name:   "stream-combo",
				Models: models,
			},
		},
	}
	handler := newMultiModelComboTestHandler(t, models, executor, cfg)

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(
		context.Background(),
		"openai",
		"stream-combo",
		[]byte(`{"model":"stream-combo","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		"",
	)
	if dataChan == nil {
		var errMsg any
		if errChan != nil {
			errMsg = <-errChan
		}
		t.Fatalf("expected non-nil dataChan after fallback to stream-model-2, got error: %+v", errMsg)
	}
	var chunks []string
	for chunk := range dataChan {
		chunks = append(chunks, string(chunk))
	}
	if len(chunks) == 0 {
		t.Fatal("expected streamed chunk from stream-model-2")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attempted) != 2 || attempted[0] != "stream-model-1" || attempted[1] != "stream-model-2" {
		t.Fatalf("attempted stream models = %v, want [stream-model-1 stream-model-2]", attempted)
	}
}

func TestComboExecution_RoundRobinAndRecursionGuard(t *testing.T) {
	models := []string{"rr-model-1", "rr-model-2"}
	var mu sync.Mutex
	var hitOrder []string

	executor := &modelExecutionCaptureExecutor{
		provider: "codex",
		execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			mu.Lock()
			hitOrder = append(hitOrder, req.Model)
			mu.Unlock()
			return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
		},
	}

	cfg := &sdkconfig.SDKConfig{
		Combos: []sdkconfig.ComboConfig{
			{
				Name:     "rr-combo",
				Strategy: "round-robin",
				Models:   models,
			},
			{
				Name:   "cycle-a",
				Models: []string{"cycle-b"},
			},
			{
				Name:   "cycle-b",
				Models: []string{"cycle-a", "rr-model-1"},
			},
		},
	}
	handler := newMultiModelComboTestHandler(t, models, executor, cfg)

	for i := 0; i < 2; i++ {
		_, _, errMsg := handler.ExecuteWithAuthManager(
			context.Background(),
			"openai",
			"rr-combo",
			[]byte(`{"model":"rr-combo"}`),
			"",
		)
		if errMsg != nil {
			t.Fatalf("rr turn %d error: %+v", i, errMsg)
		}
	}

	mu.Lock()
	if len(hitOrder) != 2 || hitOrder[0] == hitOrder[1] {
		t.Fatalf("round-robin hitOrder = %v, expected rotation between rr-model-1 and rr-model-2", hitOrder)
	}
	hitOrder = nil
	mu.Unlock()

	// Nested cycle-a -> cycle-b -> cycle-a (skipped by recursion guard) -> rr-model-1 (succeeds!)
	_, _, errMsg := handler.ExecuteWithAuthManager(
		context.Background(),
		"openai",
		"cycle-a",
		[]byte(`{"model":"cycle-a"}`),
		"",
	)
	if errMsg != nil {
		t.Fatalf("nested combo with cycle guard failed: %+v", errMsg)
	}

	// AppendComboModels exposes enabled combos in openai, claude, and gemini model lists.
	openaiModels := handler.AppendComboModels("openai", nil)
	if len(openaiModels) != 3 {
		t.Fatalf("len(openaiModels) = %d, want 3", len(openaiModels))
	}
	geminiModels := handler.AppendComboModels("gemini", nil)
	if len(geminiModels) != 3 || geminiModels[0]["name"] != "models/rr-combo" {
		t.Fatalf("geminiModels = %+v", geminiModels)
	}
}
