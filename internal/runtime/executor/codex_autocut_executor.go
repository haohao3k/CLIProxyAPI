package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/client/grokbuild"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

type codexAutocutActiveCtxKey struct{}

func withCodexAutocutActive(ctx context.Context, active bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexAutocutActiveCtxKey{}, active)
}

func isCodexAutocutActiveFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(codexAutocutActiveCtxKey{}).(bool)
	return v
}

func (e *CodexExecutor) isMeteringBypassActive(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	if opts.Alt == "responses/compact" || isCodexOpenAIImageRequest(opts) || codexCompactionSummaryFromContext(ctx) {
		return false
	}
	if opts.Headers != nil && strings.TrimSpace(opts.Headers.Get(CodexInternalCompactionHeader)) != "" {
		return false
	}
	if auth != nil && len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["metering_bypass"]); raw != "" {
			if parsed, err := strconv.ParseBool(raw); err == nil {
				return parsed
			}
		}
	}
	return e != nil && e.cfg != nil && e.cfg.Codex.MeteringBypassEnabled()
}

func (e *CodexExecutor) clampSolBaseModel(baseModel string) string {
	if e != nil && e.cfg != nil && !e.cfg.Codex.EffectiveAllowSolModel() {
		if strings.Contains(baseModel, "-sol") && !strings.Contains(baseModel, "-image") {
			return "gpt-6-astra"
		}
	}
	return baseModel
}

func payloadExplicitlyDeclaresImageGen(payload []byte) bool {
	if len(payload) == 0 || !bytes.Contains(payload, []byte("image_gen")) {
		return false
	}
	tools := ojson.GetBytesNoCopy(payload, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, t := range tools.Array() {
		if t.Get("type").String() == "image_generation" || isImageGenerationFunctionTool(t) {
			return true
		}
	}
	return false
}

func (e *CodexExecutor) checkCodexResponsesOnlyFormat(from sdktranslator.Format, baseModel string) error {
	if e != nil && e.cfg != nil && e.cfg.Codex.EffectiveAllowChatCompletions() {
		return nil
	}
	if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) || sourceFormatEqual(from, sdktranslator.FormatCodex) {
		return nil
	}
	ep := strings.TrimSpace(from.String())
	if ep == "" {
		ep = "non-responses"
	}
	RecordCodexSpecialTelemetryEvent("rejected_non_responses")
	log.Warnf("⚠️ [AC:REJECT-FORMAT] Rejected non-Responses request on %s for codex/%s (Responses API /v1/responses only)", ep, baseModel)
	return statusErr{
		code: http.StatusBadRequest,
		msg:  fmt.Sprintf("Codex models under metering-bypass only support the Responses API (/v1/responses). Request via %s is disabled to prevent 0%% prompt-cache hits and quota leaks.", ep),
	}
}

func extractResultItemText(item ojson.Result) string {
	content := item.Get("content")
	if content.Type == ojson.String {
		return content.String()
	}
	if content.IsArray() {
		var sb strings.Builder
		for _, part := range content.Array() {
			if part.Type == ojson.String {
				sb.WriteString(part.String())
			} else if txt := part.Get("text"); txt.Type == ojson.String && txt.String() != "" {
				if sb.Len() > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(txt.String())
			}
		}
		return sb.String()
	}
	return ""
}

func isWrapperOrCompactionUserText(text string) bool {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if trimmed == "" {
		return true
	}
	for _, p := range codexWrapperUserPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return isOwnCompactionNote(trimmed)
}

