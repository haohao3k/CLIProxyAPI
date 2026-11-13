package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelexposure"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"golang.org/x/net/context"
)

// modelCandidate is one provider that can serve a requested model name.
type modelCandidate struct {
	registryProvider string
	key              string
	nativeID         string
}

func (c modelCandidate) exposedID(exposure *config.ModelExposureConfig) string {
	return modelexposure.ExposedID(exposure, c.key, c.nativeID)
}

func candidatesFor(providers []string, nativeID string) []modelCandidate {
	out := make([]modelCandidate, 0, len(providers))
	for _, p := range providers {
		out = append(out, modelCandidate{registryProvider: p, key: modelexposure.ProviderKey(p), nativeID: nativeID})
	}
	return out
}

// prefixedCandidates resolves "<prefix>/<id>" through the prefixes of model-exposure. A provider
// without an entry answers to its own key; one with an entry answers only to its prefix.
func prefixedCandidates(exposure *config.ModelExposureConfig, baseModel string) []modelCandidate {
	prefix, rest, ok := strings.Cut(strings.TrimSpace(baseModel), "/")
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	rest = strings.TrimSpace(rest)
	if !ok || prefix == "" || rest == "" {
		return nil
	}
	keys := make([]string, 0, 2)
	if exposure != nil {
		for key, p := range exposure.Providers {
			if strings.EqualFold(p.Prefix, prefix) {
				keys = append(keys, key)
			}
		}
	}
	if exposure == nil || exposure.Providers == nil {
		keys = append(keys, prefix)
	} else if _, configured := exposure.Providers[prefix]; !configured {
		keys = append(keys, prefix)
	}
	var out []modelCandidate
	for _, key := range keys {
		for _, nativeID := range modelexposure.NativeCandidates(key, rest) {
			nativeID = registeredModelID(nativeID)
			for _, p := range util.GetProviderName(nativeID) {
				if modelexposure.ProviderKey(p) == key {
					out = append(out, modelCandidate{registryProvider: p, key: key, nativeID: nativeID})
				}
			}
			if len(out) > 0 {
				break
			}
		}
	}
	return out
}

// registeredModelID returns the id the registry knows a model by; lookups also accept a
// differently cased name, and the executor must receive the registered spelling.
func registeredModelID(id string) string {
	if len(registry.GetGlobalRegistry().GetModelProviders(id)) == 0 {
		if lower := strings.ToLower(id); len(registry.GetGlobalRegistry().GetModelProviders(lower)) > 0 {
			return lower
		}
	}
	return id
}

// resolveModelCandidates lists the providers a requested base model name (no suffix) may run on,
// and whether the name was provider-qualified.
func resolveModelCandidates(cfg *config.SDKConfig, baseModel string) ([]modelCandidate, bool) {
	var exposure *config.ModelExposureConfig
	if cfg != nil {
		exposure = &cfg.ModelExposure
	}
	if providers := util.GetProviderName(baseModel); len(providers) > 0 {
		return candidatesFor(providers, baseModel), false
	}
	if !strings.Contains(baseModel, "/") {
		if strings.HasPrefix(strings.ToLower(baseModel), "cursor-") && hasRegisteredCursorProvider() {
			return candidatesFor([]string{"cursor"}, baseModel), false
		}
		return nil, false
	}
	if out := prefixedCandidates(exposure, baseModel); len(out) > 0 {
		return out, true
	}
	if providers, resolved := resolveProviderQualifiedModel(baseModel, baseModel); len(providers) > 0 {
		return candidatesFor(providers, strings.TrimSpace(thinking.ParseSuffix(resolved).ModelName)), true
	}
	return nil, true
}

func modelNotFoundError(modelName string) *interfaces.ErrorMessage {
	return modelAccessError(http.StatusBadRequest, "model_not_found", "unknown provider for model "+modelName)
}

func modelNotAllowedError(modelName string) *interfaces.ErrorMessage {
	return modelAccessError(http.StatusForbidden, "model_not_allowed", "model "+modelName+" is not allowed for this API key")
}

