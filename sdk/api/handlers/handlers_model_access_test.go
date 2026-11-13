package handlers

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cursorguard"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	mxRestrictedKey = "mx-restricted-key"
	mxEmptyKey      = "mx-empty-key"
)

type modelAccessFixture struct {
	handler *BaseAPIHandler
	codex   *modelExecutionCaptureExecutor
	cursor  *modelExecutionCaptureExecutor
	cfg     *config.SDKConfig
}

func newModelAccessFixture(t *testing.T) *modelAccessFixture {
	t.Helper()
	cfg := &config.SDKConfig{
		Combos: []config.ComboConfig{{Name: "mx-combo", Models: []string{"mx-gpt-hidden"}}},
		ModelExposure: config.ModelExposureConfig{
			Enabled:          true,
			ListPrefixedOnly: true,
			AcceptBareIDs:    true,
			Providers: map[string]config.ModelExposureProvider{
				"codex":  {Mode: "allowlist", Allow: []string{"mx-gpt-open*"}},
				"cursor": {Prefix: "cur", Mode: "allowlist", Allow: []string{"mx-*"}, Deny: []string{"mx-ds*"}},
			},
		},
		CursorModelAliases: []config.CursorModelAlias{{
			Alias:         "mx-ds",
			DefaultEffort: "medium",
			Variants: map[string]string{
				"low":    "cursor-mx-ds-low",
				"medium": "cursor-mx-ds",
				"high":   "cursor-mx-ds-high",
			},
		}},
		APIKeyPolicies: []config.APIKeyPolicy{
			{Key: mxRestrictedKey, Restricted: true, Allow: []string{"combo:mx-combo", "codex/mx-gpt-open", "cur/mx-ds"}},
			{Key: mxEmptyKey, Restricted: true},
		},
	}
	cfg.SanitizeModelExposure()

	f := &modelAccessFixture{
		codex:  &modelExecutionCaptureExecutor{provider: "codex"},
		cursor: &modelExecutionCaptureExecutor{provider: "cursor"},
		cfg:    cfg,
	}
	manager := coreauth.NewManager(nil, nil, nil)
	reg := registry.GetGlobalRegistry()
	for _, entry := range []struct {
		exec   *modelExecutionCaptureExecutor
		models []string
	}{
		{f.codex, []string{"mx-gpt-open", "mx-gpt-hidden"}},
		{f.cursor, []string{"cursor-default", "cursor-mx-opus", "cursor-mx-ds-low", "cursor-mx-ds", "cursor-mx-ds-high", "cursor-zz-opus-low", "cursor-zz-opus-high", "cursor-zz-opus-max"}},
	} {
		manager.RegisterExecutor(entry.exec)
		auth := &coreauth.Auth{ID: "mx-" + entry.exec.provider, Provider: entry.exec.provider, Status: coreauth.StatusActive}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register: %v", err)
		}
		infos := make([]*registry.ModelInfo, 0, len(entry.models))
		for _, m := range entry.models {
			infos = append(infos, &registry.ModelInfo{ID: m, Object: "model", OwnedBy: entry.exec.provider})
		}
		reg.RegisterClient(auth.ID, auth.Provider, infos)
		authID := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(authID) })
	}
	f.handler = NewBaseAPIHandlers(cfg, manager)
	return f
}

func keyContext(key string) context.Context {
	return coreusage.WithAPIKey(context.Background(), key)
}

