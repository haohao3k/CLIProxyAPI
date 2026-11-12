package cursor

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Canonical thinking effort levels ordered from lowest to highest.
var canonicalEffortOrder = []string{
	"none",
	"minimal",
	"low",
	"medium",
	"high",
	"xhigh",
	"max",
}

var effortRank = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

// UsableModel describes one model entry returned by AgentService/GetUsableModels.
type UsableModel struct {
	ID          string
	DisplayName string
}

// SynthesizedFamily describes a multi-effort Cursor model family (e.g.
// "deepseek-v41-flash-1017") synthesized from per-effort variants.
type SynthesizedFamily struct {
	ID          string
	DisplayName string
	Levels      []string
	ZeroAllowed bool
}

// DecodeUsableModels decodes an agent.v1.GetUsableModelsResponse protobuf body.
func DecodeUsableModels(data []byte) ([]UsableModel, error) {
	fields, err := parseFields(data)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var out []UsableModel
	for _, f := range fields {
		if f.num != 1 || f.wire != 2 {
			continue
		}
		sub, errSub := parseFields(f.raw)
		if errSub != nil {
			continue
		}
		var id, displayModelID, displayName, displayShort string
		for _, sf := range sub {
			if sf.wire != 2 {
				continue
			}
			switch sf.num {
			case 1:
				if id == "" {
					id = strings.TrimSpace(string(sf.raw))
				}
			case 3:
				if displayModelID == "" {
					displayModelID = strings.TrimSpace(string(sf.raw))
				}
			case 4:
				if displayName == "" {
					displayName = strings.TrimSpace(string(sf.raw))
				}
			case 5:
				if displayShort == "" {
					displayShort = strings.TrimSpace(string(sf.raw))
				}
			}
		}
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		name := displayName
		if name == "" {
			name = displayShort
		}
		if name == "" {
			name = displayModelID
		}
		if name == "" {
			name = id
		}
		out = append(out, UsableModel{ID: id, DisplayName: name})
	}
	return out, nil
}

// ---------- Dynamic + Builtin Model Variant Catalog ----------

type cursorCatalogState struct {
	mu             sync.RWMutex
	usableExact    map[string]string            // lower(upstreamID) -> exact upstreamID
	familyVariants map[string]map[string]string // lower(family) -> canonicalEffort -> exact upstreamID
}

var globalCursorCatalog = &cursorCatalogState{}

var defaultSeedUsableIDs = []string{
	"default",
	"composer-2.5",
	"composer-2.5-fast",
	"gpt-5.3-codex-low", "gpt-5.3-codex", "gpt-5.3-codex-high", "gpt-5.3-codex-xhigh",
	"gpt-5.3-codex-low-fast", "gpt-5.3-codex-fast", "gpt-5.3-codex-high-fast", "gpt-5.3-codex-xhigh-fast",
	"gpt-5.6-sol-none", "gpt-5.6-sol-low", "gpt-5.6-sol-medium", "gpt-5.6-sol-high", "gpt-5.6-sol-xhigh", "gpt-5.6-sol-max",
	"gpt-5.6-sol-none-fast", "gpt-5.6-sol-low-fast", "gpt-5.6-sol-medium-fast", "gpt-5.6-sol-high-fast", "gpt-5.6-sol-xhigh-fast", "gpt-5.6-sol-max-fast",
	"gpt-5.6-terra-none", "gpt-5.6-terra-low", "gpt-5.6-terra-medium", "gpt-5.6-terra-high", "gpt-5.6-terra-xhigh", "gpt-5.6-terra-max",
	"gpt-5.6-luna-none", "gpt-5.6-luna-low", "gpt-5.6-luna-medium", "gpt-5.6-luna-high", "gpt-5.6-luna-xhigh", "gpt-5.6-luna-max",
	"gpt-5.5-none", "gpt-5.5-low", "gpt-5.5-medium", "gpt-5.5-high", "gpt-5.5-extra-high",
	"gpt-5.4-low", "gpt-5.4-medium", "gpt-5.4-high", "gpt-5.4-xhigh",
	"gpt-5.4-mini-none", "gpt-5.4-mini-low", "gpt-5.4-mini-medium", "gpt-5.4-mini-high", "gpt-5.4-mini-xhigh",
	"gpt-5.4-nano-none", "gpt-5.4-nano-low", "gpt-5.4-nano-medium", "gpt-5.4-nano-high", "gpt-5.4-nano-xhigh",
	"gpt-5.2-low", "gpt-5.2", "gpt-5.2-high", "gpt-5.2-xhigh",
	"gpt-5.1-low", "gpt-5.1", "gpt-5.1-high",
	"gpt-5-mini",
	"cursor-grok-4.5-low", "cursor-grok-4.5-medium", "cursor-grok-4.5-high",
	"cursor-grok-4.5-low-fast", "cursor-grok-4.5-medium-fast", "cursor-grok-4.5-high-fast",
	"cursor-grok-4.6-low", "cursor-grok-4.6-medium", "cursor-grok-4.6-high", "cursor-grok-4.6-xhigh",
	"cursor-grok-4.6-low-fast", "cursor-grok-4.6-medium-fast", "cursor-grok-4.6-high-fast", "cursor-grok-4.6-xhigh-fast",
	"grok-4.7-low", "grok-4.7-medium", "grok-4.7-high", "grok-4.7-xhigh",
	"grok-4.7-low-fast", "grok-4.7-medium-fast", "grok-4.7-high-fast", "grok-4.7-xhigh-fast",
	"claude-opus-5-5-low", "claude-opus-5-5-medium", "claude-opus-5-5-high", "claude-opus-5-5-xhigh", "claude-opus-5-5-max",
	"claude-opus-5-low", "claude-opus-5-medium", "claude-opus-5-high",
	"claude-opus-4-8-low", "claude-opus-4-8-medium", "claude-opus-4-8-high", "claude-opus-4-8-xhigh", "claude-opus-4-8-max",
	"claude-opus-4-7-low", "claude-opus-4-7-medium", "claude-opus-4-7-high", "claude-opus-4-7-xhigh", "claude-opus-4-7-max",
	"claude-sonnet-5-5-low", "claude-sonnet-5-5-medium", "claude-sonnet-5-5-high", "claude-sonnet-5-5-xhigh", "claude-sonnet-5-5-max",
	"claude-sonnet-5-low", "claude-sonnet-5-medium", "claude-sonnet-5-high", "claude-sonnet-5-xhigh", "claude-sonnet-5-max",
	"claude-haiku-5-5-low", "claude-haiku-5-5-medium", "claude-haiku-5-5-high", "claude-haiku-5-5-xhigh", "claude-haiku-5-5-max",
	"claude-fable-5-2-low", "claude-fable-5-2-medium", "claude-fable-5-2-high", "claude-fable-5-2-xhigh", "claude-fable-5-2-max",
	"claude-fable-5-1-low", "claude-fable-5-1-medium", "claude-fable-5-1-high", "claude-fable-5-1-xhigh", "claude-fable-5-1-max",
	"claude-fable-5-low", "claude-fable-5-medium", "claude-fable-5-high", "claude-fable-5-xhigh", "claude-fable-5-max",
	"claude-4.6-opus-high", "claude-4.6-opus-max", "claude-4.6-opus-high-thinking", "claude-4.6-opus-max-thinking",
	"claude-4.6-sonnet-medium", "claude-4.6-sonnet-medium-thinking",
	"claude-4.5-opus-high", "claude-4.5-opus-high-thinking",
	"claude-4.5-sonnet", "claude-4.5-sonnet-thinking",
	"claude-4-sonnet", "claude-4-sonnet-thinking",
	"gemini-4-carbon-low", "gemini-4-carbon-medium", "gemini-4-carbon-high",
	"gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-high",
	"gemini-3.8-flash-low", "gemini-3.8-flash-medium", "gemini-3.8-flash-high",
	"gemini-3.5-flash", "gemini-3.1-pro", "gemini-3-flash",
	"muse-wonder-1.0-minimal", "muse-wonder-1.0-low", "muse-wonder-1.0-medium", "muse-wonder-1.0-high", "muse-wonder-1.0-xhigh", "muse-wonder-1.0-max",
	"muse-spark-1.3-minimal", "muse-spark-1.3-low", "muse-spark-1.3-medium", "muse-spark-1.3-high", "muse-spark-1.3-xhigh", "muse-spark-1.3-max",
	"kimi-k3-low", "kimi-k3-high", "kimi-k3-max",
	"kimi-k2.7-code",
	"glm-5.2-high", "glm-5.2-max",
	"deepseek-v41-flash-1017-none", "deepseek-v41-flash-1017-low", "deepseek-v41-flash-1017-medium", "deepseek-v41-flash-1017-high", "deepseek-v41-flash-1017-max",
}

