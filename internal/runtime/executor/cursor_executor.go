package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cursorwire "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// CursorExecutor talks to Cursor's AgentService (HTTP/2 duplex Connect-RPC) for
// text and MCP tool-call conversations, falling back to legacy AIServer ChatService
// only when multimodal image blocks are present.
type CursorExecutor struct {
	cfg *config.Config
}

// NewCursorExecutor creates a new Cursor executor.
func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	return &CursorExecutor{cfg: cfg}
}

// Identifier returns the executor identifier.
func (e *CursorExecutor) Identifier() string { return "cursor" }

// RequestToFormat reports the upstream request format used after auth selection.
func (e *CursorExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatOpenAI
}

// PrepareRequest injects Cursor credentials into the outgoing HTTP request.
func (e *CursorExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if req.URL != nil && strings.EqualFold(req.URL.Host, cursorwire.AgentHostHeader) {
		cursorwire.ApplyAgentHeaders(req, cursorCreds(auth), cursorMachineID(auth), true)
	} else {
		cursorwire.ApplyHeaders(req, cursorCreds(auth), true)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Cursor credentials into the request and executes it.
func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("cursor executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewDevinHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

type cursorUpstreamPlan struct {
	translatedBody []byte
	upstreamModel  string
	useAgent       bool
	agentFrame     []byte
	legacyBody     []byte
}

func extractCursorEffortFromPayload(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if eff := strings.TrimSpace(ojson.GetBytes(raw, "reasoning.effort").String()); eff != "" {
		return eff
	}
	if eff := strings.TrimSpace(ojson.GetBytes(raw, "reasoning_effort").String()); eff != "" {
		return eff
	}
	if thinkType := strings.ToLower(strings.TrimSpace(ojson.GetBytes(raw, "thinking.type").String())); thinkType != "" {
		switch thinkType {
		case "disabled":
			return "none"
		case "enabled":
			if budget := ojson.GetBytes(raw, "thinking.budget_tokens"); budget.Exists() {
				return strconv.FormatInt(budget.Int(), 10)
			}
			return "medium"
		}
	}
	return ""
}

func isCursorAgentCapable(body []byte) bool {
	messages := ojson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return false
	}
	items := messages.Array()
	if len(items) == 0 {
		return false
	}
	for _, msg := range items {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, part := range content.Array() {
			if part.Type == ojson.String || !part.IsObject() {
				continue
			}
			partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
			if partType != "" && partType != "text" && partType != "input_text" && partType != "output_text" {
				return false
			}
		}
	}
	return true
}

func (e *CursorExecutor) buildUpstreamPlan(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (cursorUpstreamPlan, error) {
	var plan cursorUpstreamPlan
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	payloadEffort := extractCursorEffortFromPayload(originalPayloadSource)
	if payloadEffort == "" {
		payloadEffort = extractCursorEffortFromPayload(req.Payload)
	}

	originalTranslated := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, bytes.Clone(originalPayloadSource), stream)
	body, err := helps.TranslateRequestReturningError(ctx, opts.Headers, e.cfg, from, to, baseModel, bytes.Clone(req.Payload), stream, false)
	if err != nil {
		return plan, err
	}

	if payloadEffort == "" {
		payloadEffort = extractCursorEffortFromPayload(body)
	}

	resolvedInitial := cursorwire.ResolveUpstreamModel(req.Model, payloadEffort)
	doc := ojson.NewDoc(body)
	if err = doc.Set("model", resolvedInitial); err != nil {
		return plan, fmt.Errorf("cursor executor: failed to set model in payload: %w", err)
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	helps.ApplyPayloadConfigWithRequestDoc(e.cfg, baseModel, to.String(), from.String(), "", doc, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = doc.Bytes()

	finalModelRaw := strings.TrimSpace(ojson.GetBytes(body, "model").String())
	if finalModelRaw == "" {
		return plan, statusErr{code: http.StatusBadRequest, msg: "cursor executor: model is required"}
	}
	// Re-resolve in case a payload override rule changed `model` or `reasoning_effort`.
	overrideEffort := extractCursorEffortFromPayload(body)
	if overrideEffort == "" {
		overrideEffort = payloadEffort
	}
	upstreamModel := cursorwire.ResolveUpstreamModel(finalModelRaw, overrideEffort)
	if upstreamModel == "" {
		return plan, statusErr{code: http.StatusBadRequest, msg: "cursor executor: model is required"}
	}

	plan.translatedBody = body
	plan.upstreamModel = upstreamModel
	plan.useAgent = isCursorAgentCapable(body)

	if plan.useAgent {
		plan.agentFrame = cursorwire.BuildAgentRunFrame(
			cursorAgentMessagesFromOpenAI(body),
			upstreamModel,
			cursorAgentToolsFromOpenAI(body),
		)
		return plan, nil
	}

	legacy, errLegacy := cursorwire.GenerateChatBody(cursorMessagesFromOpenAI(body), upstreamModel)
	if errLegacy != nil {
		return plan, errLegacy
	}
	plan.legacyBody = legacy
	return plan, nil
}

// buildUpstreamRequest translates the incoming payload to the canonical OpenAI
// shape and encodes it as a Cursor Connect-RPC body.
func (e *CursorExecutor) buildUpstreamRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (translated, native []byte, err error) {
	plan, errPlan := e.buildUpstreamPlan(ctx, req, opts, stream)
	if errPlan != nil {
		return nil, nil, errPlan
	}
	if plan.useAgent {
		return plan.translatedBody, plan.agentFrame, nil
	}
	return plan.translatedBody, plan.legacyBody, nil
}

func (e *CursorExecutor) doAgentUpstream(ctx context.Context, auth *cliproxyauth.Auth, initialFrame []byte, reporter *helps.UsageReporter) (*cursorwire.AgentDuplexSession, error) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	httpClient := helps.NewDevinHTTPClient(ctx, e.cfg, auth, 0)
	if reporter != nil {
		httpClient = reporter.TrackHTTPClient(httpClient)
	}

	sess, err := cursorwire.OpenAgentDuplexSession(ctx, httpClient, func(body io.Reader) (*http.Request, error) {
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cursorwire.AgentRunEndpoint, body)
		if errReq != nil {
			return nil, errReq
		}
		cursorwire.ApplyAgentHeaders(httpReq, cursorCreds(auth), cursorMachineID(auth), true)
		var attrs map[string]string
		if auth != nil {
			attrs = auth.Attributes
		}
		util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
		helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL:       cursorwire.AgentRunEndpoint,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})
		return httpReq, nil
	}, initialFrame)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}

	httpResp := sess.Response
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := cursorwire.DecodeMaybeGzip(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("cursor agent error, status: %d, message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		sess.Close()
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}
	return sess, nil
}