func modelAccessError(status int, code, message string) *interfaces.ErrorMessage {
	// The message carries the client-supplied model name, so it is set through ojson.
	body := `{"error":{"message":"","type":"invalid_request_error","code":"","param":"model"}}`
	body, _ = ojson.Set(body, "error.code", code)
	if withMessage, errSet := ojson.Set(body, "error.message", message); errSet == nil {
		body = withMessage
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: errors.New(body)}
}

func withSuffix(model, rawSuffix string, hasSuffix bool) string {
	if hasSuffix {
		return model + "(" + rawSuffix + ")"
	}
	return model
}

// gateModelAccess applies model exposure, Cursor aliases and the caller key's policy to a client
// request, once, before combo expansion. Combo members and the models a combo, alias or
// compaction hijack picks are not checked again: an allowed name may run hidden models.
// It returns the model and body to execute (changed only for an alias) and narrows
// execOptions.AllowedProviders to the providers the caller may reach under that name.
func (h *BaseAPIHandler) gateModelAccess(ctx context.Context, entryProtocol, modelName string, rawJSON []byte, execOptions *modelExecutionOptions) (string, []byte, *interfaces.ErrorMessage) {
	if h == nil || execOptions.ModelAccessChecked || execOptions.InternalSource || len(execOptions.VisitedCombos) > 0 {
		return modelName, rawJSON, nil
	}
	execOptions.ModelAccessChecked = true
	cfg := h.CurrentConfig()
	if cfg == nil {
		return modelName, rawJSON, nil
	}
	if h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		if refusal := h.HomeModelFiltersError(ctx); refusal != nil {
			return "", nil, modelAccessError(refusal.Status, refusal.Code, refusal.Message)
		}
		return modelName, rawJSON, nil
	}
	exposure := &cfg.ModelExposure
	policy := modelexposure.PolicyFor(cfg.APIKeyPolicies, coreusage.APIKeyFromContext(ctx))
	if !exposure.Enabled && !policy.Restricted() && len(cfg.CursorModelAliases) == 0 {
		return modelName, rawJSON, nil
	}

	if combo, ok := h.resolveCombo(modelName, *execOptions); ok {
		if !policy.AllowsCombo(combo.Name) {
			return "", nil, modelNotAllowedError(modelName)
		}
		return modelName, rawJSON, nil
	}

	baseModel, rawSuffix, hasSuffix := parseComboLookupName(modelName)
	if baseModel == "" {
		return modelName, rawJSON, nil
	}
	qualified := strings.Contains(baseModel, "/")

	if alias, exposedAlias := matchCursorAlias(cfg, baseModel); alias != nil {
		if exposure.Enabled && !qualified && !exposure.AcceptBareIDs {
			return "", nil, modelNotFoundError(modelName)
		}
		if !policy.AllowsName(exposedAlias) {
			return "", nil, modelNotAllowedError(modelName)
		}
		effort := rawSuffixEffort(modelName)
		if effort == "" {
			effort = cursor.RequestEffort(entryProtocol, baseModel, rawJSON)
		}
		target, _ := cursor.PickAliasVariant(alias.Variants, effort, alias.DefaultEffort)
		if target == "" {
			return "", nil, modelNotFoundError(modelName)
		}
		target = cursor.ClientModelID(target)
		execOptions.AllowedProviders = []string{"cursor"}
		execOptions.CursorPinned = true
		return target, rewriteModelField(rawJSON, target), nil
	}

	candidates, viaPrefix := resolveModelCandidates(cfg, baseModel)
	for _, c := range candidates {
		// A registry id that already equals its exposed name (a credential prefix equal to the
		// provider key) is the prefixed form, not a bare id.
		if strings.EqualFold(baseModel, c.exposedID(exposure)) {
			viaPrefix = true
		}
	}
	if exposure.Enabled && !viaPrefix && !exposure.AcceptBareIDs {
		return "", nil, modelNotFoundError(modelName)
	}
	if len(candidates) == 0 {
		if exposure.Enabled || policy.Restricted() {
			return "", nil, modelNotFoundError(modelName)
		}
		return modelName, rawJSON, nil
	}
	exposed := candidates[:0:0]
	for _, c := range candidates {
		if ok, _ := modelexposure.DecideWithAliases(exposure, cfg.CursorModelAliases, c.key, c.nativeID); ok {
			exposed = append(exposed, c)
		}
	}
	if len(exposed) == 0 {
		return "", nil, modelNotFoundError(modelName)
	}
	allowed := exposed[:0:0]
	for _, c := range exposed {
		if policy.AllowsName(c.exposedID(exposure)) {
			allowed = append(allowed, c)
		}
	}
	if len(allowed) == 0 {
		return "", nil, modelNotAllowedError(modelName)
	}
	providers := make([]string, 0, len(allowed))
	for _, c := range allowed {
		if c.nativeID == allowed[0].nativeID {
			providers = append(providers, c.registryProvider)
		}
	}
	execOptions.AllowedProviders = providers
	if allowed[0].key == "cursor" {
		return h.pinCursorVariant(exposure, cfg, policy, entryProtocol, modelName, withSuffix(allowed[0].nativeID, rawSuffix, hasSuffix), rawJSON, execOptions)
	}
	return modelName, rawJSON, nil
}

