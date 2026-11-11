package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func testSSEBlock(event string, data map[string]any) string {
	return string(serializeSseEvent(event, data))
}

func TestCloseJsonArgs(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"cmd":"ls -la"`, `{"cmd":"ls -la"}`},
		{`{"cmd":"echo hi`, `{"cmd":"echo hi"}`},
		{`{"cmd":"echo hi", "`, `{"cmd":"echo hi", "":null}`},
		{`{"cmd":"echo hi", "__pa`, `{"cmd":"echo hi", "__pa":null}`},
		{`{"nested":{"a":[1,2,"trun`, `{"nested":{"a":[1,2,"trun"]}}`},
	}
	for _, tc := range cases {
		got := CloseJsonArgs(tc.in)
		if got != tc.want {
			t.Errorf("CloseJsonArgs(%q) = %q, want %q", tc.in, got, tc.want)
		}
		var js any
		if err := json.Unmarshal([]byte(got), &js); err != nil {
			t.Errorf("CloseJsonArgs(%q) produced invalid JSON %q: %v", tc.in, got, err)
		}
	}
}

func TestCleanToolArguments_AllowCustomPadFalse(t *testing.T) {
	// Iron Rule #7: Inside JSON function_call (e.g. codemode), `const pad =` inside `code` must NOT be cut.
	in := `{"code":"const pad = 'hello'; console.log(pad);","__pad9f3":"1111111111`
	got := CleanToolArguments(in, false)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("expected valid JSON, got %q: %v", got, err)
	}
	if parsed["code"] != "const pad = 'hello'; console.log(pad);" {
		t.Fatalf("unexpected code field: %v", parsed["code"])
	}
	if _, hasPad := parsed["__pad9f3"]; hasPad {
		t.Fatalf("expected __pad9f3 to be stripped, got %v", parsed)
	}
}

func TestStreamWithAutocut_ToolpadAndNoFalseArgpadOnJson(t *testing.T) {
	stream := strings.Join([]string{
		testSSEBlock("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":           "resp_1",
				"instructions": "Must include __pad9f3 at the end and ⟦TASK_COMPLETE_9F3⟧",
			},
		}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item":         map[string]any{"id": "fc_1", "type": "function_call", "name": "codemode", "call_id": "call_1"},
		}),
		testSSEBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_1",
			"output_index": 0,
			"delta":        `{"code":"const pad = 'inside_script'; return pad;","`,
		}),
		testSSEBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_1",
			"output_index": 0,
			"delta":        `__pad9f3":"(1) Execution objective...`,
		}),
	}, "")

	var aborted atomic.Bool
	var out bytes.Buffer
	how := StreamWithAutocut(strings.NewReader(stream), &out, nil, CodexAutocutOptions{
		Model:               "gpt-6-astra",
		FinalizeMode:        true,
		HardArgCap:          4000,
		EstimateInputTokens: 12000,
		OnAbortUpstream: func(h string) {
			aborted.Store(true)
		},
	})

	if how.How != "toolpad" {
		t.Fatalf("expected how=toolpad, got %q", how.How)
	}
	if !aborted.Load() {
		t.Fatalf("expected OnAbortUpstream to be called")
	}
	outStr := out.String()
	if !strings.Contains(outStr, `{\"code\":\"const pad = 'inside_script'; return pad;\"}`) {
		t.Fatalf("expected inner script const pad to be preserved, got:\n%s", outStr)
	}
	if strings.Contains(outStr, `"__pad9f3":"(1)`) {
		t.Fatalf("expected __pad9f3 argument to be stripped from output, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, `"type":"response.completed"`) {
		t.Fatalf("expected response.completed in output, got:\n%s", outStr)
	}
}