func (f *modelAccessFixture) listIDs(ctx context.Context) []string {
	var ids []string
	for _, m := range f.handler.ListModels(ctx, "openai") {
		id, _ := m["id"].(string)
		if strings.HasPrefix(id, "mx-") || strings.Contains(id, "/mx-") {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (f *modelAccessFixture) run(ctx context.Context, model, body string) (string, *interfaces.ErrorMessage) {
	f.codex.lastRequest.Model = ""
	f.cursor.lastRequest.Model = ""
	if body == "" {
		body = `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	}
	_, _, errMsg := f.handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(body), "")
	if errMsg != nil {
		return "", errMsg
	}
	if got, _ := f.codex.captured(); got.Model != "" {
		return got.Model, nil
	}
	got, _ := f.cursor.captured()
	return got.Model, nil
}

func TestModelListAppliesExposurePrefixesAliasesAndCombos(t *testing.T) {
	f := newModelAccessFixture(t)

	got := strings.Join(f.listIDs(context.Background()), ",")
	if want := "codex/mx-gpt-open,cur/mx-ds,cur/mx-opus,mx-combo"; got != want {
		t.Fatalf("listed = %s, want %s", got, want)
	}

	got = strings.Join(f.listIDs(keyContext(mxRestrictedKey)), ",")
	if want := "codex/mx-gpt-open,cur/mx-ds,mx-combo"; got != want {
		t.Fatalf("restricted key listed = %s, want %s", got, want)
	}

	if got := f.listIDs(keyContext(mxEmptyKey)); len(got) != 0 {
		t.Fatalf("empty-allow key listed %v, want nothing", got)
	}

	f.cfg.ModelExposure.Enabled = false
	got = strings.Join(f.listIDs(context.Background()), ",")
	if want := "mx-combo,mx-ds,mx-gpt-hidden,mx-gpt-open"; got != want {
		t.Fatalf("disabled exposure listed = %s, want %s", got, want)
	}
}

func TestModelAccessRejectsHiddenModelsAndResolvesPrefixes(t *testing.T) {
	f := newModelAccessFixture(t)
	ctx := context.Background()

	for _, model := range []string{"mx-gpt-hidden", "codex/mx-gpt-hidden", "cursor-mx-ds-high", "cur/mx-ds-high", "cursor-default", "cursor-not-registered"} {
		if _, errMsg := f.run(ctx, model, ""); errMsg == nil || errMsg.StatusCode != http.StatusBadRequest || !strings.Contains(errMsg.Error.Error(), "model_not_found") {
			t.Fatalf("%s: err = %+v, want 400 model_not_found", model, errMsg)
		}
	}

	for model, want := range map[string]string{
		"codex/mx-gpt-open": "mx-gpt-open",
		"CODEX/MX-GPT-OPEN": "mx-gpt-open",
		"mx-gpt-open":       "mx-gpt-open",
		"cur/mx-opus":       "cursor-mx-opus",
		"cur/mx-opus(high)": "cursor-mx-opus",
		"cursor/mx-opus":    "cursor-mx-opus", // built-in provider aliases keep working
		"mx-combo":          "mx-gpt-hidden",
	} {
		got, errMsg := f.run(ctx, model, "")
		if errMsg != nil {
			t.Fatalf("%s: unexpected error %+v", model, errMsg)
		}
		if base := thinking.ParseSuffix(got).ModelName; base != want {
			t.Fatalf("%s: executed %q, want %q", model, got, want)
		}
	}

	f.cfg.ModelExposure.AcceptBareIDs = false
	if _, errMsg := f.run(ctx, "mx-gpt-open", ""); errMsg == nil {
		t.Fatal("bare id accepted with accept-bare-ids=false")
	}
	if _, errMsg := f.run(ctx, "mx-combo", ""); errMsg != nil {
		t.Fatalf("combo rejected with accept-bare-ids=false: %+v", errMsg)
	}
}

func TestCursorAliasResolvesEffortToVariant(t *testing.T) {
	f := newModelAccessFixture(t)
	ctx := context.Background()

	cases := []struct{ model, body, want string }{
		{"cur/mx-ds", `{"model":"cur/mx-ds","reasoning_effort":"high"}`, "cursor-mx-ds-high"},
		{"cur/mx-ds", `{"model":"cur/mx-ds","reasoning_effort":"low"}`, "cursor-mx-ds-low"},
		{"cur/mx-ds", `{"model":"cur/mx-ds","reasoning_effort":"xhigh"}`, "cursor-mx-ds-high"},
		{"cur/mx-ds", `{"model":"cur/mx-ds"}`, "cursor-mx-ds"},
		{"cur/mx-ds(low)", `{"model":"cur/mx-ds(low)","reasoning_effort":"high"}`, "cursor-mx-ds-low"},
		{"mx-ds", `{"model":"mx-ds","reasoning_effort":"high"}`, "cursor-mx-ds-high"},
	}
	// Claude adaptive thinking reaches the alias through translation, as it reaches the executor.
	if _, _, errMsg := f.handler.ExecuteWithAuthManager(ctx, "claude", "cur/mx-ds", []byte(`{"model":"cur/mx-ds","max_tokens":8,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hi"}]}`), ""); errMsg != nil {
		t.Fatalf("claude alias request: %+v", errMsg)
	}
	if got, _ := f.cursor.captured(); got.Model != "cursor-mx-ds-low" {
		t.Fatalf("claude output_config.effort low via alias executed %q, want cursor-mx-ds-low", got.Model)
	}
	for _, tc := range cases {
		got, errMsg := f.run(ctx, tc.model, tc.body)
		if errMsg != nil {
			t.Fatalf("%s %s: unexpected error %+v", tc.model, tc.body, errMsg)
		}
		if got != tc.want {
			t.Fatalf("%s %s: executed %q, want %q", tc.model, tc.body, got, tc.want)
		}
	}
}

func TestAPIKeyPolicyGatesEveryEntryPoint(t *testing.T) {
	f := newModelAccessFixture(t)
	restricted := keyContext(mxRestrictedKey)

	for _, model := range []string{"codex/mx-gpt-open", "mx-gpt-open", "mx-combo", "cur/mx-ds", "mx-ds"} {
		if _, errMsg := f.run(restricted, model, ""); errMsg != nil {
			t.Fatalf("%s: restricted key rejected: %+v", model, errMsg)
		}
	}
	for _, model := range []string{"cur/mx-opus", "CUR/MX-OPUS", "cur/mx-opus(high)", "cursor-mx-opus", "mx-opus"} {
		_, errMsg := f.run(restricted, model, "")
		if errMsg == nil {
			t.Fatalf("%s: restricted key allowed", model)
		}
	}
	if _, errMsg := f.run(restricted, "cur/mx-opus", ""); errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("not-allowed status = %d, want 403", errMsg.StatusCode)
	}

	empty := keyContext(mxEmptyKey)
	for _, model := range []string{"mx-combo", "codex/mx-gpt-open", "cur/mx-ds"} {
		if _, errMsg := f.run(empty, model, ""); errMsg == nil {
			t.Fatalf("%s: restricted key with empty allow list was allowed", model)
		}
	}

	_, _, errChan := f.handler.ExecuteStreamWithAuthManager(restricted, "openai", "cur/mx-opus", []byte(`{"model":"cur/mx-opus"}`), "")
	if errMsg := <-errChan; errMsg == nil || errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("stream: err = %+v, want 403", errMsg)
	}
	if _, _, errMsg := f.handler.ExecuteCountWithAuthManager(restricted, "claude", "cur/mx-opus", []byte(`{"model":"cur/mx-opus"}`), ""); errMsg == nil || errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("count: err = %+v, want 403", errMsg)
	}
}

// An alias that takes a model's exposed name owns it: listed once, routed by the alias, and the
// model itself is reachable only through the alias.
func TestCursorAliasOwnsCollidingModelName(t *testing.T) {
	f := newModelAccessFixture(t)
	f.cfg.CursorModelAliases = append(f.cfg.CursorModelAliases, config.CursorModelAlias{
		Alias: "mx-opus", Variants: map[string]string{"high": "cursor-mx-ds-high"},
	})
	ctx := context.Background()

	if got := strings.Join(f.listIDs(ctx), ","); got != "codex/mx-gpt-open,cur/mx-ds,cur/mx-opus,mx-combo" {
		t.Fatalf("listed = %s", got)
	}
	var owner string
	for _, m := range f.handler.ListModels(ctx, "openai") {
		if m["id"] == "cur/mx-opus" {
			owner, _ = m["owned_by"].(string)
		}
	}
	if owner != "cursor" {
		t.Fatalf("cur/mx-opus listed from %q, want the alias entry", owner)
	}
	if got, errMsg := f.run(ctx, "cur/mx-opus", ""); errMsg != nil || got != "cursor-mx-ds-high" {
		t.Fatalf("cur/mx-opus executed %q (err %+v), want the alias variant cursor-mx-ds-high", got, errMsg)
	}
	if _, errMsg := f.run(ctx, "cursor-mx-opus", ""); errMsg == nil {
		t.Fatal("the shadowed model stayed callable by its registry id")
	}

	f.cfg.ModelExposure.ListPrefixedOnly = false
	if got := strings.Join(f.listIDs(ctx), ","); got != "codex/mx-gpt-open,cur/mx-ds,cur/mx-opus,mx-combo,mx-ds,mx-gpt-open,mx-opus" {
		t.Fatalf("list-prefixed-only=false listed = %s, want prefixed and bare ids", got)
	}
}

// A "(level)" suffix, payload effort or a family id must not reach a Cursor variant that exposure
// hides or the key may not call: the gate checks the variant the executor will run and pins it.
func TestCursorVariantSelectionCannotBypassExposureOrPolicy(t *testing.T) {
	f := newModelAccessFixture(t)
	cursor.RecordUsableModels([]cursor.UsableModel{{ID: "zz-opus-low"}, {ID: "zz-opus-high"}, {ID: "zz-opus-max"}})
	f.cfg.ModelExposure.Providers["cursor"] = config.ModelExposureProvider{Prefix: "cur", Mode: "allowlist", Allow: []string{"mx-*", "zz-*"}, Deny: []string{"mx-ds*", "zz-opus-max"}}
	f.cfg.APIKeyPolicies = append(f.cfg.APIKeyPolicies, config.APIKeyPolicy{Key: "zz-key", Restricted: true, Allow: []string{"cur/zz-opus-high", "cur/zz-opus-low"}})
	open, limited := context.Background(), keyContext("zz-key")

	for _, tc := range []struct {
		ctx         context.Context
		model, body string
		status      int
	}{
		{open, "cur/zz-opus-high(max)", "", http.StatusBadRequest},                                       // suffix to a denied variant
		{open, "cur/zz-opus", `{"model":"cur/zz-opus","reasoning_effort":"max"}`, http.StatusBadRequest}, // family id + effort
		{open, "cursor-zz-opus-low(max)", "", http.StatusBadRequest},                                     // bare registry id + suffix
		{limited, "cur/zz-opus-high(max)", "", http.StatusBadRequest},
		{limited, "cur/zz-opus-low(high)", "", 0},
	} {
		got, errMsg := f.run(tc.ctx, tc.model, tc.body)
		if tc.status == 0 {
			if errMsg != nil || got != "cursor-zz-opus-high" {
				t.Fatalf("%s: executed %q err %+v, want cursor-zz-opus-high", tc.model, got, errMsg)
			}
			continue
		}
		if errMsg == nil || errMsg.StatusCode != tc.status {
			t.Fatalf("%s %s: executed %q err %+v, want status %d", tc.model, tc.body, got, errMsg, tc.status)
		}
	}

	// A variant the key may not call but exposure shows: 403 on the resolved variant.
	f.cfg.ModelExposure.Providers["cursor"] = config.ModelExposureProvider{Prefix: "cur", Mode: "all"}
	if _, errMsg := f.run(limited, "cur/zz-opus-high(max)", ""); errMsg == nil || errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("limited key reached zz-opus-max: %+v", errMsg)
	}
	// The executor is told the model is pinned, so payload effort cannot re-pick the variant.
	got, errMsg := f.run(limited, "cur/zz-opus-high", `{"model":"cur/zz-opus-high","reasoning_effort":"max"}`)
	_, opts := f.cursor.captured()
	if errMsg != nil || got != "cursor-zz-opus-high" || opts.Metadata[coreexecutor.CursorPinnedModelMetadataKey] != true {
		t.Fatalf("pinned run: model %q err %+v pinned=%v", got, errMsg, opts.Metadata[coreexecutor.CursorPinnedModelMetadataKey])
	}
}

