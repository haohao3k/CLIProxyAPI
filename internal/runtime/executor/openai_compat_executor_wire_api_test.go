package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const (
	wireTestChatCompletion = `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"up-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	wireTestChatStream     = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"up-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"up-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	wireTestResponse = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"up-model","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`
)

func wireTestResponsesStream(withTerminal bool) string {
	var b strings.Builder
	b.WriteString("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"model\":\"up-model\",\"output\":[]}}\n\n")
	b.WriteString("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]}}\n\n")
	b.WriteString("event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"sequence_number\":2,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]}}\n\n")
	b.WriteString("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"ok\"}\n\n")
	b.WriteString("event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"sequence_number\":4,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"text\":\"ok\"}\n\n")
	b.WriteString("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":5,\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\",\"annotations\":[]}]}}\n\n")
	if withTerminal {
		b.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":6,\"response\":" + wireTestResponse + "}\n\n")
	}
	return b.String()
}

type wireTestCapture struct {
	mu    sync.Mutex
	paths []string
	body  []byte
}

func (c *wireTestCapture) record(path string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, path)
	c.body = body
}

func (c *wireTestCapture) last() (string, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.paths) == 0 {
		return "", nil
	}
	return c.paths[len(c.paths)-1], c.body
}

// newWireTestServer emulates an upstream exposing both Chat Completions and Responses.
func newWireTestServer(t *testing.T, capture *wireTestCapture, responsesTerminal bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.record(r.URL.Path, body)
		stream := ojson.GetBytes(body, "stream").Bool() || r.Header.Get("Accept") == "text/event-stream"
		switch r.URL.Path {
		case "/v1/chat/completions":
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wireTestChatStream)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, wireTestChatCompletion)
		case "/v1/responses":
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wireTestResponsesStream(responsesTerminal))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, wireTestResponse)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newWireTestExecutor(serverURL string, compat config.OpenAICompatibility) (*OpenAICompatExecutor, *cliproxyauth.Auth) {
	compat.Name = "wiretest"
	compat.BaseURL = serverURL + "/v1"
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{compat}}
	auth := &cliproxyauth.Auth{
		Provider: "wiretest",
		Attributes: map[string]string{
			"base_url":     serverURL + "/v1",
			"api_key":      "test-key",
			"compat_name":  "wiretest",
			"provider_key": "wiretest",
		},
	}
	return NewOpenAICompatExecutor("wiretest", cfg), auth
}

func collectWireTestStream(t *testing.T, result *cliproxyexecutor.StreamResult) (string, error) {
	t.Helper()
	var out strings.Builder
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		out.Write(chunk.Payload)
		out.WriteByte('\n')
	}
	return out.String(), streamErr
}

