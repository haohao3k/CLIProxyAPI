package cursor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cursorwire "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// loginPageURL is the deep-link page the user opens to authorize the proxy.
	loginPageURL = "https://www.cursor.com/loginDeepControl"
	// loginControlURL exchanges an existing browser session cookie for a client token.
	loginControlURL = "https://www.cursor.com/api/auth/loginDeepCallbackControl"
	// authPollURL is polled until the user has approved the login.
	authPollURL = "https://api2.cursor.sh/auth/poll"

	// defaultPollInterval is how often the callback is checked.
	defaultPollInterval = 2 * time.Second
	// MaxPollDuration bounds how long the proxy waits for the user to approve.
	MaxPollDuration = 10 * time.Minute
	// verifierBytes is the PKCE verifier length used by the Cursor client.
	verifierBytes = 43
)

// LoginFlow holds the state of one Cursor deep-link login: the user opens
// LoginURL in a browser, approves, and the poll endpoint then returns the token.
type LoginFlow struct {
	// LoginURL is the page the user must open to authorize.
	LoginURL string
	// UUID identifies the flow to loginDeepCallbackControl and auth/poll.
	UUID string
	// Challenge is the PKCE challenge derived from Verifier.
	Challenge string
	// Verifier is the PKCE verifier presented when validating the callback.
	Verifier string
	// ExpiresIn is how long, in seconds, the flow stays valid.
	ExpiresIn int
}

// CursorAuth drives the Cursor login flow.
type CursorAuth struct {
	httpClient *http.Client
	cfg        *config.Config
}

// NewCursorAuth creates a new Cursor authentication service.
func NewCursorAuth(cfg *config.Config) *CursorAuth {
	return NewCursorAuthWithProxy(cfg, "")
}

// NewCursorAuthWithProxy creates a Cursor authentication service with a proxy
// override. proxyURL takes precedence over cfg.ProxyURL when non-empty.
func NewCursorAuthWithProxy(cfg *config.Config, proxyURL string) *CursorAuth {
	client := &http.Client{Timeout: 30 * time.Second}
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	client = util.SetProxy(&sdkCfg, client)
	return &CursorAuth{httpClient: client, cfg: cfg}
}

