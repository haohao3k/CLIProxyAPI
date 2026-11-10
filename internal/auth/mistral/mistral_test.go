package mistral

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

type fakeConsole struct {
	t *testing.T

	mu             sync.Mutex
	startBody      map[string]string
	exchangeBody   map[string]string
	exchangePath   string
	pollStatuses   []string // consumed per poll; last one repeats
	pollHTTPStatus int
	exchangeStatus int
	exchangeAPIKey string
	budgetAuth     string
	polls          int
	server         *httptest.Server
}

func newFakeConsole(t *testing.T) *fakeConsole {
	t.Helper()
	fc := &fakeConsole{t: t, exchangeStatus: http.StatusOK, exchangeAPIKey: "secret-vibe-api-key"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/vibe/sign-in", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("start content-type = %q", ct)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode start body: %v", err)
		}
		fc.mu.Lock()
		fc.startBody = body
		fc.mu.Unlock()
		writeJSON(w, map[string]any{
			"process_id":  "proc-123",
			"sign_in_url": "https://console.mistral.ai/codestral/cli/authenticate?process_id=proc-123",
			"poll_url":    fc.server.URL + "/api/vibe/sign-in/poll/proc-123",
			"expires_at":  time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/api/vibe/sign-in/poll/proc-123", func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.polls++
		status := "pending"
		if len(fc.pollStatuses) > 0 {
			status = fc.pollStatuses[0]
			if len(fc.pollStatuses) > 1 {
				fc.pollStatuses = fc.pollStatuses[1:]
			}
		}
		httpStatus := fc.pollHTTPStatus
		fc.mu.Unlock()
		if httpStatus != 0 {
			w.WriteHeader(httpStatus)
			return
		}
		payload := map[string]any{"status": status}
		if status == "completed" {
			payload["exchange_token"] = "exchange-tok-xyz"
		}
		if status == "denied" {
			payload["message"] = "user cancelled"
		}
		writeJSON(w, payload)
	})
	mux.HandleFunc("/api/vibe/sign-in/proc-123/exchange", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode exchange body: %v", err)
		}
		fc.mu.Lock()
		fc.exchangeBody = body
		fc.exchangePath = r.URL.Path
		status := fc.exchangeStatus
		key := fc.exchangeAPIKey
		fc.mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		writeJSON(w, map[string]any{"api_key": key})
	})
	mux.HandleFunc("/v1/billing/budget", func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.budgetAuth = r.Header.Get("Authorization")
		fc.mu.Unlock()
		writeJSON(w, map[string]any{
			"vibe_budget": map[string]any{"initial_budget": 150, "usage_percentage": 10},
			"api_budget":  map[string]any{"initial_budget": 15, "usage_percentage": 0},
		})
	})
	mux.HandleFunc("/v1/billing/subscription", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"plan_name": "EDU"})
	})
	fc.server = httptest.NewServer(mux)
	t.Cleanup(fc.server.Close)
	return fc
}

func (fc *fakeConsole) auth() *MistralAuth {
	a := NewMistralAuthWithClient(fc.server.Client(), fc.server.URL+"/api")
	a.SetBillingURLs(fc.server.URL+"/v1/billing/budget", fc.server.URL+"/v1/billing/subscription")
	a.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return a
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func TestCodeChallengeIsS256OfVerifier(t *testing.T) {
	// RFC 7636 appendix B test vector.
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	if got, want := CodeChallenge(verifier), "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"; got != want {
		t.Fatalf("CodeChallenge = %q, want %q", got, want)
	}
	v, err := GenerateCodeVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) < 43 || strings.ContainsAny(v, "+/=") {
		t.Fatalf("verifier %q is not a base64url PKCE verifier", v)
	}
}

