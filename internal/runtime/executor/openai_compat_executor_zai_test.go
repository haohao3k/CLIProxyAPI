package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// TestOpenAICompatExecutorZAIThinkingMapping verifies that requests routed to
// the Z.ai provider carry the coding-plan key, hit /chat/completions, map the
// canonical reasoning effort onto GLM's thinking switch, and that payload
// rules remain the final barrier.
func TestOpenAICompatExecutorZAIThinkingMapping(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "glm-5.2", Protocol: "openai"}}
	for _, tt := range []struct {
		name         string
		model        string
		payload      string
		payloadCfg   config.PayloadConfig
		wantThinking string
	}{
		{
			name:         "effort enables thinking",
			model:        "glm-5.2(high)",
			payload:      `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
			wantThinking: "enabled",
		},
		{
			name:         "none disables thinking",
			model:        "glm-5.2(none)",
			payload:      `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
			wantThinking: "disabled",
		},
		{
			name:         "client reasoning_effort is mapped",
			model:        "glm-5.2",
			payload:      `{"model":"glm-5.2","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`,
			wantThinking: "enabled",
		},
		{
			name:         "no thinking intent leaves body untouched",
			model:        "glm-5.2",
			payload:      `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
			wantThinking: "",
		},
		{
			name:    "payload override is final",
			model:   "glm-5.2(high)",
			payload: `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
			payloadCfg: config.PayloadConfig{Override: []config.PayloadRule{
				{Models: models, Params: map[string]any{"thinking.type": "disabled"}},
			}},
			wantThinking: "disabled",
		},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tt.name, stream), func(t *testing.T) {
				type captured struct {
					path, auth string
					body       []byte
				}
				requests := make(chan captured, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					requests <- captured{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"hm\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"hm","content":"ok"},"finish_reason":"stop"}]}`)
				}))
				t.Cleanup(server.Close)

				exec := NewOpenAICompatExecutor("zai", &config.Config{Payload: tt.payloadCfg})
				auth := &cliproxyauth.Auth{
					Provider: "zai",
					Attributes: map[string]string{
						"base_url": server.URL + "/api/coding/paas/v4",
						"api_key":  "ak.sk",
					},
				}
				request := cliproxyexecutor.Request{
					Model:   tt.model,
					Payload: []byte(tt.payload),
					Metadata: map[string]any{
						"cliproxy.resolved_api_key_model_info": registry.GetZAIModels()[2],
					},
				}
				options := cliproxyexecutor.Options{
					SourceFormat:   sdktranslator.FormatOpenAI,
					ResponseFormat: sdktranslator.FormatOpenAI,
					Stream:         stream,
				}
				if stream {
					result, errStream := exec.ExecuteStream(context.Background(), auth, request, options)
					if errStream != nil {
						t.Fatalf("ExecuteStream: %v", errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatalf("chunk error: %v", chunk.Err)
						}
					}
				} else if _, errExec := exec.Execute(context.Background(), auth, request, options); errExec != nil {
					t.Fatalf("Execute: %v", errExec)
				}

				got := <-requests
				if got.path != "/api/coding/paas/v4/chat/completions" {
					t.Fatalf("path = %q", got.path)
				}
				if got.auth != "Bearer ak.sk" {
					t.Fatalf("authorization = %q", got.auth)
				}
				doc := ojson.NewDoc(got.body)
				if doc.Get("reasoning_effort").Exists() {
					t.Fatalf("reasoning_effort must not reach Z.ai: %s", got.body)
				}
				if gotThinking := doc.Get("thinking.type").String(); gotThinking != tt.wantThinking {
					t.Fatalf("thinking.type = %q, want %q; body=%s", gotThinking, tt.wantThinking, got.body)
				}
			})
		}
	}
}

func TestOpenAICompatExecutorZAIDeveloperRoleFromResponsesNormalized(t *testing.T) {
	payload := `{
		"model": "glm-5.3-flash",
		"instructions": "System instructions",
		"input": [
			{"role": "developer", "content": [{"type": "input_text", "text": "Developer guidelines"}]},
			{"role": "user", "content": [{"type": "input_text", "text": "Reply OK"}]}
		]
	}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			requests := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				requests <- body
				for _, msg := range ojson.GetBytes(body, "messages").Array() {
					if msg.Get("role").String() == "developer" {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"error":{"code":"1214","message":"Incorrect role information"}}`)
						return
					}
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
			}))
			t.Cleanup(server.Close)

			exec := NewOpenAICompatExecutor("zai", &config.Config{})
			auth := &cliproxyauth.Auth{
				Provider: "zai",
				Attributes: map[string]string{
					"base_url": server.URL + "/api/coding/paas/v4",
					"api_key":  "ak.sk",
				},
			}
			req := cliproxyexecutor.Request{
				Model:   "glm-5.3-flash",
				Payload: []byte(payload),
			}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
				Stream:         stream,
			}
			if stream {
				result, errStream := exec.ExecuteStream(context.Background(), auth, req, opts)
				if errStream != nil {
					t.Fatalf("ExecuteStream: %v", errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("chunk error: %v", chunk.Err)
					}
				}
			} else if _, errExec := exec.Execute(context.Background(), auth, req, opts); errExec != nil {
				t.Fatalf("Execute: %v", errExec)
			}

			body := <-requests
			msgs := ojson.GetBytes(body, "messages").Array()
			if len(msgs) != 3 {
				t.Fatalf("messages count = %d, want 3; body=%s", len(msgs), body)
			}
			if got := msgs[1].Get("role").String(); got != "system" {
				t.Fatalf("messages[1].role = %q, want system; body=%s", got, body)
			}
			if got := msgs[1].Get("content").String(); got != "Developer guidelines" {
				t.Fatalf("messages[1].content = %q, want %q; body=%s", got, "Developer guidelines", body)
			}
		})
	}
}