func init() {
	seed := make([]UsableModel, 0, len(defaultSeedUsableIDs))
	for _, id := range defaultSeedUsableIDs {
		seed = append(seed, UsableModel{ID: id, DisplayName: id})
	}
	RecordUsableModels(seed)
}

// ParseVariantSuffix splits a Cursor upstream model ID into (family, canonicalLevel, hasExplicitLevel).
// Examples:
//   - "deepseek-v41-flash-1017-high" -> ("deepseek-v41-flash-1017", "high", true)
//   - "gpt-5.6-sol-high-fast"        -> ("gpt-5.6-sol-fast", "high", true)
//   - "gpt-5.5-extra-high"           -> ("gpt-5.5", "xhigh", true)
//   - "cursor-grok-4.6-medium"       -> ("cursor-grok-4.6", "medium", true)
//   - "glm-5p3-flash"                -> ("glm-5p3-flash", "", false)
func ParseVariantSuffix(id string) (family string, level string, ok bool) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return "", "", false
	}
	fast := false
	work := trimmed
	if strings.HasSuffix(strings.ToLower(work), "-fast") && len(work) > len("-fast") {
		fast = true
		work = work[:len(work)-len("-fast")]
	}
	lower := strings.ToLower(work)
	suffixes := []struct {
		suf   string
		canon string
	}{
		{"-extra-high", "xhigh"},
		{"-minimal", "minimal"},
		{"-medium", "medium"},
		{"-xhigh", "xhigh"},
		{"-none", "none"},
		{"-high", "high"},
		{"-low", "low"},
		{"-max", "max"},
	}
	for _, item := range suffixes {
		if strings.HasSuffix(lower, item.suf) && len(work) > len(item.suf) {
			fam := work[:len(work)-len(item.suf)]
			if fast {
				fam += "-fast"
			}
			return fam, item.canon, true
		}
	}
	return trimmed, "", false
}

// RecordUsableModels merges newly fetched usable models into the in-memory variant catalog.
func RecordUsableModels(models []UsableModel) {
	if len(models) == 0 {
		return
	}
	globalCursorCatalog.mu.Lock()
	defer globalCursorCatalog.mu.Unlock()

	if globalCursorCatalog.usableExact == nil {
		globalCursorCatalog.usableExact = make(map[string]string, len(models))
	}
	if globalCursorCatalog.familyVariants == nil {
		globalCursorCatalog.familyVariants = make(map[string]map[string]string)
	}

	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		globalCursorCatalog.usableExact[strings.ToLower(id)] = id
		if fam, lvl, ok := ParseVariantSuffix(id); ok {
			famKey := strings.ToLower(fam)
			if globalCursorCatalog.familyVariants[famKey] == nil {
				globalCursorCatalog.familyVariants[famKey] = make(map[string]string)
			}
			globalCursorCatalog.familyVariants[famKey][lvl] = id
		}
	}
	// Bare models whose family also has explicit variants (e.g. "gpt-5.3-codex", "gpt-5.2", "gpt-5.1")
	// represent the "medium" tier when "<family>-medium" is absent.
	for famKey, variants := range globalCursorCatalog.familyVariants {
		if _, hasMedium := variants["medium"]; !hasMedium {
			if exactBare, ok := globalCursorCatalog.usableExact[famKey]; ok {
				variants["medium"] = exactBare
			}
		}
	}
}

