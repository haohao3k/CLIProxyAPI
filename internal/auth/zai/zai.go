// Package zai implements the Z.ai GLM Coding Plan login flow.
//
// The flow mirrors the official ZCode CLI (cli-oauth.ts + coding-plan-api-key.ts):
//
//  1. POST {cli}/oauth/cli/init with a client-generated Bearer poll token
//     returns a server-generated authorize URL and a flow id.
//  2. The user opens the authorize URL and signs in with the Z.ai account.
//  3. GET {cli}/oauth/cli/poll/{flow_id} until status is "ready"; the ready
//     payload carries the Z.ai OAuth access token.
//  4. POST {api}/api/auth/z/login exchanges the OAuth token for a business JWT.
//  5. The business JWT provisions (or reuses) the coding-plan API key named
//     "zcode-api-key" and copies its secret, yielding "apiKey.secretKey".
//
// The resulting coding-plan API key is long-lived and is the only credential
// persisted. Z.ai exposes no refresh grant for this flow, so expiry means
// re-login (same as the official CLI).
package zai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	// ProviderKey is the provider identifier used for auth files and routing.
	ProviderKey = "zai"

	// CodingPlanBaseURL is the OpenAI-compatible coding-plan endpoint that
	// accepts the coding-plan API key (POST {base}/chat/completions).
	CodingPlanBaseURL = "https://api.z.ai/api/coding/paas/v4"
	// AnthropicBaseURL is the Anthropic-compatible coding-plan endpoint
	// (POST {base}/v1/messages, x-api-key auth).
	AnthropicBaseURL = "https://api.z.ai/api/anthropic"
	// QuotaURL reports the coding-plan usage windows for an API key.
	QuotaURL = "https://api.z.ai/api/monitor/usage/quota/limit"

	// DefaultCLIBaseURL hosts the server-mediated CLI OAuth flow.
	DefaultCLIBaseURL = "https://zcode.z.ai/api/v1"
	// DefaultBusinessLoginURL exchanges the Z.ai OAuth token for a business JWT.
	DefaultBusinessLoginURL = "https://api.z.ai/api/auth/z/login"
	// DefaultBizBaseURL hosts the business (organization/API key) endpoints.
	DefaultBizBaseURL = "https://api.z.ai/api/biz"

	// PlanAPIKeyName matches the official client so re-login reuses the key.
	PlanAPIKeyName = "zcode-api-key"

	defaultPollInterval      = 3 * time.Second
	minPollInterval          = time.Second
	defaultFlowLifetime      = 10 * time.Minute
	maxConsecutivePollErrors = 5
	httpTimeout              = 30 * time.Second
	maxResponseBytes         = 1 << 20
)

// UserInfo describes the authenticated Z.ai account.
type UserInfo struct {
	ID    string
	Name  string
	Email string
}

