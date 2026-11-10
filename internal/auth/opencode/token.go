package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	log "github.com/sirupsen/logrus"
)

// TokenStorage stores OpenCode Go API key credentials on disk.
type TokenStorage struct {
	Type      string `json:"type"`
	APIKey    string `json:"api_key"`
	BaseURL   string `json:"base_url,omitempty"`
	Label     string `json:"label,omitempty"`
	AuthKind  string `json:"auth_kind,omitempty"`
	AddedAt   string `json:"added_at,omitempty"`
	ExpiresAt string `json:"expired,omitempty"`

	Metadata map[string]any `json:"-"`
}

// SetMetadata allows the token store to merge status fields before saving.
func (ts *TokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile writes OpenCode credentials to a JSON auth file.
func (ts *TokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = ProviderKey
	if strings.TrimSpace(ts.AuthKind) == "" {
		ts.AuthKind = "apikey"
	}
	if strings.TrimSpace(ts.BaseURL) == "" {
		ts.BaseURL = DefaultBaseURL
	} else {
		ts.BaseURL = NormalizeBaseURL(ts.BaseURL)
	}
	if strings.TrimSpace(ts.AddedAt) == "" {
		ts.AddedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if errMkdirAll := os.MkdirAll(filepath.Dir(authFilePath), 0o700); errMkdirAll != nil {
		return fmt.Errorf("opencode token storage: create directory: %w", errMkdirAll)
	}

	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("opencode token storage: merge metadata: %w", errMerge)
	}

	file, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("opencode token storage: create token file: %w", err)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Errorf("opencode token storage: close token file error: %v", errClose)
		}
	}()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(data); err != nil {
		return fmt.Errorf("opencode token storage: write token file: %w", err)
	}
	return nil
}

// CredentialFileName returns the filename used for OpenCode credentials.
// When an API key is supplied, it uses a short deterministic SHA-256 digest so
// re-importing the same key updates the same file; otherwise it uses a millisecond timestamp.
func CredentialFileName(apiKey ...string) string {
	if len(apiKey) > 0 {
		trimmed := strings.TrimSpace(apiKey[0])
		if trimmed != "" {
			sum := sha256.Sum256([]byte("opencode:" + trimmed))
			return fmt.Sprintf("opencode-%s.json", hex.EncodeToString(sum[:6]))
		}
	}
	return fmt.Sprintf("opencode-%d.json", time.Now().UnixMilli())
}
