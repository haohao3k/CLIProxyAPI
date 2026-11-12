package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

var comboRoundRobinCounters sync.Map // map[string]*atomic.Uint64

func stripComboContextMarker(model string) string {
	trimmed := strings.TrimSpace(model)
	for len(trimmed) > 4 && strings.EqualFold(trimmed[len(trimmed)-4:], "[1m]") {
		trimmed = strings.TrimSpace(trimmed[:len(trimmed)-4])
	}
	return trimmed
}

func parseComboLookupName(modelName string) (baseName string, rawSuffix string, hasSuffix bool) {
	cleaned := stripComboContextMarker(modelName)
	if cleaned == "" {
		return "", "", false
	}
	parsed := thinking.ParseSuffix(cleaned)
	baseName = stripComboContextMarker(strings.TrimSpace(parsed.ModelName))
	rawSuffix = strings.TrimSpace(parsed.RawSuffix)
	return baseName, rawSuffix, parsed.HasSuffix && rawSuffix != ""
}

func (h *BaseAPIHandler) resolveCombo(modelName string, execOptions modelExecutionOptions) (*config.ComboConfig, bool) {
	if h == nil {
		return nil, false
	}
	cfg := h.CurrentConfig()
	if cfg == nil || len(cfg.Combos) == 0 {
		return nil, false
	}
	baseName, rawSuffix, hasSuffix := parseComboLookupName(modelName)
	if baseName == "" {
		return nil, false
	}
	baseKey := strings.ToLower(baseName)
	if execOptions.VisitedCombos != nil && execOptions.VisitedCombos[baseKey] {
		return nil, false
	}
	for i := range cfg.Combos {
		combo := &cfg.Combos[i]
		if combo.Disabled {
			continue
		}
		name := strings.TrimSpace(combo.Name)
		if name == "" || len(combo.Models) == 0 {
			continue
		}
		if strings.EqualFold(name, baseName) {
			nameKey := strings.ToLower(name)
			if execOptions.VisitedCombos != nil && execOptions.VisitedCombos[nameKey] {
				return nil, false
			}
			copied := *combo
			copied.Models = make([]string, 0, len(combo.Models))
			for _, candidate := range combo.Models {
				cand := strings.TrimSpace(candidate)
				if cand == "" {
					continue
				}
				if hasSuffix && !thinking.ParseSuffix(cand).HasSuffix {
					cand = cand + "(" + rawSuffix + ")"
				}
				copied.Models = append(copied.Models, cand)
			}
			if len(copied.Models) == 0 {
				return nil, false
			}
			return &copied, true
		}
	}
	return nil, false
}

func comboOrderedCandidates(combo *config.ComboConfig) []string {
	if combo == nil || len(combo.Models) == 0 {
		return nil
	}
	candidates := make([]string, 0, len(combo.Models))
	for _, raw := range combo.Models {
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			candidates = append(candidates, trimmed)
		}
	}
	if len(candidates) <= 1 {
		return candidates
	}
	if combo.EffectiveStrategy() == config.ComboStrategyRoundRobin {
		key := strings.ToLower(strings.TrimSpace(combo.Name))
		val, _ := comboRoundRobinCounters.LoadOrStore(key, &atomic.Uint64{})
		counter := val.(*atomic.Uint64)
		start := int((counter.Add(1) - 1) % uint64(len(candidates)))
		if start == 0 {
			return candidates
		}
		rotated := make([]string, 0, len(candidates))
		rotated = append(rotated, candidates[start:]...)
		rotated = append(rotated, candidates[:start]...)
		return rotated
	}
	// Default "fallback": strictly preserve configured top-to-bottom order (0, 1, 2, ...)
	// so session-affinity and upstream prompt caches stay pinned to candidate #1 when healthy.
	return candidates
}

func nextComboExecOptions(execOptions modelExecutionOptions, comboName string) modelExecutionOptions {
	next := execOptions
	visited := make(map[string]bool, len(execOptions.VisitedCombos)+1)
	for k, v := range execOptions.VisitedCombos {
		visited[k] = v
	}
	visited[strings.ToLower(strings.TrimSpace(comboName))] = true
	next.VisitedCombos = visited
	return next
}

