package executor

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cursorwire "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// The handler pins a Cursor request to the variant it resolves with cursor.RequestEffort. For every
// request shape that must be the variant the executor picks without pinning (the behaviour before
// pinning existed), and the pinned plan must run exactly that variant.
func TestPinnedCursorVariantMatchesUnpinnedResolution(t *testing.T) {
	exec := NewCursorExecutor(&config.Config{})
	const family = "cursor-deepseek-v41-flash-1017"
	cases := []struct {
		name   string
		format sdktranslator.Format
		body   string
		want   string
	}{
		{"claude adaptive + output_config max", sdktranslator.FormatClaude, `{"model":"m","max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`, "deepseek-v41-flash-1017-max"},
		{"claude adaptive without effort", sdktranslator.FormatClaude, `{"model":"m","max_tokens":64,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`, ""},
		{"claude enabled budget", sdktranslator.FormatClaude, `{"model":"m","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`, ""},
		{"gemini thinkingLevel", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`, ""},
		{"gemini thinkingBudget", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":1024}}}`, ""},
		{"openai reasoning_effort", sdktranslator.FormatOpenAI, `{"model":"m","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`, "deepseek-v41-flash-1017-low"},
		{"responses reasoning.effort", sdktranslator.FormatOpenAIResponse, `{"model":"m","reasoning":{"effort":"high"},"input":[{"role":"user","content":"hi"}]}`, "deepseek-v41-flash-1017-high"},
		{"no effort", sdktranslator.FormatOpenAI, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, ""},
	}
	for _, tc := range cases {
		raw := []byte(tc.body)
		opts := cliproxyexecutor.Options{SourceFormat: tc.format, OriginalRequest: raw}
		unpinned, err := exec.buildUpstreamPlan(context.Background(), cliproxyexecutor.Request{Model: family, Payload: raw}, opts, false)
		if err != nil {
			t.Fatalf("%s: unpinned plan: %v", tc.name, err)
		}
		if tc.want != "" && unpinned.upstreamModel != tc.want {
			t.Fatalf("%s: unpinned executor chose %q, want %q", tc.name, unpinned.upstreamModel, tc.want)
		}

		pinnedModel := cursorwire.ClientModelID(cursorwire.ResolveUpstreamModel(family, cursorwire.RequestEffort(tc.format.String(), family, raw)))
		pinnedOpts := opts
		pinnedOpts.Metadata = map[string]any{cliproxyexecutor.CursorPinnedModelMetadataKey: true}
		pinned, err := exec.buildUpstreamPlan(context.Background(), cliproxyexecutor.Request{Model: pinnedModel, Payload: raw}, pinnedOpts, false)
		if err != nil {
			t.Fatalf("%s: pinned plan: %v", tc.name, err)
		}
		if pinned.upstreamModel != unpinned.upstreamModel {
			t.Errorf("%s: pinned %q runs %q, unpinned executor chose %q", tc.name, pinnedModel, pinned.upstreamModel, unpinned.upstreamModel)
		}
	}
}