// StartLoginFlow builds the authorization URL the user has to open. No network
// call is made: the challenge/uuid pair is generated locally and validated later
// by PollOnce.
func (a *CursorAuth) StartLoginFlow(_ context.Context) (*LoginFlow, error) {
	raw := make([]byte, verifierBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("cursor: generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	challenge := PKCEChallenge(verifier)

	id := uuid.NewString()
	return &LoginFlow{
		LoginURL:  BuildLoginURL(challenge, id),
		UUID:      id,
		Challenge: challenge,
		Verifier:  verifier,
		ExpiresIn: int(MaxPollDuration / time.Second),
	}, nil
}

// BuildLoginURL returns the deep-link authorization page URL for a PKCE
// challenge and flow uuid.
func BuildLoginURL(challenge, flowUUID string) string {
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("uuid", flowUUID)
	q.Set("mode", "login")
	return loginPageURL + "?" + q.Encode()
}

// PKCEChallenge derives the S256 PKCE challenge for a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// PollOnce performs a single callback validation attempt. ok is false while the
// user has not completed the login yet.
func (a *CursorAuth) PollOnce(ctx context.Context, flow *LoginFlow) (*LoginResult, bool, error) {
	if flow == nil {
		return nil, false, fmt.Errorf("cursor: login flow is nil")
	}
	q := url.Values{}
	q.Set("uuid", flow.UUID)
	q.Set("verifier", flow.Verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authPollURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", cursorwire.UserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("cursor login: close poll body error: %v", errClose)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		// Cursor answers 404 while the callback has not been validated yet.
		return nil, false, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, fmt.Errorf("cursor: read poll response: %w", err)
	}
	result, err := ParsePollResponse(body)
	if err != nil {
		return nil, false, err
	}
	return result, result != nil, nil
}

// ParsePollResponse decodes an auth/poll response body. It returns nil without
// error when the body carries no access token yet.
func ParsePollResponse(body []byte) (*LoginResult, error) {
	var data struct {
		AccessToken string `json:"accessToken"`
		AuthID      string `json:"authId"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("cursor: decode poll response: %w", err)
	}
	if strings.TrimSpace(data.AccessToken) == "" {
		return nil, nil
	}
	return newLoginResult(strings.TrimSpace(data.AccessToken), strings.TrimSpace(data.AuthID)), nil
}

// WaitForAuthorization polls until the login callback has been validated, the
// context is cancelled, or the flow times out.
func (a *CursorAuth) WaitForAuthorization(ctx context.Context, flow *LoginFlow) (*CursorAuthBundle, error) {
	if flow == nil {
		return nil, fmt.Errorf("cursor: login flow is nil")
	}
	deadline := time.Now().Add(MaxPollDuration)
	if flow.ExpiresIn > 0 {
		if flowDeadline := time.Now().Add(time.Duration(flow.ExpiresIn) * time.Second); flowDeadline.Before(deadline) {
			deadline = flowDeadline
		}
	}

	ticker := time.NewTicker(defaultPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("cursor: context cancelled: %w", ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("cursor: timeout waiting for authorization")
			}
			result, ok, err := a.PollOnce(ctx, flow)
			if err != nil {
				if ctx.Err() != nil {
					return nil, fmt.Errorf("cursor: context cancelled: %w", ctx.Err())
				}
				// Network hiccups while the user is still approving must not
				// abort the whole login; keep polling until the deadline.
				log.Debugf("cursor login: poll attempt failed: %v", err)
				continue
			}
			if ok {
				return &CursorAuthBundle{Login: result}, nil
			}
		}
	}
}

// ExchangeSessionToken converts a WorkosCursorSessionToken taken from the
// cursor.com browser cookie into a client token, without the interactive page.
func (a *CursorAuth) ExchangeSessionToken(ctx context.Context, sessionToken string) (*CursorAuthBundle, error) {
	sessionToken = strings.TrimSpace(sessionToken)
	if sessionToken == "" {
		return nil, fmt.Errorf("cursor: session token is required")
	}
	flow, err := a.StartLoginFlow(ctx)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(map[string]string{"uuid": flow.UUID, "challenge": flow.Challenge})
	if err != nil {
		return nil, fmt.Errorf("cursor: encode deep control payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginControlURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", cursorwire.UserAgent)
	req.Header.Set("Cookie", "WorkosCursorSessionToken="+sessionToken)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cursor: deep control request failed: %w", err)
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if errClose := resp.Body.Close(); errClose != nil {
		log.Errorf("cursor login: close deep control body error: %v", errClose)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cursor: deep control returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	return a.WaitForAuthorization(ctx, flow)
}

// CreateTokenStorage creates a new CursorTokenStorage from an auth bundle.
func (a *CursorAuth) CreateTokenStorage(bundle *CursorAuthBundle) *CursorTokenStorage {
	storage := &CursorTokenStorage{Type: "cursor"}
	if bundle != nil && bundle.Login != nil {
		storage.AccessToken = bundle.Login.AccessToken
		storage.AuthID = bundle.Login.AuthID
		storage.Cookie = bundle.Login.Cookie
	}
	return storage
}

// newLoginResult assembles a LoginResult, deriving the Cursor cookie form.
func newLoginResult(accessToken, authID string) *LoginResult {
	result := &LoginResult{AccessToken: accessToken, AuthID: authID}
	if parts := strings.SplitN(authID, "|", 2); len(parts) > 1 && parts[1] != "" {
		result.Cookie = parts[1] + "%3A%3A" + accessToken
	} else {
		result.Cookie = accessToken
	}
	return result
}

// BuildMetadata returns the auth-file metadata persisted for a Cursor login.
func BuildMetadata(bundle *CursorAuthBundle) map[string]any {
	metadata := map[string]any{
		"type":      "cursor",
		"timestamp": time.Now().UnixMilli(),
	}
	if bundle == nil || bundle.Login == nil {
		return metadata
	}
	metadata["access_token"] = bundle.Login.AccessToken
	if bundle.Login.AuthID != "" {
		metadata["auth_id"] = bundle.Login.AuthID
	}
	if bundle.Login.Cookie != "" {
		metadata["cookie"] = bundle.Login.Cookie
	}
	return metadata
}