func rewriteModelInRawJSON(rawJSON []byte, targetModel string) []byte {
	targetModel = strings.TrimSpace(targetModel)
	if len(rawJSON) == 0 || targetModel == "" {
		return rawJSON
	}
	if !ojson.GetBytesNoCopy(rawJSON, "model").Exists() {
		return rawJSON
	}
	if _, qualModel := resolveProviderQualifiedModel(targetModel, targetModel); qualModel != "" {
		targetModel = qualModel
	}
	doc := ojson.NewDoc(rawJSON)
	if err := doc.Set("model", targetModel); err != nil {
		return rawJSON
	}
	return doc.Bytes()
}

func shouldFallbackComboError(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil {
		return false
	}
	if errMsg.Error != nil && errors.Is(errMsg.Error, context.Canceled) {
		return false
	}
	return true
}

func itemContainsImageModality(item ojson.Result) bool {
	if !item.IsObject() {
		return item.Type == ojson.String && strings.Contains(item.String(), "data:image/")
	}
	itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
	switch itemType {
	case "image_url", "input_image", "image":
		return true
	}
	if item.Get("image_url").Exists() {
		return true
	}
	if mt := strings.ToLower(strings.TrimSpace(item.Get("source.media_type").String())); strings.HasPrefix(mt, "image/") {
		return true
	}
	if mt := strings.ToLower(strings.TrimSpace(item.Get("inlineData.mimeType").String())); strings.HasPrefix(mt, "image/") {
		return true
	}
	if mt := strings.ToLower(strings.TrimSpace(item.Get("inline_data.mime_type").String())); strings.HasPrefix(mt, "image/") {
		return true
	}
	for _, field := range []string{"content", "parts"} {
		sub := item.Get(field)
		if sub.Type == ojson.String && strings.Contains(sub.String(), "data:image/") {
			return true
		}
		if sub.IsArray() {
			for _, part := range sub.Array() {
				if itemContainsImageModality(part) {
					return true
				}
			}
		}
	}
	return false
}

// payloadRequiresVisionInTrailingTurn mirrors 9router detectRequiredCapabilities()
// (open-sse/services/combo.js:63-138): only inspect the trailing user run after the
// last assistant/model/tool turn so historical images don't permanently pin the combo.
func payloadRequiresVisionInTrailingTurn(rawJSON []byte) bool {
	if len(rawJSON) == 0 || !ojson.ValidBytes(rawJSON) {
		return false
	}
	for _, key := range []string{"messages", "input", "contents"} {
		list := ojson.GetBytesNoCopy(rawJSON, key)
		if !list.IsArray() {
			continue
		}
		items := list.Array()
		if len(items) == 0 {
			continue
		}
		lastNonUser := -1
		for i, item := range items {
			role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			itype := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
			if role == "assistant" || role == "model" || role == "tool" ||
				itype == "function_call" || itype == "function_call_output" || itype == "custom_tool_call_output" {
				lastNonUser = i
			}
		}
		for i := lastNonUser + 1; i < len(items); i++ {
			if itemContainsImageModality(items[i]) {
				return true
			}
		}
	}
	return false
}

func lookupComboCandidateModelInfo(candidate string) *registry.ModelInfo {
	base, _, _ := parseComboLookupName(candidate)
	if base == "" {
		return nil
	}
	provider := ""
	modelID := base
	if p, rest, ok := strings.Cut(base, "/"); ok && strings.TrimSpace(rest) != "" {
		provider = strings.ToLower(strings.TrimSpace(p))
		modelID = strings.TrimSpace(rest)
	}
	if provider != "" {
		for _, provKey := range []string{provider, "openai-compatible-" + provider} {
			if info := registry.LookupModelInfo(modelID, provKey); info != nil && len(info.SupportedInputModalities) > 0 {
				return info
			}
		}
		if info := registry.LookupStaticModelInfoByChannel(modelID, provider); info != nil && len(info.SupportedInputModalities) > 0 {
			return info
		}
	}
	if info := registry.LookupModelInfo(modelID); info != nil && len(info.SupportedInputModalities) > 0 {
		return info
	}
	return registry.LookupStaticModelInfo(modelID)
}

