package cursor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestResolveUpstreamModelVariantsAndFamilies(t *testing.T) {
	cases := []struct {
		model   string
		effort  string
		wantID  string
		comment string
	}{
		{"cursor-deepseek-v41-flash-1017", "", "deepseek-v41-flash-1017-medium", "bare family defaults to medium"},
		{"cursor-deepseek-v41-flash-1017", "high", "deepseek-v41-flash-1017-high", "bare family honors payload effort"},
		{"cursor-deepseek-v41-flash-1017", "none", "deepseek-v41-flash-1017-none", "bare family honors none effort"},
		{"cursor-deepseek-v41-flash-1017(max)", "medium", "deepseek-v41-flash-1017-max", "parenthetical suffix overrides payload effort"},
		{"cursor-deepseek-v41-flash-1017-high", "medium", "deepseek-v41-flash-1017-high", "explicit variant in model name wins over default payload effort"},
		{"cursor/deepseek-v41-flash-1017-low", "high", "deepseek-v41-flash-1017-low", "9router slash syntax with explicit variant"},
		{"cursor-grok-4.6", "high", "cursor-grok-4.6-high", "preserves literal cursor- prefix on cursor-grok-4.6 family"},
		{"cursor-grok-4.6-xhigh", "medium", "cursor-grok-4.6-xhigh", "preserves literal cursor- prefix on explicit variant"},
		{"cursor-grok-4.7", "high", "grok-4.7-high", "strips synthetic cursor- prefix on grok-4.7 family"},
		{"cursor-glm-5p3-flash", "medium", "glm-5p3-flash", "standalone model passes through"},
		{"cursor-glm-5.2", "medium", "glm-5.2-high", "nearest supported tier when medium is absent"},
		{"cursor-gpt-5.3-codex", "", "gpt-5.3-codex", "bare gpt-5.3-codex represents medium"},
		{"cursor-gpt-5.3-codex", "high", "gpt-5.3-codex-high", "gpt-5.3-codex with high effort"},
		{"cursor-gpt-5.5", "xhigh", "gpt-5.5-extra-high", "maps canonical xhigh to upstream -extra-high"},
		{"cursor-auto-smart", "", "default", "auto-smart maps to default"},
		{"cursor/auto", "medium", "default", "cursor/auto maps to default"},
		{"cursor-claude-sonnet-4-5", "", "claude-4.5-sonnet", "static sonnet-4-5 alias"},
		{"cursor-claude-sonnet-4-5", "high", "claude-4.5-sonnet-thinking", "sonnet-4-5 with thinking effort"},
		{"cursor-claude-4-sonnet", "", "claude-4-sonnet", "claude-4-sonnet bare"},
	}

	for _, tc := range cases {
		if got := ResolveUpstreamModel(tc.model, tc.effort); got != tc.wantID {
			t.Fatalf("%s: ResolveUpstreamModel(%q, %q) = %q, want %q", tc.comment, tc.model, tc.effort, got, tc.wantID)
		}
	}
}

func TestDecodeUsableModelsAndSynthesizeFamilies(t *testing.T) {
	mkDetail := func(id, disp string) []byte {
		var d []byte
		d = appendBytesField(d, 1, []byte(id))
		d = appendBytesField(d, 4, []byte(disp))
		return d
	}
	var payload []byte
	payload = appendBytesField(payload, 1, mkDetail("deepseek-v41-flash-1017-low", "DeepSeek V4.1 Flash Low"))
	payload = appendBytesField(payload, 1, mkDetail("deepseek-v41-flash-1017-medium", "DeepSeek V4.1 Flash Medium"))
	payload = appendBytesField(payload, 1, mkDetail("deepseek-v41-flash-1017-high", "DeepSeek V4.1 Flash High"))
	payload = appendBytesField(payload, 1, mkDetail("cursor-grok-4.6-high", "Grok 4.6 High"))
	payload = appendBytesField(payload, 1, mkDetail("cursor-grok-4.6-low", "Grok 4.6 Low"))

	usable, err := DecodeUsableModels(payload)
	if err != nil {
		t.Fatalf("DecodeUsableModels() error = %v", err)
	}
	if len(usable) != 5 {
		t.Fatalf("len(usable) = %d, want 5", len(usable))
	}
	if got := ClientModelID("cursor-grok-4.6-high"); got != "cursor-grok-4.6-high" {
		t.Fatalf("ClientModelID double-prefixed cursor-grok: %q", got)
	}
	if got := ClientModelID("deepseek-v41-flash-1017-high"); got != "cursor-deepseek-v41-flash-1017-high" {
		t.Fatalf("ClientModelID = %q", got)
	}

	fams := SynthesizeFamilyModels(usable)
	if len(fams) != 2 {
		t.Fatalf("SynthesizeFamilyModels() len = %d, want 2: %+v", len(fams), fams)
	}
	if fams[0].ID != "deepseek-v41-flash-1017" || strings.Join(fams[0].Levels, ",") != "low,medium,high" {
		t.Fatalf("unexpected fam[0]: %+v", fams[0])
	}
	if fams[1].ID != "cursor-grok-4.6" || strings.Join(fams[1].Levels, ",") != "low,high" {
		t.Fatalf("unexpected fam[1]: %+v", fams[1])
	}
}

