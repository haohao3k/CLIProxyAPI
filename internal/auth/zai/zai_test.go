package zai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// fakeZAI emulates the ZCode CLI OAuth + business endpoints.
type fakeZAI struct {
	t             *testing.T
	pendingPolls  int32
	polls         int32
	createdKey    int32
	existingKey   bool
	pollStatus    string
	initPollToken string
}

func (f *fakeZAI) handler() http.Handler {
	mux := http.NewServeMux()
	writeEnvelope := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "data": data})
	}
	mux.HandleFunc("/cli/oauth/cli/init", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || len(strings.TrimPrefix(auth, "Bearer ")) != 64 {
			f.t.Errorf("init: expected 32-byte hex bearer poll token, got %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"provider":"zai"`) {
			f.t.Errorf("init: unexpected body %s", body)
		}
		data := map[string]any{
			"flow_id":           "flow-123",
			"authorize_url":     "https://chat.z.ai/authorize?flow=flow-123",
			"poll_interval_sec": 1,
			"expires_at":        time.Now().Add(5 * time.Minute).UnixMilli(),
		}
		if f.initPollToken != "" {
			data["poll_token"] = f.initPollToken
		}
		writeEnvelope(w, data)
	})
	mux.HandleFunc("/cli/oauth/cli/poll/flow-123", func(w http.ResponseWriter, r *http.Request) {
		if f.initPollToken != "" && r.Header.Get("Authorization") != "Bearer "+f.initPollToken {
			f.t.Errorf("poll: expected server poll token, got %q", r.Header.Get("Authorization"))
		}
		n := atomic.AddInt32(&f.polls, 1)
		if n <= atomic.LoadInt32(&f.pendingPolls) {
			writeEnvelope(w, map[string]any{"status": "pending"})
			return
		}
		status := f.pollStatus
		if status == "" {
			status = "ready"
		}
		if status != "ready" {
			writeEnvelope(w, map[string]any{"status": status})
			return
		}
		writeEnvelope(w, map[string]any{
			"status": "ready",
			"token":  "zcode-jwt",
			"user":   map[string]any{"user_id": "u-1", "email": "dev@example.com", "name": "Dev"},
			"zai":    map[string]any{"access_token": "zai-oauth-token"},
		})
	})
	mux.HandleFunc("/auth/z/login", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"token":"zai-oauth-token"`) {
			f.t.Errorf("business login: unexpected body %s", body)
		}
		writeEnvelope(w, map[string]any{"access_token": "biz-jwt"})
	})
	requireBiz := func(r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer biz-jwt" {
			f.t.Errorf("%s: expected business bearer, got %q", r.URL.Path, got)
		}
	}
	mux.HandleFunc("/biz/customer/getCustomerInfo", func(w http.ResponseWriter, r *http.Request) {
		requireBiz(r)
		writeEnvelope(w, map[string]any{"organizations": []any{
			map[string]any{"organizationId": "org-team", "organizationName": "Team", "projects": []any{
				map[string]any{"projectId": "p-team", "projectName": "Team", "projectType": "2"},
			}},
			map[string]any{"organizationId": "org-1", "organizationName": "默认机构", "projects": []any{
				map[string]any{"projectId": "p-other", "projectName": "other", "projectType": 1},
				map[string]any{"projectId": "p-1", "projectName": "默认项目", "projectType": 1},
			}},
		}})
	})
	keysPath := "/biz/v1/organization/org-1/projects/p-1/api_keys"
	mux.HandleFunc(keysPath, func(w http.ResponseWriter, r *http.Request) {
		requireBiz(r)
		switch r.Method {
		case http.MethodGet:
			keys := []any{map[string]any{"name": "other", "apiKey": "nope"}}
			if f.existingKey {
				keys = append(keys, map[string]any{"name": PlanAPIKeyName, "apiKey": "ak-existing"})
			}
			writeEnvelope(w, keys)
		case http.MethodPost:
			atomic.AddInt32(&f.createdKey, 1)
			writeEnvelope(w, map[string]any{"name": PlanAPIKeyName, "apiKey": "ak-new"})
		}
	})
	mux.HandleFunc(keysPath+"/copy/", func(w http.ResponseWriter, r *http.Request) {
		requireBiz(r)
		key := strings.TrimPrefix(r.URL.Path, keysPath+"/copy/")
		writeEnvelope(w, map[string]any{"secretKey": "secret-for-" + key})
	})
	return mux
}

func newTestService(t *testing.T, fake *fakeZAI) *Service {
	t.Helper()
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	svc := NewService(server.Client())
	svc.SetEndpoints(server.URL+"/cli", server.URL+"/auth/z/login", server.URL+"/biz")
	return svc
}

func TestLoginFlowMintsNewCodingPlanKey(t *testing.T) {
	fake := &fakeZAI{t: t, pendingPolls: 1, initPollToken: "server-poll-token"}
	svc := newTestService(t, fake)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	flow, err := svc.StartFlow(ctx)
	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	if flow.FlowID != "flow-123" || flow.PollToken != "server-poll-token" || !strings.HasPrefix(flow.AuthorizeURL, "https://chat.z.ai/") {
		t.Fatalf("unexpected flow: %+v", flow)
	}
	if flow.Interval != time.Second {
		t.Fatalf("interval = %v, want 1s", flow.Interval)
	}
	if flow.ExpiresIn() <= 0 || flow.ExpiresIn() > 300 {
		t.Fatalf("expires in = %d (millisecond epoch not parsed?)", flow.ExpiresIn())
	}

	authz, err := svc.WaitForAuthorization(ctx, flow)
	if err != nil {
		t.Fatalf("WaitForAuthorization: %v", err)
	}
	if atomic.LoadInt32(&fake.polls) != 2 {
		t.Fatalf("polls = %d, want 2 (one pending)", fake.polls)
	}
	if authz.OAuthToken != "zai-oauth-token" || authz.ZCodeToken != "zcode-jwt" || authz.User.Email != "dev@example.com" {
		t.Fatalf("unexpected authorization: %+v", authz)
	}

	bundle, err := svc.CompleteLogin(ctx, authz)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if bundle.APIKey != "ak-new.secret-for-ak-new" {
		t.Fatalf("api key = %q", bundle.APIKey)
	}
	if bundle.BaseURL != CodingPlanBaseURL {
		t.Fatalf("base url = %q", bundle.BaseURL)
	}
	if atomic.LoadInt32(&fake.createdKey) != 1 {
		t.Fatalf("expected key creation")
	}
}

func TestMintReusesExistingKey(t *testing.T) {
	fake := &fakeZAI{t: t, existingKey: true}
	svc := newTestService(t, fake)
	key, err := svc.MintCodingPlanKey(context.Background(), "biz-jwt")
	if err != nil {
		t.Fatalf("MintCodingPlanKey: %v", err)
	}
	if key != "ak-existing.secret-for-ak-existing" {
		t.Fatalf("key = %q", key)
	}
	if atomic.LoadInt32(&fake.createdKey) != 0 {
		t.Fatalf("existing key must be reused, not recreated")
	}
}

func TestWaitForAuthorizationFailedIsTerminal(t *testing.T) {
	fake := &fakeZAI{t: t, pollStatus: "failed"}
	svc := newTestService(t, fake)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	flow, err := svc.StartFlow(ctx)
	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	if _, err = svc.WaitForAuthorization(ctx, flow); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("expected terminal failure, got %v", err)
	}
	if atomic.LoadInt32(&fake.polls) != 1 {
		t.Fatalf("terminal status must stop polling, polls = %d", fake.polls)
	}
}

func TestPollOncePending(t *testing.T) {
	fake := &fakeZAI{t: t, pendingPolls: 5}
	svc := newTestService(t, fake)
	flow := &Flow{FlowID: "flow-123", PollToken: "x"}
	authz, err := svc.PollOnce(context.Background(), flow)
	if err != nil || authz != nil {
		t.Fatalf("pending poll = (%v, %v), want (nil, nil)", authz, err)
	}
}

func TestEnvelopeErrorSurfacesMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1001,"msg":"token invalid","data":null}`))
	}))
	defer server.Close()
	svc := NewService(server.Client())
	svc.SetEndpoints("", server.URL, "")
	_, err := svc.ExchangeBusinessToken(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "token invalid") {
		t.Fatalf("expected envelope error, got %v", err)
	}
}