// matchCursorAlias finds the Cursor alias a request names, as "<cursor prefix>/<alias>" or bare,
// and returns it with its exposed name.
func matchCursorAlias(cfg *config.SDKConfig, baseModel string) (*config.CursorModelAlias, string) {
	if cfg == nil || len(cfg.CursorModelAliases) == 0 {
		return nil, ""
	}
	cursorPrefix := modelexposure.Settings(&cfg.ModelExposure, "cursor").Prefix
	name := baseModel
	if prefix, rest, ok := strings.Cut(baseModel, "/"); ok {
		if !strings.EqualFold(strings.TrimSpace(prefix), cursorPrefix) {
			return nil, ""
		}
		name = rest
	}
	alias := modelexposure.FindAlias(cfg.CursorModelAliases, name)
	if alias == nil {
		return nil, ""
	}
	return alias, cursorPrefix + "/" + alias.Alias
}

func rewriteModelField(rawJSON []byte, model string) []byte {
	if len(rawJSON) == 0 || !ojson.GetBytesNoCopy(rawJSON, "model").Exists() {
		return rawJSON
	}
	doc := ojson.NewDoc(rawJSON)
	if err := doc.Set("model", model); err != nil {
		return rawJSON
	}
	return doc.Bytes()
}

// restrictProviders keeps the providers the model access gate allowed.
func restrictProviders(providers, allowed []string, modelName string) ([]string, *interfaces.ErrorMessage) {
	if allowed == nil {
		return providers, nil
	}
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		for _, a := range allowed {
			if strings.EqualFold(strings.TrimSpace(p), strings.TrimSpace(a)) {
				out = append(out, p)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, modelNotFoundError(modelName)
	}
	return out, nil
}

const homeModelFiltersMessage = "model exposure and API key model policies are not supported in Home mode"

// ModelAccessError is a model access refusal for routes that report plain errors.
type ModelAccessError struct {
	Status  int
	Code    string
	Message string
}

func (e *ModelAccessError) Error() string { return e.Message }

// StatusCode reports the HTTP status of the refusal.
func (e *ModelAccessError) StatusCode() int { return e.Status }

// ModelFiltersActive reports whether model exposure or the caller key's policy restricts models.
func (h *BaseAPIHandler) ModelFiltersActive(ctx context.Context) bool {
	cfg := h.CurrentConfig()
	if cfg == nil {
		return false
	}
	return cfg.ModelExposure.Enabled || modelexposure.PolicyFor(cfg.APIKeyPolicies, coreusage.APIKeyFromContext(ctx)).Restricted()
}

// HomeModelFiltersError refuses Home-mode model routes while model filters are active. Home owns
// model routing and the catalog remotely, so the local registry cannot tell which provider (and so
// which exposed name) serves a model; refusing keeps the filters from being silently skipped.
func (h *BaseAPIHandler) HomeModelFiltersError(ctx context.Context) *ModelAccessError {
	if h == nil || !h.ModelFiltersActive(ctx) {
		return nil
	}
	return &ModelAccessError{Status: http.StatusServiceUnavailable, Code: "model_filters_unsupported", Message: homeModelFiltersMessage}
}

// CheckProviderModelAccess applies model exposure and the caller key's policy to a model that a
// route always sends to one provider (Codex live and realtime, Codex alpha search), registered or
// not. The model is matched as "<provider prefix>/<model>". Callers pass the effective model
// (defaults applied); an empty model means the upstream would choose, so it is refused.
func (h *BaseAPIHandler) CheckProviderModelAccess(ctx context.Context, provider, model string) error {
	if h == nil {
		return nil
	}
	cfg := h.CurrentConfig()
	if cfg == nil {
		return nil
	}
	exposure := &cfg.ModelExposure
	policy := modelexposure.PolicyFor(cfg.APIKeyPolicies, coreusage.APIKeyFromContext(ctx))
	if !exposure.Enabled && !policy.Restricted() {
		return nil
	}
	if h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return h.HomeModelFiltersError(ctx)
	}
	key := modelexposure.ProviderKey(provider)
	base, _, _ := parseComboLookupName(model)
	if prefix, rest, ok := strings.Cut(base, "/"); ok {
		if !strings.EqualFold(prefix, modelexposure.Settings(exposure, key).Prefix) && !strings.EqualFold(prefix, key) {
			return &ModelAccessError{Status: http.StatusBadRequest, Code: "model_not_found", Message: "unknown provider for model " + model}
		}
		base = rest
	}
	if base == "" {
		// Without a model the upstream picks its own default, which cannot be checked here.
		if policy.Restricted() {
			return &ModelAccessError{Status: http.StatusForbidden, Code: "model_not_allowed", Message: "a model is required for this API key"}
		}
		return &ModelAccessError{Status: http.StatusBadRequest, Code: "model_required", Message: "a model is required while model exposure is enabled"}
	}
	if ok, _ := modelexposure.DecideWithAliases(exposure, cfg.CursorModelAliases, key, base); !ok {
		return &ModelAccessError{Status: http.StatusBadRequest, Code: "model_not_found", Message: "unknown provider for model " + model}
	}
	if !policy.AllowsName(modelexposure.ExposedID(exposure, key, base)) {
		return &ModelAccessError{Status: http.StatusForbidden, Code: "model_not_allowed", Message: "model " + model + " is not allowed for this API key"}
	}
	return nil
}

