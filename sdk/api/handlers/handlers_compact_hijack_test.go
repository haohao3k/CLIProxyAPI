package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"golang.org/x/net/context"
)

func TestMaybeHijackClientCompaction_ReroutesCodexCompactToExternalCombo(t *testing.T) {
	enabled := true
	sdkCfg := &config.SDKConfig{
		CodexCompactHijack:      &enabled,
		CodexCompactHijackModel: "deepseek-v4.1-flash",
		Combos: []config.ComboConfig{
			{
				Name:   "astra-main",
				Models: []string{"codex/gpt-6-astra"},
			},
		},
	}
	h := NewBaseAPIHandlers(sdkCfg, nil)

	largeConv := "<conversation>" + strings.Repeat("turn line\n", 35000) + "</conversation>"
	payload, _ := json.Marshal(map[string]any{
		"model":        "astra-main",
		"instructions": "You are a context summarization assistant. Summarize the conversation concisely.",
		"input": []map[string]any{
			{
				"type":    "message",
				"role":    "user",
				"content": "Summarize this:\n" + largeConv,
			},
		},
	})

	newModel, newRaw := h.maybeHijackClientCompaction(context.Background(), "astra-main", payload, modelExecutionOptions{})
	if newModel != "deepseek-v4.1-flash" {
		t.Fatalf("expected model rerouted to deepseek-v4.1-flash, got %q", newModel)
	}
	var parsed map[string]any
	if err := json.Unmarshal(newRaw, &parsed); err != nil {
		t.Fatalf("unmarshal rewritten payload: %v", err)
	}
	if parsed["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("expected payload model=deepseek-v4.1-flash, got %v", parsed["model"])
	}
	inp := parsed["input"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(inp, "omitted for summarizer budget") {
		t.Fatalf("expected oversized <conversation> tag bounded, got len=%d", len(inp))
	}

	// Normal tool-carrying coding turn must NOT be hijacked even if instructions mention summarization
	normalPayload := []byte(`{
		"model": "gpt-6-astra",
		"instructions": "You are a context summarization assistant",
		"tools": [{"type": "function", "name": "bash", "parameters": {"type": "object"}}],
		"input": [{"type": "message", "role": "user", "content": "run ls"}]
	}`)
	gotModel, _ := h.maybeHijackClientCompaction(context.Background(), "gpt-6-astra", normalPayload, modelExecutionOptions{})
	if gotModel != "gpt-6-astra" {
		t.Fatalf("expected tool-carrying request NOT hijacked, got %q", gotModel)
	}
}
