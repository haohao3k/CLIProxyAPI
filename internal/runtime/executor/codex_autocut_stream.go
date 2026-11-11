package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	CodexPadKey           = "__pad9f3"
	CodexSentinelComplete = "\u27E6TASK_COMPLETE_9F3\u27E7"
	CodexSentinelAppendix = "\u27E6APPENDIX_9F3\u27E7"
	CodexFinalizeToolName = "__finalize_9f3"
	DefaultHardArgCap     = 8000
)

var (
	codexAutocutTLSSessionCache = tls.NewLRUClientSessionCache(256)

	customPadRe             = regexp.MustCompile(`const\s+pad\s*=`)
	partialPadRe            = regexp.MustCompile(`const\s+pad\s*=?\s*"?\s*$`)
	potentialPadRe          = regexp.MustCompile(`__pad9f3|const\s+pad(\s*=)?\b`)
	stripImmediateConstRe   = regexp.MustCompile(`(?:\\n|\r?\n)?\s*const\s+pad\s*=\s*\\?["']?\s*$`)
	stripTrailingKeyCommaRe = regexp.MustCompile(`,\s*(?:"[^"]*)?\s*$`)
	stripTrailingCommaRe    = regexp.MustCompile(`,\s*$`)
	emptyJsonOpenRe         = regexp.MustCompile(`^\{(?:\s*")?$`)
	danglingColonCommaRe    = regexp.MustCompile(`[:,]\s*$`)
	finalizePadTriggerRe    = regexp.MustCompile(`(?i)const\s+pad\d*\s*=|\"__pad9f3\"\s*:`)
	finalizeMarkerRe        = regexp.MustCompile(`(?i)const[\s\x{00A0}]+pad\b|"__pad9f3"\s*:`)
	fuzzySentinelRe         = regexp.MustCompile(`\x{27E6}\s*(?:TASK_COMPLETE|APPENDIX)`)
	tagSentinelRe           = regexp.MustCompile(`(?:TASK_COMPLETE_9F3|APPENDIX_9F3)`)
	potentialSentinelRe     = regexp.MustCompile(`\x{27E6}|TASK_COMPLETE|APPENDIX`)
	tailCustomExactRe       = regexp.MustCompile(`(?:__pad9f3|const\s+pad(?:\s*=?\s*"?)?)$`)
	tailJsonExactRe         = regexp.MustCompile(`(?:__pad9f3)$`)
	tailCustomPrefixRe      = regexp.MustCompile(`(?:,\s*"?|\s+)(?:c|co|con|cons|const|const\s+|const\s+p|const\s+pa|const\s+pad|const\s+pad\s*=?|const\s+pad\s*=?\s*"?|_+|_+p|_+pa|_+pad|_+pad9|_+pad9f|_+pad9f3)$|(?:c|co|con|cons|const|const\s+|const\s+p|const\s+pa|const\s+pad|const\s+pad\s*=?|const\s+pad\s*=?\s*"?|_+|_+p|_+pa|_+pad|_+pad9|_+pad9f|_+pad9f3)$`)
	tailJsonPrefixRe        = regexp.MustCompile(`(?:,\s*"?(?:_+|_+p|_+pa|_+pad|_+pad9|_+pad9f|_+pad9f3)?|_+|_+p|_+pa|_+pad|_+pad9|_+pad9f|_+pad9f3)$`)

	codexSSEAccountFallbackPatterns = []string{
		"selected model is at capacity",
		"model_at_capacity",
		"model_capacity",
		"model_overloaded",
	}
	codexSSERetryPatterns = []string{
		"server_is_overloaded",
		"servers are currently overloaded",
		"service_unavailable_error",
		"service_unavailable",
		"slow_down",
		"response.failed",
		"overloaded",
	}
	codexSSEUserOutputPatterns = []string{
		"event: response.output_text.delta",
		"event: response.function_call_arguments.delta",
		"event: response.custom_tool_call_input.delta",
		"event: response.reasoning",
		"event: response.output_item.added",
		`"type":"response.output_text.delta"`,
		`"type":"response.function_call_arguments.delta"`,
		`"type":"response.custom_tool_call_input.delta"`,
		`"type":"response.reasoning`,
		`"type":"response.output_item.added"`,
	}
)

// ============================================================================
// 1. Hardware TCP RST Tracker + Dedicated 1-to-1 HTTP/1.1 TLS Transport
// ============================================================================

type codexTCPConnTracker struct {
	mu      sync.Mutex
	conns   []net.Conn
	aborted bool
}

func (t *codexTCPConnTracker) track(c net.Conn) {
	if c == nil {
		return
	}
	t.mu.Lock()
	if t.aborted {
		t.mu.Unlock()
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = c.Close()
		return
	}
	t.conns = append(t.conns, c)
	t.mu.Unlock()
}

// hardResetAll sets SO_LINGER=0 on tracked TCP sockets and closes them immediately,
// emitting a kernel-level TCP RST packet upstream without waiting for TLS close_notify
// or HTTP/2 FIN frames.
func (t *codexTCPConnTracker) hardResetAll() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.aborted = true
	conns := t.conns
	t.conns = nil
	t.mu.Unlock()

	for _, c := range conns {
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = c.Close()
	}
}

func newCodexAutocutHTTPClient(cfg *config.Config, auth *cliproxyauth.Auth, overrideProxyURL string) (*http.Client, *http.Transport, *codexTCPConnTracker) {
	tracker := &codexTCPConnTracker{}
	dialer := &net.Dialer{
		Timeout:   20 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	proxyStr := strings.TrimSpace(overrideProxyURL)
	if proxyStr == "" && auth != nil {
		proxyStr = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyStr == "" && cfg != nil {
		proxyStr = strings.TrimSpace(cfg.ProxyURL)
	}
	var proxyFunc func(*http.Request) (*neturl.URL, error)
	if proxyStr != "" {
		if parsed, err := neturl.Parse(proxyStr); err == nil {
			proxyFunc = http.ProxyURL(parsed)
		}
	}
	if proxyFunc == nil {
		proxyFunc = http.ProxyFromEnvironment
	}

	transport := &http.Transport{
		Proxy: proxyFunc,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetNoDelay(true)
			}
			tracker.track(conn)
			return conn, nil
		},
		TLSClientConfig: &tls.Config{
			NextProtos:         []string{"http/1.1"},
			ClientSessionCache: codexAutocutTLSSessionCache,
		},
		// Guarantee zero HTTP/2 multiplexing so TCP SetLinger(0) RST hits a dedicated 1-to-1 TCP socket.
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     true,
		DisableCompression:    true,
		MaxIdleConns:          1,
		ResponseHeaderTimeout: 90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0,
	}
	return client, transport, tracker
}

// ============================================================================
// 2. 200-OK SSE Capacity / Overload Peek (go-sidecar/main.go Parity)
// ============================================================================

type codexSSEPeekResult struct {
	matched         string
	message         string
	accountFallback bool
	prefixBytes     []byte
}

func findCodexNestedMessage(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	if m, ok := obj["message"].(string); ok && strings.TrimSpace(m) != "" {
		return strings.TrimSpace(m)
	}
	for _, k := range []string{"error", "response", "last_error"} {
		if sub, ok := obj[k].(map[string]any); ok {
			if found := findCodexNestedMessage(sub); found != "" {
				return found
			}
		}
	}
	return ""
}

