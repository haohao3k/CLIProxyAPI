// Package mistral implements the Mistral Vibe browser sign-in flow.
//
// The flow mirrors the Mistral Vibe CLI (and the 9router port of it):
//
//  1. POST {base}/vibe/sign-in with a PKCE S256 code_challenge. Mistral returns
//     a process_id, a sign_in_url the user opens in a browser, a poll_url and an
//     expires_at timestamp.
//  2. GET poll_url until the status leaves "pending". HTTP 410 means the
//     session expired; "denied", "error" and "expired" are terminal failures.
//  3. When the status is "completed", POST {base}/vibe/sign-in/{process_id}/exchange
//     with the exchange_token and the PKCE code_verifier. Mistral answers with a
//     long-lived API key, which is the only credential the provider needs.
//
// The minted API key is used as a plain bearer key against
// https://api.mistral.ai/v1, so no refresh flow exists.
package mistral

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// BrowserAuthAPIBaseURL is the Mistral console API base used by Vibe sign-in.
	BrowserAuthAPIBaseURL = "https://console.mistral.ai/api"
	// SignInPath is the Vibe sign-in resource below BrowserAuthAPIBaseURL.
	SignInPath = "/vibe/sign-in"
	// BudgetURL reports the Vibe Code and API allowances of the signed-in account.
	BudgetURL = "https://api.mistral.ai/v1/billing/budget"
	// SubscriptionURL reports the subscription plan of the signed-in account.
	SubscriptionURL = "https://api.mistral.ai/v1/billing/subscription"

	// AuthMethodBrowserSignIn tags auth files minted by the browser sign-in flow.
	AuthMethodBrowserSignIn = "vibe-browser-sign-in"

	// DefaultPollInterval matches the interval used by the Vibe CLI.
	DefaultPollInterval = 3 * time.Second
	// DefaultSessionTTL applies when Mistral omits or garbles expires_at.
	DefaultSessionTTL = 10 * time.Minute
	// minSessionTTL is the shortest session lifetime honoured.
	minSessionTTL = 30 * time.Second
	// APIKeyLifetime is the nominal lifetime reported for the minted API key.
	APIKeyLifetime = 10 * 365 * 24 * time.Hour

	// httpClientTimeout bounds credential-acquisition HTTP calls.
	httpClientTimeout = 30 * time.Second
	// maxResponseBytes caps response bodies read during sign-in.
	maxResponseBytes = 1 << 20
)

var (
	// ErrSessionExpired reports that the sign-in session expired before completion.
	ErrSessionExpired = errors.New("mistral sign-in session expired")
	// ErrAccessDenied reports that the user denied, or Mistral rejected, the sign-in.
	ErrAccessDenied = errors.New("mistral sign-in denied")
)

// SignInSession is a started browser sign-in.
type SignInSession struct {
	// ProcessID identifies the sign-in process.
	ProcessID string
	// SignInURL is the page the user opens to approve the sign-in.
	SignInURL string
	// PollURL is polled for the sign-in status.
	PollURL string
	// ExpiresAt is when the sign-in session stops being valid.
	ExpiresAt time.Time
	// Interval is the delay between polls.
	Interval time.Duration
	// CodeVerifier is the PKCE verifier sent with the exchange request.
	CodeVerifier string
}

// ExpiresIn returns the remaining session lifetime in whole seconds.
func (s *SignInSession) ExpiresIn(now time.Time) int {
	if s == nil {
		return 0
	}
	remaining := int(s.ExpiresAt.Sub(now) / time.Second)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// AccountInfo is best-effort account metadata fetched after sign-in.
type AccountInfo struct {
	PlanName   string         `json:"plan_name,omitempty"`
	VibeBudget map[string]any `json:"vibe_budget,omitempty"`
	APIBudget  map[string]any `json:"api_budget,omitempty"`
}

// AuthBundle is the result of a completed sign-in.
type AuthBundle struct {
	// APIKey is the long-lived Mistral API key minted by the exchange.
	APIKey string
	// Account holds optional plan and allowance details.
	Account *AccountInfo
	// SignedInAt is when the exchange completed.
	SignedInAt time.Time
}

// MistralAuth runs the Mistral Vibe browser sign-in flow.
type MistralAuth struct {
	httpClient      *http.Client
	baseURL         string
	budgetURL       string
	subscriptionURL string
	now             func() time.Time
	sleep           func(ctx context.Context, d time.Duration) error
}

// NewMistralAuth creates a sign-in helper using config proxy settings.
func NewMistralAuth(cfg *config.Config) *MistralAuth {
	return NewMistralAuthWithProxyURL(cfg, "")
}

// NewMistralAuthWithProxyURL creates a sign-in helper with an explicit proxy URL.
func NewMistralAuthWithProxyURL(cfg *config.Config, proxyURL string) *MistralAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return NewMistralAuthWithClient(util.SetProxy(&sdkCfg, &http.Client{Timeout: httpClientTimeout}), "")
}