func deriveCodexConversationSessionSeed(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	itemsRes := ojson.GetBytesNoCopy(payload, "input")
	if !itemsRes.IsArray() {
		itemsRes = ojson.GetBytesNoCopy(payload, "messages")
	}
	if !itemsRes.IsArray() {
		if inputStr := strings.TrimSpace(itemsRes.String()); inputStr != "" {
			if len(inputStr) > 256 {
				inputStr = inputStr[:256]
			}
			return "u1:" + inputStr
		}
		return ""
	}

	var firstRealUser string
	var firstTurnAnchor string
	var assistantAccum strings.Builder

	for _, item := range itemsRes.Array() {
		if !item.IsObject() {
			continue
		}
		role := strings.TrimSpace(item.Get("role").String())
		itType := strings.TrimSpace(item.Get("type").String())

		if firstRealUser == "" && role == "user" && (itType == "" || itType == "message") {
			txt := strings.TrimSpace(extractResultItemText(item))
			if !isWrapperOrCompactionUserText(txt) {
				firstRealUser = txt
			}
		}

		if firstTurnAnchor == "" {
			if itType == "function_call" || itType == "custom_tool_call" {
				if cid := strings.TrimSpace(item.Get("call_id").String()); cid != "" {
					firstTurnAnchor = "fc:" + cid
				} else if id := strings.TrimSpace(item.Get("id").String()); id != "" {
					firstTurnAnchor = "id:" + id
				}
			} else if role == "assistant" && (itType == "" || itType == "message") {
				if id := strings.TrimSpace(item.Get("id").String()); id != "" {
					firstTurnAnchor = "msg:" + id
				} else if assistantAccum.Len() < 64 {
					assistantAccum.WriteString(strings.TrimSpace(extractResultItemText(item)))
					if assistantAccum.Len() >= 32 {
						s := assistantAccum.String()
						if len(s) > 64 {
							s = s[:64]
						}
						firstTurnAnchor = "asst:" + s
					}
				}
			}
		}

		if len(firstRealUser) >= 24 && firstTurnAnchor != "" {
			break
		}
	}

	if len(firstRealUser) >= 24 {
		if len(firstRealUser) > 256 {
			firstRealUser = firstRealUser[:256]
		}
		return "u1:" + firstRealUser
	}
	if firstTurnAnchor != "" {
		if len(firstTurnAnchor) > 128 {
			firstTurnAnchor = firstTurnAnchor[:128]
		}
		return "a2:" + firstTurnAnchor
	}
	if firstRealUser != "" {
		return "u0:" + firstRealUser
	}
	return ""
}

func normalizeCandidateSessionID(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" || len(v) > 256 {
		return ""
	}
	return strings.Clone(v)
}

func resolveCodexAutocutSessionID(ctx context.Context, from sdktranslator.Format, req cliproxyexecutor.Request, headers http.Header, auth *cliproxyauth.Auth) string {
	for _, field := range []string{"prompt_cache_key", "session_id", "conversation_id", "metadata.user_id"} {
		if candidate := normalizeCandidateSessionID(ojson.GetBytesNoCopy(req.Payload, field).String()); candidate != "" {
			return candidate
		}
	}
	if headers != nil {
		for _, k := range []string{"Session-Id", "session_id", "X-Session-Id", "Thread-Id", "X-Amp-Thread-Id", "X-Claude-Code-Session-Id", "X-Client-Request-Id"} {
			if candidate := normalizeCandidateSessionID(headers.Get(k)); candidate != "" {
				return candidate
			}
		}
	}
	if sid := normalizeCandidateSessionID(helps.ProviderSessionUUID("codex", req.Metadata)); sid != "" {
		return sid
	}
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		modelName := thinking.ParseSuffix(req.Model).ModelName
		if cached, ok, err := helps.ClaudeCodePromptCache(ctx, modelName, req.Payload, headers); err == nil && ok && cached.ID != "" {
			return cached.ID
		}
	}
	scopeKey := strings.TrimSpace(helps.APIKeyFromContext(ctx))
	if scopeKey == "" && auth != nil {
		scopeKey = strings.TrimSpace(auth.ID)
	}
	if convSeed := deriveCodexConversationSessionSeed(req.Payload); convSeed != "" {
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:conv:"+scopeKey+":"+convSeed)).String()
	}
	if scopeKey != "" {
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+scopeKey)).String()
	}
	return "codex-default"
}

func (e *CodexExecutor) handleLocalTitleGenExecute(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string) (cliproxyexecutor.Response, bool) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_TITLEGEN_LOCAL")), "off") {
		return cliproxyexecutor.Response{}, false
	}
	title, ok := detectCodexTitleGenRequest(req.Payload)
	if !ok {
		return cliproxyexecutor.Response{}, false
	}
	log.Infof("[TITLEGEN-LOCAL] intercepted non-stream | title=%q model=%s", title, baseModel)
	RecordCodexSpecialTelemetryEvent("titlegen_local")
	to := sdktranslator.FromString("codex")
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	completedData := buildLocalCodexTitleCompletedJSON(title, baseModel)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, originalPayload, req.Payload, completedData, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	return cliproxyexecutor.Response{
		Payload: out,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, true
}