func extractCodexSseErrorMessage(rawText string, fallback string) string {
	for _, line := range strings.Split(rawText, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(trimmed[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			continue
		}
		if msg := findCodexNestedMessage(parsed); msg != "" {
			return msg
		}
	}
	if strings.Contains(strings.ToLower(rawText), "selected model is at capacity") {
		return "Selected model is at capacity. Please try a different model."
	}
	return fallback
}

func peekCodexSSETransientError(body io.Reader) codexSSEPeekResult {
	const peekMaxBytes = 256 * 1024
	var buf bytes.Buffer
	tmp := make([]byte, 8192)
	var matched string
	accountFallback := false

	for buf.Len() < peekMaxBytes {
		n, err := body.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			lower := strings.ToLower(buf.String())

			for _, p := range codexSSEAccountFallbackPatterns {
				if strings.Contains(lower, p) {
					matched = p
					accountFallback = true
					break
				}
			}
			if matched != "" {
				break
			}
			for _, p := range codexSSERetryPatterns {
				if strings.Contains(lower, p) {
					matched = p
					break
				}
			}
			if matched != "" {
				break
			}
			userOut := false
			for _, p := range codexSSEUserOutputPatterns {
				if strings.Contains(lower, p) {
					userOut = true
					break
				}
			}
			if userOut {
				break
			}
		}
		if err != nil {
			break
		}
	}

	if matched != "" {
		msg := extractCodexSseErrorMessage(buf.String(), fmt.Sprintf("Upstream SSE transient error (%s)", matched))
		return codexSSEPeekResult{
			matched:         matched,
			message:         msg,
			accountFallback: accountFallback,
		}
	}

	return codexSSEPeekResult{prefixBytes: buf.Bytes()}
}

// ============================================================================
// 3. Verbatim :9gl-med Go Sidecar Autocut State Machine
// ============================================================================

// CloseJsonArgs balances truncated JSON object arguments cleanly so the client
// receives syntactically valid JSON even after an immediate mid-stream cut.
func CloseJsonArgs(s string) string {
	t := strings.TrimRightFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == ','
	})
	if !strings.HasPrefix(t, "{") {
		return strings.TrimRightFunc(s, unicode.IsSpace)
	}

	inStr := false
	escaped := false
	lastStrStart := -1
	for i := 1; i < len(t); i++ {
		c := t[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inStr {
			escaped = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			if inStr {
				lastStrStart = i
			}
		}
	}
	if escaped && len(t) > 0 {
		t = t[:len(t)-1]
	}
	endsOpenString := inStr
	if inStr {
		t += `"`
	}

	if danglingColonCommaRe.MatchString(t) {
		t += "null"
	} else if endsOpenString && lastStrStart > 0 && !strings.HasSuffix(t, "}") {
		j := lastStrStart - 1
		for j >= 1 && unicode.IsSpace(rune(t[j])) {
			j--
		}
		if t[j] == ',' || t[j] == '{' {
			var stack []byte
			inS := false
			esc := false
			for k := 0; k < lastStrStart; k++ {
				c := t[k]
				if esc {
					esc = false
					continue
				}
				if c == '\\' && inS {
					esc = true
					continue
				}
				if c == '"' {
					inS = !inS
					continue
				}
				if inS {
					continue
				}
				if c == '{' || c == '[' {
					stack = append(stack, c)
				} else if (c == '}' && len(stack) > 0 && stack[len(stack)-1] == '{') ||
					(c == ']' && len(stack) > 0 && stack[len(stack)-1] == '[') {
					stack = stack[:len(stack)-1]
				}
			}
			if len(stack) > 0 && stack[len(stack)-1] == '{' {
				t += ":null"
			}
		}
	}

	var stack []byte
	inS := false
	esc := false
	for i := 0; i < len(t); i++ {
		c := t[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' && inS {
			esc = true
			continue
		}
		if c == '"' {
			inS = !inS
			continue
		}
		if inS {
			continue
		}
		if c == '{' || c == '[' {
			stack = append(stack, c)
		} else if (c == '}' && len(stack) > 0 && stack[len(stack)-1] == '{') ||
			(c == ']' && len(stack) > 0 && stack[len(stack)-1] == '[') {
			stack = stack[:len(stack)-1]
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			t += "}"
		} else {
			t += "]"
		}
	}

	var js any
	if err := json.Unmarshal([]byte(t), &js); err == nil {
		return t
	}
	return "{}"
}

