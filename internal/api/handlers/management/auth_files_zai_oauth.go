package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/zai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// zaiOAuthService is the subset of the Z.ai login service used by the
// management flow.
type zaiOAuthService interface {
	StartFlow(ctx context.Context) (*zai.Flow, error)
	WaitForAuthorization(ctx context.Context, flow *zai.Flow) (*zai.Authorization, error)
	CompleteLogin(ctx context.Context, authz *zai.Authorization) (*zai.Bundle, error)
}

var newZAIOAuthService = func(cfg *config.Config) zaiOAuthService {
	return zai.NewServiceWithConfig(cfg)
}

// zaiFlowGrace bounds the background poller beyond the flow's own expiry.
const zaiFlowGrace = time.Minute

// RequestZAIToken starts the Z.ai GLM Coding Plan login (ZCode CLI polling
// flow). The response carries the authorize URL to open and a state used with
// the OAuth status endpoint; completion is detected by server-side polling, so
// no redirect reaches this server.
func (h *Handler) RequestZAIToken(c *gin.Context) {
	ctx := PopulateAuthContext(context.Background(), c)
	const providerName = "zai"

	sessionProxyURL := oauthSessionProxyURLFromContext(c)
	cfg := h.oauthConfigWithProxy(sessionProxyURL)
	svc := newZAIOAuthService(cfg)

	flow, errStart := svc.StartFlow(ctx)
	if errStart != nil || flow == nil {
		message := "unknown error"
		if errStart != nil {
			message = errStart.Error()
		}
		log.Errorf("Failed to start Z.ai login: %s", redactSessionProxyInString(message, sessionProxyURL))
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to generate authorization url"})
		return
	}

	state := fmt.Sprintf("zai-%d", time.Now().UnixNano())
	RegisterOAuthSessionWithProxy(state, providerName, sessionProxyURL)
	response := gin.H{"status": "ok", "url": flow.AuthorizeURL, "state": state, "flow": "device"}
	if expiresIn := flow.ExpiresIn(); expiresIn > 0 {
		response["expires_in"] = expiresIn
	}
	bindOAuthSessionTarget(c, state, response)

	go func() {
		deadline := flow.ExpiresAt
		if deadline.IsZero() {
			deadline = time.Now().Add(10 * time.Minute)
		}
		pollCtx, cancelPoll := context.WithDeadline(ctx, deadline.Add(zaiFlowGrace))
		defer cancelPoll()
		go watchOAuthSessionCancel(pollCtx, cancelPoll, state, providerName)

		authz, errWait := svc.WaitForAuthorization(pollCtx, flow)
		if errWait != nil {
			if IsOAuthSessionPending(state, providerName) {
				SetOAuthSessionError(state, redactSessionProxyInString(oauthSessionErrorWithCause("Authentication failed", errWait), sessionProxyURL))
			}
			return
		}
		if !IsOAuthSessionPending(state, providerName) {
			return
		}
		bundle, errComplete := svc.CompleteLogin(pollCtx, authz)
		if errComplete != nil {
			if IsOAuthSessionPending(state, providerName) {
				SetOAuthSessionError(state, redactSessionProxyInString(oauthSessionErrorWithCause("Failed to provision Z.ai coding-plan API key", errComplete), sessionProxyURL))
			}
			return
		}
		record, errRecord := zai.BuildAuthRecord(bundle)
		if errRecord != nil {
			SetOAuthSessionError(state, errRecord.Error())
			return
		}
		savedPath, okSaved := h.commitAndCompleteBuiltinOAuthSession(ctx, state, providerName, sessionProxyURL, record, "Failed to save authentication tokens", false)
		if !okSaved {
			return
		}
		log.Infof("Z.ai authentication successful; credential saved to %s", strings.TrimSpace(savedPath))
	}()

	c.JSON(http.StatusOK, response)
}