func TestBuildAgentRunFramePreservesToolHistory(t *testing.T) {
	msgs := []AgentMessage{
		{Role: "developer", Content: "Follow workspace rules."},
		{Role: "user", Content: "Read package.json"},
		{
			Role: "assistant",
			ToolCalls: []AgentToolCall{
				{ID: "call_123", Name: "read", Arguments: `{"path":"package.json"}`},
			},
		},
		{Role: "tool", ToolCallID: "call_123", Content: `{"name":"demo"}`},
	}
	tools := []AgentTool{
		{
			Name:        "read",
			Description: "Read file",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
			},
		},
	}
	framed := BuildAgentRunFrame(msgs, "deepseek-v41-flash-1017-high", tools)
	for _, needle := range []string{
		"Follow workspace rules.",
		"Read package.json",
		"[tool_call id=call_123 name=read",
		"[tool_result id=call_123]",
		`{"name":"demo"}`,
		"deepseek-v41-flash-1017-high",
	} {
		if !bytes.Contains(framed, []byte(needle)) {
			t.Fatalf("BuildAgentRunFrame missing %q", needle)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAgentDuplexSessionContextAckAndMcpToolCall(t *testing.T) {
	clientReceivedAck := make(chan bool, 1)

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			sr, sw := io.Pipe()
			go func() {
				defer func() { _ = sw.Close() }()
				// Read initial run_request frame from client.
				buf := make([]byte, 4096)
				_, _ = req.Body.Read(buf)

				// Send ExecServerMessage (field 2) with request_context (field 10).
				var execReq []byte
				execReq = appendVarintFieldAlways(execReq, 1, 7)
				execReq = appendBytesField(execReq, 15, []byte("exec_ctx_1"))
				execReq = appendBytesField(execReq, 10, nil)
				_, _ = sw.Write(wrapConnectFrame(0, appendBytesField(nil, 2, execReq)))

				// Read client's request_context ack frame!
				n, _ := req.Body.Read(buf)
				if n > 5 && bytes.Contains(buf[:n], []byte("exec_ctx_1")) {
					clientReceivedAck <- true
				} else {
					clientReceivedAck <- false
				}

				// Send Cursor 3.12+ MCP provider probe ExecServerMessage with metadata
				// fields 1 (id), 19 (span_context), 36 (mcp_provider_probe), 55 (flags).
				var probeExec []byte
				probeExec = appendVarintFieldAlways(probeExec, 1, 7)
				probeExec = appendBytesField(probeExec, 15, []byte("exec_probe_36"))
				probeExec = appendBytesField(probeExec, 19, []byte("span_ctx_trace"))
				probeExec = appendBytesField(probeExec, 36, appendBytesField(nil, 1, []byte("9router")))
				probeExec = appendVarintFieldAlways(probeExec, 55, 0)
				_, _ = sw.Write(wrapConnectFrame(0, appendBytesField(nil, 2, probeExec)))

				// Client must reject field 36 on the duplex stream to unlock McpArgs (field 11).
				nProbe, _ := req.Body.Read(buf)
				if nProbe <= 5 || !bytes.Contains(buf[:nProbe], []byte("exec_probe_36")) {
					return
				}

				// Now send ExecServerMessage (field 2) with McpArgs (field 11).
				var entry []byte
				entry = appendBytesField(entry, 1, []byte("path"))
				entry = appendBytesField(entry, 2, encodeAgentValue("README.md"))

				var mcp []byte
				mcp = appendBytesField(mcp, 1, []byte("9router-read"))
				mcp = appendBytesField(mcp, 2, entry)
				mcp = appendBytesField(mcp, 3, []byte("call_abc"))
				mcp = appendBytesField(mcp, 5, []byte("read"))

				var mcpExec []byte
				mcpExec = appendVarintFieldAlways(mcpExec, 1, 8)
				mcpExec = appendBytesField(mcpExec, 11, mcp)
				_, _ = sw.Write(wrapConnectFrame(0, appendBytesField(nil, 2, mcpExec)))
			}()

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       sr,
			}, nil
		}),
	}

	initFrame := BuildAgentRunFrame([]AgentMessage{{Role: "user", Content: "read README.md"}}, "deepseek-v41-flash-1017-high", nil)
	sess, err := OpenAgentDuplexSession(context.Background(), client, func(body io.Reader) (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPost, AgentRunEndpoint, body)
	}, initFrame)
	if err != nil {
		t.Fatalf("OpenAgentDuplexSession() error = %v", err)
	}

	var gotTool *AgentToolCall
	if err := sess.Consume("deepseek-v41-flash-1017-high", func(ev AgentStreamEvent) bool {
		if ev.Type == AgentEventToolCall {
			gotTool = ev.ToolCall
		}
		return true
	}); err != nil {
		t.Fatalf("Consume() error = %v", err)
	}
	if !<-clientReceivedAck {
		t.Fatalf("server did not receive request_context ack frame on duplex stream")
	}
	if gotTool == nil || gotTool.ID != "call_abc" || gotTool.Name != "read" || gotTool.Arguments != `{"path":"README.md"}` {
		t.Fatalf("unexpected tool call: %+v", gotTool)
	}
}