// Identity returns the best human-readable account identifier.
func (u UserInfo) Identity() string {
	for _, value := range []string{u.Email, u.Name, u.ID} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// Flow describes one pending CLI OAuth login.
type Flow struct {
	FlowID       string
	PollToken    string
	AuthorizeURL string
	ExpiresAt    time.Time
	Interval     time.Duration
}

// ExpiresIn returns the remaining lifetime of the flow in whole seconds.
func (f *Flow) ExpiresIn() int {
	if f == nil || f.ExpiresAt.IsZero() {
		return 0
	}
	remaining := int(time.Until(f.ExpiresAt).Seconds())
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Authorization is the result of an approved CLI OAuth flow.
type Authorization struct {
	ZCodeToken string
	OAuthToken string
	User       UserInfo
}

// Bundle is the final login result.
type Bundle struct {
	// APIKey is the coding-plan credential ("apiKey.secretKey").
	APIKey  string
	BaseURL string
	User    UserInfo
}

// Service talks to the Z.ai login endpoints.
type Service struct {
	client           *http.Client
	cliBaseURL       string
	businessLoginURL string
	bizBaseURL       string
	now              func() time.Time
}

// NewService builds a service using the given HTTP client (nil = default).
func NewService(client *http.Client) *Service {
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	return &Service{
		client:           client,
		cliBaseURL:       DefaultCLIBaseURL,
		businessLoginURL: DefaultBusinessLoginURL,
		bizBaseURL:       DefaultBizBaseURL,
		now:              time.Now,
	}
}

// NewServiceWithConfig builds a service whose HTTP client honors the
// configured proxy settings.
func NewServiceWithConfig(cfg *config.Config) *Service {
	client := &http.Client{Timeout: httpTimeout}
	if cfg != nil {
		client = util.SetProxy(&cfg.SDKConfig, client)
	}
	return NewService(client)
}

// SetEndpoints overrides the upstream endpoints (used by tests). Empty values
// keep the current setting.
func (s *Service) SetEndpoints(cliBaseURL, businessLoginURL, bizBaseURL string) {
	if s == nil {
		return
	}
	if v := strings.TrimRight(strings.TrimSpace(cliBaseURL), "/"); v != "" {
		s.cliBaseURL = v
	}
	if v := strings.TrimSpace(businessLoginURL); v != "" {
		s.businessLoginURL = v
	}
	if v := strings.TrimRight(strings.TrimSpace(bizBaseURL), "/"); v != "" {
		s.bizBaseURL = v
	}
}

// StartFlow begins a CLI OAuth login and returns the authorize URL.
func (s *Service) StartFlow(ctx context.Context) (*Flow, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("zai: service unavailable")
	}
	clientToken, errToken := newPollToken()
	if errToken != nil {
		return nil, fmt.Errorf("zai: generate poll token: %w", errToken)
	}
	body, _ := json.Marshal(map[string]string{"provider": ProviderKey})
	var data struct {
		FlowID          string      `json:"flow_id"`
		PollToken       string      `json:"poll_token"`
		AuthorizeURL    string      `json:"authorize_url"`
		ExpiresAt       json.Number `json:"expires_at"`
		PollIntervalSec json.Number `json:"poll_interval_sec"`
	}
	if errDo := s.doEnvelope(ctx, http.MethodPost, s.cliBaseURL+"/oauth/cli/init", clientToken, body, &data); errDo != nil {
		return nil, fmt.Errorf("zai: start login: %w", errDo)
	}
	flowID := strings.TrimSpace(data.FlowID)
	authorizeURL := strings.TrimSpace(data.AuthorizeURL)
	if flowID == "" || authorizeURL == "" {
		return nil, fmt.Errorf("zai: start login: response is missing flow_id or authorize_url")
	}
	// The server-issued poll token is authoritative when present.
	pollToken := strings.TrimSpace(data.PollToken)
	if pollToken == "" {
		pollToken = clientToken
	}
	interval := defaultPollInterval
	if seconds, errParse := data.PollIntervalSec.Int64(); errParse == nil && seconds > 0 {
		interval = time.Duration(seconds) * time.Second
	}
	if interval < minPollInterval {
		interval = minPollInterval
	}
	now := s.now()
	expiresAt := now.Add(defaultFlowLifetime)
	if parsed := parseEpoch(data.ExpiresAt); !parsed.IsZero() && parsed.After(now) {
		expiresAt = parsed
	}
	return &Flow{
		FlowID:       flowID,
		PollToken:    pollToken,
		AuthorizeURL: authorizeURL,
		ExpiresAt:    expiresAt,
		Interval:     interval,
	}, nil
}

// errPending signals that the user has not approved the flow yet.
var errPending = fmt.Errorf("zai: authorization pending")

// PollOnce checks the flow status once. It returns (nil, nil) while the user
// has not yet approved the flow.
func (s *Service) PollOnce(ctx context.Context, flow *Flow) (*Authorization, error) {
	authz, err := s.poll(ctx, flow)
	if err == errPending {
		return nil, nil
	}
	return authz, err
}

func (s *Service) poll(ctx context.Context, flow *Flow) (*Authorization, error) {
	if s == nil || flow == nil || strings.TrimSpace(flow.FlowID) == "" {
		return nil, fmt.Errorf("zai: login flow is not initialized")
	}
	var data map[string]any
	endpoint := s.cliBaseURL + "/oauth/cli/poll/" + url.PathEscape(flow.FlowID)
	if errDo := s.doEnvelope(ctx, http.MethodGet, endpoint, flow.PollToken, nil, &data); errDo != nil {
		return nil, errDo
	}
	switch status := strings.ToLower(strings.TrimSpace(stringField(data, "status"))); status {
	case "", "pending":
		return nil, errPending
	case "failed", "denied", "expired", "cancelled", "canceled":
		return nil, &terminalError{msg: fmt.Sprintf("zai: authorization %s", status)}
	case "ready":
		return parseReady(data)
	default:
		return nil, &terminalError{msg: fmt.Sprintf("zai: unexpected poll status %q", status)}
	}
}

// terminalError marks poll failures that must not be retried.
type terminalError struct{ msg string }

func (e *terminalError) Error() string { return e.msg }

// parseReady extracts tokens from the ready payload. The Z.ai OAuth tokens are
// nested under data[provider] ({"zai":{"access_token":...}}); top-level fields
// are accepted as a fallback against payload drift.
func parseReady(data map[string]any) (*Authorization, error) {
	providerData, _ := data[ProviderKey].(map[string]any)
	oauthToken := firstNonEmpty(
		stringField(providerData, "access_token"),
		stringField(providerData, "accessToken"),
		stringField(data, "accessToken"),
		stringField(data, "access_token"),
	)
	if oauthToken == "" {
		return nil, &terminalError{msg: "zai: authorization response is missing the access token"}
	}
	user, _ := data["user"].(map[string]any)
	return &Authorization{
		ZCodeToken: stringField(data, "token"),
		OAuthToken: oauthToken,
		User: UserInfo{
			ID:    firstNonEmpty(stringField(user, "user_id"), stringField(user, "id")),
			Name:  firstNonEmpty(stringField(user, "name"), stringField(user, "username")),
			Email: stringField(user, "email"),
		},
	}, nil
}

// WaitForAuthorization polls until the flow is approved, rejected, expired, or
// ctx is cancelled.
func (s *Service) WaitForAuthorization(ctx context.Context, flow *Flow) (*Authorization, error) {
	if s == nil || flow == nil {
		return nil, fmt.Errorf("zai: login flow is not initialized")
	}
	interval := flow.Interval
	if interval < minPollInterval {
		interval = defaultPollInterval
	}
	deadline := flow.ExpiresAt
	if deadline.IsZero() {
		deadline = s.now().Add(defaultFlowLifetime)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	consecutiveErrors := 0
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("zai: waiting for authorization: %w", ctx.Err())
		case <-timer.C:
		}
		if s.now().After(deadline) {
			return nil, fmt.Errorf("zai: authorization timed out")
		}
		authz, err := s.poll(ctx, flow)
		switch {
		case err == nil:
			return authz, nil
		case err == errPending:
			consecutiveErrors = 0
		default:
			if _, terminal := err.(*terminalError); terminal {
				return nil, err
			}
			consecutiveErrors++
			if consecutiveErrors >= maxConsecutivePollErrors {
				return nil, err
			}
		}
		timer.Reset(interval)
	}
}

