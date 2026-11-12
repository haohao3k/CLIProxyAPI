package executor

import (
	"bytes"
	"context"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestNormalizeCursorUpstreamModel(t *testing.T) {
	for in, want := range map[string]string{
		"cursor-claude-4-sonnet":       "claude-4-sonnet",
		"CURSOR-gpt-5":                 "gpt-5",
		"gpt-5":                        "gpt-5",
		"cursor-":                      "cursor-",
		"cursor-claude-4-sonnet(high)": "claude-4-sonnet",
	} {
		if got := normalizeCursorUpstreamModel(in); got != want {
			t.Fatalf("normalizeCursorUpstreamModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCursorMessagesFromOpenAI(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"b"}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"1"}]},
		{"content":"no role"}
	]}`)
	msgs := cursorMessagesFromOpenAI(body)
	if len(msgs) != 3 {
		t.Fatalf("len = %d, msgs = %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "system" || msgs[0].Content != "sys" {
		t.Fatalf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Content != "a\nb" {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
	if msgs[2].Role != "user" || msgs[2].Content != "no role" {
		t.Fatalf("msg2 = %+v", msgs[2])
	}
}

func TestBuildOpenAIStreamChunkAndCompletion(t *testing.T) {
	chunk := buildOpenAIStreamChunk("cursor-gpt-5", "r", "t", nil)
	if !bytes.HasPrefix(chunk, []byte("data: ")) {
		t.Fatalf("missing data prefix: %s", chunk)
	}
	payload := chunk[len("data: "):]
	if ojson.GetBytes(payload, "choices.0.delta.content").String() != "t" ||
		ojson.GetBytes(payload, "choices.0.delta.reasoning_content").String() != "r" ||
		ojson.GetBytes(payload, "model").String() != "cursor-gpt-5" {
		t.Fatalf("unexpected chunk: %s", payload)
	}
	final := buildOpenAIStreamChunk("m", "", "", []byte(`{"prompt_tokens":1}`))[len("data: "):]
	if ojson.GetBytes(final, "choices.0.finish_reason").String() != "stop" || ojson.GetBytes(final, "usage.prompt_tokens").Int() != 1 {
		t.Fatalf("unexpected final chunk: %s", final)
	}

	completion := buildOpenAICompletion("m", "why", "answer", []byte(`{"total_tokens":3}`))
	if ojson.GetBytes(completion, "choices.0.message.content").String() != "answer" ||
		ojson.GetBytes(completion, "choices.0.message.reasoning_content").String() != "why" ||
		ojson.GetBytes(completion, "usage.total_tokens").Int() != 3 {
		t.Fatalf("unexpected completion: %s", completion)
	}
}

func TestCursorCredsPrefersAccessToken(t *testing.T) {
	if got := cursorCreds(nil); got != "" {
		t.Fatalf("nil auth creds = %q", got)
	}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"cookie": "u%3A%3Atok", "access_token": "tok"}}
	if got := cursorCreds(auth); got != "tok" {
		t.Fatalf("creds = %q", got)
	}
	auth = &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "k"}}
	if got := cursorCreds(auth); got != "k" {
		t.Fatalf("attr creds = %q", got)
	}
}

func TestCursorBuildUpstreamRequestAppliesPayloadRulesLast(t *testing.T) {
	cfg := &config.Config{}
	cfg.Payload.Override = []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "*", Protocol: "openai"}},
		Params: map[string]any{"messages.0.content": "patched by rule"},
	}}
	exec := NewCursorExecutor(cfg)
	req := cliproxyexecutor.Request{Model: "cursor-claude-4-sonnet", Payload: []byte(`{"model":"cursor-claude-4-sonnet","messages":[{"role":"user","content":"hi"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
	body, native, err := exec.buildUpstreamRequest(context.Background(), req, opts, true)
	if err != nil {
		t.Fatalf("buildUpstreamRequest() error = %v", err)
	}
	if got := ojson.GetBytes(body, "messages.0.content").String(); got != "patched by rule" {
		t.Fatalf("payload override not applied, content = %q", got)
	}
	if got := ojson.GetBytes(body, "model").String(); got != "claude-4-sonnet" {
		t.Fatalf("model = %q, want normalized upstream id", got)
	}
	if !bytes.Contains(native, []byte("patched by rule")) || bytes.Contains(native, []byte(`"hi"`)) {
		t.Fatalf("native body must be encoded from the final payload")
	}
	if !bytes.Contains(native, []byte("claude-4-sonnet")) || bytes.Contains(native, []byte("cursor-claude")) {
		t.Fatalf("native body must carry the normalized model id")
	}
}

func TestCursorBuildUpstreamPlanResolvesEffortVariants(t *testing.T) {
	exec := NewCursorExecutor(&config.Config{})

	// 1. Base family model + Responses API reasoning.effort="high" -> deepseek-v41-flash-1017-high
	req := cliproxyexecutor.Request{
		Model:   "cursor-deepseek-v41-flash-1017",
		Payload: []byte(`{"model":"cursor-deepseek-v41-flash-1017","reasoning":{"effort":"high"},"input":[{"role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	plan, err := exec.buildUpstreamPlan(context.Background(), req, opts, true)
	if err != nil {
		t.Fatalf("buildUpstreamPlan() error = %v", err)
	}
	if !plan.useAgent {
		t.Fatalf("expected AgentService path for text request")
	}
	if plan.upstreamModel != "deepseek-v41-flash-1017-high" {
		t.Fatalf("upstreamModel = %q, want deepseek-v41-flash-1017-high", plan.upstreamModel)
	}
	if !bytes.Contains(plan.agentFrame, []byte("deepseek-v41-flash-1017-high")) {
		t.Fatalf("agentFrame missing resolved variant id")
	}

	// 2. Explicit variant in model name wins over default payload reasoning.effort="medium"
	reqExplicit := cliproxyexecutor.Request{
		Model:   "cursor-deepseek-v41-flash-1017-max",
		Payload: []byte(`{"model":"cursor-deepseek-v41-flash-1017-max","reasoning":{"effort":"medium"},"input":[{"role":"user","content":"hello"}]}`),
	}
	planExplicit, err := exec.buildUpstreamPlan(context.Background(), reqExplicit, opts, true)
	if err != nil {
		t.Fatalf("buildUpstreamPlan(explicit) error = %v", err)
	}
	if planExplicit.upstreamModel != "deepseek-v41-flash-1017-max" {
		t.Fatalf("explicit variant overridden: got %q, want deepseek-v41-flash-1017-max", planExplicit.upstreamModel)
	}
}
