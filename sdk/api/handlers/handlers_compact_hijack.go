package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/MoeForge/moego/ojson"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

const (
	clientCompactSystemMarker    = "context summarization assistant"
	codexInternalCompactHdr      = "X-Codex-Internal-Compaction"
	clientCompactMaxPayloadChars = 320000
	clientCompactConvLimitChars  = 300000
)

func isLikelyCodexTargetModel(h *BaseAPIHandler, modelName string, execOptions modelExecutionOptions) bool {
	m := strings.ToLower(strings.TrimSpace(modelName))
	if m == "" {
		return false
	}
	checkSingle := func(candidate string) bool {
		c := strings.ToLower(strings.TrimSpace(candidate))
		if strings.HasPrefix(c, "codex/") || strings.HasPrefix(c, "cx/") {
			return true
		}
		if idx := strings.IndexByte(c, '/'); idx != -1 {
			prov := c[:idx]
			if prov != "codex" && prov != "cx" {
				return false
			}
			c = c[idx+1:]
		}
		return strings.HasPrefix(c, "gpt-") || strings.HasPrefix(c, "codex") ||
			strings.HasPrefix(c, "o1") || strings.HasPrefix(c, "o3") || strings.HasPrefix(c, "o4")
	}
	if checkSingle(m) {
		return true
	}
	if h != nil {
		if combo, ok := h.resolveCombo(modelName, execOptions); ok && combo != nil {
			for _, cm := range combo.Models {
				if checkSingle(cm) {
					return true
				}
			}
		}
	}
	return false
}

func extractCompactItemBlobText(item ojson.Result) string {
	if !item.Exists() {
		return ""
	}
	content := item.Get("content")
	if content.Type == ojson.String {
		return content.String()
	}
	if content.IsArray() {
		var parts []string
		for _, c := range content.Array() {
			if txt := c.Get("text"); txt.Type == ojson.String && txt.String() != "" {
				parts = append(parts, txt.String())
			}
		}
		return strings.Join(parts, "\n")
	}
	if txt := item.Get("text"); txt.Type == ojson.String {
		return txt.String()
	}
	return ""
}

func boundConversationTagString(text string) (string, bool) {
	if len(text) <= clientCompactMaxPayloadChars {
		return text, false
	}
	const openTag = "<conversation>"
	const closeTag = "</conversation>"
	convStart := strings.Index(text, openTag)
	convEnd := strings.LastIndex(text, closeTag)
	if convStart == -1 || convEnd <= convStart+len(openTag) {
		return text, false
	}
	prefix := text[:convStart+len(openTag)]
	suffix := text[convEnd:]
	conv := text[convStart+len(openTag) : convEnd]
	if len(conv) <= clientCompactConvLimitChars {
		return text, false
	}
	omitted := len(conv) - clientCompactConvLimitChars
	trimmedConv := fmt.Sprintf("%s\n…[middle %dch omitted for summarizer budget]…\n%s", conv[:80000], omitted, conv[len(conv)-220000:])
	return prefix + trimmedConv + suffix, true
}

// maybeHijackClientCompaction detects client-side /compact summarization turns
// (e.g. Pi coding agent's "You are a context summarization assistant..." with no tools
// and <= 4 items) targeting a Codex model and transparently reroutes them to the cheap
// external summarizer model/combo BEFORE combo resolution.
func (h *BaseAPIHandler) maybeHijackClientCompaction(ctx context.Context, modelName string, rawJSON []byte, execOptions modelExecutionOptions) (string, []byte) {
	if h == nil || len(rawJSON) == 0 || execOptions.InternalSource {
		return modelName, rawJSON
	}
	cfg := h.CurrentConfig()
	if cfg == nil || !cfg.CodexCompactHijackEnabled() {
		return modelName, rawJSON
	}
	hdrs := modelExecutionHeaders(ctx, execOptions.Headers)
	if hdrs != nil && strings.TrimSpace(hdrs.Get(codexInternalCompactHdr)) != "" {
		return modelName, rawJSON
	}
	if !bytes.Contains(rawJSON, []byte(clientCompactSystemMarker)) {
		return modelName, rawJSON
	}

	target := strings.TrimSpace(os.Getenv("CODEX_COMPACT_HIJACK_MODEL"))
	if target == "" {
		target = strings.TrimSpace(cfg.EffectiveCodexCompactHijackModel())
	}
	if target == "" || strings.EqualFold(target, strings.TrimSpace(modelName)) {
		return modelName, rawJSON
	}
	if !isLikelyCodexTargetModel(h, modelName, execOptions) {
		return modelName, rawJSON
	}

	if tools := ojson.GetBytesNoCopy(rawJSON, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		return modelName, rawJSON
	}

	itemsRes := ojson.GetBytesNoCopy(rawJSON, "input")
	if !itemsRes.IsArray() {
		itemsRes = ojson.GetBytesNoCopy(rawJSON, "messages")
	}
	if !itemsRes.IsArray() {
		return modelName, rawJSON
	}
	items := itemsRes.Array()
	if len(items) == 0 || len(items) > 4 {
		return modelName, rawJSON
	}

	var blobParts []string
	if inst := ojson.GetBytesNoCopy(rawJSON, "instructions"); inst.Type == ojson.String && inst.String() != "" {
		blobParts = append(blobParts, inst.String())
	}
	limit := len(items)
	if limit > 2 {
		limit = 2
	}
	for i := 0; i < limit; i++ {
		blobParts = append(blobParts, extractCompactItemBlobText(items[i]))
	}
	blob := strings.Join(blobParts, "\n")
	if !strings.Contains(blob, clientCompactSystemMarker) {
		return modelName, rawJSON
	}

	var parsed map[string]any
	if err := json.Unmarshal(rawJSON, &parsed); err != nil || parsed == nil {
		return modelName, rawJSON
	}
	parsed["model"] = target

	boundItems := func(arr []any) {
		for _, rawIt := range arr {
			it, ok := rawIt.(map[string]any)
			if !ok || it == nil {
				continue
			}
			switch content := it["content"].(type) {
			case string:
				if bounded, changed := boundConversationTagString(content); changed {
					it["content"] = bounded
				}
			case []any:
				for _, rawPart := range content {
					if part, ok := rawPart.(map[string]any); ok && part != nil {
						if txt, ok := part["text"].(string); ok {
							if bounded, changed := boundConversationTagString(txt); changed {
								part["text"] = bounded
							}
						}
					}
				}
			}
		}
	}
	if inp, ok := parsed["input"].([]any); ok {
		boundItems(inp)
	}
	if msgs, ok := parsed["messages"].([]any); ok {
		boundItems(msgs)
	}

	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return modelName, rawJSON
	}
	log.Infof("[CODEX-COMPACT-HIJACK] client compaction detected (model=%s, items=%d) -> rerouted to %s", modelName, len(items), target)
	return target, rewritten
}