// isOuterJsonClosed checks whether the outermost JSON object has been cleanly closed
// with '}' outside any string literals, ensuring no false positives on inner code strings.
func isOuterJsonClosed(s string) bool {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{") {
		return false
	}
	depth := 0
	inStr := false
	escaped := false
	for i := 0; i < len(t); i++ {
		c := t[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inStr {
			escaped = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// CleanToolArguments strips __pad9f3 (and custom `const pad =` ONLY when allowCustomPad is true,
// obeying Iron Rule #7) and closes truncated JSON objects cleanly.
func CleanToolArguments(args string, allowCustomPad bool) string {
	padIdx := strings.Index(args, CodexPadKey)
	customPadIdx := -1
	partialIdx := -1
	if allowCustomPad {
		if loc := customPadRe.FindStringIndex(args); loc != nil {
			customPadIdx = loc[0]
		}
		if loc := partialPadRe.FindStringIndex(args); loc != nil {
			partialIdx = loc[0]
		}
	}
	if padIdx == -1 && customPadIdx == -1 && partialIdx == -1 {
		return args
	}

	cutPos := len(args)
	for _, idx := range []int{padIdx, customPadIdx, partialIdx} {
		if idx != -1 && idx < cutPos {
			cutPos = idx
		}
	}
	clean := args[:cutPos]
	if !allowCustomPad {
		clean = stripImmediateConstRe.ReplaceAllString(clean, "")
	}
	clean = stripTrailingKeyCommaRe.ReplaceAllString(clean, "")
	clean = stripTrailingCommaRe.ReplaceAllString(clean, "")
	clean = strings.TrimRightFunc(clean, unicode.IsSpace)

	if !strings.HasPrefix(clean, "{") {
		return clean
	}
	if clean == "" || emptyJsonOpenRe.MatchString(clean) {
		return "{}"
	}
	return CloseJsonArgs(clean)
}

type codexDetectMatch struct {
	idx int
	how string
}

func detectSentinelInText(text string, fromIdx int) *codexDetectMatch {
	scanStart := fromIdx
	if scanStart < 0 {
		scanStart = 0
	}
	if scanStart > len(text) {
		scanStart = len(text)
	}
	slice := text[scanStart:]

	if i := strings.Index(slice, CodexSentinelComplete); i != -1 {
		return &codexDetectMatch{idx: scanStart + i, how: "marker"}
	}
	if i := strings.Index(slice, CodexSentinelAppendix); i != -1 {
		return &codexDetectMatch{idx: scanStart + i, how: "apxfuzz"}
	}
	if loc := fuzzySentinelRe.FindStringIndex(slice); loc != nil {
		how := "apxfuzz"
		if strings.Contains(slice, "TASK_COMPLETE") {
			how = "marker"
		}
		return &codexDetectMatch{idx: scanStart + loc[0], how: how}
	}
	if loc := tagSentinelRe.FindStringIndex(slice); loc != nil {
		how := "apxfuzz"
		if strings.Contains(slice, "TASK_COMPLETE") {
			how = "marker"
		}
		return &codexDetectMatch{idx: scanStart + loc[0], how: how}
	}
	return nil
}

func detectPadInArgs(argsText string, fromIdx int, allowCustomPad bool) *codexDetectMatch {
	scanStart := fromIdx
	if scanStart < 0 {
		scanStart = 0
	}
	if scanStart > len(argsText) {
		scanStart = len(argsText)
	}
	slice := argsText[scanStart:]

	if i := strings.Index(slice, CodexPadKey); i != -1 {
		return &codexDetectMatch{idx: scanStart + i, how: "toolpad"}
	}
	if allowCustomPad {
		if loc := customPadRe.FindStringIndex(slice); loc != nil {
			return &codexDetectMatch{idx: scanStart + loc[0], how: "argpad"}
		}
	}
	return nil
}

func padMarkerIndex(text string, fromIdx int) int {
	scanStart := fromIdx
	if scanStart < 0 {
		scanStart = 0
	}
	if scanStart > len(text) {
		return -1
	}
	matches := finalizeMarkerRe.FindAllStringIndex(text[scanStart:], -1)
	if len(matches) == 0 {
		return -1
	}
	return scanStart + matches[len(matches)-1][0]
}

func potentialPrefixSafeLen(text string, deliveredLen int) int {
	patterns := []string{"const pad", "const\u00A0pad", `"__pad9f3"`}
	for hold := 1; hold <= 20 && hold <= len(text); hold++ {
		suffix := text[len(text)-hold:]
		for _, p := range patterns {
			if strings.HasPrefix(p, suffix) {
				return len(text) - hold
			}
		}
	}
	marker := padMarkerIndex(text, autocutMaxInt(0, deliveredLen-30))
	if marker >= 0 {
		return marker
	}
	return len(text)
}

func autocutMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func serializeSseEvent(event string, data any) []byte {
	if m, ok := data.(map[string]any); ok {
		if done, ok := m["done"].(bool); ok && done && len(m) == 1 {
			return []byte("data: [DONE]\n\n")
		}
	}
	var dataBytes []byte
	switch v := data.(type) {
	case string:
		dataBytes = []byte(v)
	case []byte:
		dataBytes = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			dataBytes = []byte("{}")
		} else {
			dataBytes = b
		}
	}
	var buf bytes.Buffer
	if event != "" {
		buf.WriteString("event: ")
		buf.WriteString(event)
		buf.WriteByte('\n')
	}
	buf.WriteString("data: ")
	buf.Write(dataBytes)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

func parseSseBlock(block string) (string, map[string]any, bool) {
	var event string
	var dataLines []string
	lines := strings.Split(block, "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimLeft(line[5:], " \t"))
		}
	}
	dataRaw := strings.Join(dataLines, "\n")
	if dataRaw == "" {
		return event, nil, false
	}
	if dataRaw == "[DONE]" {
		return event, map[string]any{"done": true}, true
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(dataRaw), &parsed); err != nil {
		return event, nil, false
	}
	return event, parsed, true
}

func synthCodexCompleted(responseId string, totalDeliveredChars int, model string, inputTokens int, outputItems []any) map[string]any {
	est := autocutMaxInt(1, (totalDeliveredChars+2)/4)
	if responseId == "" {
		responseId = fmt.Sprintf("resp_%d", time.Now().UnixMilli())
	}
	if model == "" {
		model = "codex"
	}
	if outputItems == nil {
		outputItems = []any{}
	}
	usage := map[string]any{
		"input_tokens":         inputTokens,
		"input_tokens_details": map[string]any{"cached_tokens": 0},
		"output_tokens":        est,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": 0,
		},
		"total_tokens": inputTokens + est,
	}
	return map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":         responseId,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      model,
			"output":     outputItems,
			"error":      nil,
			"usage":      usage,
		},
	}
}

type CodexAutocutOptions struct {
	Model               string
	SessionID           string
	FinalizeMode        bool
	HardArgCap          int
	PostcallAbortMin    int
	EstimateInputTokens int
	OnAbortUpstream     func(how string)
}

type CodexAutocutTelemetry struct {
	How            string
	DeliveredChars int
	OutputTokens   int
	RaceLost       bool
}

var introPayloadEvents = map[string]bool{
	"response.output_text.delta":             true,
	"response.function_call_arguments.delta": true,
	"response.custom_tool_call_input.delta":  true,
	"response.reasoning.delta":               true,
	"response.reasoning_summary_text.delta":  true,
	"response.completed":                     true,
	"response.done":                          true,
	"response.failed":                        true,
	"error":                                  true,
}