// Fixed-provider routes (Codex live, realtime, alpha search) use CheckProviderModelAccess; Home mode
// refuses while filters are active rather than skipping them.
func TestProviderModelAccessAndHomeRefusal(t *testing.T) {
	f := newModelAccessFixture(t)
	for _, tc := range []struct {
		ctx    context.Context
		model  string
		status int
	}{
		{keyContext(mxEmptyKey), "mx-gpt-open", http.StatusForbidden},
		{keyContext(mxEmptyKey), "", http.StatusForbidden},
		{keyContext(mxRestrictedKey), "gpt-live-1-codex", http.StatusBadRequest}, // exposure first
		{keyContext(mxRestrictedKey), "codex/mx-gpt-open", 0},
		{keyContext(mxRestrictedKey), "mx-gpt-open", 0},
		{context.Background(), "gpt-live-1-codex", http.StatusBadRequest}, // codex allowlist hides it
		{context.Background(), "mx-gpt-open", 0},
		{context.Background(), "", http.StatusBadRequest}, // exposure on: the upstream default cannot be checked
	} {
		err := f.handler.CheckProviderModelAccess(tc.ctx, "codex", tc.model)
		status := 0
		if err != nil {
			status = err.(*ModelAccessError).StatusCode()
		}
		if status != tc.status {
			t.Errorf("CheckProviderModelAccess(%q) status %d, want %d (%v)", tc.model, status, tc.status, err)
		}
	}

	f.handler.AuthManager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	if _, errMsg := f.run(keyContext(mxRestrictedKey), "codex/mx-gpt-open", ""); errMsg == nil || errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Home mode with filters active: %+v, want 503", errMsg)
	}
}