// ClientModelID converts an upstream Cursor model ID into CLIProxyAPI's exposed model ID
// ("cursor-<id>", avoiding double "cursor-cursor-*" when upstream already begins with "cursor-").
func ClientModelID(upstreamID string) string {
	id := strings.TrimSpace(upstreamID)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(id), "cursor-") {
		return id
	}
	return "cursor-" + id
}

// FamilyThinkingLevels returns ordered canonical effort levels for a family if known.
func FamilyThinkingLevels(upstreamOrClientID string) ([]string, bool) {
	key := strings.ToLower(strings.TrimSpace(upstreamOrClientID))
	globalCursorCatalog.mu.RLock()
	defer globalCursorCatalog.mu.RUnlock()

	variants := globalCursorCatalog.familyVariants[key]
	if len(variants) == 0 && strings.HasPrefix(key, "cursor-") {
		variants = globalCursorCatalog.familyVariants[strings.TrimPrefix(key, "cursor-")]
	}
	if len(variants) < 2 {
		return nil, false
	}
	levels := make([]string, 0, len(variants))
	zeroAllowed := false
	for _, canon := range canonicalEffortOrder {
		if _, ok := variants[canon]; ok {
			levels = append(levels, canon)
			if canon == "none" {
				zeroAllowed = true
			}
		}
	}
	return levels, zeroAllowed
}

// SynthesizeFamilyModels returns base family entries for multi-effort Cursor model families
// whose bare family ID is not already present in usable.
func SynthesizeFamilyModels(usable []UsableModel) []SynthesizedFamily {
	usableSet := make(map[string]struct{}, len(usable))
	familyMap := make(map[string]map[string]string)
	familyOrder := make([]string, 0)
	familyDisplay := make(map[string]string)

	for _, m := range usable {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		usableSet[strings.ToLower(id)] = struct{}{}
		if fam, lvl, ok := ParseVariantSuffix(id); ok {
			famKey := strings.ToLower(fam)
			if familyMap[famKey] == nil {
				familyMap[famKey] = make(map[string]string)
				familyOrder = append(familyOrder, fam)
			}
			familyMap[famKey][lvl] = id
			if familyDisplay[famKey] == "" && m.DisplayName != "" {
				familyDisplay[famKey] = stripDisplayEffortSuffix(m.DisplayName)
			}
		}
	}

	out := make([]SynthesizedFamily, 0, len(familyOrder))
	for _, fam := range familyOrder {
		famKey := strings.ToLower(fam)
		if _, exists := usableSet[famKey]; exists {
			continue
		}
		variants := familyMap[famKey]
		if len(variants) < 2 {
			continue
		}
		levels := make([]string, 0, len(variants))
		zeroAllowed := false
		for _, canon := range canonicalEffortOrder {
			if _, ok := variants[canon]; ok {
				levels = append(levels, canon)
				if canon == "none" {
					zeroAllowed = true
				}
			}
		}
		disp := familyDisplay[famKey]
		if disp == "" {
			disp = fam
		}
		out = append(out, SynthesizedFamily{
			ID:          fam,
			DisplayName: disp,
			Levels:      levels,
			ZeroAllowed: zeroAllowed,
		})
	}
	return out
}

func stripDisplayEffortSuffix(name string) string {
	trimmed := strings.TrimSpace(name)
	for _, suf := range []string{" Extra High", " Minimal", " Medium", " XHigh", " High", " Low", " Max", " None"} {
		if strings.HasSuffix(trimmed, suf) && len(trimmed) > len(suf) {
			return strings.TrimSpace(trimmed[:len(trimmed)-len(suf)])
		}
	}
	return trimmed
}

func parseModelParentheticalSuffix(raw string) (modelName string, suffix string) {
	s := strings.TrimSpace(raw)
	if !strings.HasSuffix(s, ")") {
		return s, ""
	}
	openIdx := strings.LastIndexByte(s, '(')
	if openIdx <= 0 {
		return s, ""
	}
	return strings.TrimSpace(s[:openIdx]), strings.TrimSpace(s[openIdx+1 : len(s)-1])
}

func normalizeCursorEffort(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "":
		return ""
	case "none", "off", "disable", "disabled", "0":
		return "none"
	case "minimal":
		return "minimal"
	case "low":
		return "low"
	case "medium", "med", "auto", "-1":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "extra-high", "extra_high":
		return "xhigh"
	case "max":
		return "max"
	}
	if n, err := strconv.Atoi(s); err == nil {
		switch {
		case n <= 0:
			return "none"
		case n <= 2048:
			return "low"
		case n <= 8192:
			return "medium"
		case n <= 24576:
			return "high"
		default:
			return "max"
		}
	}
	return ""
}

func pickNearestVariant(variants map[string]string, targetEffort string) string {
	if len(variants) == 0 {
		return ""
	}
	if exact, ok := variants[targetEffort]; ok {
		return exact
	}
	targetRank, ok := effortRank[targetEffort]
	if !ok {
		targetRank = effortRank["medium"]
	}
	bestID := ""
	bestDist := 999
	bestRank := -1
	for _, canon := range canonicalEffortOrder {
		id, exists := variants[canon]
		if !exists {
			continue
		}
		r := effortRank[canon]
		dist := r - targetRank
		if dist < 0 {
			dist = -dist
		}
		// Tie-break toward the higher capability tier (e.g. medium(3) between low(2) and high(4) -> high).
		if dist < bestDist || (dist == bestDist && r > bestRank) {
			bestDist = dist
			bestRank = r
			bestID = id
		}
	}
	return bestID
}

