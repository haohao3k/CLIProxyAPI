package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	mistralauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/mistral"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// MistralAuthenticator implements the Mistral Vibe browser sign-in flow. The
// flow mints a long-lived Mistral API key, saved as a "mistral" auth file and
// served through the OpenAI-compatibility executor.
type MistralAuthenticator struct{}

// NewMistralAuthenticator constructs a new Mistral authenticator.
func NewMistralAuthenticator() Authenticator {
	return &MistralAuthenticator{}
}

// Provider returns the provider key for Mistral.
func (MistralAuthenticator) Provider() string {
	return util.MistralProvider
}

// RefreshLead returns nil: the minted API key does not expire.
func (MistralAuthenticator) RefreshLead() *time.Duration {
	return nil
}

// Login runs the browser sign-in and returns the credential record.
func (a MistralAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	authSvc := mistralauth.NewMistralAuth(cfg)

	fmt.Println("Starting Mistral Vibe browser sign-in...")
	session, err := authSvc.StartSignIn(ctx)
	if err != nil {
		return nil, fmt.Errorf("mistral: failed to start browser sign-in: %w", err)
	}

	fmt.Printf("\nTo authenticate, please visit:\n%s\n\n", session.SignInURL)
	if !opts.NoBrowser {
		if browser.IsAvailable() {
			if errOpen := browser.OpenURL(session.SignInURL); errOpen != nil {
				log.Warnf("Failed to open browser automatically: %v", errOpen)
			} else {
				fmt.Println("Browser opened automatically.")
			}
		} else {
			log.Warn("No browser available; please open the URL manually")
		}
	}

	fmt.Println("Waiting for authorization...")
	if expiresIn := session.ExpiresIn(time.Now()); expiresIn > 0 {
		fmt.Printf("(This will timeout in %d seconds if not authorized)\n", expiresIn)
	}

	bundle, errWait := authSvc.WaitForAPIKey(ctx, session)
	if errWait != nil {
		return nil, fmt.Errorf("mistral: %w", errWait)
	}
	return BuildMistralAuthRecord(bundle)
}

// BuildMistralAuthRecord converts a completed sign-in into an auth record.
func BuildMistralAuthRecord(bundle *mistralauth.AuthBundle) (*coreauth.Auth, error) {
	if bundle == nil || strings.TrimSpace(bundle.APIKey) == "" {
		return nil, fmt.Errorf("mistral: sign-in returned no API key")
	}
	fileName := mistralauth.CredentialFileName(bundle.APIKey)
	return &coreauth.Auth{
		ID:       fileName,
		Provider: util.MistralProvider,
		FileName: fileName,
		Label:    mistralauth.Label(bundle.Account),
		Metadata: mistralauth.BuildMetadata(bundle),
	}, nil
}