func TestStartSignInSendsPKCEAndParsesSession(t *testing.T) {
	fc := newFakeConsole(t)
	session, err := fc.auth().StartSignIn(context.Background())
	if err != nil {
		t.Fatalf("StartSignIn: %v", err)
	}
	if session.ProcessID != "proc-123" {
		t.Errorf("process id = %q", session.ProcessID)
	}
	if !strings.HasPrefix(session.SignInURL, "https://console.mistral.ai/codestral/cli/authenticate") {
		t.Errorf("sign-in url = %q", session.SignInURL)
	}
	if session.PollURL != fc.server.URL+"/api/vibe/sign-in/poll/proc-123" {
		t.Errorf("poll url = %q", session.PollURL)
	}
	if session.Interval != DefaultPollInterval {
		t.Errorf("interval = %s", session.Interval)
	}
	if got := session.ExpiresIn(time.Now()); got < 500 || got > 600 {
		t.Errorf("expires in = %d", got)
	}
	if fc.startBody["code_challenge_method"] != "S256" {
		t.Errorf("code_challenge_method = %q", fc.startBody["code_challenge_method"])
	}
	if fc.startBody["code_challenge"] != CodeChallenge(session.CodeVerifier) {
		t.Errorf("code_challenge does not match the verifier")
	}
}

func TestStartSignInRejectsMalformedAndForeignPollURL(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"missing poll_url": {"process_id": "p", "sign_in_url": "https://x"},
		"foreign poll_url": {"process_id": "p", "sign_in_url": "https://x", "poll_url": "https://evil.example.com/poll/p"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, payload) }))
			defer srv.Close()
			if _, err := NewMistralAuthWithClient(srv.Client(), srv.URL).StartSignIn(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestStartSignInDefaultsExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"process_id": "p", "sign_in_url": "https://x", "poll_url": "https://console.mistral.ai/api/vibe/sign-in/poll/p", "expires_at": "garbage"})
	}))
	defer srv.Close()
	a := NewMistralAuthWithClient(srv.Client(), srv.URL)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return fixed }
	session, err := a.StartSignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := session.ExpiresIn(fixed); got != int(DefaultSessionTTL/time.Second) {
		t.Fatalf("expires in = %d, want %d", got, int(DefaultSessionTTL/time.Second))
	}
}

func TestStartSignInReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := NewMistralAuthWithClient(srv.Client(), srv.URL).StartSignIn(context.Background())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
}

func TestPollOnceStates(t *testing.T) {
	fc := newFakeConsole(t)
	a := fc.auth()
	session, err := a.StartSignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	fc.pollStatuses = []string{"pending"}
	if key, pending, errPoll := a.PollOnce(context.Background(), session); errPoll != nil || !pending || key != "" {
		t.Fatalf("pending: key=%q pending=%v err=%v", key, pending, errPoll)
	}

	fc.pollStatuses = []string{"denied"}
	if _, _, errPoll := a.PollOnce(context.Background(), session); !errors.Is(errPoll, ErrAccessDenied) || !strings.Contains(errPoll.Error(), "user cancelled") {
		t.Fatalf("denied: err=%v", errPoll)
	}

	fc.pollStatuses = []string{"expired"}
	if _, _, errPoll := a.PollOnce(context.Background(), session); !errors.Is(errPoll, ErrSessionExpired) {
		t.Fatalf("expired: err=%v", errPoll)
	}

	fc.pollHTTPStatus = http.StatusGone
	if _, _, errPoll := a.PollOnce(context.Background(), session); !errors.Is(errPoll, ErrSessionExpired) {
		t.Fatalf("410: err=%v", errPoll)
	}
	fc.pollHTTPStatus = 0

	if _, _, errPoll := a.PollOnce(context.Background(), &SignInSession{ProcessID: "proc-123", PollURL: session.PollURL}); !errors.Is(errPoll, ErrAccessDenied) {
		t.Fatalf("missing verifier: err=%v", errPoll)
	}
}

func TestPollOnceTreatsTransportErrorAsPending(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	pollURL := srv.URL + "/poll"
	srv.Close()
	a := NewMistralAuthWithClient(&http.Client{Timeout: time.Second}, srv.URL)
	_, pending, err := a.PollOnce(context.Background(), &SignInSession{ProcessID: "p", PollURL: pollURL, CodeVerifier: "v"})
	if err != nil || !pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}