// reorderComboCandidatesByVision mirrors 9router reorderByCapabilities()
// (open-sse/services/combo.js:167-184, 285-294): when the trailing user turn carries
// an image, stably float vision-capable (and unknown/custom) candidates ahead of
// explicitly text-only candidates without dropping any fallback candidate.
func reorderComboCandidatesByVision(comboName string, candidates []string, rawJSON []byte) []string {
	if len(candidates) <= 1 || !payloadRequiresVisionInTrailingTurn(rawJSON) {
		return candidates
	}
	visionTier := make([]string, 0, len(candidates))
	textOnlyTier := make([]string, 0, len(candidates))
	for _, cand := range candidates {
		info := lookupComboCandidateModelInfo(cand)
		if info != nil && len(info.SupportedInputModalities) > 0 && (!info.UserDefined || info.ExplicitInputModalities) {
			hasVision := false
			for _, m := range info.SupportedInputModalities {
				switch strings.ToLower(strings.TrimSpace(m)) {
				case "image", "images", "vision":
					hasVision = true
				}
			}
			if !hasVision {
				textOnlyTier = append(textOnlyTier, cand)
				continue
			}
		}
		visionTier = append(visionTier, cand)
	}
	if len(textOnlyTier) == 0 || len(visionTier) == 0 {
		return candidates
	}
	reordered := make([]string, 0, len(candidates))
	reordered = append(reordered, visionTier...)
	reordered = append(reordered, textOnlyTier...)
	changed := false
	for i := range candidates {
		if candidates[i] != reordered[i] {
			changed = true
			break
		}
	}
	if changed {
		log.Infof("🔀 [COMBO] %q: auto-switched candidate order for [vision] -> %v", comboName, reordered)
	}
	return reordered
}

func (h *BaseAPIHandler) executeComboWithAuthManagerFormats(
	ctx context.Context,
	combo *config.ComboConfig,
	entryProtocol, exitProtocol string,
	rawJSON []byte,
	alt string,
	allowImageModel bool,
	execOptions modelExecutionOptions,
) ([]byte, http.Header, *interfaces.ErrorMessage) {
	candidates := reorderComboCandidatesByVision(combo.Name, comboOrderedCandidates(combo), rawJSON)
	if len(candidates) == 0 {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("combo %q has no target models", combo.Name),
		}
	}
	nextOpts := nextComboExecOptions(execOptions, combo.Name)
	var lastErr *interfaces.ErrorMessage
	for idx, candidate := range candidates {
		if ctx != nil && ctx.Err() != nil {
			if lastErr != nil {
				return nil, nil, lastErr
			}
			return nil, nil, &interfaces.ErrorMessage{
				StatusCode: http.StatusRequestTimeout,
				Error:      ctx.Err(),
			}
		}
		candidateJSON := rewriteModelInRawJSON(rawJSON, candidate)
		resp, headers, errMsg := h.executeWithAuthManagerFormats(ctx, entryProtocol, exitProtocol, candidate, candidateJSON, alt, allowImageModel, nextOpts)
		if errMsg == nil {
			return resp, headers, nil
		}
		lastErr = errMsg
		if !shouldFallbackComboError(errMsg) || idx == len(candidates)-1 {
			break
		}
		nextCandidate := candidates[idx+1]
		log.Warnf("🔀 [COMBO] %q: candidate %d/%d (%s) failed (status=%d, err=%v), falling back -> %s", combo.Name, idx+1, len(candidates), candidate, errMsg.StatusCode, errMsg.Error, nextCandidate)
	}
	return nil, nil, lastErr
}

func (h *BaseAPIHandler) executeComboStreamWithAuthManagerFormats(
	ctx context.Context,
	combo *config.ComboConfig,
	entryProtocol, exitProtocol string,
	rawJSON []byte,
	alt string,
	allowImageModel bool,
	execOptions modelExecutionOptions,
) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	candidates := reorderComboCandidatesByVision(combo.Name, comboOrderedCandidates(combo), rawJSON)
	if len(candidates) == 0 {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("combo %q has no target models", combo.Name),
		}
		close(errChan)
		return nil, nil, errChan
	}
	nextOpts := nextComboExecOptions(execOptions, combo.Name)
	var lastErr *interfaces.ErrorMessage
	for idx, candidate := range candidates {
		if ctx != nil && ctx.Err() != nil {
			if lastErr == nil {
				lastErr = &interfaces.ErrorMessage{
					StatusCode: http.StatusRequestTimeout,
					Error:      ctx.Err(),
				}
			}
			break
		}
		candidateJSON := rewriteModelInRawJSON(rawJSON, candidate)
		dataChan, headers, errChan := h.executeStreamWithAuthManagerFormats(ctx, entryProtocol, exitProtocol, candidate, candidateJSON, alt, allowImageModel, nextOpts)
		if dataChan != nil {
			return dataChan, headers, errChan
		}
		var errMsg *interfaces.ErrorMessage
		if errChan != nil {
			errMsg = <-errChan
		}
		if errMsg == nil {
			errMsg = &interfaces.ErrorMessage{
				StatusCode: http.StatusBadGateway,
				Error:      fmt.Errorf("combo %q candidate %q failed to open stream", combo.Name, candidate),
			}
		}
		lastErr = errMsg
		if !shouldFallbackComboError(errMsg) || idx == len(candidates)-1 {
			break
		}
		nextCandidate := candidates[idx+1]
		log.Warnf("🔀 [COMBO] %q (stream): candidate %d/%d (%s) failed (status=%d, err=%v), falling back -> %s", combo.Name, idx+1, len(candidates), candidate, errMsg.StatusCode, errMsg.Error, nextCandidate)
	}
	outErrChan := make(chan *interfaces.ErrorMessage, 1)
	if lastErr != nil {
		outErrChan <- lastErr
	}
	close(outErrChan)
	return nil, nil, outErrChan
}