// ExchangeBusinessToken converts the Z.ai OAuth token into a business JWT.
func (s *Service) ExchangeBusinessToken(ctx context.Context, oauthToken string) (string, error) {
	oauthToken = strings.TrimSpace(oauthToken)
	if oauthToken == "" {
		return "", fmt.Errorf("zai: business login is missing the OAuth token")
	}
	body, _ := json.Marshal(map[string]string{"token": oauthToken})
	var data struct {
		AccessToken    string `json:"access_token"`
		AccessTokenAlt string `json:"accessToken"`
		Token          string `json:"token"`
	}
	if errDo := s.doEnvelope(ctx, http.MethodPost, s.businessLoginURL, "", body, &data); errDo != nil {
		return "", fmt.Errorf("zai: business login: %w", errDo)
	}
	token := firstNonEmpty(data.AccessToken, data.AccessTokenAlt, data.Token)
	if token == "" {
		return "", fmt.Errorf("zai: business login response is missing access_token")
	}
	return token, nil
}

// MintCodingPlanKey provisions or reuses the "zcode-api-key" coding-plan API
// key and returns the combined "apiKey.secretKey" credential.
func (s *Service) MintCodingPlanKey(ctx context.Context, businessToken string) (string, error) {
	businessToken = strings.TrimSpace(businessToken)
	if businessToken == "" {
		return "", fmt.Errorf("zai: API key mint is missing the business token")
	}
	var info customerInfo
	if errInfo := s.doEnvelope(ctx, http.MethodGet, s.bizBaseURL+"/customer/getCustomerInfo", businessToken, nil, &info); errInfo != nil {
		return "", fmt.Errorf("zai: customer info: %w", errInfo)
	}
	orgID, projectID, errSelect := selectOrgProject(&info)
	if errSelect != nil {
		return "", errSelect
	}
	keysURL := fmt.Sprintf("%s/v1/organization/%s/projects/%s/api_keys", s.bizBaseURL, url.PathEscape(orgID), url.PathEscape(projectID))

	apiKey := ""
	var listed []struct {
		Name   string `json:"name"`
		APIKey string `json:"apiKey"`
	}
	if errList := s.doEnvelope(ctx, http.MethodGet, keysURL, businessToken, nil, &listed); errList == nil {
		for _, key := range listed {
			if key.Name == PlanAPIKeyName && strings.TrimSpace(key.APIKey) != "" {
				apiKey = strings.TrimSpace(key.APIKey)
				break
			}
		}
	}
	if apiKey == "" {
		body, _ := json.Marshal(map[string]string{"name": PlanAPIKeyName})
		var created struct {
			APIKey string `json:"apiKey"`
		}
		if errCreate := s.doEnvelope(ctx, http.MethodPost, keysURL, businessToken, body, &created); errCreate != nil {
			return "", fmt.Errorf("zai: create API key: %w", errCreate)
		}
		apiKey = strings.TrimSpace(created.APIKey)
	}
	if apiKey == "" {
		return "", fmt.Errorf("zai: API key response is missing apiKey")
	}

	var copied struct {
		SecretKey string `json:"secretKey"`
	}
	if errCopy := s.doEnvelope(ctx, http.MethodGet, keysURL+"/copy/"+url.PathEscape(apiKey), businessToken, nil, &copied); errCopy != nil {
		return "", fmt.Errorf("zai: copy API key secret: %w", errCopy)
	}
	secret := strings.TrimSpace(copied.SecretKey)
	if secret == "" {
		return "", fmt.Errorf("zai: API key copy response is missing secretKey")
	}
	return apiKey + "." + secret, nil
}