func TestStreamWithAutocut_FinalizeRewriteAndCut(t *testing.T) {
	stream := strings.Join([]string{
		testSSEBlock("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_fin"}}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item":         map[string]any{"id": "ct_1", "type": "custom_tool_call", "name": "__finalize_9f3", "call_id": "call_fin"},
		}),
		testSSEBlock("response.custom_tool_call_input.delta", map[string]any{
			"type":         "response.custom_tool_call_input.delta",
			"item_id":      "ct_1",
			"output_index": 0,
			"delta":        "Here is the final answer to your question.\nconst pad = \"(1) Summary...",
		}),
	}, "")

	var out bytes.Buffer
	how := StreamWithAutocut(strings.NewReader(stream), &out, nil, CodexAutocutOptions{
		Model:               "gpt-6-astra",
		FinalizeMode:        true,
		HardArgCap:          4000,
		EstimateInputTokens: 5000,
	})

	if how.How != "finalize" {
		t.Fatalf("expected how=finalize, got %q", how.How)
	}
	outStr := out.String()
	if !strings.Contains(outStr, "Here is the final answer to your question.") {
		t.Fatalf("expected final answer text in output, got:\n%s", outStr)
	}
	if strings.Contains(outStr, "const pad") || strings.Contains(outStr, "__finalize_9f3") {
		t.Fatalf("expected finalize tool and pad to be hidden from client, got:\n%s", outStr)
	}
}

func TestStreamWithAutocut_HardArgCap(t *testing.T) {
	longVal := strings.Repeat("a", 4100)
	stream := strings.Join([]string{
		testSSEBlock("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_cap"}}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item":         map[string]any{"id": "fc_cap", "type": "function_call", "name": "write", "call_id": "call_cap"},
		}),
		testSSEBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_cap",
			"output_index": 0,
			"delta":        `{"content":"` + longVal,
		}),
	}, "")

	var out bytes.Buffer
	how := StreamWithAutocut(strings.NewReader(stream), &out, nil, CodexAutocutOptions{
		Model:               "gpt-6-astra",
		FinalizeMode:        true,
		HardArgCap:          4000,
		EstimateInputTokens: 1000,
	})

	if how.How != "argcap" {
		t.Fatalf("expected how=argcap, got %q", how.How)
	}
	if !strings.Contains(out.String(), `"type":"response.completed"`) {
		t.Fatalf("expected response.completed in output")
	}
}

func TestMultiDeltaLookaheadPreCut(t *testing.T) {
	// Iron Rule #6: 53-char item_id + split "__pad9f3" across 4 deltas
	rawOpenAiDelta := func(seq int, delta string) string {
		dBytes, _ := json.Marshal(delta)
		return "event: response.function_call_arguments.delta\n" +
			`data: {"type":"response.function_call_arguments.delta","sequence_number":` + string(rune('0'+seq)) +
			`,"item_id":"fc_01a169ff8a9b777cbb957f79f5266297000123456789abcd","output_index":1,"delta":` + string(dBytes) + "}\n\n"
	}
	stream := strings.Join([]string{
		testSSEBlock("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_split"}}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 1,
			"item":         map[string]any{"id": "fc_01a169ff8a9b777cbb957f79f5266297000123456789abcd", "type": "function_call", "name": "read", "call_id": "c_sp"},
		}),
		rawOpenAiDelta(1, `{"path":"README.md"`),
		rawOpenAiDelta(2, `,"__`),
		rawOpenAiDelta(3, `pad`),
		rawOpenAiDelta(4, `9`),
		rawOpenAiDelta(5, `f3":"(1) Execution objective...`),
	}, "")

	var firstAbortReason string
	var out bytes.Buffer
	how := StreamWithAutocut(strings.NewReader(stream), &out, nil, CodexAutocutOptions{
		Model:               "gpt-6-astra",
		FinalizeMode:        true,
		HardArgCap:          8000,
		EstimateInputTokens: 8000,
		OnAbortUpstream: func(reason string) {
			if firstAbortReason == "" {
				firstAbortReason = reason
			}
		},
	})

	if firstAbortReason != "pre-scan-toolpad" {
		t.Fatalf("expected firstAbortReason=pre-scan-toolpad, got %q", firstAbortReason)
	}
	if how.How != "toolpad" {
		t.Fatalf("expected final how=toolpad, got %q", how.How)
	}
	if !strings.Contains(out.String(), `"arguments":"{\"path\":\"README.md\"}"`) {
		t.Fatalf("expected clean arguments, got:\n%s", out.String())
	}
}