func (e *CodexExecutor) handleLocalTitleGenStream(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string) (*cliproxyexecutor.StreamResult, bool) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_TITLEGEN_LOCAL")), "off") {
		return nil, false
	}
	title, ok := detectCodexTitleGenRequest(req.Payload)
	if !ok {
		return nil, false
	}
	log.Infof("[TITLEGEN-LOCAL] intercepted stream | title=%q model=%s", title, baseModel)
	RecordCodexSpecialTelemetryEvent("titlegen_local")
	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	isGrokClient := grokbuild.IsGrokClientContext(ctx, opts.Headers)
	sseBytes := buildLocalCodexTitleSSE(title, baseModel)
	scanner := helps.NewCodexSSEScanner(ctx, bytes.NewReader(sseBytes), len(sseBytes)+1024)
	claudeTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
	var param any
	var allChunks [][]byte
	for scanner.Scan() {
		line := scanner.Bytes()
		var translatedLine []byte
		if transformed, ok := grokbuild.TransformKeepaliveSSELine(line, isGrokClient); ok {
			translatedLine = transformed
		} else if bytes.HasPrefix(line, dataTag) {
			data := bytes.TrimSpace(line[5:])
			translatedLine = append([]byte("data: "), data...)
		} else {
			translatedLine = bytes.Clone(line)
		}
		chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, originalPayload, req.Payload, translatedLine, &param, claudeTokens)
		allChunks = append(allChunks, chunks...)
	}
	out := make(chan cliproxyexecutor.StreamChunk, len(allChunks))
	for _, c := range allChunks {
		out <- cliproxyexecutor.StreamChunk{Payload: c}
	}
	close(out)
	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  out,
	}, true
}

// applyMeteringBypassToDoc executes server-side Autocompact, 250k Hard Input Cap,
// Toolpad/Finalize injection, and :9gl-med model/effort/service_tier clamps on doc.
// Returns the post-compaction estimated input tokens and whether finalizeMode is active.
func (e *CodexExecutor) applyMeteringBypassToDoc(ctx context.Context, doc *ojson.Doc, baseModel, sessionID string, headers http.Header) (estInputTokens int, finalizeMode bool) {
	if doc == nil || e == nil || e.cfg == nil {
		return 0, false
	}
	raw := doc.Bytes()
	var bodyMap map[string]any
	if err := json.Unmarshal(raw, &bodyMap); err != nil || bodyMap == nil {
		return 0, false
	}

	cxCfg := &e.cfg.Codex
	finalizeMode = cxCfg.AutocutFinalizeEnabled()
	responsesLite := isCodexResponsesLiteRequestDoc(doc, headers)

	// Layers 3-6: Autocompact (Pass 0 keepRecent=0 + Pass 1 LRU + Pass 2 external chain) + 250k Hard Cap
	maybeCodexAutocompactMap(ctx, e.cfg, bodyMap, sessionID)
	enforceCodexInputHardCapMap(e.cfg, bodyMap)
	estInputTokens = estimateCodexTokensOf(bodyMap)

	// Layers 1-2: Toolpad + Forced Finalize (__finalize_9f3) + Trailing Developer Reminders
	injectCodexMeteringBypassMap(bodyMap, CodexInjectOptions{
		ResponsesLite: responsesLite,
		Model:         baseModel,
		Reinforcement: cxCfg.EffectiveAutocutReinforcement(),
		SessionKey:    sessionID,
		FinalizeMode:  finalizeMode,
		MaxToolChars:  cxCfg.EffectiveAutocutMaxToolChars(),
	})

	// :9gl-med Clamps (-sol -> gpt-6-astra, reasoning.effort -> medium, service_tier -> priority)
	applyCodexMeteringClampsMap(bodyMap, e.cfg, responsesLite)

	if sessionID != "" {
		if existingKey, _ := bodyMap["prompt_cache_key"].(string); strings.TrimSpace(existingKey) == "" {
			bodyMap["prompt_cache_key"] = sessionID
		}
	}

	mutated, err := json.Marshal(bodyMap)
	if err != nil {
		return estInputTokens, finalizeMode
	}
	origDoc := ojson.NewDoc(raw)
	newDoc := ojson.NewDoc(mutated)
	restoreToolPropertyOrderAndEnsurePadLast(origDoc, newDoc)
	*doc = *newDoc
	return estInputTokens, finalizeMode
}