const (
	wireTestChatRequest      = `{"model":"up-model","messages":[{"role":"user","content":"hi"}]}`
	wireTestResponsesRequest = `{"model":"up-model","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"store":true,"metadata":{"k":"v"}}`
	wireTestClaudeRequest    = `{"model":"up-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
)

func TestOpenAICompatWireAPIChatDefaultUsesChatCompletions(t *testing.T) {
	capture := &wireTestCapture{}
	server := newWireTestServer(t, capture, true)
	executor, auth := newWireTestExecutor(server.URL, config.OpenAICompatibility{})

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestChatRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	path, body := capture.last()
	if path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", path)
	}
	if !ojson.GetBytes(body, "messages").Exists() {
		t.Fatalf("expected chat messages in upstream body: %s", body)
	}
	if got := ojson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("content = %q, payload %s", got, resp.Payload)
	}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestChatRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	if _, errStream := collectWireTestStream(t, result); errStream != nil {
		t.Fatalf("stream error: %v", errStream)
	}
	if path, _ = capture.last(); path != "/v1/chat/completions" {
		t.Fatalf("stream path = %q, want /v1/chat/completions", path)
	}
}

func TestOpenAICompatWireAPIResponsesChatClientNonStream(t *testing.T) {
	capture := &wireTestCapture{}
	server := newWireTestServer(t, capture, true)
	executor, auth := newWireTestExecutor(server.URL, config.OpenAICompatibility{WireAPI: "responses"})

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestChatRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	path, body := capture.last()
	if path != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", path)
	}
	if !ojson.GetBytes(body, "input").IsArray() || ojson.GetBytes(body, "messages").Exists() {
		t.Fatalf("expected Responses input body, got %s", body)
	}
	if ojson.GetBytes(body, "stream").Bool() {
		t.Fatalf("non-stream request must not set stream=true: %s", body)
	}
	if ojson.GetBytes(body, "stream_options").Exists() || ojson.GetBytes(body, "max_tokens").Exists() {
		t.Fatalf("chat-only fields leaked into Responses body: %s", body)
	}
	if ojson.GetBytes(body, "reasoning").Exists() {
		t.Fatalf("default reasoning must be dropped without client intent: %s", body)
	}
	if strings.Contains(string(body), "reasoning.encrypted_content") {
		t.Fatalf("encrypted reasoning include must be dropped without client intent: %s", body)
	}
	if got := ojson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("content = %q, payload %s", got, resp.Payload)
	}
	if got := ojson.GetBytes(resp.Payload, "object").String(); got != "chat.completion" {
		t.Fatalf("object = %q, payload %s", got, resp.Payload)
	}
}

func TestOpenAICompatWireAPIResponsesChatClientStream(t *testing.T) {
	capture := &wireTestCapture{}
	server := newWireTestServer(t, capture, true)
	executor, auth := newWireTestExecutor(server.URL, config.OpenAICompatibility{WireAPI: "responses"})

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestChatRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	out, errStream := collectWireTestStream(t, result)
	if errStream != nil {
		t.Fatalf("stream error: %v (output %s)", errStream, out)
	}
	path, body := capture.last()
	if path != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", path)
	}
	if !ojson.GetBytes(body, "stream").Bool() {
		t.Fatalf("stream request must set stream=true: %s", body)
	}
	if ojson.GetBytes(body, "stream_options").Exists() {
		t.Fatalf("stream_options.include_usage must not be sent to Responses: %s", body)
	}
	if !strings.Contains(out, `"content":"ok"`) {
		t.Fatalf("expected chat content delta, got %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("expected chat finish_reason, got %s", out)
	}
}

func TestOpenAICompatWireAPIResponsesResponsesClientPassthrough(t *testing.T) {
	capture := &wireTestCapture{}
	server := newWireTestServer(t, capture, true)
	executor, auth := newWireTestExecutor(server.URL, config.OpenAICompatibility{WireAPI: "responses"})

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestResponsesRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	path, body := capture.last()
	if path != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", path)
	}
	// Native Responses requests are forwarded without Codex request shaping.
	if !ojson.GetBytes(body, "store").Bool() {
		t.Fatalf("store must be preserved: %s", body)
	}
	if ojson.GetBytes(body, "metadata.k").String() != "v" {
		t.Fatalf("metadata must be preserved: %s", body)
	}
	if ojson.GetBytes(body, "instructions").Exists() || ojson.GetBytes(body, "include").Exists() {
		t.Fatalf("codex-specific fields must not be injected: %s", body)
	}
	if got := ojson.GetBytes(resp.Payload, "output.0.content.0.text").String(); got != "ok" {
		t.Fatalf("text = %q, payload %s", got, resp.Payload)
	}
	if got := ojson.GetBytes(resp.Payload, "object").String(); got != "response" {
		t.Fatalf("object = %q, payload %s", got, resp.Payload)
	}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestResponsesRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	out, errStream := collectWireTestStream(t, result)
	if errStream != nil {
		t.Fatalf("stream error: %v (output %s)", errStream, out)
	}
	if path, _ = capture.last(); path != "/v1/responses" {
		t.Fatalf("stream path = %q, want /v1/responses", path)
	}
	for _, want := range []string{`"type":"response.output_text.delta"`, `"type":"response.completed"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream output missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("native Responses stream must not synthesize [DONE]: %s", out)
	}
}

func TestOpenAICompatWireAPIResponsesClaudeClient(t *testing.T) {
	capture := &wireTestCapture{}
	server := newWireTestServer(t, capture, true)
	executor, auth := newWireTestExecutor(server.URL, config.OpenAICompatibility{WireAPI: "responses"})

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestClaudeRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if path, _ := capture.last(); path != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", path)
	}
	if got := ojson.GetBytes(resp.Payload, "content.0.text").String(); got != "ok" {
		t.Fatalf("claude text = %q, payload %s", got, resp.Payload)
	}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestClaudeRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	out, errStream := collectWireTestStream(t, result)
	if errStream != nil {
		t.Fatalf("stream error: %v (output %s)", errStream, out)
	}
	for _, want := range []string{"text_delta", "message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("claude stream missing %s: %s", want, out)
		}
	}
}