// doUpstream sends the legacy native Cursor body to api2 ChatService.
func (e *CursorExecutor) doUpstream(ctx context.Context, auth *cliproxyauth.Auth, native []byte, reporter *helps.UsageReporter) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorwire.ChatEndpoint, bytes.NewReader(native))
	if err != nil {
		return nil, err
	}
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       cursorwire.ChatEndpoint,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewDevinHTTPClient(ctx, e.cfg, auth, 0)
	if reporter != nil {
		httpClient = reporter.TrackHTTPClient(httpClient)
	}
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := cursorwire.DecodeMaybeGzip(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("cursor executor: close response body error: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}
	return httpResp, nil
}

// Execute performs a non-streaming chat completion request to Cursor.
func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	to := sdktranslator.FromString("openai")
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	plan, errBuild := e.buildUpstreamPlan(ctx, req, opts, false)
	if errBuild != nil {
		return resp, errBuild
	}
	body := plan.translatedBody

	var text, reasoning strings.Builder
	var toolCalls []cursorwire.AgentToolCall
	var respHeaders http.Header

	if plan.useAgent {
		sess, errSess := e.doAgentUpstream(ctx, auth, plan.agentFrame, reporter)
		if errSess != nil {
			return resp, errSess
		}
		defer sess.Close()
		respHeaders = sess.Response.Header.Clone()

		if err = sess.Consume(plan.upstreamModel, func(ev cursorwire.AgentStreamEvent) bool {
			switch ev.Type {
			case cursorwire.AgentEventText:
				text.WriteString(ev.Text)
			case cursorwire.AgentEventThinking:
				reasoning.WriteString(ev.Text)
			case cursorwire.AgentEventToolCall:
				if ev.ToolCall != nil {
					toolCalls = append(toolCalls, *ev.ToolCall)
				}
			}
			return true
		}); err != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, err)
			return resp, wrapCursorStreamError(err)
		}
	} else {
		httpResp, errResp := e.doUpstream(ctx, auth, plan.legacyBody, reporter)
		if errResp != nil {
			return resp, errResp
		}
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("cursor executor: close response body error: %v", errClose)
			}
		}()
		respHeaders = httpResp.Header.Clone()

		if err = cursorwire.ReadFrames(httpResp.Body, func(thinkingText, content string) {
			reasoning.WriteString(thinkingText)
			text.WriteString(content)
		}); err != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, err)
			return resp, wrapCursorStreamError(err)
		}
	}

	var completionSummary strings.Builder
	completionSummary.WriteString(reasoning.String())
	completionSummary.WriteString(text.String())
	for _, tc := range toolCalls {
		completionSummary.WriteString(tc.Name)
		completionSummary.WriteString(tc.Arguments)
	}

	respID := fmt.Sprintf("chatcmpl-msg_%d", time.Now().UnixMilli())
	created := time.Now().Unix()
	usage := cursorEstimateUsage(baseModel, body, completionSummary.String())
	data := buildOpenAICompletionFull(respID, created, req.Model, reasoning.String(), text.String(), toolCalls, usage)

	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, data, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	return cliproxyexecutor.Response{Payload: out, Headers: respHeaders}, nil
}