// CompleteLogin turns an approved authorization into a coding-plan credential.
func (s *Service) CompleteLogin(ctx context.Context, authz *Authorization) (*Bundle, error) {
	if authz == nil {
		return nil, fmt.Errorf("zai: authorization is missing")
	}
	businessToken, errBusiness := s.ExchangeBusinessToken(ctx, authz.OAuthToken)
	if errBusiness != nil {
		return nil, errBusiness
	}
	apiKey, errMint := s.MintCodingPlanKey(ctx, businessToken)
	if errMint != nil {
		return nil, errMint
	}
	return &Bundle{APIKey: apiKey, BaseURL: CodingPlanBaseURL, User: authz.User}, nil
}

type customerInfo struct {
	Organizations []struct {
		OrganizationID   string `json:"organizationId"`
		OrganizationName string `json:"organizationName"`
		Projects         []struct {
			ProjectID   string `json:"projectId"`
			ProjectName string `json:"projectName"`
			ProjectType any    `json:"projectType"`
		} `json:"projects"`
	} `json:"organizations"`
}

// selectOrgProject prefers the account's default organization and its
// default personal project; team projects (projectType "2") are skipped.
func selectOrgProject(info *customerInfo) (string, string, error) {
	type candidate struct {
		orgID, orgName string
		projects       []struct{ id, name string }
	}
	var candidates []candidate
	if info != nil {
		for _, org := range info.Organizations {
			orgID := strings.TrimSpace(org.OrganizationID)
			if orgID == "" {
				continue
			}
			c := candidate{orgID: orgID, orgName: org.OrganizationName}
			for _, project := range org.Projects {
				projectID := strings.TrimSpace(project.ProjectID)
				if projectID == "" || strings.TrimSpace(fmt.Sprint(project.ProjectType)) == "2" {
					continue
				}
				c.projects = append(c.projects, struct{ id, name string }{projectID, project.ProjectName})
			}
			if len(c.projects) > 0 {
				candidates = append(candidates, c)
			}
		}
	}
	if len(candidates) == 0 {
		return "", "", fmt.Errorf("zai: account has no organization project (is a GLM Coding Plan active?)")
	}
	chosen := candidates[0]
	for _, c := range candidates {
		if isDefaultName(c.orgName) {
			chosen = c
			break
		}
	}
	project := chosen.projects[0]
	for _, p := range chosen.projects {
		if isDefaultName(p.name) {
			project = p
			break
		}
	}
	return chosen.orgID, project.id, nil
}