func (h *BaseAPIHandler) executeComboCountWithAuthManager(
	ctx context.Context,
	combo *config.ComboConfig,
	handlerType string,
	rawJSON []byte,
	alt string,
	execOptions modelExecutionOptions,
) ([]byte, http.Header, *interfaces.ErrorMessage) {
	candidates := comboOrderedCandidates(combo)
	if len(candidates) == 0 {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("combo %q has no target models", combo.Name),
		}
	}
	nextOpts := nextComboExecOptions(execOptions, combo.Name)
	var lastErr *interfaces.ErrorMessage
	for idx, candidate := range candidates {
		if ctx != nil && ctx.Err() != nil {
			if lastErr != nil {
				return nil, nil, lastErr
			}
			return nil, nil, &interfaces.ErrorMessage{
				StatusCode: http.StatusRequestTimeout,
				Error:      ctx.Err(),
			}
		}
		candidateJSON := rewriteModelInRawJSON(rawJSON, candidate)
		resp, headers, errMsg := h.executeCountWithAuthManager(ctx, handlerType, candidate, candidateJSON, alt, nextOpts)
		if errMsg == nil {
			return resp, headers, nil
		}
		lastErr = errMsg
		if !shouldFallbackComboError(errMsg) || idx == len(candidates)-1 {
			break
		}
		log.Debugf("combo %q (count): candidate %d/%d (%s) failed (status=%d, err=%v), falling back to next model", combo.Name, idx+1, len(candidates), candidate, errMsg.StatusCode, errMsg.Error)
	}
	return nil, nil, lastErr
}

// AppendComboModels appends enabled model combos from configuration to a handler's /v1/models list.
func (h *BaseAPIHandler) AppendComboModels(handlerType string, baseModels []map[string]any) []map[string]any {
	if h == nil {
		return baseModels
	}
	cfg := h.CurrentConfig()
	if cfg == nil || len(cfg.Combos) == 0 {
		return baseModels
	}
	seen := make(map[string]struct{}, len(baseModels))
	for _, m := range baseModels {
		if id, ok := m["id"].(string); ok && strings.TrimSpace(id) != "" {
			seen[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
		}
		if name, ok := m["name"].(string); ok && strings.TrimSpace(name) != "" {
			trimmed := strings.TrimPrefix(strings.TrimSpace(name), "models/")
			seen[strings.ToLower(trimmed)] = struct{}{}
		}
	}
	out := make([]map[string]any, 0, len(baseModels)+len(cfg.Combos))
	out = append(out, baseModels...)
	kind := strings.ToLower(strings.TrimSpace(handlerType))
	for _, combo := range cfg.Combos {
		if combo.Disabled {
			continue
		}
		name := strings.TrimSpace(combo.Name)
		if name == "" || len(combo.Models) == 0 {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		switch kind {
		case "gemini":
			out = append(out, map[string]any{
				"name":                       "models/" + name,
				"displayName":                name,
				"description":                "Combo: " + strings.Join(combo.Models, " -> "),
				"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
			})
		case "claude":
			out = append(out, map[string]any{
				"id":           name,
				"type":         "model",
				"object":       "model",
				"display_name": name,
				"created_at":   "2026-01-01T00:00:00Z",
				"owned_by":     "combo",
			})
		default:
			out = append(out, map[string]any{
				"id":       name,
				"object":   "model",
				"created":  int64(1735689600),
				"owned_by": "combo",
			})
		}
	}
	return out
}