// An openai-compatibility model registered under a credential prefix equal to the provider key is
// listed once under that prefix, not doubled, and stays callable by that name.
func TestCredentialPrefixedModelIsNotDoublePrefixed(t *testing.T) {
	f := newModelAccessFixture(t)
	compat := &modelExecutionCaptureExecutor{provider: "openai-compatible-zp-mx"}
	f.handler.AuthManager.RegisterExecutor(compat)
	auth := &coreauth.Auth{ID: "mx-zp", Provider: compat.provider, Status: coreauth.StatusActive}
	if _, err := f.handler.AuthManager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "zp-mx/mx-glm", Object: "model"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	f.cfg.ModelExposure.AcceptBareIDs = false

	var listed []string
	for _, m := range f.handler.ListModels(context.Background(), "openai") {
		if id, _ := m["id"].(string); strings.Contains(id, "mx-glm") {
			listed = append(listed, id)
		}
	}
	if strings.Join(listed, ",") != "zp-mx/mx-glm" {
		t.Fatalf("listed %v, want [zp-mx/mx-glm]", listed)
	}
	if _, _, errMsg := f.handler.ExecuteWithAuthManager(context.Background(), "openai", "zp-mx/mx-glm", []byte(`{"model":"zp-mx/mx-glm"}`), ""); errMsg != nil {
		t.Fatalf("calling the listed name: %+v", errMsg)
	}
	if got, _ := compat.captured(); got.Model != "zp-mx/mx-glm" {
		t.Fatalf("executed %q, want the registry id zp-mx/mx-glm", got.Model)
	}
}