func TestStreamWithAutocut_DetectsToolpadRacelostWhenCompletedInBuffer(t *testing.T) {
	stream := strings.Join([]string{
		testSSEBlock("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_rl"}}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item":         map[string]any{"id": "fc_rl", "type": "function_call", "name": "bash", "call_id": "c_rl"},
		}),
		testSSEBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_rl",
			"output_index": 0,
			"delta":        `{"command":"ls","__pad9f3":"short"}`,
		}),
		testSSEBlock("response.completed", map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_rl", "status": "completed"},
		}),
	}, "")

	var out bytes.Buffer
	how := StreamWithAutocut(strings.NewReader(stream), &out, nil, CodexAutocutOptions{
		Model:               "gpt-6-astra",
		FinalizeMode:        true,
		HardArgCap:          8000,
		EstimateInputTokens: 5000,
	})

	if how.How != "toolpad-racelost" {
		t.Fatalf("expected how=toolpad-racelost when response.completed is in buffer, got %q", how.How)
	}
	if !how.RaceLost {
		t.Fatalf("expected tel.RaceLost=true when response.completed is in buffer")
	}
}

func TestStreamWithAutocut_DiscardsPreambleMessageBeforeToolCall(t *testing.T) {
	stream := strings.Join([]string{
		testSSEBlock("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_pre"}}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item":         map[string]any{"id": "msg_pre", "type": "message", "role": "assistant"},
		}),
		testSSEBlock("response.content_part.added", map[string]any{
			"type":          "response.content_part.added",
			"item_id":       "msg_pre",
			"output_index":  0,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": ""},
		}),
		testSSEBlock("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       "msg_pre",
			"output_index":  0,
			"content_index": 0,
			"delta":         "Preamble text that must be dropped when tool call follows.",
		}),
		testSSEBlock("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       "msg_pre",
			"output_index":  0,
			"content_index": 0,
			"text":          "Preamble text that must be dropped when tool call follows.",
		}),
		testSSEBlock("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": 0,
			"item":         map[string]any{"id": "msg_pre", "type": "message", "status": "completed", "role": "assistant"},
		}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 1,
			"item":         map[string]any{"id": "fc_real", "type": "function_call", "name": "bash", "call_id": "c_real"},
		}),
		testSSEBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_real",
			"output_index": 1,
			"delta":        `{"command":"ls","__pad9f3":"(1) Execution objective...`,
		}),
	}, "")

	var out bytes.Buffer
	how := StreamWithAutocut(strings.NewReader(stream), &out, nil, CodexAutocutOptions{
		Model:               "gpt-6-astra",
		FinalizeMode:        true,
		HardArgCap:          8000,
		EstimateInputTokens: 5000,
	})

	if how.How != "toolpad" {
		t.Fatalf("expected how=toolpad, got %q", how.How)
	}
	if strings.Contains(out.String(), "Preamble text that must be dropped") {
		t.Fatalf("expected preamble message to be discarded, got:\n%s", out.String())
	}
}

func TestStreamWithAutocut_PreScanToolpadNotBlindedByFinalizeToolInCreated(t *testing.T) {
	stream := strings.Join([]string{
		testSSEBlock("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":    "resp_pre_scan",
				"tools": []map[string]any{{"type": "custom", "name": "__finalize_9f3"}},
			},
		}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item":         map[string]any{"id": "fc_long_item_id_0123456789abcdef0123456789abcdef0123", "type": "function_call", "name": "bash", "call_id": "c_1"},
		}),
		testSSEBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_long_item_id_0123456789abcdef0123456789abcdef0123",
			"output_index": 0,
			"delta":        `{"command":"ls","__pad`,
		}),
		testSSEBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_long_item_id_0123456789abcdef0123456789abcdef0123",
			"output_index": 0,
			"delta":        `9f3":"(1) Execution objective...`,
		}),
	}, "")

	var out bytes.Buffer
	var abortReasons []string
	how := StreamWithAutocut(strings.NewReader(stream), &out, nil, CodexAutocutOptions{
		Model:               "gpt-6-astra",
		FinalizeMode:        true,
		HardArgCap:          8000,
		EstimateInputTokens: 5000,
		OnAbortUpstream: func(reason string) {
			abortReasons = append(abortReasons, reason)
		},
	})

	if how.How != "toolpad" {
		t.Fatalf("expected how=toolpad, got %q", how.How)
	}
	if len(abortReasons) == 0 || abortReasons[0] != "pre-scan-toolpad" {
		t.Fatalf("expected first abort reason to be pre-scan-toolpad even when __finalize_9f3 is in response.created, got %v", abortReasons)
	}
}