// NewMistralAuthWithClient creates a sign-in helper with a custom HTTP client
// and console API base URL (empty uses BrowserAuthAPIBaseURL). It exists for
// tests and embedders.
func NewMistralAuthWithClient(client *http.Client, baseURL string) *MistralAuth {
	if client == nil {
		client = &http.Client{Timeout: httpClientTimeout}
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = BrowserAuthAPIBaseURL
	}
	return &MistralAuth{
		httpClient:      client,
		baseURL:         baseURL,
		budgetURL:       BudgetURL,
		subscriptionURL: SubscriptionURL,
		now:             time.Now,
		sleep:           sleepContext,
	}
}

// SetBillingURLs overrides the billing endpoints used by FetchAccountInfo.
func (a *MistralAuth) SetBillingURLs(budgetURL, subscriptionURL string) {
	if a == nil {
		return
	}
	a.budgetURL = strings.TrimSpace(budgetURL)
	a.subscriptionURL = strings.TrimSpace(subscriptionURL)
}

// GenerateCodeVerifier returns a PKCE verifier: 64 random bytes, base64url.
func GenerateCodeVerifier() (string, error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mistral: generate code verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// CodeChallenge returns the S256 PKCE challenge for verifier.
func CodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *MistralAuth) signInURL() string {
	return a.baseURL + SignInPath
}

