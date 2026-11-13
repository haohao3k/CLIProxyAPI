package handlers

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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
		{f.cursor, []string{"cursor-default", "cursor-mx-opus", "cursor-mx-ds-low", "cursor-mx-ds", "cursor-mx-ds-high"}},
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
		{"cur/mx-ds", `{"model":"cur/mx-ds"}`, "cursor-mx-ds(medium)"},
		{"cur/mx-ds(low)", `{"model":"cur/mx-ds(low)","reasoning_effort":"high"}`, "cursor-mx-ds-low"},
		{"mx-ds", `{"model":"mx-ds","reasoning_effort":"high"}`, "cursor-mx-ds-high"},
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