func TestStreamWithAutocut_PreScanUnpaddedClose(t *testing.T) {
	stream := strings.Join([]string{
		testSSEBlock("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_unpadded"}}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item":         map[string]any{"id": "fc_unpadded_1", "type": "function_call", "name": "bash", "call_id": "c_unp"},
		}),
		testSSEBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_unpadded_1",
			"output_index": 0,
			"delta":        `{"command":"git status --short"}`,
		}),
		testSSEBlock("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": 0,
			"item":         map[string]any{"id": "fc_unpadded_1", "type": "function_call", "name": "bash", "call_id": "c_unp", "arguments": `{"command":"git status --short"}`},
		}),
	}, "")

	var firstAbortReason string
	var out bytes.Buffer
	how := StreamWithAutocut(strings.NewReader(stream), &out, nil, CodexAutocutOptions{
		Model:               "gpt-6-astra",
		FinalizeMode:        true,
		HardArgCap:          8000,
		EstimateInputTokens: 8000,
		OnAbortUpstream: func(reason string) {
			if firstAbortReason == "" {
				firstAbortReason = reason
			}
		},
	})

	if firstAbortReason != "pre-scan-unpadded-close" {
		t.Fatalf("expected firstAbortReason=pre-scan-unpadded-close, got %q", firstAbortReason)
	}
	if how.How != "toolpad-unpadded" {
		t.Fatalf("expected how=toolpad-unpadded, got %q", how.How)
	}
	if !strings.Contains(out.String(), `"arguments":"{\"command\":\"git status --short\"}"`) {
		t.Fatalf("expected clean unpadded arguments delivered, got:\n%s", out.String())
	}
}

func TestStreamWithAutocut_FinalizeUnpaddedDoneCut(t *testing.T) {
	stream := strings.Join([]string{
		testSSEBlock("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":    "resp_fin_unp",
				"tools": []map[string]any{{"type": "custom", "name": "__finalize_9f3"}},
			},
		}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item":         map[string]any{"id": "c_fin_1", "type": "custom_tool_call", "name": "__finalize_9f3", "call_id": "call_fin_1"},
		}),
		testSSEBlock("response.custom_tool_call_input.delta", map[string]any{
			"type":         "response.custom_tool_call_input.delta",
			"item_id":      "c_fin_1",
			"output_index": 0,
			"delta":        "Here is the complete solution without pad.",
		}),
		testSSEBlock("response.custom_tool_call_input.done", map[string]any{
			"type":         "response.custom_tool_call_input.done",
			"item_id":      "c_fin_1",
			"output_index": 0,
			"input":        "Here is the complete solution without pad.",
		}),
	}, "")

	var firstAbortReason string
	var out bytes.Buffer
	how := StreamWithAutocut(strings.NewReader(stream), &out, nil, CodexAutocutOptions{
		Model:               "gpt-6-astra",
		FinalizeMode:        true,
		HardArgCap:          8000,
		EstimateInputTokens: 8000,
		OnAbortUpstream: func(reason string) {
			if firstAbortReason == "" {
				firstAbortReason = reason
			}
		},
	})

	if firstAbortReason != "pre-scan-finalize-done" {
		t.Fatalf("expected firstAbortReason=pre-scan-finalize-done, got %q", firstAbortReason)
	}
	if how.How != "finalize-unpadded" {
		t.Fatalf("expected how=finalize-unpadded, got %q", how.How)
	}
	if !strings.Contains(out.String(), "Here is the complete solution without pad.") {
		t.Fatalf("expected full answer text delivered to client, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"type":"response.completed"`) {
		t.Fatalf("expected response.completed synthesized downstream, got:\n%s", out.String())
	}
}