// ResolveUpstreamModel maps any Cursor model identifier (CLIProxyAPI "cursor-*" ID,
// 9router "cursor/*" ID, static alias, explicit thinking-level ID, or base family ID)
// plus an optional payload reasoning effort onto the exact upstream Cursor model ID.
func ResolveUpstreamModel(rawModel, payloadEffort string) string {
	base, rawSuffix := parseModelParentheticalSuffix(rawModel)
	if slashIdx := strings.IndexByte(base, '/'); slashIdx >= 0 {
		prefix := strings.ToLower(strings.TrimSpace(base[:slashIdx]))
		if prefix == "cursor" || prefix == "cu" {
			base = strings.TrimSpace(base[slashIdx+1:])
		}
	}
	if base == "" || strings.EqualFold(base, "cursor-") {
		return base
	}

	explicitSuffixEffort := normalizeCursorEffort(rawSuffix)
	bodyEffort := normalizeCursorEffort(payloadEffort)

	globalCursorCatalog.mu.RLock()
	defer globalCursorCatalog.mu.RUnlock()

	candidate := base
	lowerBase := strings.ToLower(base)
	// Preserve literal "cursor-*" upstream IDs (e.g. "cursor-grok-4.6-high" or family "cursor-grok-4.6").
	if strings.HasPrefix(lowerBase, "cursor-") && len(base) > len("cursor-") {
		_, exactWithPrefix := globalCursorCatalog.usableExact[lowerBase]
		_, familyWithPrefix := globalCursorCatalog.familyVariants[lowerBase]
		keepPrefix := exactWithPrefix || familyWithPrefix
		if !keepPrefix {
			if fam, _, hasVar := ParseVariantSuffix(base); hasVar {
				if _, ok := globalCursorCatalog.familyVariants[strings.ToLower(fam)]; ok {
					keepPrefix = true
				}
			}
		}
		if !keepPrefix {
			candidate = base[len("cursor-"):]
		}
	}

	// Normalize static/legacy aliases.
	switch strings.ToLower(candidate) {
	case "auto", "auto-smart", "default":
		return "default"
	case "claude-sonnet-4-5":
		candidate = "claude-4.5-sonnet"
	case "claude-opus-4-5":
		candidate = "claude-4.5-opus-high"
	case "claude-haiku-4-5":
		candidate = "claude-4.5-haiku"
	case "claude-sonnet-4-6":
		candidate = "claude-4.6-sonnet-medium"
	case "claude-opus-4-6":
		if explicitSuffixEffort == "max" || explicitSuffixEffort == "xhigh" || (explicitSuffixEffort == "" && (bodyEffort == "max" || bodyEffort == "xhigh")) {
			return "claude-4.6-opus-max"
		}
		return "claude-4.6-opus-high"
	}

	// If the model name ALREADY carries an explicit variant suffix (e.g. "deepseek-v41-flash-1017-high"
	// or "cursor-grok-4.6-xhigh"), keep it as-is unless the caller explicitly added a "(level)" override.
	if fam, _, hasExplicitVariant := ParseVariantSuffix(candidate); hasExplicitVariant {
		if explicitSuffixEffort != "" {
			if variants := globalCursorCatalog.familyVariants[strings.ToLower(fam)]; len(variants) > 0 {
				if resolved := pickNearestVariant(variants, explicitSuffixEffort); resolved != "" {
					return resolved
				}
			}
		}
		if exact, ok := globalCursorCatalog.usableExact[strings.ToLower(candidate)]; ok {
			return exact
		}
		return candidate
	}
	if strings.HasSuffix(strings.ToLower(candidate), "-thinking") {
		if exact, ok := globalCursorCatalog.usableExact[strings.ToLower(candidate)]; ok {
			return exact
		}
		return candidate
	}

	wantEffort := explicitSuffixEffort
	if wantEffort == "" {
		wantEffort = bodyEffort
	}

	lowerCand := strings.ToLower(candidate)
	if variants := globalCursorCatalog.familyVariants[lowerCand]; len(variants) > 0 {
		if wantEffort == "" {
			if exactBare, ok := globalCursorCatalog.usableExact[lowerCand]; ok {
				return exactBare
			}
			wantEffort = "medium"
		}
		if resolved := pickNearestVariant(variants, wantEffort); resolved != "" {
			return resolved
		}
	}

	// Handle "-thinking" toggle pairs such as "claude-4.5-sonnet" -> "claude-4.5-sonnet-thinking"
	// when a positive thinking effort was explicitly requested via payload or parenthetical suffix.
	if wantEffort != "" && wantEffort != "none" {
		if exactThinking, ok := globalCursorCatalog.usableExact[lowerCand+"-thinking"]; ok && lowerCand == "claude-4.5-sonnet" {
			return exactThinking
		}
	}

	if exact, ok := globalCursorCatalog.usableExact[lowerCand]; ok {
		return exact
	}
	return candidate
}

// ---------- AgentService/Run Protobuf & Connect-RPC Wire Protocol ----------

// AgentToolCall represents one assistant tool call in conversation history or response.
type AgentToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// AgentMessage represents one message in an AgentService conversation.
type AgentMessage struct {
	Role       string
	Content    string
	ToolCalls  []AgentToolCall
	ToolCallID string
}

// AgentTool represents one function/MCP tool declaration exposed to AgentService.
type AgentTool struct {
	Name        string
	Description string
	Parameters  any
}

// AgentEventType enumerates events emitted by RunAgentStream.
type AgentEventType string

const (
	AgentEventText     AgentEventType = "text"
	AgentEventThinking AgentEventType = "thinking"
	AgentEventToolCall AgentEventType = "tool_call"
)

// AgentStreamEvent is a single decoded event from AgentService/Run.
type AgentStreamEvent struct {
	Type     AgentEventType
	Text     string
	ToolCall *AgentToolCall
}

func appendVarintFieldAlways(dst []byte, field int, v uint64) []byte {
	dst = appendTag(dst, field, 0)
	return appendVarint(dst, v)
}

func appendFixed64Float(dst []byte, field int, v float64) []byte {
	dst = appendTag(dst, field, 1)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
	return append(dst, buf[:]...)
}