func TestOpenAICompatWireAPIResponsesStreamWithoutTerminalFails(t *testing.T) {
	capture := &wireTestCapture{}
	server := newWireTestServer(t, capture, false)
	executor, auth := newWireTestExecutor(server.URL, config.OpenAICompatibility{WireAPI: "responses"})

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestChatRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	if _, errStream := collectWireTestStream(t, result); errStream == nil {
		t.Fatal("expected truncated Responses stream error")
	}
}

func TestOpenAICompatWireAPIPerModelOverride(t *testing.T) {
	capture := &wireTestCapture{}
	server := newWireTestServer(t, capture, true)
	executor, auth := newWireTestExecutor(server.URL, config.OpenAICompatibility{
		Models: []config.OpenAICompatibilityModel{
			{Name: "up-model", Alias: "alias-model", WireAPI: "responses"},
			{Name: "chat-model", Alias: "chat-alias"},
		},
	})

	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestChatRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if path, _ := capture.last(); path != "/v1/responses" {
		t.Fatalf("per-model responses override path = %q", path)
	}

	chatReq := strings.Replace(wireTestChatRequest, "up-model", "chat-model", 1)
	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "chat-model", Payload: []byte(chatReq)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if path, _ := capture.last(); path != "/v1/chat/completions" {
		t.Fatalf("inherited chat wire path = %q", path)
	}
}

func TestOpenAICompatWireAPIResponsesKeepsClientReasoning(t *testing.T) {
	capture := &wireTestCapture{}
	server := newWireTestServer(t, capture, true)
	executor, auth := newWireTestExecutor(server.URL, config.OpenAICompatibility{WireAPI: "responses"})

	payload := `{"model":"up-model","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(payload)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	_, body := capture.last()
	if got := ojson.GetBytes(body, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, body %s", got, body)
	}
}

func TestOpenAICompatWireAPIAttributeFallback(t *testing.T) {
	capture := &wireTestCapture{}
	server := newWireTestServer(t, capture, true)
	executor := NewOpenAICompatExecutor("wiretest", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "wiretest",
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
			"api_key":  "test-key",
			"wire_api": "responses",
		},
	}
	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "up-model", Payload: []byte(wireTestChatRequest)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if path, _ := capture.last(); path != "/v1/responses" {
		t.Fatalf("attribute wire_api path = %q", path)
	}
}

// Payload rules are the final barrier for Responses-wire requests: the configured
// override must survive the reasoning cleanup, thinking suffix and stream shaping.
func TestOpenAICompatWireAPIResponsesPayloadRulesAreFinal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		capture := &wireTestCapture{}
		server := newWireTestServer(t, capture, true)
		executor, auth := newWireTestExecutor(server.URL, config.OpenAICompatibility{WireAPI: "responses"})
		executor.cfg.Payload = config.PayloadConfig{
			Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "up-model", Protocol: "codex"}},
				Params: map[string]any{"reasoning.effort": "low", "max_output_tokens": 77},
			}},
		}
		req := cliproxyexecutor.Request{Model: "up-model(high)", Payload: []byte(wireTestChatRequest)}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
		if stream {
			result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatalf("ExecuteStream error: %v", err)
			}
			if _, errStream := collectWireTestStream(t, result); errStream != nil {
				t.Fatalf("stream error: %v", errStream)
			}
		} else if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		path, body := capture.last()
		if path != "/v1/responses" {
			t.Fatalf("stream=%t path = %q", stream, path)
		}
		if got := ojson.GetBytes(body, "reasoning.effort").String(); got != "low" {
			t.Fatalf("stream=%t reasoning.effort = %q, body %s", stream, got, body)
		}
		if got := ojson.GetBytes(body, "max_output_tokens").Int(); got != 77 {
			t.Fatalf("stream=%t max_output_tokens = %d, body %s", stream, got, body)
		}
	}
}