func isDefaultName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(normalized, "默认机构") || strings.Contains(normalized, "默认项目") || normalized == "default"
}

// doEnvelope executes a request and decodes the data member of the
// {code,msg,data} envelope into target. Bare (non-envelope) bodies are decoded
// directly.
func (s *Service) doEnvelope(ctx context.Context, method, endpoint, bearer string, body []byte, target any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, errReq := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if errReq != nil {
		return errReq
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer = strings.TrimSpace(bearer); bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, errDo := s.client.Do(req)
	if errDo != nil {
		return fmt.Errorf("request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if errRead != nil {
		return fmt.Errorf("read response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Msg     string          `json:"msg"`
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if errJSON := json.Unmarshal(raw, &envelope); errJSON != nil {
		return fmt.Errorf("decode response: %w", errJSON)
	}
	isEnvelope := len(envelope.Code) > 0 || envelope.Success != nil || len(envelope.Data) > 0
	if !isEnvelope {
		if errDecode := json.Unmarshal(raw, target); errDecode != nil {
			return fmt.Errorf("decode response: %w", errDecode)
		}
		return nil
	}
	if !isSuccessCode(envelope.Code) || (envelope.Success != nil && !*envelope.Success) {
		if msg := strings.TrimSpace(envelope.Msg); msg != "" {
			return fmt.Errorf("rejected: %s", msg)
		}
		return fmt.Errorf("rejected with code %s", strings.Trim(string(envelope.Code), `"`))
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	if errDecode := json.Unmarshal(envelope.Data, target); errDecode != nil {
		return fmt.Errorf("decode response data: %w", errDecode)
	}
	return nil
}

func isSuccessCode(raw json.RawMessage) bool {
	code := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	switch code {
	case "", "null", "0", "200":
		return true
	default:
		return false
	}
}

// parseEpoch accepts absolute epochs in seconds or milliseconds.
func parseEpoch(value json.Number) time.Time {
	raw, errParse := value.Int64()
	if errParse != nil || raw <= 0 {
		return time.Time{}
	}
	if raw > 1e12 {
		return time.UnixMilli(raw)
	}
	return time.Unix(raw, 0)
}

func stringField(data map[string]any, key string) string {
	if data == nil {
		return ""
	}
	if value, ok := data[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func snippet(data []byte) string {
	text := strings.TrimSpace(string(data))
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return text
}

func newPollToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
