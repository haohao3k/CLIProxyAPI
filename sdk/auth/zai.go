package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/zai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// newZAIService is replaceable in tests.
var newZAIService = func(cfg *config.Config) *zai.Service {
	return zai.NewServiceWithConfig(cfg)
}

// ZAIAuthenticator implements the Z.ai GLM Coding Plan login (ZCode CLI
// polling flow followed by coding-plan API key provisioning).
type ZAIAuthenticator struct{}

// NewZAIAuthenticator constructs a Z.ai authenticator.
func NewZAIAuthenticator() Authenticator {
	return &ZAIAuthenticator{}
}

// Provider returns the provider key for Z.ai.
func (ZAIAuthenticator) Provider() string { return zai.ProviderKey }

// RefreshLead returns nil: the coding-plan API key does not expire on a
// schedule and Z.ai offers no refresh grant for this flow.
func (ZAIAuthenticator) RefreshLead() *time.Duration { return nil }

// Login runs the interactive Z.ai login and returns the auth record.
func (ZAIAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	svc := newZAIService(cfg)

	fmt.Println("Starting Z.ai (GLM Coding Plan) authentication...")
	flow, errStart := svc.StartFlow(ctx)
	if errStart != nil {
		return nil, errStart
	}
	fmt.Printf("\nTo authenticate, please visit:\n%s\n\n", flow.AuthorizeURL)
	if !opts.NoBrowser && browser.IsAvailable() {
		if errOpen := browser.OpenURL(flow.AuthorizeURL); errOpen != nil {
			log.Warnf("Failed to open browser automatically: %v", errOpen)
		} else {
			fmt.Println("Browser opened automatically.")
		}
	}
	fmt.Println("Waiting for authorization...")
	if expiresIn := flow.ExpiresIn(); expiresIn > 0 {
		fmt.Printf("(This will timeout in %d seconds if not authorized)\n", expiresIn)
	}

	authz, errWait := svc.WaitForAuthorization(ctx, flow)
	if errWait != nil {
		return nil, errWait
	}
	fmt.Println("Authorized. Provisioning the coding-plan API key...")
	bundle, errComplete := svc.CompleteLogin(ctx, authz)
	if errComplete != nil {
		return nil, errComplete
	}
	record, errRecord := zai.BuildAuthRecord(bundle)
	if errRecord != nil {
		return nil, errRecord
	}
	fmt.Println("\nZ.ai authentication successful!")
	return record, nil
}
