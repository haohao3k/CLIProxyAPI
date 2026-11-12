package helps

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// IsZAIUpstream reports whether an OpenAI-compatible request targets the Z.ai
// (GLM) API: either the built-in "zai" provider or a compat entry whose base
// URL points at api.z.ai / open.bigmodel.cn.
func IsZAIUpstream(provider, baseURL string) bool {
	if strings.EqualFold(strings.TrimSpace(provider), "zai") {
		return true
	}
	parsed, errParse := url.Parse(strings.TrimSpace(baseURL))
	if errParse != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "api.z.ai" || host == "open.bigmodel.cn"
}

// NormalizeZAIThinkingDoc maps the canonical OpenAI reasoning_effort produced
// by the thinking pipeline onto GLM's thinking switch. GLM's chat completions
// API controls reasoning with thinking.type ("enabled"/"disabled") and has no
// effort levels, so "none" disables thinking and every other level enables it.
// Requests without reasoning_effort are left untouched (GLM's server default
// applies, and an explicit client-provided thinking object passes through).
// It returns true when doc was modified.
func NormalizeZAIThinkingDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	effortRes := doc.Get("reasoning_effort")
	if !effortRes.Exists() {
		return false
	}
	effort := strings.ToLower(strings.TrimSpace(effortRes.String()))
	_ = doc.Delete("reasoning_effort")
	if effort == "" {
		return true
	}
	thinkingType := "enabled"
	if effort == "none" {
		thinkingType = "disabled"
	}
	if thinkingRes := doc.Get("thinking"); thinkingRes.Exists() && !thinkingRes.IsObject() {
		_ = doc.Delete("thinking")
	}
	_ = doc.Set("thinking.type", thinkingType)
	return true
}

// NormalizeChatDeveloperRoleDoc converts OpenAI Responses "developer" message roles
// into standard Chat Completions "system" roles for non-OpenAI chat upstreams (such
// as Z.ai GLM Coding PaaS v4 and OpenCode Go /chat/completions) that reject
// role="developer" with HTTP 400 (e.g. Z.ai code 1214 "Incorrect role information").
func NormalizeChatDeveloperRoleDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	messages := doc.Get("messages")
	if !messages.IsArray() {
		return false
	}
	changed := false
	for i, msg := range messages.Array() {
		if strings.EqualFold(strings.TrimSpace(msg.Get("role").String()), "developer") {
			path := fmt.Sprintf("messages.%d", i)
			_ = doc.Set(path+".role", "system")
			flattenTextOnlyChatContentDoc(doc, path, msg.Get("content"))
			changed = true
		}
	}
	return changed
}