func wrapConnectFrame(flag byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// encodeAgentValue encodes an arbitrary JSON-like Go value as google.protobuf.Value.
func encodeAgentValue(v any) []byte {
	switch val := v.(type) {
	case nil:
		return appendVarintFieldAlways(nil, 1, 0)
	case bool:
		var b uint64
		if val {
			b = 1
		}
		return appendVarintFieldAlways(nil, 4, b)
	case float64:
		return appendFixed64Float(nil, 2, val)
	case float32:
		return appendFixed64Float(nil, 2, float64(val))
	case int:
		return appendFixed64Float(nil, 2, float64(val))
	case int64:
		return appendFixed64Float(nil, 2, float64(val))
	case json.Number:
		f, _ := val.Float64()
		return appendFixed64Float(nil, 2, f)
	case string:
		return appendBytesField(nil, 3, []byte(val))
	case []any:
		var list []byte
		for _, item := range val {
			list = appendBytesField(list, 1, encodeAgentValue(item))
		}
		return appendBytesField(nil, 6, list)
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var st []byte
		for _, k := range keys {
			var entry []byte
			entry = appendBytesField(entry, 1, []byte(k))
			entry = appendBytesField(entry, 2, encodeAgentValue(val[k]))
			st = appendBytesField(st, 1, entry)
		}
		return appendBytesField(nil, 5, st)
	default:
		return appendBytesField(nil, 3, []byte(fmt.Sprint(val)))
	}
}

func decodeAgentValue(data []byte) any {
	fields, err := parseFields(data)
	if err != nil {
		return nil
	}
	for _, f := range fields {
		switch {
		case f.num == 1 && f.wire == 0:
			return nil
		case f.num == 4 && f.wire == 0:
			return f.uvar != 0
		case f.num == 2 && f.wire == 1:
			// parseFields skips fixed64 raw bytes unless captured; handle via raw scan below if needed.
		case f.num == 3 && f.wire == 2:
			return string(f.raw)
		case f.num == 5 && f.wire == 2:
			res := make(map[string]any)
			sub, errSub := parseFields(f.raw)
			if errSub != nil {
				return res
			}
			for _, entry := range sub {
				if entry.num != 1 || entry.wire != 2 {
					continue
				}
				pair, errPair := parseFields(entry.raw)
				if errPair != nil {
					continue
				}
				var key string
				var valRaw []byte
				hasVal := false
				for _, pf := range pair {
					if pf.num == 1 && pf.wire == 2 {
						key = string(pf.raw)
					} else if pf.num == 2 && pf.wire == 2 {
						valRaw = pf.raw
						hasVal = true
					}
				}
				if key != "" {
					if hasVal {
						res[key] = decodeAgentValue(valRaw)
					} else {
						res[key] = nil
					}
				}
			}
			return res
		case f.num == 6 && f.wire == 2:
			var list []any
			sub, errSub := parseFields(f.raw)
			if errSub != nil {
				return list
			}
			for _, item := range sub {
				if item.num == 1 && item.wire == 2 {
					list = append(list, decodeAgentValue(item.raw))
				}
			}
			return list
		}
	}
	// Check fixed64 number_value (tag = (2<<3)|1 = 0x11) directly from data.
	if v, ok := scanFixed64Field(data, 2); ok {
		return math.Float64frombits(v)
	}
	return nil
}

func scanFixed64Field(data []byte, wantField int) (uint64, bool) {
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 {
			return 0, false
		}
		data = data[n:]
		num := int(tag >> 3)
		wire := int(tag & 7)
		switch wire {
		case 0:
			_, m := binary.Uvarint(data)
			if m <= 0 {
				return 0, false
			}
			data = data[m:]
		case 1:
			if len(data) < 8 {
				return 0, false
			}
			if num == wantField {
				return binary.LittleEndian.Uint64(data[:8]), true
			}
			data = data[8:]
		case 2:
			l, m := binary.Uvarint(data)
			if m <= 0 || uint64(len(data)-m) < l {
				return 0, false
			}
			data = data[m+int(l):]
		case 5:
			if len(data) < 4 {
				return 0, false
			}
			data = data[4:]
		default:
			return 0, false
		}
	}
	return 0, false
}

func encodeMcpToolDefinition(tool AgentTool) []byte {
	schema := tool.Parameters
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	var b []byte
	b = appendBytesField(b, 1, []byte(tool.Name))
	b = appendBytesField(b, 2, []byte(tool.Description))
	b = appendBytesField(b, 3, encodeAgentValue(schema))
	b = appendBytesField(b, 4, []byte("mcp"))
	b = appendBytesField(b, 5, []byte(tool.Name))
	return b
}

func encodeMcpTools(tools []AgentTool) []byte {
	if len(tools) == 0 {
		return nil
	}
	var b []byte
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		b = appendBytesField(b, 1, encodeMcpToolDefinition(t))
	}
	return b
}

func decodeMcpArgs(data []byte) *AgentToolCall {
	fields, err := parseFields(data)
	if err != nil {
		return nil
	}
	var name, toolCallID, toolName string
	args := make(map[string]any)
	for _, f := range fields {
		if f.wire != 2 {
			continue
		}
		switch f.num {
		case 1:
			name = string(f.raw)
		case 2:
			pair, errPair := parseFields(f.raw)
			if errPair != nil {
				continue
			}
			var k string
			var vRaw []byte
			hasVal := false
			for _, pf := range pair {
				if pf.num == 1 && pf.wire == 2 {
					k = string(pf.raw)
				} else if pf.num == 2 && pf.wire == 2 {
					vRaw = pf.raw
					hasVal = true
				}
			}
			if k != "" {
				if hasVal {
					args[k] = decodeAgentValue(vRaw)
				} else {
					args[k] = nil
				}
			}
		case 3:
			toolCallID = string(f.raw)
		case 5:
			toolName = string(f.raw)
		}
	}
	resolvedName := strings.TrimSpace(toolName)
	if resolvedName == "" {
		resolvedName = strings.TrimSpace(name)
	}
	if resolvedName == "" {
		return nil
	}
	if strings.TrimSpace(toolCallID) == "" {
		toolCallID = "call_" + uuid.NewString()
	}
	argsJSON, errMarshal := json.Marshal(args)
	if errMarshal != nil {
		argsJSON = []byte("{}")
	}
	return &AgentToolCall{
		ID:        toolCallID,
		Name:      resolvedName,
		Arguments: string(argsJSON),
	}
}

