package management

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	opencodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/opencode"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ImportOpenCodeKey validates (unless skip_validate is set) and saves an
// OpenCode Go API key as an auth record so keys can be added from the management API.
func (h *Handler) ImportOpenCodeKey(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	if h.cfg.AuthDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth directory not configured"})
		return
	}

	var payload struct {
		APIKey       string `json:"api_key"`
		APIKeyDash   string `json:"api-key"`
		BaseURL      string `json:"base_url"`
		BaseURLDash  string `json:"base-url"`
		Label        string `json:"label"`
		SkipValidate bool   `json:"skip_validate"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request", "message": err.Error()})
		return
	}
	apiKey := strings.TrimSpace(payload.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(payload.APIKeyDash)
	}
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key is required"})
		return
	}
	rawBaseURL := strings.TrimSpace(payload.BaseURL)
	if rawBaseURL == "" {
		rawBaseURL = strings.TrimSpace(payload.BaseURLDash)
	}
	baseURL := opencodeauth.NormalizeBaseURL(rawBaseURL)

	label := strings.TrimSpace(payload.Label)
	if label == "" {
		label = opencodeauth.DefaultLabel
	}

	ctx := PopulateAuthContext(context.Background(), c)
	if !payload.SkipValidate {
		if err := opencodeauth.NewOpenCodeAuth(h.cfg).ValidateKey(ctx, baseURL, apiKey); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "key validation failed", "message": err.Error()})
			return
		}
	}

	storage := &opencodeauth.TokenStorage{
		APIKey:  apiKey,
		BaseURL: baseURL,
		Label:   label,
	}
	fileName := opencodeauth.CredentialFileName(apiKey)
	now := time.Now().UnixMilli()
	record := &coreauth.Auth{
		ID:       fileName,
		Provider: opencodeauth.ProviderKey,
		FileName: fileName,
		Label:    label,
		Storage:  storage,
		Metadata: map[string]any{
			"type":         opencodeauth.ProviderKey,
			"api_key":      apiKey,
			"access_token": apiKey,
			"base_url":     baseURL,
			"auth_kind":    "apikey",
			"label":        label,
			"timestamp":    now,
		},
		Attributes: map[string]string{
			"api_key":   apiKey,
			"auth_kind": "apikey",
			"base_url":  baseURL,
		},
	}
	savedPath, errSave := h.saveTokenRecord(ctx, record)
	if errSave != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save key", "message": errSave.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "auth_file": savedPath, "file": fileName})
}