// ExecuteStream performs a streaming chat completion request to Cursor.
func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	plan, errBuild := e.buildUpstreamPlan(ctx, req, opts, true)
	if errBuild != nil {
		return nil, errBuild
	}
	body := plan.translatedBody

	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	originalPayload = bytes.Clone(originalPayload)

	if plan.useAgent {
		sess, errSess := e.doAgentUpstream(ctx, auth, plan.agentFrame, reporter)
		if errSess != nil {
			return nil, errSess
		}
		respHeaders := sess.Response.Header.Clone()
		out := make(chan cliproxyexecutor.StreamChunk)

		go func() {
			defer close(out)
			defer sess.Close()

			claudeInputTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
			var param any
			var produced strings.Builder
			respID := fmt.Sprintf("chatcmpl-msg_%d", time.Now().UnixMilli())
			created := time.Now().Unix()
			finishReason := "stop"
			toolIdx := 0

			emit := func(line []byte) bool {
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, line, &param, claudeInputTokens)
				for i := range chunks {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
					case <-ctx.Done():
						return false
					}
				}
				return true
			}

			cancelled := false
			errRead := sess.Consume(plan.upstreamModel, func(ev cursorwire.AgentStreamEvent) bool {
				switch ev.Type {
				case cursorwire.AgentEventThinking:
					if ev.Text == "" {
						return true
					}
					produced.WriteString(ev.Text)
					if !emit(buildOpenAIStreamChunkWithMeta(respID, created, req.Model, ev.Text, "", "", nil)) {
						cancelled = true
						return false
					}
				case cursorwire.AgentEventText:
					if ev.Text == "" {
						return true
					}
					produced.WriteString(ev.Text)
					if !emit(buildOpenAIStreamChunkWithMeta(respID, created, req.Model, "", ev.Text, "", nil)) {
						cancelled = true
						return false
					}
				case cursorwire.AgentEventToolCall:
					if ev.ToolCall == nil {
						return true
					}
					produced.WriteString(ev.ToolCall.Name)
					produced.WriteString(ev.ToolCall.Arguments)
					finishReason = "tool_calls"
					if !emit(buildOpenAIStreamToolCallChunk(respID, created, req.Model, toolIdx, *ev.ToolCall)) {
						cancelled = true
						return false
					}
					toolIdx++
				}
				return true
			})
			if cancelled {
				return
			}
			if errRead != nil {
				errRead = wrapCursorStreamError(errRead)
				helps.RecordAPIResponseError(ctx, e.cfg, errRead)
				reporter.PublishFailure(ctx, errRead)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
				case <-ctx.Done():
				}
				return
			}

			usageDetail := cursorEstimateUsage(baseModel, body, produced.String())
			reporter.Publish(ctx, helps.ParseOpenAIUsage(buildOpenAICompletion(req.Model, "", "", usageDetail)))
			if !emit(buildOpenAIStreamChunkWithMeta(respID, created, req.Model, "", "", finishReason, usageDetail)) {
				return
			}
			_ = emit([]byte("[DONE]"))
		}()

		return &cliproxyexecutor.StreamResult{Headers: respHeaders, Chunks: out}, nil
	}

	httpResp, errResp := e.doUpstream(ctx, auth, plan.legacyBody, reporter)
	if errResp != nil {
		return nil, errResp
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("cursor executor: close response body error: %v", errClose)
			}
		}()

		claudeInputTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
		var param any
		var produced strings.Builder

		emit := func(line []byte) bool {
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, line, &param, claudeInputTokens)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}

		cancelled := false
		errRead := cursorwire.ReadFrames(httpResp.Body, func(thinkingText, content string) {
			if cancelled || (thinkingText == "" && content == "") {
				return
			}
			produced.WriteString(thinkingText)
			produced.WriteString(content)
			if !emit(buildOpenAIStreamChunk(req.Model, thinkingText, content, nil)) {
				cancelled = true
			}
		})
		if cancelled {
			return
		}
		if errRead != nil {
			errRead = wrapCursorStreamError(errRead)
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			reporter.PublishFailure(ctx, errRead)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
			case <-ctx.Done():
			}
			return
		}

		usageDetail := cursorEstimateUsage(baseModel, body, produced.String())
		reporter.Publish(ctx, helps.ParseOpenAIUsage(buildOpenAICompletion(req.Model, "", "", usageDetail)))
		if !emit(buildOpenAIStreamChunk(req.Model, "", "", usageDetail)) {
			return
		}
		_ = emit([]byte("[DONE]"))
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates the token count for Cursor requests locally.
func (e *CursorExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	translated := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, bytes.Clone(req.Payload), false)
	enc, err := helps.TokenizerForModel(normalizeCursorUpstreamModel(baseModel))
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("cursor executor: tokenizer init failed: %w", err)
	}
	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("cursor executor: token counting failed: %w", err)
	}
	usageJSON := helps.BuildOpenAIUsageJSON(count)
	return cliproxyexecutor.Response{Payload: sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, usageJSON)}, nil
}

