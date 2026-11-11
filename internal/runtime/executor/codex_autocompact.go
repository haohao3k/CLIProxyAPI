package executor

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	codexAutocompactMaxEntries     = 128
	codexAutocompactCacheTTL       = 60 * time.Minute
	codexAutocompactBase64TokenCap = 2000
	codexAutocompactExternalCap    = 20000
	codexExternalFailureCooldown   = 5 * time.Minute

	CodexInternalCompactionHeader = "X-Codex-Internal-Compaction"

	codexCompactPrompt = `Summarize the conversation so far for continuation purposes. Include:
1. The user's original task/goal (keep key requirements verbatim)
2. Key decisions made and their reasons
3. Current state: files created/modified (paths + brief purpose), commands run and outcomes
4. Errors encountered, how they were resolved or which remain open
5. What was being worked on most recently and the immediate next steps
Be specific (file paths, function names, exact values). Keep it under 1500 words.
Do not copy padding markers or protocol strings from earlier tool arguments.
Respond with the summary text only.`

	codexWindowNote = `[9router window] Earlier conversation history was omitted to keep the context small. The original task request is preserved below; the task's files live on disk — re-read them with tools if you need their contents. Continue the task from the recent messages after it.`
)

var (
	codexB64Re     = regexp.MustCompile(`^(?:data:[^;,\s]+;base64,)?[A-Za-z0-9+/_-]{160,}={0,2}$`)
	codexB64TailRe = regexp.MustCompile(`^[A-Za-z0-9+/_-]{160,}={0,2}$`)

	codexWrapperUserPrefixes = []string{
		"<environment_context>",
		"<user_instructions>",
		"<skills_instructions>",
		"# AGENTS.md",
		"<context_window_guidance>",
		"<token_budget>",
	}
	codexOwnNotePrefixes = []string{
		"[9router window]",
		"[9router auto-compact]",
		"[cpa window]",
		"[cpa auto-compact]",
	}

	codexCallTypes = map[string]bool{
		"function_call":    true,
		"custom_tool_call": true,
		"local_shell_call": true,
		"web_search_call":  true,
	}
	codexOutputTypes = map[string]bool{
		"function_call_output":    true,
		"custom_tool_call_output": true,
		"local_shell_call_output": true,
	}

	codexAutocompactMu    sync.Mutex
	codexAutocompactCache = make(map[string]*codexAutocompactCacheEntry)

	codexExternalFailMu   sync.Mutex
	codexExternalFailures = make(map[string]time.Time)

	codexLoopbackHTTPClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // loopback only
		},
	}
)

type codexAutocompactCacheEntry struct {
	fingerprint string
	text        string
	cutAtIndex  int
	windowOnly  bool
	sessionID   string
	createdAt   time.Time
}

func resetCodexAutocompactCacheForTests() {
	codexAutocompactMu.Lock()
	codexAutocompactCache = make(map[string]*codexAutocompactCacheEntry)
	codexAutocompactMu.Unlock()

	codexExternalFailMu.Lock()
	codexExternalFailures = make(map[string]time.Time)
	codexExternalFailMu.Unlock()
}

func autocompactEnvInt(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func isBase64OrDataURI(v string) bool {
	if len(v) < 160 {
		return false
	}
	headEnd := len(v)
	if headEnd > 64 {
		headEnd = 64
	}
	if strings.HasPrefix(v, "data:") && strings.Contains(v[:headEnd], ";base64,") {
		return true
	}
	if len(v) > 4096 {
		return codexB64Re.MatchString(v[:512]) && codexB64TailRe.MatchString(v[len(v)-512:])
	}
	return codexB64Re.MatchString(v)
}

func estimateCodexTokensOf(v any) int {
	switch val := v.(type) {
	case nil:
		return 0
	case string:
		if val == "" {
			return 0
		}
		if isBase64OrDataURI(val) {
			est := (len(val) + 9) / 10
			if est > codexAutocompactBase64TokenCap {
				return codexAutocompactBase64TokenCap
			}
			return est
		}
		nonASCII := 0
		for i := 0; i < len(val); i++ {
			if val[i] > 127 {
				nonASCII++
			}
		}
		if float64(nonASCII) > float64(len(val))*0.3 {
			return int(math.Ceil(float64(len(val)) / 2.5))
		}
		return (len(val) + 3) / 4
	case []any:
		sum := 0
		for _, item := range val {
			sum += estimateCodexTokensOf(item)
		}
		return sum
	case map[string]any:
		sum := 0
		for _, item := range val {
			sum += estimateCodexTokensOf(item)
		}
		return sum
	default:
		return 0
	}
}

func fastObjectCharLen(v any) int {
	switch val := v.(type) {
	case nil:
		return 4
	case string:
		return len(val) + 2
	case float64, int, int64, bool:
		return 4
	case []any:
		s := 2
		for _, elem := range val {
			s += fastObjectCharLen(elem) + 1
		}
		return s
	case map[string]any:
		s := 2
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s += len(k) + 3 + fastObjectCharLen(val[k]) + 1
		}
		return s
	default:
		return 1
	}
}