func TestAutocompact_ImmutableOutputTrimAndPairAtomicHeadAnchor(t *testing.T) {
	// Iron Rule #5: keepRecent=0 trims oversized outputs on turn 1, and retreating headAnchor
	// never splits function_call <-> function_call_output pairs.
	items := []any{
		map[string]any{"type": "message", "role": "user", "content": "Initial task"},
	}
	for i := 0; i < 12; i++ {
		cid := "call_pair_" + string(rune('a'+i))
		items = append(items,
			map[string]any{"type": "function_call", "call_id": cid, "name": "read", "arguments": `{"path":"a.go"}`},
			map[string]any{"type": "function_call_output", "call_id": cid, "output": strings.Repeat("Z", 35000)},
		)
	}
	count := trimOversizedOutputs(items, 30000, 0)
	if count != 12 {
		t.Fatalf("expected all 12 oversized outputs trimmed immediately (keepRecent=0), got %d", count)
	}
	lastOut := items[len(items)-1].(map[string]any)["output"].(string)
	if !strings.Contains(lastOut, "trimmed 5000 chars of this old tool output") {
		t.Fatalf("expected immediate truncation marker on newest output, got len=%d", len(lastOut))
	}

	cutIdx := findCodexCutPoint(items, 8000)
	bodyMap := map[string]any{"input": items}
	rewriteCodexInput(bodyMap, "[summary]", cutIdx, 4000)
	rewritten := bodyMap["input"].([]any)
	// Verify every function_call in rewritten has a matching function_call_output and vice versa
	calls := map[string]bool{}
	outputs := map[string]bool{}
	for _, raw := range rewritten {
		it, _ := raw.(map[string]any)
		switch it["type"] {
		case "function_call":
			calls[it["call_id"].(string)] = true
		case "function_call_output":
			outputs[it["call_id"].(string)] = true
		}
	}
	for cid := range calls {
		if !outputs[cid] {
			t.Fatalf("severed pair: function_call %s missing output after rewriteCodexInput", cid)
		}
	}
	for cid := range outputs {
		if !calls[cid] {
			t.Fatalf("severed pair: function_call_output %s missing call after rewriteCodexInput", cid)
		}
	}
}