func formatAgentMessageText(m AgentMessage) string {
	var parts []string
	if m.Role == "tool" {
		header := fmt.Sprintf("[tool_result id=%s]", m.ToolCallID)
		parts = append(parts, header)
	}
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, m.Content)
	}
	if m.Role == "assistant" && len(m.ToolCalls) > 0 {
		for _, tc := range m.ToolCalls {
			args := tc.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			parts = append(parts, fmt.Sprintf("[tool_call id=%s name=%s args=%s]", tc.ID, tc.Name, args))
		}
	}
	return strings.Join(parts, "\n")
}

func formatTranscriptEntry(m AgentMessage, toolNameByCallID map[string]string) string {
	body := formatAgentMessageText(m)
	if strings.TrimSpace(body) == "" {
		return ""
	}
	switch m.Role {
	case "assistant":
		return "[Assistant]:\n" + body
	case "tool":
		if name := strings.TrimSpace(toolNameByCallID[m.ToolCallID]); name != "" {
			return fmt.Sprintf("[MCP Tool Result (%s)]:\n%s", name, body)
		}
		return "[MCP Tool Result]:\n" + body
	default:
		return "[User]:\n" + body
	}
}

// BuildAgentRunFrame encodes the initial Connect-RPC frame for AgentService/Run.
// System/developer prompts and multi-turn history are folded into user_message
// because RunRequest with an empty ConversationStateStructure starts a fresh
// stateless agent session (ignoring UserMessageAction.field_7 on Cursor 3.12+)
// and RunRequest.custom_system_prompt causes AgentService to return an empty turn.
func BuildAgentRunFrame(messages []AgentMessage, upstreamModel string, tools []AgentTool) []byte {
	var sysParts []string
	var chatMsgs []AgentMessage
	toolNameByCallID := make(map[string]string)
	for _, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			if strings.TrimSpace(m.Content) != "" {
				sysParts = append(sysParts, m.Content)
			}
			continue
		}
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if strings.TrimSpace(tc.ID) != "" && strings.TrimSpace(tc.Name) != "" {
					toolNameByCallID[tc.ID] = tc.Name
				}
			}
		}
		chatMsgs = append(chatMsgs, m)
	}
	systemText := strings.Join(sysParts, "\n\n")

	var historyMsgs []AgentMessage
	var rawUser string

	if len(chatMsgs) == 0 {
		rawUser = "Continue."
	} else if len(chatMsgs) == 1 && chatMsgs[0].Role == "user" {
		rawUser = strings.TrimSpace(chatMsgs[0].Content)
		if rawUser == "" {
			rawUser = "Continue."
		}
	} else if chatMsgs[len(chatMsgs)-1].Role == "user" {
		historyMsgs = chatMsgs[:len(chatMsgs)-1]
		var histEntries []string
		for _, hm := range historyMsgs {
			if entry := formatTranscriptEntry(hm, toolNameByCallID); entry != "" {
				histEntries = append(histEntries, entry)
			}
		}
		cur := strings.TrimSpace(chatMsgs[len(chatMsgs)-1].Content)
		if cur == "" {
			cur = "Continue."
		}
		if len(histEntries) > 0 {
			rawUser = "<conversation_history>\n" + strings.Join(histEntries, "\n\n") + "\n</conversation_history>\n\n" + cur
		} else {
			rawUser = cur
		}
	} else {
		// Trailing tool result(s) or assistant prefill: include the entire chronological
		// transcript inside <conversation_history> so stateless AgentService/Run sees the
		// original user prompt, the assistant's own MCP tool call(s), and the tool result(s).
		historyMsgs = chatMsgs
		var histEntries []string
		for _, hm := range chatMsgs {
			if entry := formatTranscriptEntry(hm, toolNameByCallID); entry != "" {
				histEntries = append(histEntries, entry)
			}
		}
		if len(histEntries) > 0 {
			rawUser = "<conversation_history>\n" + strings.Join(histEntries, "\n\n") +
				"\n</conversation_history>\n\nAll requested MCP tool calls shown in <conversation_history> above have completed and their results are provided above. Continue responding to the user based on those results without re-running completed tool calls."
		} else {
			rawUser = "Continue."
		}
	}

	userText := rawUser
	if systemText != "" {
		userText = systemText + "\n\n" + rawUser
	}

	var userMsg []byte
	userMsg = appendBytesField(userMsg, 1, []byte(userText))
	userMsg = appendBytesField(userMsg, 2, []byte(uuid.NewString()))
	userMsg = appendBytesField(userMsg, 3, nil) // selected_context (empty)
	userMsg = appendVarintFieldAlways(userMsg, 4, 1)

	var userAction []byte
	userAction = appendBytesField(userAction, 1, userMsg)
	conversationAction := appendBytesField(nil, 1, userAction)

	var requestedModel []byte
	requestedModel = appendBytesField(requestedModel, 1, []byte(upstreamModel))
	requestedModel = appendVarintFieldAlways(requestedModel, 7, 1)

	var modelDetails []byte
	modelDetails = appendBytesField(modelDetails, 1, []byte(upstreamModel))
	modelDetails = appendBytesField(modelDetails, 3, []byte(upstreamModel))
	modelDetails = appendBytesField(modelDetails, 4, []byte(upstreamModel))

	mcpTools := encodeMcpTools(tools)

	var runRequest []byte
	runRequest = appendBytesField(runRequest, 1, nil) // empty ConversationStateStructure
	runRequest = appendBytesField(runRequest, 2, conversationAction)
	runRequest = appendBytesField(runRequest, 3, modelDetails)
	if len(mcpTools) > 0 {
		runRequest = appendBytesField(runRequest, 4, mcpTools)
	}
	runRequest = appendBytesField(runRequest, 9, requestedModel)

	return wrapConnectFrame(0, appendBytesField(nil, 1, runRequest))
}

func wrapExecClientMessage(execMsgID uint64, execID string, resultField int, resultPayload []byte) []byte {
	var b []byte
	if execMsgID != 0 {
		b = appendVarintFieldAlways(b, 1, execMsgID)
	}
	b = appendBytesField(b, 15, []byte(execID))
	b = appendBytesField(b, resultField, resultPayload)
	return wrapConnectFrame(0, appendBytesField(nil, 2, b))
}