func fingerprintCodexItems(items []any, endExclusive int) string {
	h := sha1.New()
	if endExclusive > len(items) {
		endExclusive = len(items)
	}
	for i := 0; i < endExclusive; i++ {
		_, _ = h.Write([]byte(strconv.Itoa(fastObjectCharLen(items[i]))))
		_, _ = h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func isOwnCompactionNote(text string) bool {
	t := strings.TrimLeft(text, " \t\r\n")
	for _, p := range codexOwnNotePrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func codexUserItemText(it map[string]any) string {
	if it == nil {
		return ""
	}
	switch content := it["content"].(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, raw := range content {
			switch c := raw.(type) {
			case string:
				parts = append(parts, c)
			case map[string]any:
				if txt, ok := c["text"].(string); ok {
					parts = append(parts, txt)
				}
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func isWrapperUserItem(it map[string]any) bool {
	t := strings.TrimLeft(codexUserItemText(it), " \t\r\n")
	for _, p := range codexWrapperUserPrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// trimOversizedOutputs trims oversized tool outputs in place.
// Iron Rule #5: MUST always be called with keepRecent = 0 so tool outputs > maxChars
// are trimmed on their VERY FIRST turn and remain 100% immutable across subsequent turns.
func trimOversizedOutputs(items []any, maxChars int, keepRecent int) int {
	if len(items) == 0 || maxChars < 500 {
		return 0
	}
	trimmed := 0
	half := maxChars / 2
	recentMaxChars := maxChars * 2
	if recentMaxChars < 60000 {
		recentMaxChars = 60000
	}
	recentHalf := recentMaxChars / 2

	for i := 0; i < len(items); i++ {
		isRecent := keepRecent > 0 && i >= len(items)-keepRecent
		capLen := maxChars
		h := half
		if isRecent {
			capLen = recentMaxChars
			h = recentHalf
		}
		it, ok := items[i].(map[string]any)
		if !ok || it == nil {
			continue
		}
		itType, _ := it["type"].(string)
		if !codexOutputTypes[itType] {
			continue
		}
		switch out := it["output"].(type) {
		case string:
			if len(out) > capLen {
				omitted := len(out) - capLen
				it["output"] = fmt.Sprintf("%s\n\n[...9router: trimmed %d chars of this old tool output to save context...]\n\n%s", out[:h], omitted, out[len(out)-h:])
				trimmed++
			}
		case []any:
			for _, rawP := range out {
				if p, ok := rawP.(map[string]any); ok && p != nil {
					if txt, ok := p["text"].(string); ok && len(txt) > capLen {
						omitted := len(txt) - capLen
						p["text"] = fmt.Sprintf("%s\n\n[...9router: trimmed %d chars of this old tool output to save context...]\n\n%s", txt[:h], omitted, txt[len(txt)-h:])
						trimmed++
					}
				}
			}
		}
	}
	return trimmed
}

func adjustCodexCutBoundary(items []any, k int) int {
	guard := 0
	for k < len(items)-1 && guard < 1000 {
		guard++
		var prev, cur map[string]any
		if k > 0 {
			prev, _ = items[k-1].(map[string]any)
		}
		cur, _ = items[k].(map[string]any)
		prevType := autocutStrField(prev, "type")
		curType := autocutStrField(cur, "type")
		if prev != nil && codexCallTypes[prevType] && cur != nil && codexOutputTypes[curType] && autocutStrField(cur, "call_id") == autocutStrField(prev, "call_id") {
			k++
			continue
		}
		if cur != nil && codexOutputTypes[curType] {
			k++
			continue
		}
		break
	}
	return k
}

func findCodexCutPoint(items []any, keepTokenBudget int) int {
	maxKeptItems := len(items)
	if len(items) > 150 {
		maxKeptItems = autocompactEnvInt("CODEX_AUTOCOMPACT_MAX_KEEP_ITEMS", 80)
	}
	acc := 0
	k := len(items)
	kept := 0
	for i := len(items) - 1; i >= 0; i-- {
		if kept >= 4 && (acc >= keepTokenBudget || (maxKeptItems > 0 && kept >= maxKeptItems)) {
			k = i + 1
			break
		}
		acc += estimateCodexTokensOf(items[i])
		kept++
		k = i
	}
	if k < 2 {
		k = len(items) - 4
		if k < 2 {
			k = 2
		}
	}
	if len(items)-k < 2 || k < 2 {
		return -1
	}
	k2 := adjustCodexCutBoundary(items, k)
	keptEst := 0
	for i := k2; i < len(items); i++ {
		keptEst += estimateCodexTokensOf(items[i])
	}
	limitEst := int(float64(keepTokenBudget) * 1.5)
	for keptEst > limitEst && len(items)-k2 > 1 {
		keptEst -= estimateCodexTokensOf(items[k2])
		k2++
	}
	k2 = adjustCodexCutBoundary(items, k2)
	if k2 == len(items)-1 && k2 > 0 {
		if cur, ok := items[k2].(map[string]any); ok && codexOutputTypes[autocutStrField(cur, "type")] {
			cid := autocutStrField(cur, "call_id")
			floor := k2 - 6
			if floor < 0 {
				floor = 0
			}
			for j := k2 - 1; j >= floor; j-- {
				if prev, ok := items[j].(map[string]any); ok && codexCallTypes[autocutStrField(prev, "type")] && (cid == "" || autocutStrField(prev, "call_id") == cid) {
					k2 = j
					break
				}
			}
		}
	}
	if len(items)-k2 < 1 {
		return -1
	}
	return k2
}

func codexHeadAnchorCount(items []any, capTokens int) int {
	if capTokens <= 0 || len(items) == 0 {
		return 0
	}
	// Iron Rule #5: maxHeadItems = 60 unconditional
	maxHeadItems := autocompactEnvInt("CODEX_AUTOCOMPACT_MAX_HEAD_ITEMS", 60)
	limit := len(items)
	if maxHeadItems > 0 && maxHeadItems < limit {
		limit = maxHeadItems
	}
	acc := 0
	H := 0
	for i := 0; i < limit; i++ {
		it, _ := items[i].(map[string]any)
		if it != nil && isOwnCompactionNote(codexUserItemText(it)) {
			break
		}
		est := estimateCodexTokensOf(items[i])
		if acc+est > capTokens {
			break
		}
		acc += est
		H = i + 1
	}
	return H
}

func isAnyCodexCallItem(raw any) bool {
	it, ok := raw.(map[string]any)
	if !ok || it == nil {
		return false
	}
	t := autocutStrField(it, "type")
	return codexCallTypes[t] || t == "shell_call" || t == "apply_patch_call" || t == "mc_tool_call"
}

func isAnyCodexOutputItem(raw any) bool {
	it, ok := raw.(map[string]any)
	if !ok || it == nil {
		return false
	}
	t := autocutStrField(it, "type")
	return codexOutputTypes[t] || t == "shell_call_output" || t == "apply_patch_call_output"
}

func dropOrphanCodexOutputs(items []any) []any {
	callIDs := make(map[string]bool)
	for _, raw := range items {
		it, ok := raw.(map[string]any)
		if !ok || it == nil {
			continue
		}
		t := autocutStrField(it, "type")
		if codexCallTypes[t] {
			if cid := autocutStrField(it, "call_id"); cid != "" {
				callIDs[cid] = true
			}
		} else if t == "shell_call" || t == "apply_patch_call" || t == "mc_tool_call" {
			cid := autocutStrField(it, "call_id")
			if cid == "" {
				cid = autocutStrField(it, "id")
			}
			if cid != "" {
				callIDs[cid] = true
			}
		}
	}
	out := make([]any, 0, len(items))
	for _, raw := range items {
		it, ok := raw.(map[string]any)
		if ok && it != nil {
			t := autocutStrField(it, "type")
			if codexOutputTypes[t] || t == "shell_call_output" || t == "apply_patch_call_output" {
				if cid := autocutStrField(it, "call_id"); cid != "" && !callIDs[cid] {
					continue
				}
			}
		}
		out = append(out, raw)
	}
	return out
}

func sameAnyRef(slice []any, target map[string]any) bool {
	if target == nil {
		return false
	}
	for _, elem := range slice {
		if m, ok := elem.(map[string]any); ok && len(m) == len(target) {
			// Compare by serialized length + text as map equality proxy
			if fastObjectCharLen(m) == fastObjectCharLen(target) && codexUserItemText(m) == codexUserItemText(target) {
				return true
			}
		}
	}
	return false
}

// rewriteCodexInput rewrites body["input"] into [head anchor] + [summary/window note] + [firstUser] + [kept tail]
// using retreating pair-atomic boundary H -= 1 (Iron Rule #5).
func rewriteCodexInput(body map[string]any, text string, cutAtIndex int, headCapTokens int) {
	original, ok := body["input"].([]any)
	if !ok || cutAtIndex <= 0 || cutAtIndex > len(original) {
		return
	}
	kept := make([]any, len(original)-cutAtIndex)
	copy(kept, original[cutAtIndex:])

	var firstUser map[string]any
	for i := 0; i < cutAtIndex; i++ {
		it, ok := original[i].(map[string]any)
		if !ok || it == nil {
			continue
		}
		role := autocutStrField(it, "role")
		itType := autocutStrField(it, "type")
		if (role == "user" || itType == "message") && role != "assistant" &&
			!strings.Contains(itType, "call") && !strings.Contains(itType, "output") && !strings.Contains(itType, "reasoning") &&
			!isWrapperUserItem(it) && !isOwnCompactionNote(codexUserItemText(it)) && len(strings.TrimSpace(codexUserItemText(it))) >= 8 {
			firstUser = it
			break
		}
	}

	kept = dropOrphanCodexOutputs(kept)
	cleanedKept := make([]any, 0, len(kept))
	for _, raw := range kept {
		if it, ok := raw.(map[string]any); ok && it != nil && isOwnCompactionNote(codexUserItemText(it)) {
			continue
		}
		cleanedKept = append(cleanedKept, raw)
	}
	kept = cleanedKept

	if headCapTokens <= 0 {
		headCapTokens = autocompactEnvInt("CODEX_AUTOCOMPACT_HEAD_ANCHOR_TOKENS", 40000)
	}
	H := codexHeadAnchorCount(original, headCapTokens)
	// Iron Rule #5: Retreating pair-atomic boundary H -= 1
	for H > 0 && (isAnyCodexOutputItem(original[H]) || isAnyCodexCallItem(original[H-1])) {
		H--
	}
	if H >= cutAtIndex {
		H = 0
	}

	var head []any
	if H > 0 {
		head = make([]any, H)
		copy(head, original[:H])
	}

	noteItem := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": text},
		},
	}

	rebuilt := make([]any, 0, len(head)+2+len(kept))
	rebuilt = append(rebuilt, head...)
	rebuilt = append(rebuilt, noteItem)
	if firstUser != nil && !sameAnyRef(kept, firstUser) && !sameAnyRef(head, firstUser) {
		rebuilt = append(rebuilt, firstUser)
	}
	rebuilt = append(rebuilt, kept...)
	body["input"] = rebuilt
}

func storeAutocompactCache(fp, text string, cutAtIndex int, windowOnly bool, sessionID string) {
	now := time.Now()
	codexAutocompactMu.Lock()
	defer codexAutocompactMu.Unlock()

	for k, v := range codexAutocompactCache {
		if now.Sub(v.createdAt) > codexAutocompactCacheTTL {
			delete(codexAutocompactCache, k)
		}
	}
	for len(codexAutocompactCache) >= codexAutocompactMaxEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range codexAutocompactCache {
			if oldestKey == "" || v.createdAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.createdAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(codexAutocompactCache, oldestKey)
	}
	codexAutocompactCache[fp] = &codexAutocompactCacheEntry{
		fingerprint: fp,
		text:        text,
		cutAtIndex:  cutAtIndex,
		windowOnly:  windowOnly,
		sessionID:   sessionID,
		createdAt:   now,
	}
}

func snapshotSortedAutocompactCache(sessionID string) []*codexAutocompactCacheEntry {
	now := time.Now()
	codexAutocompactMu.Lock()
	var list []*codexAutocompactCacheEntry
	for k, v := range codexAutocompactCache {
		if now.Sub(v.createdAt) > codexAutocompactCacheTTL {
			delete(codexAutocompactCache, k)
			continue
		}
		cp := *v
		list = append(list, &cp)
	}
	codexAutocompactMu.Unlock()

	sort.Slice(list, func(i, j int) bool {
		if sessionID != "" {
			iMatch := list[i].sessionID == sessionID
			jMatch := list[j].sessionID == sessionID
			if iMatch != jMatch {
				return iMatch
			}
		}
		return list[i].cutAtIndex > list[j].cutAtIndex
	})
	return list
}

func touchAutocompactCache(fp, sessionID string) {
	codexAutocompactMu.Lock()
	if entry, ok := codexAutocompactCache[fp]; ok && entry != nil {
		entry.createdAt = time.Now()
		if sessionID != "" && entry.sessionID == "" {
			entry.sessionID = sessionID
		}
	}
	codexAutocompactMu.Unlock()
}

func resolveExternalSummarizerTarget(cfg *config.Config) (targetURL string, apiKey string, modelChain []string) {
	var cxCfg *config.CodexConfig
	if cfg != nil {
		cxCfg = &cfg.Codex
	}

	targetURL = strings.TrimSpace(os.Getenv("CODEX_AUTOCOMPACT_EXTERNAL_URL"))
	if targetURL == "" {
		port := 8317
		scheme := "http"
		if cfg != nil {
			if cfg.Port > 0 {
				port = cfg.Port
			}
			if cfg.TLS.Enable {
				scheme = "https"
			}
		}
		targetURL = fmt.Sprintf("%s://127.0.0.1:%d/v1/responses", scheme, port)
	}

	apiKey = strings.TrimSpace(os.Getenv("CODEX_AUTOCOMPACT_EXTERNAL_KEY"))
	if apiKey == "" && cfg != nil {
		for _, k := range cfg.APIKeys {
			if trimmed := strings.TrimSpace(k); trimmed != "" {
				apiKey = trimmed
				break
			}
		}
	}

	seen := make(map[string]bool)
	var rawChain []string
	addModel := func(m string) {
		for _, part := range strings.Split(m, ",") {
			t := strings.TrimSpace(part)
			if t != "" && !seen[t] {
				seen[t] = true
				rawChain = append(rawChain, t)
			}
		}
	}
	addModel(cxCfg.EffectiveAutocompactExternalModel())
	for _, m := range cxCfg.EffectiveAutocompactExternalModels() {
		addModel(m)
	}

	now := time.Now()
	var healthy, cooling []string
	codexExternalFailMu.Lock()
	for _, m := range rawChain {
		if failedAt, ok := codexExternalFailures[m]; ok {
			if now.Sub(failedAt) < codexExternalFailureCooldown {
				cooling = append(cooling, m)
				continue
			}
			delete(codexExternalFailures, m)
		}
		healthy = append(healthy, m)
	}
	codexExternalFailMu.Unlock()

	modelChain = append(healthy, cooling...)
	return targetURL, apiKey, modelChain
}

func markExternalModelFailure(model string) {
	codexExternalFailMu.Lock()
	codexExternalFailures[model] = time.Now()
	codexExternalFailMu.Unlock()
}

func clearExternalModelFailure(model string) {
	codexExternalFailMu.Lock()
	delete(codexExternalFailures, model)
	codexExternalFailMu.Unlock()
}

func compactCodexViaExternal(ctx context.Context, cfg *config.Config, items []any, from, to int) string {
	targetURL, apiKey, modelChain := resolveExternalSummarizerTarget(cfg)
	if targetURL == "" || len(modelChain) == 0 {
		return ""
	}

	var payloadBuf strings.Builder
	span := to - from
	const maxItemsToSerialize = 200
	for i := from; i < to; i++ {
		if span > maxItemsToSerialize && i == from+40 {
			skipped := to - 160 - i
			if skipped > 0 {
				payloadBuf.WriteString(fmt.Sprintf("\n…[%d intermediate conversation items omitted to bound summarizer payload]…\n", skipped))
				i = to - 160 - 1
				continue
			}
		}
		b, err := json.Marshal(items[i])
		if err != nil || len(b) == 0 {
			continue
		}
		s := string(b)
		if len(s) > codexAutocompactExternalCap {
			s = fmt.Sprintf("%s\n…[item truncated, %dch total]…\n%s", s[:10000], len(s), s[len(s)-8000:])
		}
		payloadBuf.WriteString(s)
		payloadBuf.WriteByte('\n')
	}

	payload := payloadBuf.String()
	if payload == "" {
		return ""
	}
	if len(payload) > 320000 {
		payload = fmt.Sprintf("%s\n…[middle %dch omitted for summarizer budget]…\n%s", payload[:80000], len(payload)-320000, payload[len(payload)-240000:])
	}

	content := fmt.Sprintf(
		"%s\n\nThe conversation items below are JSON-encoded (openai wire format, oldest first). Summarize them following the instructions above.\n\n<conversation>\n%s\n</conversation>",
		codexCompactPrompt, payload,
	)

	isResponsesEndpoint := strings.Contains(strings.ToLower(targetURL), "/responses")
	timeoutMs := autocompactEnvInt("CODEX_AUTOCOMPACT_EXTERNAL_TIMEOUT_MS", 240000)
	if timeoutMs < 5000 {
		timeoutMs = 5000
	}

	for _, model := range modelChain {
		for attempt := 1; attempt <= 2; attempt++ {
			if ctx != nil && ctx.Err() != nil {
				return ""
			}
			var reqObj map[string]any
			if isResponsesEndpoint {
				reqObj = map[string]any{
					"model": model,
					"input": []any{
						map[string]any{
							"type": "message",
							"role": "user",
							"content": []any{
								map[string]any{"type": "input_text", "text": content},
							},
						},
					},
					"stream": false,
				}
			} else {
				reqObj = map[string]any{
					"model": model,
					"messages": []any{
						map[string]any{"role": "user", "content": content},
					},
					"stream":      false,
					"temperature": 0.2,
				}
			}
			reqBytes, err := json.Marshal(reqObj)
			if err != nil {
				break
			}

			parentCtx := ctx
			if parentCtx == nil {
				parentCtx = context.Background()
			}
			attemptCtx, cancel := context.WithTimeout(parentCtx, time.Duration(timeoutMs)*time.Millisecond)
			httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, targetURL, bytes.NewReader(reqBytes))
			if err != nil {
				cancel()
				break
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set(CodexInternalCompactionHeader, "1")
			if apiKey != "" {
				httpReq.Header.Set("Authorization", "Bearer "+apiKey)
			}

			resp, errDo := codexLoopbackHTTPClient.Do(httpReq)
			if errDo != nil {
				cancel()
				markExternalModelFailure(model)
				log.Warnf("[AUTOCOMPACT] external compact failed on %s: %v", model, errDo)
				break
			}
			respBytes, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			cancel()

			if resp.StatusCode == http.StatusTooManyRequests && attempt < 2 {
				select {
				case <-time.After(2 * time.Second):
				case <-parentCtx.Done():
					return ""
				}
				continue
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				markExternalModelFailure(model)
				log.Warnf("[AUTOCOMPACT] external compact HTTP %d on %s", resp.StatusCode, model)
				break
			}

			summary, okExtract := extractSummaryFromExternalResponseBytes(respBytes)
			if !okExtract {
				markExternalModelFailure(model)
				break
			}

			summary = strings.ReplaceAll(summary, CodexPadKey, "")
			summary = strings.ReplaceAll(summary, CodexSentinelComplete, "")
			summary = strings.TrimSpace(summary)

			if len(summary) < 80 {
				markExternalModelFailure(model)
				log.Warnf("[AUTOCOMPACT] external compact summary too short on %s (%dch)", model, len(summary))
				break
			}

			clearExternalModelFailure(model)
			log.Infof("[AUTOCOMPACT] external compact via %s | payload %dKB | summary %dch", model, len(payload)/1024, len(summary))
			return summary
		}
	}
	return ""
}

func extractSummaryFromParsedResponseMap(parsed map[string]any) string {
	if parsed == nil {
		return ""
	}
	if choices, ok := parsed["choices"].([]any); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			if msg, ok := c0["message"].(map[string]any); ok {
				if s, ok := msg["content"].(string); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	}
	if outText, ok := parsed["output_text"].(string); ok && strings.TrimSpace(outText) != "" {
		return outText
	}
	target := parsed
	if respObj, ok := parsed["response"].(map[string]any); ok && respObj != nil {
		if outText, ok := respObj["output_text"].(string); ok && strings.TrimSpace(outText) != "" {
			return outText
		}
		target = respObj
	}
	if outArr, ok := target["output"].([]any); ok {
		var parts []string
		for _, rawIt := range outArr {
			it, _ := rawIt.(map[string]any)
			if autocutStrField(it, "type") != "message" {
				continue
			}
			if cArr, ok := it["content"].([]any); ok {
				for _, rawC := range cArr {
					if cm, ok := rawC.(map[string]any); ok {
						if txt, ok := cm["text"].(string); ok && txt != "" {
							parts = append(parts, txt)
						}
					}
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

func extractSummaryFromExternalResponseBytes(respBytes []byte) (string, bool) {
	trimmed := bytes.TrimSpace(respBytes)
	if len(trimmed) == 0 {
		return "", false
	}
	var parsed map[string]any
	if err := json.Unmarshal(trimmed, &parsed); err == nil && parsed != nil {
		return extractSummaryFromParsedResponseMap(parsed), true
	}
	// Fallback for dynamic OpenAI-Compat / Responses upstreams that stream SSE despite stream=false (9router commit 062b396d).
	if !bytes.Contains(trimmed, []byte("data:")) {
		return "", false
	}
	var deltaBuf strings.Builder
	var completedSummary string
	sawValidEvent := false
	for _, rawLine := range strings.Split(string(trimmed), "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil || ev == nil {
			continue
		}
		sawValidEvent = true
		evType := autocutStrField(ev, "type")
		switch evType {
		case "response.output_text.delta":
			if d, ok := ev["delta"].(string); ok {
				deltaBuf.WriteString(d)
			}
		case "response.completed", "response.done":
			if s := extractSummaryFromParsedResponseMap(ev); strings.TrimSpace(s) != "" {
				completedSummary = s
			}
		default:
			if choices, ok := ev["choices"].([]any); ok && len(choices) > 0 {
				if c0, ok := choices[0].(map[string]any); ok {
					if dMap, ok := c0["delta"].(map[string]any); ok {
						if c, ok := dMap["content"].(string); ok {
							deltaBuf.WriteString(c)
						}
					}
				}
			}
		}
	}
	if !sawValidEvent {
		return "", false
	}
	if strings.TrimSpace(completedSummary) != "" {
		return completedSummary, true
	}
	return deltaBuf.String(), true
}

// maybeCodexAutocompactMap runs Pass 0 (immutable tool-output trimming), Pass 1 (128-entry LRU cache),
// and Pass 2 (external summarizer chain or zero-cost window fallback) on body in place.
// Fail-open by design.
func maybeCodexAutocompactMap(ctx context.Context, cfg *config.Config, body map[string]any, sessionID string) {
	if body == nil {
		return
	}
	var cxCfg *config.CodexConfig
	if cfg != nil {
		cxCfg = &cfg.Codex
	}
	if !cxCfg.AutocompactActive() {
		return
	}
	threshold := cxCfg.EffectiveAutocompactThreshold()
	if threshold < 10000 {
		return
	}
	keepBudget := cxCfg.EffectiveAutocompactKeepTokens()
	if keepBudget < 2000 {
		keepBudget = 2000
	}
	windowOnlyTokens := autocompactEnvInt("CODEX_AUTOCOMPACT_WINDOW_ONLY_TOKENS", 600000)
	trimChars := autocompactEnvInt("CODEX_AUTOCOMPACT_TRIM_OUTPUTS_CHARS", 30000)
	maxItems := autocompactEnvInt("CODEX_AUTOCOMPACT_MAX_ITEMS", 150)
	headAnchorTokens := autocompactEnvInt("CODEX_AUTOCOMPACT_HEAD_ANCHOR_TOKENS", 40000)

	items, ok := body["input"].([]any)
	if !ok || len(items) < 2 {
		return
	}
	topTools, _ := body["tools"].([]any)
	hasTools := len(topTools) > 0
	if !hasTools {
		for _, rawIt := range items {
			if it, ok := rawIt.(map[string]any); ok && autocutStrField(it, "type") == "additional_tools" {
				if st, _ := it["tools"].([]any); len(st) > 0 {
					hasTools = true
					break
				}
			}
		}
	}

	// Pass 0 (Iron Rule #5): trim oversized tool outputs immediately with keepRecent = 0
	if trimmed := trimOversizedOutputs(items, trimChars, 0); trimmed > 0 {
		log.Debugf("[AUTOCOMPACT] trimmed %d oversized tool output(s) > %dch", trimmed, trimChars)
	}

	est0 := estimateCodexTokensOf(body)
	overItemCap := maxItems > 0 && len(items) > maxItems
	if est0 < threshold && !overItemCap {
		return
	}
	if !hasTools {
		log.Debugf("[AUTOCOMPACT] est %d >= %d but request carries no tools (compact-shaped) -> run verbatim", est0, threshold)
		return
	}

	origItems := make([]any, len(items))
	copy(origItems, items)

	// Pass 1: 128-entry LRU compaction cache
	sortedCache := snapshotSortedAutocompactCache(sessionID)
	fpMemo := make(map[int]string)
	bestPriorSummary := ""

	for _, entry := range sortedCache {
		if sessionID != "" && entry.sessionID != "" && entry.sessionID != sessionID {
			continue
		}
		if entry.cutAtIndex < len(origItems)-1 {
			computedFp, ok := fpMemo[entry.cutAtIndex]
			if !ok {
				computedFp = fingerprintCodexItems(origItems, entry.cutAtIndex)
				fpMemo[entry.cutAtIndex] = computedFp
			}
			if computedFp == entry.fingerprint {
				if !entry.windowOnly && entry.text != "" && bestPriorSummary == "" {
					bestPriorSummary = entry.text
				}
				restored := make([]any, len(origItems))
				copy(restored, origItems)
				body["input"] = restored
				rewriteCodexInput(body, entry.text, entry.cutAtIndex, headAnchorTokens)
				postEst := estimateCodexTokensOf(body)
				curInput, _ := body["input"].([]any)
				postOverItems := maxItems > 0 && len(origItems) > maxItems && len(curInput) > maxItems
				if postEst < threshold && !postOverItems {
					touchAutocompactCache(entry.fingerprint, sessionID)
					RecordCodexSpecialTelemetryEvent("autocompact_cache")
					log.Infof("[AUTOCOMPACT] rewrite (cache) | items %d->%d | est %d->%d tokens", len(origItems), len(curInput), est0, postEst)
					return
				}
				restoredOrig := make([]any, len(origItems))
				copy(restoredOrig, origItems)
				body["input"] = restoredOrig
			}
		}
	}

	// Pass 2: fresh cut
	items, _ = body["input"].([]any)
	k := findCodexCutPoint(items, keepBudget)
	if k < 0 {
		return
	}
	keptEst := 0
	for i := k; i < len(items); i++ {
		keptEst += estimateCodexTokensOf(items[i])
	}

	targetURL, _, modelChain := resolveExternalSummarizerTarget(cfg)
	hasExternal := targetURL != "" && len(modelChain) > 0

	if (!hasExternal && est0 >= windowOnlyTokens) || float64(keptEst) > float64(est0)*0.6 {
		fp := fingerprintCodexItems(items, k)
		fallbackText := bestPriorSummary
		isWinOnly := false
		if fallbackText == "" {
			fallbackText = codexWindowNote
			isWinOnly = true
		}
		storeAutocompactCache(fp, fallbackText, k, isWinOnly, sessionID)
		rewriteCodexInput(body, fallbackText, k, headAnchorTokens)
		curInput, _ := body["input"].([]any)
		RecordCodexSpecialTelemetryEvent("autocompact_window")
		log.Infof("[AUTOCOMPACT] window-only rewrite | est %d->%d tokens | items %d->%d", est0, estimateCodexTokensOf(body), len(items), len(curInput))
		return
	}

	fp := fingerprintCodexItems(items, k)
	summary := compactCodexViaExternal(ctx, cfg, items, 0, k)
	if len(summary) < 80 {
		fallbackText := bestPriorSummary
		isWinOnly := false
		if fallbackText == "" {
			fallbackText = codexWindowNote
			isWinOnly = true
		}
		storeAutocompactCache(fp, fallbackText, k, isWinOnly, sessionID)
		rewriteCodexInput(body, fallbackText, k, headAnchorTokens)
		curInput, _ := body["input"].([]any)
		RecordCodexSpecialTelemetryEvent("autocompact_window")
		log.Infof("[AUTOCOMPACT] window-only rewrite (external unavailable) | est %d->%d tokens | items %d->%d", est0, estimateCodexTokensOf(body), len(items), len(curInput))
		return
	}

	text := fmt.Sprintf("[9router auto-compact] The earlier conversation history was compacted for continuation. Summary of everything before the recent messages:\n\n%s\n\n[End of summary — the recent messages follow.]", summary)
	storeAutocompactCache(fp, text, k, false, sessionID)
	rewriteCodexInput(body, text, k, headAnchorTokens)
	curInput, _ := body["input"].([]any)
	RecordCodexSpecialTelemetryEvent("autocompact_external")
	log.Infof("[AUTOCOMPACT] rewrite (fresh,external) | summary %dch | items %d->%d | est %d->%d tokens", len(summary), len(items), len(curInput), est0, estimateCodexTokensOf(body))
}

// enforceCodexInputHardCapMap guarantees tool-carrying requests NEVER exceed EffectiveAutocompactHardCapTokens()
// (default 250,000 est tokens).
func enforceCodexInputHardCapMap(cfg *config.Config, body map[string]any) {
	if body == nil {
		return
	}
	var cxCfg *config.CodexConfig
	if cfg != nil {
		cxCfg = &cfg.Codex
	}
	capTokens := cxCfg.EffectiveAutocompactHardCapTokens()
	if capTokens < 50000 {
		return
	}
	topTools, _ := body["tools"].([]any)
	items, _ := body["input"].([]any)
	if len(items) < 2 {
		return
	}
	hasTools := len(topTools) > 0
	if !hasTools {
		for _, rawIt := range items {
			if it, ok := rawIt.(map[string]any); ok && autocutStrField(it, "type") == "additional_tools" {
				if st, _ := it["tools"].([]any); len(st) > 0 {
					hasTools = true
					break
				}
			}
		}
	}
	if !hasTools {
		return
	}

	est := estimateCodexTokensOf(body)
	if est <= capTokens {
		return
	}
	est0 := est
	keepBudget := cxCfg.EffectiveAutocompactKeepTokens()
	if keepBudget < 2000 {
		keepBudget = 2000
	}
	headAnchorTokens := autocompactEnvInt("CODEX_AUTOCOMPACT_HEAD_ANCHOR_TOKENS", 40000)

	for est > capTokens {
		curItems, _ := body["input"].([]any)
		k := findCodexCutPoint(curItems, keepBudget)
		if k < 2 {
			break
		}
		rewriteCodexInput(body, codexWindowNote, k, headAnchorTokens)
		newEst := estimateCodexTokensOf(body)
		if newEst >= est {
			break
		}
		est = newEst
	}
	RecordCodexSpecialTelemetryEvent("hard_cap")
	log.Warnf("[AUTOCOMPACT] hard cap %d enforced: est %d->%d tokens", capTokens, est0, est)
}