func TestWaitForAPIKeyExchangesAndFetchesAccount(t *testing.T) {
	fc := newFakeConsole(t)
	a := fc.auth()
	session, err := a.StartSignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fc.pollStatuses = []string{"pending", "pending", "completed"}

	bundle, err := a.WaitForAPIKey(context.Background(), session)
	if err != nil {
		t.Fatalf("WaitForAPIKey: %v", err)
	}
	if bundle.APIKey != "secret-vibe-api-key" {
		t.Errorf("api key = %q", bundle.APIKey)
	}
	if fc.polls != 3 {
		t.Errorf("polls = %d, want 3", fc.polls)
	}
	if fc.exchangePath != "/api/vibe/sign-in/proc-123/exchange" {
		t.Errorf("exchange path = %q", fc.exchangePath)
	}
	if fc.exchangeBody["exchange_token"] != "exchange-tok-xyz" || fc.exchangeBody["code_verifier"] != session.CodeVerifier {
		t.Errorf("exchange body = %v", fc.exchangeBody)
	}
	if fc.budgetAuth != "Bearer secret-vibe-api-key" {
		t.Errorf("budget auth = %q", fc.budgetAuth)
	}
	if bundle.Account == nil || bundle.Account.PlanName != "EDU" || bundle.Account.VibeBudget["initial_budget"] != float64(150) {
		t.Errorf("account = %+v", bundle.Account)
	}
}

func TestWaitForAPIKeyExchangeFailures(t *testing.T) {
	for name, mutate := range map[string]func(fc *fakeConsole){
		"http error":  func(fc *fakeConsole) { fc.exchangeStatus = http.StatusBadRequest },
		"missing key": func(fc *fakeConsole) { fc.exchangeAPIKey = "" },
	} {
		t.Run(name, func(t *testing.T) {
			fc := newFakeConsole(t)
			a := fc.auth()
			session, err := a.StartSignIn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			fc.pollStatuses = []string{"completed"}
			mutate(fc)
			if _, err = a.WaitForAPIKey(context.Background(), session); err == nil || !strings.Contains(err.Error(), "exchange") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestWaitForAPIKeyStopsAtExpiry(t *testing.T) {
	fc := newFakeConsole(t)
	a := fc.auth()
	session, err := a.StartSignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	session.ExpiresAt = time.Now().Add(-time.Second)
	if _, err = a.WaitForAPIKey(context.Background(), session); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitForAPIKeyHonoursContext(t *testing.T) {
	fc := newFakeConsole(t)
	a := fc.auth()
	session, err := a.StartSignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = a.WaitForAPIKey(ctx, session); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildMetadataAndFileName(t *testing.T) {
	signedIn := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	bundle := &AuthBundle{
		APIKey:     "secret-key",
		SignedInAt: signedIn,
		Account:    &AccountInfo{PlanName: "EDU", VibeBudget: map[string]any{"initial_budget": 150}},
	}
	metadata := BuildMetadata(bundle)
	for key, want := range map[string]any{
		"type":        util.MistralProvider,
		"api_key":     "secret-key",
		"base_url":    util.MistralDefaultBaseURL,
		"compat_name": util.MistralProvider,
		"label":       "Mistral (EDU)",
		"auth_method": AuthMethodBrowserSignIn,
		"plan_name":   "EDU",
	} {
		if metadata[key] != want {
			t.Errorf("%s = %v, want %v", key, metadata[key], want)
		}
	}
	if _, ok := metadata["api_budget"]; ok {
		t.Error("empty api_budget should be omitted")
	}
	if Label(nil) != "Mistral Subscription" {
		t.Errorf("default label = %q", Label(nil))
	}
	name := CredentialFileName("secret-key")
	if name != CredentialFileName(" secret-key ") || !strings.HasPrefix(name, "mistral-vibe-") || !strings.HasSuffix(name, ".json") {
		t.Errorf("file name = %q", name)
	}
	if name == CredentialFileName("other-key") {
		t.Error("different keys must map to different files")
	}
}