func createRequestContextResponse(execFields []protoField) []byte {
	id, execID := extractExecIDs(execFields)
	requestContextSuccess := appendBytesField(nil, 1, nil)
	requestContextResult := appendBytesField(nil, 1, requestContextSuccess)
	return wrapExecClientMessage(id, execID, 10, requestContextResult)
}

func rejectExecRequest(execFields []protoField) []byte {
	id, execID := extractExecIDs(execFields)
	variant := 0
	for _, f := range execFields {
		// Skip ExecServerMessage metadata fields:
		// 1 = id (varint), 15 = exec_id (string), 19 = span_context (bytes), 55 = flags (varint).
		// All ExecServerMessage command variants are length-delimited submessages (wire == 2)
		// whose ExecClientMessage result uses the identical field number.
		if f.num == 1 || f.num == 15 || f.num == 19 || f.num == 55 || f.wire != 2 {
			continue
		}
		variant = f.num
		break
	}
	if variant < 2 {
		return nil
	}
	if variant == 9 {
		return wrapExecClientMessage(id, execID, 9, nil)
	}
	rejected := appendBytesField(nil, 2, appendBytesField(nil, 2, []byte("Tool not available in this environment. Use the MCP tools provided instead.")))
	return wrapExecClientMessage(id, execID, variant, rejected)
}

func extractExecIDs(execFields []protoField) (uint64, string) {
	var id uint64
	var execID string
	for _, f := range execFields {
		if f.num == 1 && f.wire == 0 {
			id = f.uvar
		} else if f.num == 15 && f.wire == 2 {
			execID = string(f.raw)
		}
	}
	return id, execID
}

func encodeKvClientMessage(kvID uint64, resultField int, resultPayload, metadata []byte) []byte {
	var b []byte
	if kvID != 0 {
		b = appendVarintFieldAlways(b, 1, kvID)
	}
	b = appendBytesField(b, resultField, resultPayload)
	if len(metadata) > 0 {
		b = appendBytesField(b, 4, metadata)
	}
	return wrapConnectFrame(0, appendBytesField(nil, 3, b))
}

// IsComposerModel reports whether the upstream Cursor model is a Composer variant.
func IsComposerModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.LastIndexByte(m, '/'); idx >= 0 {
		m = m[idx+1:]
	}
	return m == "composer" || strings.HasPrefix(m, "composer-")
}

func visibleComposerContentFromThinking(thinking string) string {
	const endTag = "</think>"
	idx := strings.LastIndex(thinking, endTag)
	if idx < 0 {
		return ""
	}
	return strings.TrimLeft(thinking[idx+len(endTag):], " \t\r\n")
}

func decompressAgentFrame(flag byte, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	trimmed := bytes.TrimSpace(data)
	// Uncompressed JSON trailer (e.g. `{}` or `{"error":...}`).
	if flag&flagCompressed == 0 && len(trimmed) > 0 && trimmed[0] == '{' {
		return data, nil
	}
	if len(trimmed) > 10 && bytes.HasPrefix(trimmed, []byte(`{"error"`)) {
		return data, nil
	}
	if flag&(flagCompressed|flagEndStream) == 0 {
		return data, nil
	}
	if zr, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
		out, errRead := io.ReadAll(zr)
		_ = zr.Close()
		if errRead == nil {
			return out, nil
		}
	}
	if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		out, errRead := io.ReadAll(zr)
		_ = zr.Close()
		if errRead == nil {
			return out, nil
		}
	}
	fr := flate.NewReader(bytes.NewReader(data))
	out, errFlate := io.ReadAll(fr)
	_ = fr.Close()
	if errFlate == nil {
		return out, nil
	}
	return data, nil
}

func firstBytesField(fields []protoField, num int) ([]byte, bool) {
	for _, f := range fields {
		if f.num == num && f.wire == 2 {
			return f.raw, true
		}
	}
	return nil, false
}

func hasField(fields []protoField, num int) bool {
	for _, f := range fields {
		if f.num == num {
			return true
		}
	}
	return false
}

// AgentDuplexSession holds an open bidirectional HTTP/2 stream to AgentService/Run.
type AgentDuplexSession struct {
	Response *http.Response
	writeCh  chan []byte
	doneCh   chan struct{}
	closeOne sync.Once
	pw       *io.PipeWriter
}