func movePadKeyLastAtPropsPath(origDoc, doc *ojson.Doc, propsPath string) {
	if doc == nil {
		return
	}
	padPath := propsPath + "." + CodexPadKey
	padVal := doc.Get(padPath)
	if !padVal.Exists() {
		return
	}
	padRaw := padVal.Raw
	// Restore client's original property insertion order when available so Go's
	// alphabetical map marshaling never scrambles tool schemas or busts prompt cache.
	if origDoc != nil {
		if origProps := origDoc.Get(propsPath); origProps.IsObject() {
			_ = doc.SetRaw(propsPath, origProps.Raw)
		}
	}
	_ = doc.Delete(padPath)
	_ = doc.SetRaw(padPath, padRaw)
}

func movePadKeyLastForToolAtPath(origDoc, doc *ojson.Doc, toolPath string) {
	if doc == nil {
		return
	}
	t := doc.Get(toolPath)
	if !t.IsObject() {
		return
	}
	if strings.TrimSpace(t.Get("type").String()) == "namespace" {
		if sub := t.Get("tools"); sub.IsArray() {
			for j := range sub.Array() {
				movePadKeyLastForToolAtPath(origDoc, doc, fmt.Sprintf("%s.tools.%d", toolPath, j))
			}
		}
		return
	}
	if t.Get("parameters.properties").IsObject() {
		movePadKeyLastAtPropsPath(origDoc, doc, toolPath+".parameters.properties")
	}
	if t.Get("function.parameters.properties").IsObject() {
		movePadKeyLastAtPropsPath(origDoc, doc, toolPath+".function.parameters.properties")
	}
}

// restoreToolPropertyOrderAndEnsurePadLast prevents Go's encoding/json alphabetical map key
// sorting from placing "__pad9f3" ('_' = ASCII 95) BEFORE lowercase tool parameters ('a'..'z' = 97..122),
// which otherwise causes the model to generate "__pad9f3" first and cut before real arguments!
func restoreToolPropertyOrderAndEnsurePadLast(origDoc, doc *ojson.Doc) {
	if doc == nil {
		return
	}
	if tools := doc.Get("tools"); tools.IsArray() {
		for i := range tools.Array() {
			movePadKeyLastForToolAtPath(origDoc, doc, fmt.Sprintf("tools.%d", i))
		}
	}
	if input := doc.Get("input"); input.IsArray() {
		for idx, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
				continue
			}
			if sub := item.Get("tools"); sub.IsArray() {
				for j := range sub.Array() {
					movePadKeyLastForToolAtPath(origDoc, doc, fmt.Sprintf("input.%d.tools.%d", idx, j))
				}
			}
		}
	}
}

// enforceMeteringClampsPostFinalizeDoc ensures user payload rules in FinalizePayloadDoc
// cannot accidentally re-enable high/xhigh reasoning effort (top-level or configuration_update),
// re-introduce unsupported parameters, or strip service_tier="priority" / store=false.
func (e *CodexExecutor) enforceMeteringClampsPostFinalizeDoc(doc *ojson.Doc) {
	if doc == nil || e == nil || e.cfg == nil {
		return
	}
	cxCfg := &e.cfg.Codex
	isHighEffort := func(eff string) bool {
		switch strings.ToLower(strings.TrimSpace(eff)) {
		case "high", "xhigh", "max", "ultra":
			return true
		default:
			return false
		}
	}
	if !cxCfg.EffectiveAllowHighEffort() {
		if isHighEffort(doc.Get("reasoning.effort").String()) {
			_ = doc.Set("reasoning.effort", "medium")
		}
		if isHighEffort(doc.Get("effort").String()) {
			_ = doc.Set("effort", "medium")
		}
		if input := doc.Get("input"); input.IsArray() {
			for idx, item := range input.Array() {
				if strings.TrimSpace(item.Get("type").String()) != "configuration_update" {
					continue
				}
				if isHighEffort(item.Get("reasoning.effort").String()) {
					_ = doc.Set(fmt.Sprintf("input.%d.reasoning.effort", idx), "medium")
				}
				if isHighEffort(item.Get("effort").String()) {
					_ = doc.Set(fmt.Sprintf("input.%d.effort", idx), "medium")
				}
			}
		}
	}
	if !cxCfg.EffectiveAllowSolModel() {
		if m := strings.TrimSpace(doc.Get("model").String()); strings.Contains(m, "-sol") && !strings.Contains(m, "-image") {
			_ = doc.Set("model", "gpt-6-astra")
		}
	}
	helps.SetBoolIfDifferentDoc(doc, "store", false)
	helps.SetBoolIfDifferentDoc(doc, "stream", true)
	helps.SetStringIfDifferentDoc(doc, "service_tier", "priority")
	for _, unsupported := range []string{
		"max_output_tokens", "max_tokens", "max_completion_tokens",
		"temperature", "top_p", "frequency_penalty", "presence_penalty",
		"logprobs", "top_logprobs", "n", "seed", "user", "metadata",
		"background", "previous_response_id", "prompt_cache_retention", "safety_identifier",
	} {
		_ = doc.Delete(unsupported)
	}
	restoreToolPropertyOrderAndEnsurePadLast(nil, doc)
}