func TestSelectOrgProjectRequiresPersonalProject(t *testing.T) {
	info := &customerInfo{}
	if _, _, err := selectOrgProject(info); err == nil {
		t.Fatal("expected error without organizations")
	}
}

func TestBuildAuthRecord(t *testing.T) {
	record, err := BuildAuthRecord(&Bundle{APIKey: "ak.sk", User: UserInfo{Email: "Dev@Example.com", ID: "u-1"}})
	if err != nil {
		t.Fatalf("BuildAuthRecord: %v", err)
	}
	if record.Provider != "zai" || record.FileName != "zai-dev_at_example.com.json" || record.Label != "Dev@Example.com" {
		t.Fatalf("unexpected record identity: %+v", record)
	}
	if record.Metadata["type"] != "zai" || record.Metadata["api_key"] != "ak.sk" || record.Metadata["base_url"] != CodingPlanBaseURL {
		t.Fatalf("unexpected metadata: %+v", record.Metadata)
	}
	if record.Attributes[cliproxyauth.AttributeAPIKey] != "ak.sk" || record.AuthKind() != cliproxyauth.AuthKindOAuth {
		t.Fatalf("unexpected attributes: %+v kind=%s", record.Attributes, record.AuthKind())
	}
	if _, err = BuildAuthRecord(&Bundle{}); err == nil {
		t.Fatal("expected error for empty API key")
	}
}

func TestApplyAuthFileAttributes(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "zai", Label: "zai"}
	ApplyAuthFileAttributes(auth, map[string]any{"type": "zai", "api_key": " key-1 "})
	if auth.Attributes["base_url"] != CodingPlanBaseURL || auth.Attributes["api_key"] != "key-1" {
		t.Fatalf("unexpected attributes: %+v", auth.Attributes)
	}
	if auth.AuthKind() != cliproxyauth.AuthKindAPIKey {
		t.Fatalf("plain API-key file kind = %q", auth.AuthKind())
	}

	custom := &cliproxyauth.Auth{Provider: "zai"}
	ApplyAuthFileAttributes(custom, map[string]any{"base_url": "https://open.bigmodel.cn/api/coding/paas/v4/", "access_token": "tok", "email": "a@b.c"})
	if custom.Attributes["base_url"] != "https://open.bigmodel.cn/api/coding/paas/v4" || custom.Attributes["api_key"] != "tok" || custom.Label != "a@b.c" {
		t.Fatalf("unexpected custom attributes: %+v label=%q", custom.Attributes, custom.Label)
	}
}