// OpenAgentDuplexSession starts the HTTP/2 POST to AgentService/Run, queues initialFrame
// immediately on the request stream, and returns once response headers arrive.
func OpenAgentDuplexSession(ctx context.Context, httpClient *http.Client, buildReq func(body io.Reader) (*http.Request, error), initialFrame []byte) (*AgentDuplexSession, error) {
	pr, pw := io.Pipe()
	req, err := buildReq(pr)
	if err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}
	req.ContentLength = -1

	sess := &AgentDuplexSession{
		writeCh: make(chan []byte, 32),
		doneCh:  make(chan struct{}),
		pw:      pw,
	}
	sess.writeCh <- initialFrame

	go func() {
		defer func() { _ = pw.Close() }()
		for {
			select {
			case frame, ok := <-sess.writeCh:
				if !ok {
					return
				}
				if len(frame) == 0 {
					continue
				}
				if _, errWrite := pw.Write(frame); errWrite != nil {
					return
				}
			case <-sess.doneCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		sess.Close()
		return nil, errDo
	}
	sess.Response = resp
	return sess, nil
}

// Send queues a client Connect-RPC frame on the open HTTP/2 request body stream.
func (s *AgentDuplexSession) Send(frame []byte) {
	if s == nil || len(frame) == 0 {
		return
	}
	select {
	case s.writeCh <- frame:
	case <-s.doneCh:
	}
}

// Close terminates both directions of the duplex AgentService stream.
func (s *AgentDuplexSession) Close() {
	if s == nil {
		return
	}
	s.closeOne.Do(func() {
		close(s.doneCh)
		if s.pw != nil {
			_ = s.pw.Close()
		}
		if s.Response != nil && s.Response.Body != nil {
			_ = s.Response.Body.Close()
		}
	})
}

// Consume reads Connect-RPC frames from AgentService/Run, automatically replies to
// request_context / KV / IDE builtin probes on the duplex request stream, and invokes
// onEvent for text, thinking, and MCP tool_call deltas.
func (s *AgentDuplexSession) Consume(upstreamModel string, onEvent func(AgentStreamEvent) bool) error {
	defer s.Close()
	if s == nil || s.Response == nil || s.Response.Body == nil {
		return fmt.Errorf("cursor agent: nil session response")
	}

	composer := IsComposerModel(upstreamModel)
	var thinkingAcc strings.Builder
	emittedVisible := 0
	emittedText := false
	emittedToolCall := false
	finished := false
	cancelled := false

	emit := func(ev AgentStreamEvent) {
		if cancelled {
			return
		}
		if !onEvent(ev) {
			cancelled = true
			finished = true
		}
	}

	flushThinkingFallback := func() {
		if emittedText || emittedToolCall || thinkingAcc.Len() == 0 {
			return
		}
		full := thinkingAcc.String()
		if visible := visibleComposerContentFromThinking(full); visible != "" {
			if len(visible) > emittedVisible {
				emittedText = true
				emit(AgentStreamEvent{Type: AgentEventText, Text: visible[emittedVisible:]})
				emittedVisible = len(visible)
			}
			return
		}
		if composer {
			return
		}
	}

	reader := bufio.NewReader(s.Response.Body)
	for !finished {
		var header [5]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if cancelled {
				return nil
			}
			return fmt.Errorf("cursor agent: read frame header: %w", err)
		}
		flag := header[0]
		dataLen := int(binary.BigEndian.Uint32(header[1:5]))
		if dataLen > maxFrameBytes {
			return fmt.Errorf("cursor agent: frame of %d bytes exceeds limit", dataLen)
		}
		var raw []byte
		if dataLen > 0 {
			raw = make([]byte, dataLen)
			if _, err := io.ReadFull(reader, raw); err != nil {
				if cancelled {
					return nil
				}
				return fmt.Errorf("cursor agent: read frame body: %w", err)
			}
		}
		payload, errDecomp := decompressAgentFrame(flag, raw)
		if errDecomp != nil {
			return fmt.Errorf("cursor agent: decompress frame: %w", errDecomp)
		}
		if flag&flagEndStream != 0 {
			if errEnd := parseEndStream(payload); errEnd != nil {
				return errEnd
			}
			break
		}
		if len(payload) == 0 {
			continue
		}

		serverFields, errParse := parseFields(payload)
		if errParse != nil {
			continue
		}

		// Field 1: interaction_update
		if updateRaw, ok := firstBytesField(serverFields, 1); ok {
			updateFields, errUpd := parseFields(updateRaw)
			if errUpd == nil {
				if textMsgRaw, hasText := firstBytesField(updateFields, 1); hasText {
					if textFields, errT := parseFields(textMsgRaw); errT == nil {
						if deltaBytes, okD := firstBytesField(textFields, 1); okD && len(deltaBytes) > 0 {
							emittedText = true
							emit(AgentStreamEvent{Type: AgentEventText, Text: string(deltaBytes)})
						}
					}
				}
				if thinkMsgRaw, hasThink := firstBytesField(updateFields, 4); hasThink {
					if thinkFields, errTh := parseFields(thinkMsgRaw); errTh == nil {
						if deltaBytes, okD := firstBytesField(thinkFields, 1); okD && len(deltaBytes) > 0 {
							delta := string(deltaBytes)
							thinkingAcc.WriteString(delta)
							full := thinkingAcc.String()
							if composer || strings.Contains(full, "</think>") {
								visible := visibleComposerContentFromThinking(full)
								if len(visible) > emittedVisible {
									chunk := visible[emittedVisible:]
									emittedVisible = len(visible)
									emittedText = true
									emit(AgentStreamEvent{Type: AgentEventText, Text: chunk})
								}
							} else {
								emit(AgentStreamEvent{Type: AgentEventThinking, Text: delta})
							}
						}
					}
				}
				if hasField(updateFields, 14) {
					flushThinkingFallback()
					finished = true
				}
			}
		}

		if finished {
			break
		}

		// Field 4: KvServerMessage -> send KV ack so stream continues.
		if kvRaw, ok := firstBytesField(serverFields, 4); ok {
			if kvFields, errKv := parseFields(kvRaw); errKv == nil {
				var kvID uint64
				var meta []byte
				for _, kf := range kvFields {
					if kf.num == 1 && kf.wire == 0 {
						kvID = kf.uvar
					} else if kf.num == 4 && kf.wire == 2 {
						meta = kf.raw
					}
				}
				if hasField(kvFields, 2) {
					s.Send(encodeKvClientMessage(kvID, 2, appendBytesField(nil, 1, nil), meta))
				} else if hasField(kvFields, 3) {
					s.Send(encodeKvClientMessage(kvID, 3, nil, meta))
				}
			}
		}

		// Field 2: ExecServerMessage -> request_context (10), MCP tool call (11), or IDE builtin probe.
		if execRaw, ok := firstBytesField(serverFields, 2); ok {
			if execFields, errEx := parseFields(execRaw); errEx == nil {
				if hasField(execFields, 10) {
					s.Send(createRequestContextResponse(execFields))
				} else if mcpRaw, hasMcp := firstBytesField(execFields, 11); hasMcp {
					tc := decodeMcpArgs(mcpRaw)
					if tc != nil {
						emittedToolCall = true
						emit(AgentStreamEvent{Type: AgentEventToolCall, ToolCall: tc})
						finished = true
					} else {
						return &StreamError{Code: "internal", Detail: "Cursor AgentService requested an invalid MCP tool call"}
					}
				} else if rejection := rejectExecRequest(execFields); len(rejection) > 0 {
					s.Send(rejection)
				} else {
					return &StreamError{Code: "invalid_argument", Detail: "Cursor AgentService requested an unsupported IDE tool"}
				}
			}
		}
	}

	if !cancelled {
		flushThinkingFallback()
	}
	return nil
}