type codexAutocutReadCloser struct {
	pr           *io.PipeReader
	rawBody      io.ReadCloser
	tracker      *codexTCPConnTracker
	transport    *http.Transport
	stopCtxReset func() bool
	closeOnce    sync.Once
}

func (r *codexAutocutReadCloser) Read(p []byte) (int, error) {
	if r.pr != nil {
		return r.pr.Read(p)
	}
	if r.rawBody != nil {
		return r.rawBody.Read(p)
	}
	return 0, io.EOF
}

func (r *codexAutocutReadCloser) Close() error {
	var closeErr error
	r.closeOnce.Do(func() {
		if r.stopCtxReset != nil {
			r.stopCtxReset()
		}
		if r.tracker != nil {
			r.tracker.hardResetAll()
		}
		if r.pr != nil {
			_ = r.pr.Close()
		}
		if r.rawBody != nil {
			closeErr = r.rawBody.Close()
		}
		if r.transport != nil {
			r.transport.CloseIdleConnections()
		}
	})
	return closeErr
}

var codexSidecarRetryDelay = 2000 * time.Millisecond

func isCodexSidecarRetryableStatus(status int) bool {
	return status == http.StatusInternalServerError ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

// executeViaInProcessGoSidecar runs the exact 9router Go Sidecar data-plane natively in-process:
//   - Dedicated 1-to-1 HTTP/1.1 TLS socket (TLSNextProto empty map, ForceAttemptHTTP2=false, DisableKeepAlives=true)
//   - Native CLIProxyAPI Codex OAuth credentials & headers (Authorization Bearer, Chatgpt-Account-Id, Originator)
//   - In-place retry up to 2 attempts on 5xx or non-accountFallback 200-OK SSE transient overloads
//   - Immediate conductor multi-account failover on "selected model is at capacity" / model_at_capacity
//   - 5h Quota Circuit Breaker observation + StreamWithAutocut with SetLinger(0) kernel TCP RST
func (e *CodexExecutor) executeViaInProcessGoSidecar(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	baseReq *http.Request,
	upstreamBody []byte,
	prepareHeaders func(*http.Request),
	upstreamModel string,
	sessionID string,
	finalizeMode bool,
	estInputTokens int,
	reporter *helps.UsageReporter,
	streamMode bool,
) (*http.Response, error) {
	wireBody := upstreamBody
	if baseReq != nil && baseReq.GetBody != nil {
		if rc, errGet := baseReq.GetBody(); errGet == nil && rc != nil {
			if b, errRead := io.ReadAll(rc); errRead == nil && len(b) > 0 {
				wireBody = b
			}
			_ = rc.Close()
		}
	}

	const maxRetries = 2
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}

		httpClient, transport, tracker := newCodexAutocutHTTPClient(e.cfg, auth, "")
		if reporter != nil {
			if streamMode {
				httpClient = reporter.TrackHTTPClientRoundTripOnly(httpClient)
			} else {
				httpClient = reporter.TrackHTTPClient(httpClient)
			}
		}

		attemptReq, errNew := http.NewRequestWithContext(ctx, baseReq.Method, baseReq.URL.String(), bytes.NewReader(wireBody))
		if errNew != nil {
			transport.CloseIdleConnections()
			return nil, errNew
		}
		attemptReq.Header = baseReq.Header.Clone()
		prepareSidecarHeaders := func(r *http.Request) {
			if prepareHeaders != nil {
				prepareHeaders(r)
			}
			// Force identity encoding and 1-to-1 connection isolation for instant TCP RST.
			r.Header.Set("Accept-Encoding", "identity")
			r.Header.Set("Connection", "close")
			r.Close = true
		}
		prepareSidecarHeaders(attemptReq)

		stopCtxReset := context.AfterFunc(ctx, func() {
			tracker.hardResetAll()
		})

		httpResp, errDo := doCodexRequest(httpClient, attemptReq, upstreamBody, prepareSidecarHeaders, nil)
		if errDo != nil {
			stopCtxReset()
			tracker.hardResetAll()
			transport.CloseIdleConnections()
			if attempt < maxRetries && ctx.Err() == nil {
				log.Warnf("[GO-SIDECAR] upstream network error (%v) -> retrying attempt %d/%d", errDo, attempt+1, maxRetries)
				select {
				case <-time.After(codexSidecarRetryDelay):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return nil, errDo
		}

		usedPct, remainingPct, quotaDeltaPct, hasQuota := observeCodexQuotaCircuitBreakerWithMeta(ctx, e.cfg, auth, httpResp.Header, sessionID, upstreamModel)

		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			if isCodexSidecarRetryableStatus(httpResp.StatusCode) && attempt < maxRetries && ctx.Err() == nil {
				stopCtxReset()
				_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, 4096))
				_ = httpResp.Body.Close()
				tracker.hardResetAll()
				transport.CloseIdleConnections()
				log.Warnf("[GO-SIDECAR] HTTP %d -> retrying attempt %d/%d", httpResp.StatusCode, attempt+1, maxRetries)
				select {
				case <-time.After(codexSidecarRetryDelay):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			// Hand non-retryable or final status error back to CodexExecutor's status handler,
			// ensuring tracker/transport still clean up when httpResp.Body is closed.
			rawErrBody := httpResp.Body
			httpResp.Body = &codexAutocutReadCloser{
				pr:           nil,
				rawBody:      rawErrBody,
				tracker:      tracker,
				transport:    transport,
				stopCtxReset: stopCtxReset,
			}
			return httpResp, nil
		}

		rawBody := httpResp.Body
		peek := peekCodexSSETransientError(rawBody)
		if peek.matched != "" {
			stopCtxReset()
			tracker.hardResetAll()
			_ = rawBody.Close()
			transport.CloseIdleConnections()

			if !peek.accountFallback && attempt < maxRetries && ctx.Err() == nil {
				log.Warnf("[GO-SIDECAR] 200-OK SSE transient error (%s: %s) -> retrying attempt %d/%d", peek.matched, peek.message, attempt+1, maxRetries)
				select {
				case <-time.After(codexSidecarRetryDelay):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}

			errPayload, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": peek.message,
					"type":    "server_error",
					"code":    "service_unavailable",
				},
			})
			log.Warnf("[GO-SIDECAR] 200-OK SSE capacity/overload (%s: %s, accountFallback=%v) -> failing over account", peek.matched, peek.message, peek.accountFallback)
			return nil, newCodexBootstrapOverloadErr(errPayload)
		}

		hardArgCap := DefaultHardArgCap
		telemetryLogPath := ""
		if e != nil && e.cfg != nil {
			hardArgCap = e.cfg.Codex.EffectiveAutocutHardArgCap()
			telemetryLogPath = resolveCodexTelemetryLogPath(e.cfg, "")
		}
		authID, authLabel, authEmail := extractAuthIdentity(auth)

		combinedUpstream := io.MultiReader(bytes.NewReader(peek.prefixBytes), rawBody)
		pr, pw := io.Pipe()
		go func() {
			_ = StreamWithAutocut(combinedUpstream, pw, nil, CodexAutocutOptions{
				Model:               upstreamModel,
				SessionID:           sessionID,
				FinalizeMode:        finalizeMode,
				HardArgCap:          hardArgCap,
				PostcallAbortMin:    5000,
				EstimateInputTokens: estInputTokens,
				AuthID:              authID,
				AuthLabel:           authLabel,
				AuthEmail:           authEmail,
				HasQuota:            hasQuota,
				UsedPct:             usedPct,
				RemainingPct:        remainingPct,
				QuotaDeltaPct:       quotaDeltaPct,
				TelemetryLogPath:    telemetryLogPath,
				OnAbortUpstream: func(how string) {
					tracker.hardResetAll()
					_ = rawBody.Close()
				},
			})
			_ = pw.Close()
		}()

		httpResp.Body = &codexAutocutReadCloser{
			pr:           pr,
			rawBody:      rawBody,
			tracker:      tracker,
			transport:    transport,
			stopCtxReset: stopCtxReset,
		}
		return httpResp, nil
	}

	return nil, statusErr{code: http.StatusBadGateway, msg: "go-sidecar retry loop exhausted"}
}