// A configured Cursor alias used as a combo member resolves like a direct request: the member skips
// the access checks, not the alias mapping.
func TestCursorAliasResolvesAsComboMember(t *testing.T) {
	f := newModelAccessFixture(t)
	f.cfg.Combos = append(f.cfg.Combos, config.ComboConfig{Name: "mx-alias-combo", Models: []string{"cur/mx-ds"}})
	got, errMsg := f.run(context.Background(), "mx-alias-combo", `{"model":"mx-alias-combo","reasoning_effort":"high"}`)
	if errMsg != nil || got != "cursor-mx-ds-high" {
		t.Fatalf("combo member alias executed %q (err %+v), want cursor-mx-ds-high", got, errMsg)
	}
}

// A Cursor credential refused by the spend guard fails its combo member with a credential-scoped
// 429, so the combo falls through to its next member; a direct call gets the refusal.
func TestComboFallsThroughWhenCursorSpendCapIsReached(t *testing.T) {
	f := newModelAccessFixture(t)
	f.cursor.execute = func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
		return coreexecutor.Response{}, &cursorguard.BlockedError{Reason: cursorguard.Reason{Code: cursorguard.ReasonCapReached, Message: "cap"}}
	}
	f.cfg.Combos = append(f.cfg.Combos, config.ComboConfig{Name: "mx-spend-combo", Models: []string{"cur/mx-opus", "codex/mx-gpt-open"}})
	got, errMsg := f.run(context.Background(), "mx-spend-combo", "")
	if errMsg != nil || got != "mx-gpt-open" {
		t.Fatalf("combo executed %q (err %+v), want fall-through to mx-gpt-open", got, errMsg)
	}
	if _, errMsg = f.run(context.Background(), "cur/mx-opus", ""); errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests || !strings.Contains(errMsg.Error.Error(), "cursor_spend_cap_reached") {
		t.Fatalf("direct call: %+v, want 429 cursor_spend_cap_reached", errMsg)
	}
}