// pinCursorVariant resolves a Cursor request to the exact upstream variant the executor would
// pick (same resolver, same effort inputs: "(level)" suffix, reasoning fields, thinking budget),
// requires exposure and the key policy to admit that variant as well as the requested name, and
// pins it: the model becomes the variant's registry id with no suffix and the executor is told
// not to re-pick from the payload effort.
func (h *BaseAPIHandler) pinCursorVariant(exposure *config.ModelExposureConfig, cfg *config.SDKConfig, policy modelexposure.Policy, entryProtocol, modelName, nativeModel string, rawJSON []byte, execOptions *modelExecutionOptions) (string, []byte, *interfaces.ErrorMessage) {
	upstream := cursor.ResolveUpstreamModel(nativeModel, cursor.RequestEffort(entryProtocol, thinking.ParseSuffix(nativeModel).ModelName, rawJSON))
	if strings.TrimSpace(upstream) == "" {
		return "", nil, modelNotFoundError(modelName)
	}
	variant := cursor.ClientModelID(upstream)
	if ok, _ := modelexposure.DecideWithAliases(exposure, cfg.CursorModelAliases, "cursor", variant); !ok {
		return "", nil, modelNotFoundError(modelName)
	}
	if !policy.AllowsName(modelexposure.ExposedID(exposure, "cursor", variant)) {
		return "", nil, modelNotAllowedError(modelName)
	}
	execOptions.CursorPinned = true
	return variant, rewriteModelField(rawJSON, variant), nil
}

// rawSuffixEffort returns the effort a "(level)" model suffix asks for, if any.
func rawSuffixEffort(modelName string) string {
	if parsed := thinking.ParseSuffix(stripComboContextMarker(modelName)); parsed.HasSuffix {
		return strings.TrimSpace(parsed.RawSuffix)
	}
	return ""
}