// StartSignIn begins a browser sign-in and returns the session to poll.
func (a *MistralAuth) StartSignIn(ctx context.Context) (*SignInSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	verifier, err := GenerateCodeVerifier()
	if err != nil {
		return nil, err
	}
	reqBody, err := json.Marshal(map[string]string{
		"code_challenge":        CodeChallenge(verifier),
		"code_challenge_method": "S256",
	})
	if err != nil {
		return nil, fmt.Errorf("mistral sign-in: encode request: %w", err)
	}
	status, body, err := a.doJSON(ctx, http.MethodPost, a.signInURL(), reqBody)
	if err != nil {
		return nil, fmt.Errorf("mistral sign-in: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("mistral sign-in: failed to start browser sign-in (status %d): %s", status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		ProcessID string `json:"process_id"`
		SignInURL string `json:"sign_in_url"`
		PollURL   string `json:"poll_url"`
		ExpiresAt string `json:"expires_at"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("mistral sign-in: parse response: %w", err)
	}
	payload.ProcessID = strings.TrimSpace(payload.ProcessID)
	payload.SignInURL = strings.TrimSpace(payload.SignInURL)
	payload.PollURL = strings.TrimSpace(payload.PollURL)
	if payload.ProcessID == "" || payload.SignInURL == "" || payload.PollURL == "" {
		return nil, fmt.Errorf("mistral sign-in: browser sign-in response was malformed")
	}
	if err = a.validateConsoleURL(payload.PollURL); err != nil {
		return nil, fmt.Errorf("mistral sign-in: poll_url: %w", err)
	}

	now := a.now()
	expiresAt := now.Add(DefaultSessionTTL)
	if parsed, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(payload.ExpiresAt)); errParse == nil {
		expiresAt = parsed
		if expiresAt.Sub(now) < minSessionTTL {
			expiresAt = now.Add(minSessionTTL)
		}
	}
	return &SignInSession{
		ProcessID:    payload.ProcessID,
		SignInURL:    payload.SignInURL,
		PollURL:      payload.PollURL,
		ExpiresAt:    expiresAt,
		Interval:     DefaultPollInterval,
		CodeVerifier: verifier,
	}, nil
}

// PollOnce polls the sign-in status once. It returns the minted API key once
// the sign-in completed, ("", true, nil) while it is still pending, or an
// error for terminal failures (ErrSessionExpired, ErrAccessDenied, exchange
// failures, malformed responses). Transport errors are reported as pending so
// a transient network failure does not abort the sign-in.
func (a *MistralAuth) PollOnce(ctx context.Context, session *SignInSession) (string, bool, error) {
	if session == nil {
		return "", false, fmt.Errorf("mistral sign-in: session is nil")
	}
	if strings.TrimSpace(session.CodeVerifier) == "" {
		return "", false, fmt.Errorf("%w: missing PKCE code verifier", ErrAccessDenied)
	}
	pollURL := strings.TrimSpace(session.PollURL)
	if pollURL == "" {
		pollURL = a.signInURL() + "/poll/" + url.PathEscape(session.ProcessID)
	}
	status, body, err := a.doJSON(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		log.Debugf("mistral sign-in: poll request failed, retrying: %v", err)
		return "", true, nil
	}
	if status == http.StatusGone {
		return "", false, ErrSessionExpired
	}
	var payload struct {
		Status        string `json:"status"`
		Message       string `json:"message"`
		ExchangeToken string `json:"exchange_token"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return "", false, fmt.Errorf("mistral sign-in: malformed poll response (status %d)", status)
	}
	switch strings.ToLower(strings.TrimSpace(payload.Status)) {
	case "pending":
		return "", true, nil
	case "expired":
		return "", false, withMessage(ErrSessionExpired, payload.Message)
	case "denied", "error":
		message := strings.TrimSpace(payload.Message)
		if message == "" {
			message = "mistral sign-in " + strings.ToLower(strings.TrimSpace(payload.Status))
		}
		return "", false, withMessage(ErrAccessDenied, message)
	case "completed":
		if strings.TrimSpace(payload.ExchangeToken) == "" {
			return "", true, nil
		}
		apiKey, errExchange := a.Exchange(ctx, session.ProcessID, payload.ExchangeToken, session.CodeVerifier)
		if errExchange != nil {
			return "", false, errExchange
		}
		return apiKey, false, nil
	default:
		return "", true, nil
	}
}

// Exchange trades a completed sign-in's exchange token for an API key.
func (a *MistralAuth) Exchange(ctx context.Context, processID, exchangeToken, codeVerifier string) (string, error) {
	reqBody, err := json.Marshal(map[string]string{
		"exchange_token": exchangeToken,
		"code_verifier":  codeVerifier,
	})
	if err != nil {
		return "", fmt.Errorf("mistral exchange: encode request: %w", err)
	}
	exchangeURL := a.signInURL() + "/" + url.PathEscape(strings.TrimSpace(processID)) + "/exchange"
	status, body, err := a.doJSON(ctx, http.MethodPost, exchangeURL, reqBody)
	if err != nil {
		return "", fmt.Errorf("mistral exchange: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("mistral exchange failed (status %d): %s", status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		APIKey string `json:"api_key"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("mistral exchange: parse response: %w", err)
	}
	apiKey := strings.TrimSpace(payload.APIKey)
	if apiKey == "" {
		return "", fmt.Errorf("mistral exchange: no api_key returned")
	}
	return apiKey, nil
}

// WaitForAPIKey polls until the sign-in completes, fails, expires or ctx ends.
// On success it also fetches best-effort account details.
func (a *MistralAuth) WaitForAPIKey(ctx context.Context, session *SignInSession) (*AuthBundle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if session == nil {
		return nil, fmt.Errorf("mistral sign-in: session is nil")
	}
	interval := session.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	for {
		if !session.ExpiresAt.IsZero() && !a.now().Before(session.ExpiresAt) {
			return nil, ErrSessionExpired
		}
		if errSleep := a.sleep(ctx, interval); errSleep != nil {
			return nil, errSleep
		}
		apiKey, pending, err := a.PollOnce(ctx, session)
		if err != nil {
			return nil, err
		}
		if pending {
			continue
		}
		bundle := &AuthBundle{APIKey: apiKey, SignedInAt: a.now()}
		account, errAccount := a.FetchAccountInfo(ctx, apiKey)
		if errAccount != nil {
			log.Debugf("mistral sign-in: account info unavailable: %v", errAccount)
		} else {
			bundle.Account = account
		}
		return bundle, nil
	}
}

// FetchAccountInfo reads the subscription plan and allowances for apiKey.
// The budget endpoint is required; the subscription endpoint is optional.
func (a *MistralAuth) FetchAccountInfo(ctx context.Context, apiKey string) (*AccountInfo, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("mistral account: api key is empty")
	}
	info := &AccountInfo{}
	budget, errBudget := a.getWithKey(ctx, a.budgetURL, apiKey)
	if errBudget != nil {
		return nil, errBudget
	}
	var budgetPayload struct {
		VibeBudget map[string]any `json:"vibe_budget"`
		APIBudget  map[string]any `json:"api_budget"`
	}
	if err := json.Unmarshal(budget, &budgetPayload); err != nil {
		return nil, fmt.Errorf("mistral account: parse budget: %w", err)
	}
	info.VibeBudget = budgetPayload.VibeBudget
	info.APIBudget = budgetPayload.APIBudget
	if sub, errSub := a.getWithKey(ctx, a.subscriptionURL, apiKey); errSub == nil {
		var subPayload struct {
			PlanName string `json:"plan_name"`
		}
		if json.Unmarshal(sub, &subPayload) == nil {
			info.PlanName = strings.TrimSpace(subPayload.PlanName)
		}
	}
	return info, nil
}

func (a *MistralAuth) getWithKey(ctx context.Context, endpoint, apiKey string) ([]byte, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, fmt.Errorf("mistral account: endpoint is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("mistral account: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mistral account: request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("mistral account: close response body error: %v", errClose)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("mistral account: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mistral account: %s returned status %d", endpoint, resp.StatusCode)
	}
	return body, nil
}

func (a *MistralAuth) doJSON(ctx context.Context, method, endpoint string, body []byte) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("mistral sign-in: close response body error: %v", errClose)
		}
	}()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// validateConsoleURL accepts https URLs on mistral.ai, or URLs on the same
