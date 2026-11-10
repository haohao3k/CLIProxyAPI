package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoMistralLogin runs the Mistral Vibe browser sign-in and saves the minted
// API key as a "mistral" auth file.
func DoMistralLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	manager := newAuthManager()
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser:    options.NoBrowser,
		CallbackPort: options.CallbackPort,
		ProxyURL:     options.ProxyURL,
		Metadata:     map[string]string{},
		Prompt:       options.Prompt,
	}

	record, savedPath, err := manager.Login(context.Background(), util.MistralProvider, cfg, authOpts)
	if err != nil {
		log.Errorf("Mistral authentication failed: %v", err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("Mistral authentication successful!")
}