func autocutStrField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func autocutIntField(m map[string]any, key string, def int) int {
	if m == nil {
		return def
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func autocutCloneMap(m map[string]any) map[string]any {
	res := make(map[string]any, len(m)+1)
	for k, v := range m {
		res[k] = v
	}
	return res
}

// applyCodexAutocutSessionTelemetry updates per-session violation streaks (:9gl-med, Iron Rule #4)
// and race-loss session guard based on StreamWithAutocut telemetry.
func applyCodexAutocutSessionTelemetry(sessionID, model string, tel CodexAutocutTelemetry) {
	how := tel.How
	if how == "" {
		how = "eof"
	}
	deliveredEst := tel.DeliveredChars
	isMicroToolpad := how == "toolpad" && deliveredEst < 800
	isShortFinalize := how == "finalize" && deliveredEst < 1200
	isUnpadded := strings.Contains(how, "unpadded")
	isTooShort := isMicroToolpad || isShortFinalize || isUnpadded

	if how == "terminal" || how == "argcap" || strings.HasPrefix(how, "eof") || tel.RaceLost || isTooShort {
		recordCodexAutocutViolation(sessionID, 2)
	} else if how != "upstream-error" {
		decayCodexAutocutViolations(sessionID)
	}
	recordCodexSessionRisk(sessionID, how)
	if tel.RaceLost {
		log.Warnf("[AC:RACE-LOST] session=%s model=%s how=%s delivered=%d", sessionID, model, how, deliveredEst)
	} else {
		log.Debugf("✂ AUTOCUT · how=%s delivered=%d model=%s session=%s", how, deliveredEst, model, sessionID)
	}
}

// StreamWithAutocut reads raw SSE bytes from upstream, runs the verbatim :9gl-med
// Autocut + Finalize state machine, fires OnAbortUpstream (SO_LINGER=0 TCP RST)
// before setting cutTriggered (Iron Rule #9), and writes clean SSE events downstream.
func StreamWithAutocut(upstream io.Reader, out io.Writer, flusher http.Flusher, opts CodexAutocutOptions) CodexAutocutTelemetry {
	cutTriggered := false
	cutHow := ""
	socketPreAborted := false
	responseId := fmt.Sprintf("resp_%d", time.Now().UnixMilli())
	totalDeliveredChars := 0
	var outputItems []any

	// Iron Rule #10: introHoldActive = !opts.FinalizeMode and downCh = 8192 coalesced
	introHoldActive := !opts.FinalizeMode
	var heldIntro [][]byte
	var holdBuffer [][]byte

	finalizeActive := false
	finalizeItemId := ""
	finalizeToolItemId := ""
	finalizeOutIndex := 0
	finalizeAwaitEvidence := false
	finalizeText := ""
	finalizeDeliveredLen := 0
	terminalSent := false

	var currentItem map[string]any
	currentOutputIndex := 0
	armedAfterCall := false
	lastPadlessArgsLen := 0

	textAccum := ""
	textDeliveredLen := 0
	argsAccum := ""
	argsDeliveredLen := 0
	rawBuffer := ""

	hardArgCap := opts.HardArgCap
	if hardArgCap <= 0 {
		hardArgCap = DefaultHardArgCap
	}
	postcallAbortMin := opts.PostcallAbortMin
	if postcallAbortMin <= 0 {
		postcallAbortMin = 5000
	}

	downCh := make(chan []byte, 8192)
	doneWriter := make(chan struct{})
	go func() {
		defer close(doneWriter)
		var batch bytes.Buffer
		for b := range downCh {
			batch.Reset()
			batch.Write(b)
			for len(downCh) > 0 && batch.Len() < 64*1024 {
				batch.Write(<-downCh)
			}
			_, _ = out.Write(batch.Bytes())
			if flusher != nil {
				flusher.Flush()
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
	}()
	defer func() {
		close(downCh)
		<-doneWriter
	}()

	// Iron Rule #9: invoke OnAbortUpstream (SetLinger(0) TCP RST) BEFORE cutTriggered = true.
	triggerImmediateAbort := func(how string) {
		if cutTriggered {
			return
		}
		if opts.OnAbortUpstream != nil {
			opts.OnAbortUpstream(how)
		}
		cutTriggered = true
		cutHow = how
	}

	writeDownstream := func(b []byte) {
		if len(b) == 0 {
			return
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		downCh <- cp
	}

	enqueueEvent := func(event string, data any, chars int) {
		if introHoldActive && !introPayloadEvents[event] {
			heldIntro = append(heldIntro, serializeSseEvent(event, data))
			return
		}
		if introHoldActive {
			for _, h := range heldIntro {
				writeDownstream(h)
			}
			heldIntro = nil
			introHoldActive = false
		}
		totalDeliveredChars += chars
		writeDownstream(serializeSseEvent(event, data))
	}

	flushHold := func() {
		for _, h := range holdBuffer {
			writeDownstream(h)
		}
		holdBuffer = nil
	}

	type heldPreambleEvent struct {
		event string
		data  any
		chars int
	}
	preambleHolding := false
	var preambleBuffer []heldPreambleEvent
	var preambleOutputItems []any

	discardPreambleHold := func() {
		preambleHolding = false
		preambleBuffer = nil
		preambleOutputItems = nil
	}

	flushPreambleHold := func() {
		preambleHolding = false
		if len(preambleBuffer) == 0 && len(preambleOutputItems) == 0 {
			return
		}
		for _, ev := range preambleBuffer {
			enqueueEvent(ev.event, ev.data, ev.chars)
		}
		preambleBuffer = nil
		outputItems = append(outputItems, preambleOutputItems...)
		preambleOutputItems = nil
	}

	executeCut := func(how string) {
		triggerImmediateAbort(how)
		raceLost := strings.Contains(rawBuffer, `"response.completed"`) ||
			strings.Contains(rawBuffer, "event: response.completed")
		recordedHow := how
		if raceLost && how != "" && !strings.HasSuffix(how, "-racelost") {
			recordedHow = how + "-racelost"
		}
		cutHow = recordedHow
		synth := synthCodexCompleted(responseId, totalDeliveredChars, opts.Model, opts.EstimateInputTokens, outputItems)
		enqueueEvent("response.completed", synth, 0)
	}

	closeFinalizeAndCut := func(how string) {
		if how != "" {
			triggerImmediateAbort(how)
		}
		// Iron Rule #8: NEVER hard-cap or truncate __finalize_9f3 message text mid-stream;
		// only strip from the pad marker onward.
		marker := padMarkerIndex(finalizeText, 0)
		clean := finalizeText
		if marker >= 0 {
			clean = finalizeText[:marker]
		}
		clean = strings.TrimRightFunc(clean, unicode.IsSpace)
		if len(clean) > finalizeDeliveredLen {
			rem := clean[finalizeDeliveredLen:]
			enqueueEvent("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       finalizeItemId,
				"output_index":  finalizeOutIndex,
				"content_index": 0,
				"delta":         rem,
			}, len(rem))
			finalizeDeliveredLen = len(clean)
		}
		finalText := clean
		enqueueEvent("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       finalizeItemId,
			"output_index":  finalizeOutIndex,
			"content_index": 0,
			"text":          finalText,
		}, 0)
		enqueueEvent("response.content_part.done", map[string]any{
			"type":          "response.content_part.done",
			"item_id":       finalizeItemId,
			"output_index":  finalizeOutIndex,
			"content_index": 0,
			"part": map[string]any{
				"type":        "output_text",
				"text":        finalText,
				"annotations": []any{},
			},
		}, 0)
		msgItem := map[string]any{
			"id":     finalizeItemId,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{
					"type":        "output_text",
					"text":        finalText,
					"annotations": []any{},
				},
			},
		}
		enqueueEvent("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": finalizeOutIndex,
			"item":         msgItem,
		}, 0)
		outputItems = append(outputItems, msgItem)
		finalizeActive = false
		if how != "" {
			executeCut(how)
		}
	}

	flushText := func(eventTemplate map[string]any, force bool) {
		if len(textAccum) <= textDeliveredLen {
			return
		}
		safeEnd := len(textAccum)
		if !force {
			scanStart := autocutMaxInt(0, textDeliveredLen-30)
			unscanned := textAccum[scanStart:]
			if loc := potentialSentinelRe.FindStringIndex(unscanned); loc != nil {
				potentialIdx := scanStart + loc[0]
				if potentialIdx >= textDeliveredLen {
					safeEnd = potentialIdx
				}
			}
		}
		if safeEnd > textDeliveredLen {
			text := textAccum[textDeliveredLen:safeEnd]
			ev := autocutCloneMap(eventTemplate)
			ev["delta"] = text
			if preambleHolding {
				preambleBuffer = append(preambleBuffer, heldPreambleEvent{
					event: "response.output_text.delta",
					data:  ev,
					chars: len(text),
				})
			} else {
				enqueueEvent("response.output_text.delta", ev, len(text))
			}
			textDeliveredLen = safeEnd
		}
	}

	flushArgs := func(eventType string, eventTemplate map[string]any, force bool, allowCustomPad bool) {
		if len(argsAccum) <= argsDeliveredLen {
			return
		}
		safeEnd := len(argsAccum)
		if !force {
			scanStart := autocutMaxInt(0, argsDeliveredLen-30)
			unscanned := argsAccum[scanStart:]
			relIdx := -1
			if allowCustomPad {
				if loc := potentialPadRe.FindStringIndex(unscanned); loc != nil {
					relIdx = loc[0]
				}
			} else {
				relIdx = strings.Index(unscanned, CodexPadKey)
			}
			if relIdx != -1 {
				potentialIdx := scanStart + relIdx
				if potentialIdx >= argsDeliveredLen {
					safeEnd = potentialIdx
				}
			} else {
				tailStart := autocutMaxInt(0, len(argsAccum)-24)
				tail := argsAccum[tailStart:]
				var loc []int
				if allowCustomPad {
					loc = tailCustomExactRe.FindStringIndex(tail)
					if loc == nil {
						loc = tailCustomPrefixRe.FindStringIndex(tail)
					}
				} else {
					loc = tailJsonExactRe.FindStringIndex(tail)
					if loc == nil {
						loc = tailJsonPrefixRe.FindStringIndex(tail)
					}
				}
				if loc != nil {
					cut := tailStart + loc[0]
					if cut >= argsDeliveredLen {
						safeEnd = cut
					}
				}
			}
		}
		if safeEnd > argsDeliveredLen {
			text := argsAccum[argsDeliveredLen:safeEnd]
			ev := autocutCloneMap(eventTemplate)
			ev["delta"] = text
			enqueueEvent(eventType, ev, len(text))
			argsDeliveredLen = safeEnd
		}
	}

	processEvent := func(eventType string, data map[string]any) {
		if cutTriggered {
			return
		}

		if eventType == "response.created" {
			if resp, ok := data["response"].(map[string]any); ok {
				if id := autocutStrField(resp, "id"); id != "" {
					responseId = id
				}
			} else if id := autocutStrField(data, "id"); id != "" {
				responseId = id
			}
			enqueueEvent("response.created", data, 0)
			return
		}

		if eventType == "response.in_progress" {
			enqueueEvent("response.in_progress", data, 0)
			return
		}

		if eventType == "response.output_item.added" {
			if finalizeActive && finalizeAwaitEvidence {
				closeFinalizeAndCut("finalize-safe")
				return
			}
			item, _ := data["item"].(map[string]any)
			itemName := autocutStrField(item, "name")
			itemType := autocutStrField(item, "type")

			if opts.FinalizeMode && itemName == CodexFinalizeToolName {
				discardPreambleHold()
				armedAfterCall = false
				if len(holdBuffer) > 0 {
					flushHold()
				}
				finalizeActive = true
				finalizeAwaitEvidence = false
				finalizeToolItemId = autocutStrField(item, "id")
				finalizeItemId = fmt.Sprintf("msg_final_%x", time.Now().UnixNano())
				finalizeOutIndex = autocutIntField(data, "output_index", currentOutputIndex)
				finalizeText = ""
				finalizeDeliveredLen = 0

				enqueueEvent("response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": finalizeOutIndex,
					"item": map[string]any{
						"id":      finalizeItemId,
						"type":    "message",
						"status":  "in_progress",
						"role":    "assistant",
						"content": []any{},
					},
				}, 0)
				enqueueEvent("response.content_part.added", map[string]any{
					"type":          "response.content_part.added",
					"item_id":       finalizeItemId,
					"output_index":  finalizeOutIndex,
					"content_index": 0,
					"part": map[string]any{
						"type":        "output_text",
						"text":        "",
						"annotations": []any{},
					},
				}, 0)
				return
			}

			if armedAfterCall {
				if itemType == "message" {
					holdBuffer = nil
					executeCut("postcall-safe")
					return
				}
				nextIsCall := itemType == "function_call" || itemType == "custom_tool_call"
				if postcallAbortMin > 0 && nextIsCall && lastPadlessArgsLen >= postcallAbortMin {
					holdBuffer = nil
					executeCut("postcall-safe")
					return
				}
				holdBuffer = append(holdBuffer, serializeSseEvent("response.output_item.added", data))
				currentItem = item
				currentOutputIndex = autocutIntField(data, "output_index", currentOutputIndex)
				if itemType == "message" {
					textAccum = ""
					textDeliveredLen = 0
				}
				return
			}

			currentItem = item
			currentOutputIndex = autocutIntField(data, "output_index", currentOutputIndex)
			if itemType == "message" {
				textAccum = ""
				textDeliveredLen = 0
				if opts.FinalizeMode {
					preambleHolding = true
					preambleBuffer = append(preambleBuffer, heldPreambleEvent{
						event: "response.output_item.added",
						data:  data,
						chars: 0,
					})
					return
				}
			} else if itemType == "function_call" || itemType == "custom_tool_call" {
				discardPreambleHold()
				argsAccum = ""
				argsDeliveredLen = 0
			}
			enqueueEvent("response.output_item.added", data, 0)
			return
		}

		if eventType == "response.output_text.delta" {
			delta := autocutStrField(data, "delta")
			textAccum += delta

			sentinel := detectSentinelInText(textAccum, autocutMaxInt(0, textDeliveredLen-40))
			if sentinel != nil {
				flushPreambleHold()
				cleanText := strings.TrimRightFunc(textAccum[:sentinel.idx], unicode.IsSpace)
				if armedAfterCall && len(holdBuffer) > 0 {
					holdBuffer = nil
					if len(cleanText) > 0 {
						ev := autocutCloneMap(data)
						ev["delta"] = cleanText
						enqueueEvent("response.output_text.delta", ev, len(cleanText))
					}
					executeCut(sentinel.how)
					return
				}
				if len(cleanText) > textDeliveredLen {
					rem := cleanText[textDeliveredLen:]
					ev := autocutCloneMap(data)
					ev["delta"] = rem
					enqueueEvent("response.output_text.delta", ev, len(rem))
				}
				itemId := autocutStrField(currentItem, "id")
				if itemId == "" {
					itemId = autocutStrField(data, "item_id")
				}
				outIdx := autocutIntField(data, "output_index", currentOutputIndex)
				contentIdx := autocutIntField(data, "content_index", 0)
				enqueueEvent("response.output_text.done", map[string]any{
					"type":          "response.output_text.done",
					"item_id":       itemId,
					"output_index":  outIdx,
					"content_index": contentIdx,
					"text":          cleanText,
				}, 0)
				enqueueEvent("response.content_part.done", map[string]any{
					"type":          "response.content_part.done",
					"item_id":       itemId,
					"output_index":  outIdx,
					"content_index": contentIdx,
					"part": map[string]any{
						"type":        "output_text",
						"text":        cleanText,
						"annotations": []any{},
					},
				}, 0)
				msgItem := map[string]any{
					"id":     itemId,
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"content": []any{
						map[string]any{
							"type":        "output_text",
							"text":        cleanText,
							"annotations": []any{},
						},
					},
				}
				enqueueEvent("response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": outIdx,
					"item":         msgItem,
				}, 0)
				outputItems = append(outputItems, msgItem)
				executeCut(sentinel.how)
				return
			}

			if armedAfterCall && len(holdBuffer) > 0 {
				if potentialSentinelRe.MatchString(textAccum) {
					return
				}
				if len(textAccum) > 80 {
					armedAfterCall = false
					flushHold()
					flushText(data, false)
					return
				}
				return
			}

			flushText(data, false)
			return
		}

		if eventType == "response.output_text.done" {
			if armedAfterCall && len(holdBuffer) > 0 {
				holdBuffer = nil
				executeCut("postcall")
				return
			}
			flushText(data, true)
			if preambleHolding {
				preambleBuffer = append(preambleBuffer, heldPreambleEvent{
					event: "response.output_text.done",
					data:  data,
					chars: 0,
				})
				return
			}
			enqueueEvent("response.output_text.done", data, 0)
			return
		}

		if eventType == "response.function_call_arguments.delta" || eventType == "response.custom_tool_call_input.delta" {
			if finalizeActive && (finalizeToolItemId == "" || autocutStrField(data, "item_id") == finalizeToolItemId) {
				delta := autocutStrField(data, "delta")
				finalizeText += delta
				tailStart := autocutMaxInt(0, finalizeDeliveredLen-40)
				finalizeTail := finalizeText[tailStart:]
				if finalizePadTriggerRe.MatchString(finalizeTail) {
					triggerImmediateAbort("finalize")
					closeFinalizeAndCut("finalize")
					return
				}
				safeLen := potentialPrefixSafeLen(finalizeText, finalizeDeliveredLen)
				if safeLen > finalizeDeliveredLen {
					chunk := finalizeText[finalizeDeliveredLen:safeLen]
					finalizeDeliveredLen = safeLen
					enqueueEvent("response.output_text.delta", map[string]any{
						"type":          "response.output_text.delta",
						"item_id":       finalizeItemId,
						"output_index":  finalizeOutIndex,
						"content_index": 0,
						"delta":         chunk,
					}, len(chunk))
				}
				return
			}

			delta := autocutStrField(data, "delta")
			argsAccum += delta
			isCustom := eventType == "response.custom_tool_call_input.delta" || autocutStrField(currentItem, "type") == "custom_tool_call"

			pad := detectPadInArgs(argsAccum, autocutMaxInt(0, argsDeliveredLen-40), isCustom)
			if pad != nil {
				triggerImmediateAbort(pad.how)
				cleanArgs := CleanToolArguments(argsAccum, isCustom)

				deliveredPrefix := argsAccum[:argsDeliveredLen]
				if strings.HasPrefix(deliveredPrefix, "{") && !isCustom {
					closed := CloseJsonArgs(deliveredPrefix)
					if len(closed) > len(deliveredPrefix) {
						closure := closed[len(deliveredPrefix):]
						ev := autocutCloneMap(data)
						ev["delta"] = closure
						enqueueEvent(eventType, ev, len(closure))
						argsDeliveredLen = len(closed)
					}
				} else if len(cleanArgs) > argsDeliveredLen {
					undelivered := cleanArgs[argsDeliveredLen:]
					ev := autocutCloneMap(data)
					ev["delta"] = undelivered
					enqueueEvent(eventType, ev, len(undelivered))
					argsDeliveredLen = len(cleanArgs)
				}

				itemId := autocutStrField(currentItem, "id")
				if itemId == "" {
					itemId = autocutStrField(data, "item_id")
				}
				outIdx := autocutIntField(data, "output_index", currentOutputIndex)
				if isCustom {
					enqueueEvent("response.custom_tool_call_input.done", map[string]any{
						"type":         "response.custom_tool_call_input.done",
						"item_id":      itemId,
						"output_index": outIdx,
						"input":        cleanArgs,
					}, 0)
				} else {
					enqueueEvent("response.function_call_arguments.done", map[string]any{
						"type":         "response.function_call_arguments.done",
						"item_id":      itemId,
						"output_index": outIdx,
						"arguments":    cleanArgs,
					}, 0)
				}

				itemType := autocutStrField(currentItem, "type")
				if itemType == "" {
					itemType = "function_call"
				}
				var toolItem map[string]any
				if isCustom {
					toolItem = map[string]any{
						"id":      itemId,
						"type":    "custom_tool_call",
						"status":  "completed",
						"name":    autocutStrField(currentItem, "name"),
						"call_id": autocutStrField(currentItem, "call_id"),
						"input":   cleanArgs,
					}
				} else {
					toolItem = map[string]any{
						"id":        itemId,
						"type":      itemType,
						"status":    "completed",
						"name":      autocutStrField(currentItem, "name"),
						"call_id":   autocutStrField(currentItem, "call_id"),
						"arguments": cleanArgs,
					}
					if ns := autocutStrField(currentItem, "namespace"); ns != "" {
						toolItem["namespace"] = ns
					}
				}
				enqueueEvent("response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": outIdx,
					"item":         toolItem,
				}, 0)
				outputItems = append(outputItems, toolItem)
				executeCut(pad.how)
				return
			}

			if hardArgCap > 0 && len(argsAccum) >= hardArgCap {
				triggerImmediateAbort("argcap")
				flushArgs(eventType, data, true, isCustom)

				finalArgs := argsAccum
				deliveredPrefix := argsAccum[:argsDeliveredLen]
				if strings.HasPrefix(deliveredPrefix, "{") && !isCustom {
					closed := CloseJsonArgs(deliveredPrefix)
					if len(closed) > len(deliveredPrefix) {
						closure := closed[len(deliveredPrefix):]
						ev := autocutCloneMap(data)
						ev["delta"] = closure
						enqueueEvent(eventType, ev, len(closure))
						argsDeliveredLen = len(closed)
						finalArgs = closed
					} else {
						finalArgs = deliveredPrefix
					}
				} else if len(argsAccum) > argsDeliveredLen {
					undelivered := argsAccum[argsDeliveredLen:]
					ev := autocutCloneMap(data)
					ev["delta"] = undelivered
					enqueueEvent(eventType, ev, len(undelivered))
					argsDeliveredLen = len(argsAccum)
					finalArgs = argsAccum
				}

				itemId := autocutStrField(currentItem, "id")
				if itemId == "" {
					itemId = autocutStrField(data, "item_id")
				}
				outIdx := autocutIntField(data, "output_index", currentOutputIndex)
				if isCustom {
					enqueueEvent("response.custom_tool_call_input.done", map[string]any{
						"type":         "response.custom_tool_call_input.done",
						"item_id":      itemId,
						"output_index": outIdx,
						"input":        finalArgs,
					}, 0)
				} else {
					enqueueEvent("response.function_call_arguments.done", map[string]any{
						"type":         "response.function_call_arguments.done",
						"item_id":      itemId,
						"output_index": outIdx,
						"arguments":    finalArgs,
					}, 0)
				}

				itemType := autocutStrField(currentItem, "type")
				if itemType == "" {
					itemType = "function_call"
				}
				var toolItem map[string]any
				if isCustom {
					toolItem = map[string]any{
						"id":      itemId,
						"type":    "custom_tool_call",
						"status":  "completed",
						"name":    autocutStrField(currentItem, "name"),
						"call_id": autocutStrField(currentItem, "call_id"),
						"input":   finalArgs,
					}
				} else {
					toolItem = map[string]any{
						"id":        itemId,
						"type":      itemType,
						"status":    "completed",
						"name":      autocutStrField(currentItem, "name"),
						"call_id":   autocutStrField(currentItem, "call_id"),
						"arguments": finalArgs,
					}
					if ns := autocutStrField(currentItem, "namespace"); ns != "" {
						toolItem["namespace"] = ns
					}
				}
				enqueueEvent("response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": outIdx,
					"item":         toolItem,
				}, 0)
				outputItems = append(outputItems, toolItem)
				executeCut("argcap")
				return
			}

			flushArgs(eventType, data, false, isCustom)
			return
		}

		if finalizeActive && (eventType == "response.custom_tool_call_input.done" || eventType == "response.function_call_arguments.done") &&
			(finalizeToolItemId == "" || autocutStrField(data, "item_id") == finalizeToolItemId) {
			how := "finalize"
			if !finalizePadTriggerRe.MatchString(finalizeText) {
				how = "finalize-unpadded"
			}
			triggerImmediateAbort(how)
			closeFinalizeAndCut(how)
			return
		}

		if eventType == "response.output_item.done" {
			item, _ := data["item"].(map[string]any)
			itemName := autocutStrField(item, "name")
			itemType := autocutStrField(item, "type")
			if finalizeActive && itemName == CodexFinalizeToolName {
				how := "finalize"
				if !finalizePadTriggerRe.MatchString(finalizeText) {
					how = "finalize-unpadded"
				}
				triggerImmediateAbort(how)
				closeFinalizeAndCut(how)
				return
			}

			if itemType == "function_call" || itemType == "custom_tool_call" {
				isCustom := itemType == "custom_tool_call"
				deltaEvent := "response.function_call_arguments.delta"
				rawPayload := autocutStrField(item, "arguments")
				if isCustom {
					deltaEvent = "response.custom_tool_call_input.delta"
					if inp := autocutStrField(item, "input"); inp != "" {
						rawPayload = inp
					}
				}
				hasPad := strings.Contains(rawPayload, CodexPadKey) || (isCustom && customPadRe.MatchString(rawPayload))
				if rawPayload != "" && hasPad {
					how := "toolpad"
					if isCustom {
						how = "argpad"
					}
					triggerImmediateAbort(how)
					cleanArgs := CleanToolArguments(rawPayload, isCustom)
					newItem := autocutCloneMap(item)
					if isCustom {
						newItem["input"] = cleanArgs
						delete(newItem, "arguments")
					} else {
						newItem["arguments"] = cleanArgs
					}
					if len(cleanArgs) > argsDeliveredLen {
						undelivered := cleanArgs[argsDeliveredLen:]
						ev := autocutCloneMap(data)
						ev["delta"] = undelivered
						enqueueEvent(deltaEvent, ev, len(undelivered))
					}
					doneEv := autocutCloneMap(data)
					doneEv["item"] = newItem
					enqueueEvent("response.output_item.done", doneEv, 0)
					outputItems = append(outputItems, newItem)
					executeCut(how)
					return
				}

				if len(argsAccum) > argsDeliveredLen {
					rem := argsAccum[argsDeliveredLen:]
					ev := autocutCloneMap(data)
					ev["delta"] = rem
					enqueueEvent(deltaEvent, ev, len(rem))
					argsDeliveredLen = len(argsAccum)
				}
				enqueueEvent("response.output_item.done", data, 0)
				if item != nil {
					outputItems = append(outputItems, item)
				}
				armedAfterCall = true
				lastPadlessArgsLen = len(argsAccum)
				if socketPreAborted {
					executeCut("toolpad-unpadded")
					return
				}
				return
			}

			if itemType == "message" {
				flushText(data, true)
				if preambleHolding {
					preambleBuffer = append(preambleBuffer, heldPreambleEvent{
						event: "response.output_item.done",
						data:  data,
						chars: 0,
					})
					if item != nil {
						preambleOutputItems = append(preambleOutputItems, item)
					}
					preambleHolding = false
					return
				}
				enqueueEvent("response.output_item.done", data, 0)
				if item != nil {
					outputItems = append(outputItems, item)
				}
				return
			}

			enqueueEvent("response.output_item.done", data, 0)
			if item != nil {
				outputItems = append(outputItems, item)
			}
			return
		}

		if eventType == "response.completed" || eventType == "response.done" {
			if terminalSent {
				return
			}
			if finalizeActive {
				closeFinalizeAndCut("")
			} else {
				flushPreambleHold()
			}
			if resp, ok := data["response"].(map[string]any); ok {
				if outArr, ok := resp["output"].([]any); ok {
					hasFinalize := false
					for _, rawIt := range outArr {
						if it, ok := rawIt.(map[string]any); ok && autocutStrField(it, "name") == CodexFinalizeToolName {
							hasFinalize = true
							break
						}
					}
					if hasFinalize {
						newResp := autocutCloneMap(resp)
						if len(outputItems) > 0 {
							newResp["output"] = outputItems
						} else {
							var filtered []any
							for _, rawIt := range outArr {
								if it, ok := rawIt.(map[string]any); !ok || autocutStrField(it, "name") != CodexFinalizeToolName {
									filtered = append(filtered, rawIt)
								}
							}
							newResp["output"] = filtered
						}
						data = autocutCloneMap(data)
						data["response"] = newResp
					}
				}
			}

			if armedAfterCall && len(holdBuffer) > 0 {
				holdBuffer = nil
				executeCut("terminal")
				return
			}

			flushText(data, true)
			if len(argsAccum) > argsDeliveredLen {
				rem := argsAccum[argsDeliveredLen:]
				isCustom := autocutStrField(currentItem, "type") == "custom_tool_call"
				deltaEvent := "response.function_call_arguments.delta"
				if isCustom {
					deltaEvent = "response.custom_tool_call_input.delta"
				}
				enqueueEvent(deltaEvent, map[string]any{
					"type":         deltaEvent,
					"item_id":      autocutStrField(currentItem, "id"),
					"output_index": currentOutputIndex,
					"delta":        rem,
				}, len(rem))
				argsDeliveredLen = len(argsAccum)
			}
			enqueueEvent(eventType, data, 0)
			cutTriggered = true
			terminalSent = true
			cutHow = "terminal"
			return
		}

		if eventType == "response.failed" || eventType == "error" {
			enqueueEvent(eventType, data, 0)
			cutTriggered = true
			terminalSent = true
			cutHow = "upstream-error"
			return
		}

		if preambleHolding && (eventType == "response.content_part.added" || eventType == "response.content_part.done") {
			preambleBuffer = append(preambleBuffer, heldPreambleEvent{
				event: eventType,
				data:  data,
				chars: 0,
			})
			return
		}

		reasoningChars := 0
		if eventType == "response.reasoning.delta" || eventType == "response.reasoning_summary_text.delta" {
			reasoningChars = len(autocutStrField(data, "delta"))
		}
		if armedAfterCall && len(holdBuffer) > 0 {
			totalDeliveredChars += reasoningChars
			holdBuffer = append(holdBuffer, serializeSseEvent(eventType, data))
			return
		}
		enqueueEvent(eventType, data, reasoningChars)
	}

	buf := make([]byte, 32*1024)
	socketPreAborted = false
	for !cutTriggered {
		n, err := upstream.Read(buf)
		if n > 0 {
			chunkSlice := buf[:n]
			rawBuffer += string(chunkSlice)
			// Iron Rule #6: Multi-delta lookahead pre-scanner — stitch pending tool-call
			// delta literals from rawBuffer in <2us and fire SetLinger(0) TCP RST before
			// unmarshaling preceding events! NEVER check raw strings.Contains(rawBuffer, CodexPadKey).
			if !socketPreAborted && opts.OnAbortUpstream != nil {
				lookahead := extractToolLookaheadDeltas(rawBuffer)
				if lookahead != "" {
					combinedArgs := argsAccum + lookahead
					combinedFin := finalizeText + lookahead
					if !finalizeActive && strings.Contains(combinedArgs, CodexPadKey) {
						socketPreAborted = true
						opts.OnAbortUpstream("pre-scan-toolpad")
					} else if (finalizeActive || strings.Contains(rawBuffer, `"type":"custom_tool_call"`)) && finalizePadTriggerRe.MatchString(combinedFin) {
						socketPreAborted = true
						opts.OnAbortUpstream("pre-scan-finalize")
					} else if !finalizeActive && hardArgCap > 0 && len(combinedArgs) >= hardArgCap {
						socketPreAborted = true
						opts.OnAbortUpstream("pre-scan-argcap")
					} else if !finalizeActive && isOuterJsonClosed(combinedArgs) && !strings.Contains(combinedArgs, CodexPadKey) {
						socketPreAborted = true
						opts.OnAbortUpstream("pre-scan-unpadded-close")
					}
				}
				if !socketPreAborted && (finalizeActive || strings.Contains(rawBuffer, `"type":"custom_tool_call"`)) &&
					(strings.Contains(rawBuffer, "custom_tool_call_input.done") || (strings.Contains(rawBuffer, `"type":"response.output_item.done"`) && strings.Contains(rawBuffer, CodexFinalizeToolName))) {
					socketPreAborted = true
					opts.OnAbortUpstream("pre-scan-finalize-done")
				}
			}
			for !cutTriggered {
				n1 := strings.Index(rawBuffer, "\n\n")
				n2 := strings.Index(rawBuffer, "\r\n\r\n")
				if n1 == -1 && n2 == -1 {
					break
				}
				var sepIdx, sepLen int
				if n1 != -1 && (n2 == -1 || n1 <= n2) {
					sepIdx = n1
					sepLen = 2
				} else {
					sepIdx = n2
					sepLen = 4
				}
				block := rawBuffer[:sepIdx]
				rawBuffer = rawBuffer[sepIdx+sepLen:]
				if strings.TrimSpace(block) == "" {
					continue
				}
				// Fast zero-unmarshal passthrough for large encrypted reasoning items
				if !introHoldActive && !armedAfterCall && len(block) > 512 &&
					strings.Contains(block, `"type":"reasoning"`) &&
					!strings.Contains(block, "function_call") &&
					!strings.Contains(block, "custom_tool_call") &&
					!strings.Contains(block, CodexFinalizeToolName) &&
					!strings.Contains(block, `"type":"message"`) {
					writeDownstream([]byte(block + "\n\n"))
					continue
				}
				rawEv, parsed, ok := parseSseBlock(block)
				if !ok || parsed == nil {
					continue
				}
				evType := rawEv
				if evType == "" {
					evType = autocutStrField(parsed, "type")
				}
				processEvent(evType, parsed)
			}
		}
		if err != nil {
			if !cutTriggered {
				triggerImmediateAbort("eof")
				if finalizeActive {
					closeFinalizeAndCut("")
				} else {
					flushPreambleHold()
				}
				flushText(map[string]any{}, true)
				if len(argsAccum) > argsDeliveredLen {
					rem := argsAccum[argsDeliveredLen:]
					isCustom := autocutStrField(currentItem, "type") == "custom_tool_call"
					deltaEvent := "response.function_call_arguments.delta"
					if isCustom {
						deltaEvent = "response.custom_tool_call_input.delta"
					}
					enqueueEvent(deltaEvent, map[string]any{
						"type":         deltaEvent,
						"item_id":      autocutStrField(currentItem, "id"),
						"output_index": currentOutputIndex,
						"delta":        rem,
					}, len(rem))
					argsDeliveredLen = len(argsAccum)
				}
				raceLost := strings.Contains(rawBuffer, `"response.completed"`) ||
					strings.Contains(rawBuffer, "event: response.completed")
				recordedHow := "eof"
				if socketPreAborted {
					recordedHow = "toolpad-unpadded"
				}
				if raceLost && !strings.HasSuffix(recordedHow, "-racelost") {
					recordedHow += "-racelost"
				}
				cutHow = recordedHow
				synth := synthCodexCompleted(responseId, totalDeliveredChars, opts.Model, opts.EstimateInputTokens, outputItems)
				enqueueEvent("response.completed", synth, 0)
			}
			break
		}
	}

	tel := CodexAutocutTelemetry{
		How:            cutHow,
		DeliveredChars: totalDeliveredChars,
		OutputTokens:   autocutMaxInt(1, (totalDeliveredChars+2)/4),
		RaceLost:       strings.HasSuffix(cutHow, "-racelost") || cutHow == "terminal",
	}
	applyCodexAutocutSessionTelemetry(opts.SessionID, opts.Model, tel)
	return tel
}