func TestCodexExecutor_MeteringBypassEndToEnd_NativeOAuthAndGoSidecar(t *testing.T) {
	prevDelay := codexSidecarRetryDelay
	codexSidecarRetryDelay = 10 * time.Millisecond
	defer func() { codexSidecarRetryDelay = prevDelay }()

	var attemptCounter atomic.Int32
	var capturedAuthHeader atomic.Value
	var capturedAccountID atomic.Value
	var capturedAcceptEncoding atomic.Value
	var capturedProto atomic.Value
	var capturedBody atomic.Value

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attemptCounter.Add(1)
		capturedAuthHeader.Store(r.Header.Get("Authorization"))
		capturedAccountID.Store(r.Header.Get("Chatgpt-Account-Id"))
		capturedAcceptEncoding.Store(r.Header.Get("Accept-Encoding"))
		capturedProto.Store(r.Proto)
		var reqBody bytes.Buffer
		_, _ = reqBody.ReadFrom(r.Body)
		capturedBody.Store(reqBody.String())

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-codex-primary-used-percent", "15")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// Attempt 1 simulates a 200-OK SSE transient server_is_overloaded -> Go Sidecar retries in-place!
		if n == 1 {
			_, _ = w.Write(serializeSseEvent("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"code": "server_is_overloaded", "message": "Our servers are currently overloaded."},
			}))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		// Attempt 2 streams __finalize_9f3 answer + const pad -> Go Sidecar rewrites to assistant message & aborts!
		_, _ = w.Write(serializeSseEvent("response.created", map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_native_oauth", "model": "gpt-6-astra"},
		}))
		_, _ = w.Write(serializeSseEvent("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item":         map[string]any{"id": "ct_99", "type": "custom_tool_call", "name": "__finalize_9f3", "call_id": "call_99"},
		}))
		_, _ = w.Write(serializeSseEvent("response.custom_tool_call_input.delta", map[string]any{
			"type":         "response.custom_tool_call_input.delta",
			"item_id":      "ct_99",
			"output_index": 0,
			"delta":        "Xin chào từ Native Codex OAuth + Go Sidecar!\nconst pad = \"(1) Summary of reasoning...",
		}))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstreamSrv.Close()

	enabled := true
	cbMin := 30
	cfg := &config.Config{
		Codex: config.CodexConfig{
			MeteringBypass:                  &enabled,
			AutocutFinalize:                 &enabled,
			AutocutReinforcement:            "always",
			MaxConcurrentRequests:           2,
			QuotaCircuitBreakerMinRemaining: &cbMin,
		},
	}
	exec := NewCodexExecutor(cfg)

	// Native CLIProxyAPI Codex OAuth credential
	auth := &cliproxyauth.Auth{
		ID:       "codex-oauth-test-1",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "oauth_native_token_xyz",
			"account_id":   "acct_native_12345",
		},
		Attributes: map[string]string{
			"base_url": upstreamSrv.URL,
		},
	}

	clientPayload := []byte(`{
		"model": "gpt-6.1-sol",
		"reasoning": {"effort": "xhigh"},
		"stream": true,
		"input": [{"type": "message", "role": "user", "content": "Chào bạn!"}]
	}`)

	streamRes, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-6.1-sol",
		Payload: clientPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		OriginalRequest: clientPayload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}

	var combined strings.Builder
	for chunk := range streamRes.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}

	if attemptCounter.Load() != 2 {
		t.Fatalf("expected 2 upstream attempts (1 transient SSE overload retry + 1 clean stream), got %d", attemptCounter.Load())
	}
	if got := capturedAuthHeader.Load().(string); got != "Bearer oauth_native_token_xyz" {
		t.Fatalf("expected native OAuth Bearer token, got %q", got)
	}
	if got := capturedAccountID.Load().(string); got != "acct_native_12345" {
		t.Fatalf("expected native OAuth Chatgpt-Account-Id=acct_native_12345, got %q", got)
	}
	if got := capturedAcceptEncoding.Load().(string); got != "identity" {
		t.Fatalf("expected Accept-Encoding=identity for Go Sidecar, got %q", got)
	}
	if got := capturedProto.Load().(string); got != "HTTP/1.1" {
		t.Fatalf("expected strict HTTP/1.1 socket (zero HTTP/2 multiplexing), got %q", got)
	}

	upBody := capturedBody.Load().(string)
	if !strings.Contains(upBody, `"model":"gpt-6-astra"`) {
		t.Fatalf("expected -sol clamped to gpt-6-astra, got body: %s", upBody)
	}
	if !strings.Contains(upBody, `"effort":"medium"`) {
		t.Fatalf("expected xhigh clamped to medium, got body: %s", upBody)
	}
	if !strings.Contains(upBody, `"service_tier":"priority"`) {
		t.Fatalf("expected service_tier=priority, got body: %s", upBody)
	}
	if !strings.Contains(upBody, `"name":"__finalize_9f3"`) || !strings.Contains(upBody, `"tool_choice":"required"`) {
		t.Fatalf("expected __finalize_9f3 + tool_choice=required injected, got body: %s", upBody)
	}

	downstreamSSE := combined.String()
	if !strings.Contains(downstreamSSE, "Xin chào từ Native Codex OAuth + Go Sidecar!") {
		t.Fatalf("expected rewritten assistant message in downstream SSE, got:\n%s", downstreamSSE)
	}
	if strings.Contains(downstreamSSE, "const pad") || strings.Contains(downstreamSSE, "__finalize_9f3") {
		t.Fatalf("expected __finalize_9f3 and const pad stripped from downstream SSE, got:\n%s", downstreamSSE)
	}
}

