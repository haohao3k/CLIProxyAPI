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
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

var comboRoundRobinCounters sync.Map // map[string]*atomic.Uint64

func (h *BaseAPIHandler) resolveCombo(modelName string, execOptions modelExecutionOptions) (*config.ComboConfig, bool) {
	if h == nil {
		return nil, false
	}
	cfg := h.CurrentConfig()
	if cfg == nil || len(cfg.Combos) == 0 {
		return nil, false
	}
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return nil, false
	}
	trimmedKey := strings.ToLower(trimmed)
	if execOptions.VisitedCombos != nil && execOptions.VisitedCombos[trimmedKey] {
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
		if strings.EqualFold(name, trimmed) {
			nameKey := strings.ToLower(name)
			if execOptions.VisitedCombos != nil && execOptions.VisitedCombos[nameKey] {
				return nil, false
			}
			copied := *combo
			copied.Models = append([]string(nil), combo.Models...)
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

func (h *BaseAPIHandler) executeComboWithAuthManagerFormats(
	ctx context.Context,
	combo *config.ComboConfig,
	entryProtocol, exitProtocol string,
	rawJSON []byte,
	alt string,
	allowImageModel bool,
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
		resp, headers, errMsg := h.executeWithAuthManagerFormats(ctx, entryProtocol, exitProtocol, candidate, candidateJSON, alt, allowImageModel, nextOpts)
		if errMsg == nil {
			return resp, headers, nil
		}
		lastErr = errMsg
		if !shouldFallbackComboError(errMsg) || idx == len(candidates)-1 {
			break
		}
		log.Debugf("combo %q: candidate %d/%d (%s) failed (status=%d, err=%v), falling back to next model", combo.Name, idx+1, len(candidates), candidate, errMsg.StatusCode, errMsg.Error)
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
	candidates := comboOrderedCandidates(combo)
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
		log.Debugf("combo %q (stream): candidate %d/%d (%s) failed to open stream (status=%d, err=%v), falling back to next model", combo.Name, idx+1, len(candidates), candidate, errMsg.StatusCode, errMsg.Error)
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