// Refresh is a no-op: Cursor issues a long-lived token and no refresh token.
func (e *CursorExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("cursor executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}

func wrapCursorStreamError(err error) error {
	var streamErr *cursorwire.StreamError
	if errors.As(err, &streamErr) {
		return statusErr{code: streamErr.HTTPStatus(), msg: streamErr.Error()}
	}
	return err
}

func cursorAgentMessagesFromOpenAI(body []byte) []cursorwire.AgentMessage {
	messages := ojson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return nil
	}
	items := messages.Array()
	out := make([]cursorwire.AgentMessage, 0, len(items))
	for _, msg := range items {
		role := strings.TrimSpace(msg.Get("role").String())
		if role == "" {
			role = "user"
		}
		text := flattenOpenAIContent(msg.Get("content"))
		toolCallID := strings.TrimSpace(msg.Get("tool_call_id").String())

		var toolCalls []cursorwire.AgentToolCall
		if tcs := msg.Get("tool_calls"); tcs.IsArray() {
			for _, tc := range tcs.Array() {
				name := strings.TrimSpace(tc.Get("function.name").String())
				if name == "" {
					name = strings.TrimSpace(tc.Get("name").String())
				}
				if name == "" {
					continue
				}
				args := tc.Get("function.arguments").String()
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				toolCalls = append(toolCalls, cursorwire.AgentToolCall{
					ID:        strings.TrimSpace(tc.Get("id").String()),
					Name:      name,
					Arguments: args,
				})
			}
		}

		if text == "" && len(toolCalls) == 0 && role != "tool" {
			continue
		}
		out = append(out, cursorwire.AgentMessage{
			Role:       role,
			Content:    text,
			ToolCalls:  toolCalls,
			ToolCallID: toolCallID,
		})
	}
	return out
}

func cursorAgentToolsFromOpenAI(body []byte) []cursorwire.AgentTool {
	tools := ojson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return nil
	}
	items := tools.Array()
	out := make([]cursorwire.AgentTool, 0, len(items))
	for _, item := range items {
		fn := item.Get("function")
		target := item
		if fn.IsObject() {
			target = fn
		}
		name := strings.TrimSpace(target.Get("name").String())
		if name == "" {
			continue
		}
		desc := target.Get("description").String()
		var schema any
		paramsNode := target.Get("parameters")
		if !paramsNode.Exists() {
			paramsNode = item.Get("input_schema")
		}
		if paramsNode.Exists() && len(paramsNode.Raw) > 0 {
			_ = json.Unmarshal([]byte(paramsNode.Raw), &schema)
		}
		out = append(out, cursorwire.AgentTool{
			Name:        name,
			Description: desc,
			Parameters:  schema,
		})
	}
	return out
}