func TestCodexAutocutTelemetry_JSONLAndQuotaDeltaAndRaceLostUsage(t *testing.T) {
	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, codexAutocutTelemetryFileName)

	auth := &cliproxyauth.Auth{
		ID:       "codex-forensic-acct",
		Label:    "forensic@example.com",
		Provider: "codex",
		Metadata: map[string]any{"email": "forensic@example.com"},
	}

	// 1. Observe initial 12% quota -> delta 0, then 14.5% -> delta +2.5% (strictly observational, Iron Rule #4!)
	clearCodexAutocutViolations("innocent-session-42")
	hdr1 := http.Header{"X-Codex-Primary-Used-Percent": []string{"12.0"}}
	u1, r1, d1, ok1 := observeCodexQuotaCircuitBreakerWithMeta(context.Background(), nil, auth, hdr1, "sess-forensic-1", "gpt-6-astra")
	if !ok1 || u1 != 12.0 || r1 != 88.0 || d1 != 0 {
		t.Fatalf("unexpected initial quota observation: ok=%v u=%v r=%v d=%v", ok1, u1, r1, d1)
	}

	hdr2 := http.Header{"X-Codex-Primary-Used-Percent": []string{"14.5"}}
	u2, r2, d2, ok2 := observeCodexQuotaCircuitBreakerWithMeta(context.Background(), nil, auth, hdr2, "sess-forensic-1", "gpt-6-astra")
	if !ok2 || u2 != 14.5 || r2 != 85.5 || d2 != 2.5 {
		t.Fatalf("expected +2.5%% quota delta on second observation, got ok=%v u=%v r=%v d=%v", ok2, u2, r2, d2)
	}
	if gotStreak := getCodexAutocutStreak("innocent-session-42"); gotStreak != 0 {
		t.Fatalf("Iron Rule #4 violation: account quota delta must NEVER arm innocent sessions, got streak=%d", gotStreak)
	}

	// 2. Simulate a RACE-LOST turn where response.completed + usage arrived in rawBuffer alongside toolpad
	raceStream := strings.Join([]string{
		testSSEBlock("response.created", map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_race_1"},
		}),
		testSSEBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item":         map[string]any{"id": "fc_race", "type": "function_call", "name": "bash", "call_id": "call_race"},
		}),
		testSSEBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_race",
			"output_index": 0,
			"delta":        `{"cmd":"pwd","__pad9f3":"short"}`,
		}),
		testSSEBlock("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp_race_1",
				"usage": map[string]any{
					"input_tokens":         42000,
					"output_tokens":        315,
					"input_tokens_details": map[string]any{"cached_tokens": 38000},
				},
			},
		}),
	}, "")

	var out bytes.Buffer
	tel := StreamWithAutocut(strings.NewReader(raceStream), &out, nil, CodexAutocutOptions{
		Model:               "gpt-6-astra",
		SessionID:           "sess-forensic-1",
		FinalizeMode:        true,
		HardArgCap:          8000,
		EstimateInputTokens: 42000,
		AuthID:              auth.ID,
		AuthLabel:           auth.Label,
		AuthEmail:           "forensic@example.com",
		HasQuota:            true,
		UsedPct:             u2,
		RemainingPct:        r2,
		QuotaDeltaPct:       d2,
		TelemetryLogPath:    jsonlPath,
		OnAbortUpstream:     func(string) {},
	})

	if !tel.RaceLost || tel.How != "toolpad-racelost" {
		t.Fatalf("expected RaceLost=true and how=toolpad-racelost, got %+v", tel)
	}
	if tel.UpstreamInputTokens != 42000 || tel.UpstreamOutputTokens != 315 || tel.UpstreamCachedTokens != 38000 {
		t.Fatalf("expected upstream usage 42000/315/38000 extracted from rawBuffer, got %d/%d/%d",
			tel.UpstreamInputTokens, tel.UpstreamOutputTokens, tel.UpstreamCachedTokens)
	}
	if tel.LastToolName != "bash" || tel.ToolCallCount != 1 {
		t.Fatalf("expected tool=bash(1), got %s(%d)", tel.LastToolName, tel.ToolCallCount)
	}

	rawJSONL, errRead := os.ReadFile(jsonlPath)
	if errRead != nil || len(rawJSONL) == 0 {
		t.Fatalf("expected persisted JSONL forensic file at %s, err=%v", jsonlPath, errRead)
	}
	var persisted CodexAutocutTurnRecord
	if err := json.Unmarshal(bytes.TrimSpace(rawJSONL), &persisted); err != nil {
		t.Fatalf("failed to unmarshal JSONL line: %v", err)
	}
	if !persisted.LeakRisk || !persisted.RaceLost || persisted.UpstreamInputTokens != 42000 || persisted.QuotaDeltaPct != 2.5 {
		t.Fatalf("unexpected persisted record: %+v", persisted)
	}

	snap := SnapshotCodexAutocutTelemetry(nil, tmpDir, CodexTelemetryQueryOptions{
		Limit:     10,
		LeaksOnly: true,
		SessionID: "sess-forensic-1",
		AuthQuery: "forensic",
	})
	if len(snap.RecentTurns) == 0 || snap.RecentTurns[0].How != "toolpad-racelost" {
		t.Fatalf("expected toolpad-racelost in SnapshotCodexAutocutTelemetry LeaksOnly query, got %+v", snap.RecentTurns)
	}
	if len(snap.QuotaDeltas) == 0 || snap.QuotaDeltas[0].DeltaPct != 2.5 {
		t.Fatalf("expected +2.5%% quota delta event in snapshot, got %+v", snap.QuotaDeltas)
	}
}