// host as the configured console base (used by tests and custom deployments),
// so a tampered sign-in response cannot redirect polling elsewhere.
func (a *MistralAuth) validateConsoleURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if base, errBase := url.Parse(a.baseURL); errBase == nil && strings.EqualFold(base.Host, parsed.Host) {
		return nil
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("must use https: %q", rawURL)
	}
	if host != "mistral.ai" && !strings.HasSuffix(host, ".mistral.ai") {
		return fmt.Errorf("host %q is not on mistral.ai", host)
	}
	return nil
}

// CredentialFileName returns a stable auth file name for apiKey, so signing in
// again with the same key overwrites its file instead of adding a duplicate.
func CredentialFileName(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return "mistral-vibe-" + hex.EncodeToString(sum[:])[:12] + ".json"
}

// Label returns a display label for a signed-in account.
func Label(account *AccountInfo) string {
	if account != nil && strings.TrimSpace(account.PlanName) != "" {
		return "Mistral (" + strings.TrimSpace(account.PlanName) + ")"
	}
	return "Mistral Subscription"
}

// BuildMetadata returns the auth file metadata for a completed sign-in. The
// record is a "mistral" auth file that the file synthesizer routes through the
// OpenAI-compatibility executor.
func BuildMetadata(bundle *AuthBundle) map[string]any {
	if bundle == nil {
		return nil
	}
	signedInAt := bundle.SignedInAt
	if signedInAt.IsZero() {
		signedInAt = time.Now()
	}
	metadata := map[string]any{
		"type":         util.MistralProvider,
		"api_key":      bundle.APIKey,
		"base_url":     util.MistralDefaultBaseURL,
		"compat_name":  util.MistralProvider,
		"label":        Label(bundle.Account),
		"auth_method":  AuthMethodBrowserSignIn,
		"signed_in_at": signedInAt.UTC().Format(time.RFC3339),
		"timestamp":    signedInAt.UnixMilli(),
	}
	if bundle.Account != nil {
		if bundle.Account.PlanName != "" {
			metadata["plan_name"] = bundle.Account.PlanName
		}
		if len(bundle.Account.VibeBudget) > 0 {
			metadata["vibe_budget"] = bundle.Account.VibeBudget
		}
		if len(bundle.Account.APIBudget) > 0 {
			metadata["api_budget"] = bundle.Account.APIBudget
		}
	}
	return metadata
}

func withMessage(base error, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return base
	}
	return fmt.Errorf("%w: %s", base, message)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