// cursorMessagesFromOpenAI flattens an OpenAI chat payload into legacy Cursor turns.
func cursorMessagesFromOpenAI(body []byte) []cursorwire.Message {
	messages := ojson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return nil
	}
	items := messages.Array()
	out := make([]cursorwire.Message, 0, len(items))
	for _, msg := range items {
		role := strings.TrimSpace(msg.Get("role").String())
		if role == "" {
			role = "user"
		}
		text := flattenOpenAIContent(msg.Get("content"))
		if text == "" {
			continue
		}
		out = append(out, cursorwire.Message{Role: role, Content: text})
	}
	return out
}

func flattenOpenAIContent(content ojson.Result) string {
	switch {
	case !content.Exists() || content.Type == ojson.Null:
		return ""
	case content.Type == ojson.String:
		return content.String()
	case content.IsArray():
		items := content.Array()
		parts := make([]string, 0, len(items))
		for _, item := range items {
			if item.Type == ojson.String {
				if s := item.String(); s != "" {
					parts = append(parts, s)
				}
				continue
			}
			if text := item.Get("text").String(); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func buildOpenAIStreamChunk(model, reasoning, text string, usage []byte) []byte {
	finishReason := ""
	if usage != nil {
		finishReason = "stop"
	}
	return buildOpenAIStreamChunkWithMeta("", 0, model, reasoning, text, finishReason, usage)
}

func buildOpenAIStreamChunkWithMeta(id string, created int64, model, reasoning, text, finishReason string, usage []byte) []byte {
	doc := ojson.NewDocString(`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{}}]}`)
	if id != "" {
		_ = doc.Set("id", id)
	}
	if created > 0 {
		_ = doc.Set("created", created)
	}
	_ = doc.Set("model", model)
	if reasoning != "" {
		_ = doc.Set("choices.0.delta.reasoning_content", reasoning)
	}
	if text != "" {
		_ = doc.Set("choices.0.delta.role", "assistant")
		_ = doc.Set("choices.0.delta.content", text)
	}
	if finishReason != "" {
		_ = doc.Set("choices.0.finish_reason", finishReason)
	}
	if usage != nil {
		_ = doc.SetRawBytes("usage", usage)
	}
	return append([]byte("data: "), doc.Bytes()...)
}

func buildOpenAIStreamToolCallChunk(id string, created int64, model string, index int, tc cursorwire.AgentToolCall) []byte {
	doc := ojson.NewDocString(`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"type":"function","function":{"name":"","arguments":""}}]}}]}`)
	if id != "" {
		_ = doc.Set("id", id)
	}
	if created > 0 {
		_ = doc.Set("created", created)
	}
	_ = doc.Set("model", model)
	_ = doc.Set("choices.0.delta.tool_calls.0.index", index)
	_ = doc.Set("choices.0.delta.tool_calls.0.id", tc.ID)
	_ = doc.Set("choices.0.delta.tool_calls.0.function.name", tc.Name)
	_ = doc.Set("choices.0.delta.tool_calls.0.function.arguments", tc.Arguments)
	return append([]byte("data: "), doc.Bytes()...)
}

func buildOpenAICompletion(model, reasoning, text string, usage []byte) []byte {
	return buildOpenAICompletionFull("", 0, model, reasoning, text, nil, usage)
}

func buildOpenAICompletionFull(id string, created int64, model, reasoning, text string, toolCalls []cursorwire.AgentToolCall, usage []byte) []byte {
	doc := ojson.NewDocString(`{"object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":""}}]}`)
	if id != "" {
		_ = doc.Set("id", id)
	}
	if created > 0 {
		_ = doc.Set("created", created)
	}
	_ = doc.Set("model", model)
	if len(toolCalls) > 0 && text == "" {
		_ = doc.SetRaw("choices.0.message.content", "null")
	} else {
		_ = doc.Set("choices.0.message.content", text)
	}
	if reasoning != "" {
		_ = doc.Set("choices.0.message.reasoning_content", reasoning)
	}
	if len(toolCalls) > 0 {
		_ = doc.Set("choices.0.finish_reason", "tool_calls")
		_ = doc.SetRaw("choices.0.message.tool_calls", "[]")
		for _, tc := range toolCalls {
			tcDoc := ojson.NewDocString(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
			_ = tcDoc.Set("id", tc.ID)
			_ = tcDoc.Set("function.name", tc.Name)
			_ = tcDoc.Set("function.arguments", tc.Arguments)
			_ = doc.SetRawBytes("choices.0.message.tool_calls.-1", tcDoc.Bytes())
		}
	}
	if usage != nil {
		_ = doc.SetRawBytes("usage", usage)
	}
	return doc.Bytes()
}

func cursorEstimateUsage(model string, requestBody []byte, completion string) []byte {
	enc, err := helps.TokenizerForModel(normalizeCursorUpstreamModel(model))
	if err != nil {
		return nil
	}
	promptTokens, err := helps.CountOpenAIChatTokens(enc, requestBody)
	if err != nil {
		return nil
	}
	ids, _, err := enc.Encode(completion)
	if err != nil {
		return nil
	}
	completionTokens := int64(len(ids))
	doc := ojson.NewDocString(`{}`)
	_ = doc.Set("prompt_tokens", promptTokens)
	_ = doc.Set("completion_tokens", completionTokens)
	_ = doc.Set("total_tokens", promptTokens+completionTokens)
	return doc.Bytes()
}

// normalizeCursorUpstreamModel strips the CLIProxyAPI "cursor-" prefix and any
// thinking suffix, so callers such as TokenizerForModel receive clean IDs.
func normalizeCursorUpstreamModel(model string) string {
	base := strings.TrimSpace(thinking.ParseSuffix(strings.TrimSpace(model)).ModelName)
	if len(base) > len("cursor-") && strings.EqualFold(base[:len("cursor-")], "cursor-") {
		base = base[len("cursor-"):]
	}
	return base
}

func cursorCreds(a *cliproxyauth.Auth) string {
	if a == nil {
		return ""
	}
	if a.Metadata != nil {
		for _, key := range []string{"access_token", "cookie"} {
			if v, ok := a.Metadata[key].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"access_token", "cookie", "api_key"} {
			if v := strings.TrimSpace(a.Attributes[key]); v != "" {
				return v
			}
		}
	}
	return ""
}

func cursorMachineID(a *cliproxyauth.Auth) string {
	if a == nil {
		return ""
	}
	if a.Metadata != nil {
		for _, key := range []string{"machine_id", "machineId"} {
			if v, ok := a.Metadata[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"machine_id", "machineId"} {
			if v := strings.TrimSpace(a.Attributes[key]); v != "" {
				return v
			}
		}
	}
	return ""
}
