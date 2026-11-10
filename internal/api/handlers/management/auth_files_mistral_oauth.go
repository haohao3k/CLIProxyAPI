package management

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	mistralauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/mistral"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	log "github.com/sirupsen/logrus"
)

type mistralOAuthService interface {
	StartSignIn(ctx context.Context) (*mistralauth.SignInSession, error)
	WaitForAPIKey(ctx context.Context, session *mistralauth.SignInSession) (*mistralauth.AuthBundle, error)
}

var newMistralOAuthService = func(cfg *config.Config, proxyURL string) mistralOAuthService {
	return mistralauth.NewMistralAuthWithProxyURL(cfg, proxyURL)
}

// RequestMistralToken starts the Mistral Vibe browser sign-in. The response
// carries the sign-in URL and a state; progress is reported through the
// shared OAuth session status endpoint (get-auth-status / oauth/status).
func (h *Handler) RequestMistralToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Mistral authentication...")

	sessionProxyURL := oauthSessionProxyURLFromContext(c)
	cfg := h.oauthConfigWithProxy(sessionProxyURL)
	state := fmt.Sprintf("mistral-%d", time.Now().UnixNano())
	authSvc := newMistralOAuthService(cfg, sessionProxyURL)

	session, errStart := authSvc.StartSignIn(ctx)
	if errStart != nil {
		log.Errorf("Failed to start Mistral browser sign-in: %s", redactSessionProxyInString(errStart.Error(), sessionProxyURL))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start browser sign-in"})
		return
	}

	RegisterOAuthSessionWithProxy(state, util.MistralProvider, sessionProxyURL)
	response := gin.H{"status": "ok", "url": session.SignInURL, "state": state, "flow": "device"}
	if expiresIn := session.ExpiresIn(time.Now()); expiresIn > 0 {
		response["expires_in"] = expiresIn
	}
	bindOAuthSessionTarget(c, state, response)

	go func() {
		pollCtx, cancelPoll := context.WithCancel(ctx)
		defer cancelPoll()
		go watchOAuthSessionCancel(pollCtx, cancelPoll, state, util.MistralProvider)

		fmt.Println("Waiting for Mistral authentication...")
		bundle, errWait := authSvc.WaitForAPIKey(pollCtx, session)
		if errWait != nil {
			if !IsOAuthSessionPending(state, util.MistralProvider) {
				return
			}
			log.Errorf("Mistral authentication failed: %s", redactSessionProxyInString(errWait.Error(), sessionProxyURL))
			SetOAuthSessionError(state, redactSessionProxyInString(oauthSessionErrorWithCause("Authentication failed", errWait), sessionProxyURL))
			return
		}
		if !IsOAuthSessionPending(state, util.MistralProvider) {
			return
		}

		record, errRecord := sdkAuth.BuildMistralAuthRecord(bundle)
		if errRecord != nil {
			log.Errorf("Mistral sign-in returned no API key: %v", errRecord)
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}
		savedPath, okSaved := h.commitAndCompleteBuiltinOAuthSession(ctx, state, util.MistralProvider, sessionProxyURL, record, "Failed to save token to file", false)
		if !okSaved {
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use Mistral services through this CLI")
	}()

	c.JSON(http.StatusOK, response)
}