// extractToolLookaheadDeltas scans raw SSE text in rawBuffer for pending
// function_call_arguments.delta or custom_tool_call_input.delta events and
// concatenates their decoded "delta" string values without full JSON parsing (Iron Rule #6).
func extractToolLookaheadDeltas(raw string) string {
	if !strings.Contains(raw, "function_call_arguments.delta") &&
		!strings.Contains(raw, "custom_tool_call_input.delta") {
		return ""
	}
	var sb strings.Builder
	search := raw
	for {
		idx := strings.Index(search, `"delta":`)
		if idx == -1 {
			break
		}
		lineStart := strings.LastIndexByte(search[:idx], '\n')
		if lineStart == -1 {
			lineStart = 0
		} else if lineStart > 0 {
			prevLineStart := strings.LastIndexByte(search[:lineStart], '\n')
			if prevLineStart == -1 {
				lineStart = 0
			} else {
				lineStart = prevLineStart + 1
			}
		}
		lineEndRel := strings.IndexByte(search[idx:], '\n')
		winEnd := len(search)
		if lineEndRel != -1 {
			winEnd = idx + lineEndRel
		}
		lineWindow := search[lineStart:winEnd]
		search = search[idx+8:]
		search = strings.TrimLeft(search, " \t")
		if len(search) == 0 || search[0] != '"' {
			continue
		}
		if !strings.Contains(lineWindow, "function_call_arguments.delta") &&
			!strings.Contains(lineWindow, "custom_tool_call_input.delta") {
			continue
		}
		escaped := false
		endQuote := -1
		for i := 1; i < len(search); i++ {
			c := search[i]
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				endQuote = i
				break
			}
		}
		if endQuote == -1 {
			break
		}
		rawLiteral := search[:endQuote+1]
		search = search[endQuote+1:]
		var decoded string
		if err := json.Unmarshal([]byte(rawLiteral), &decoded); err == nil {
			sb.WriteString(decoded)
		}
	}
	return sb.String()
}
