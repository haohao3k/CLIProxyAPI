package zai

import (
	"fmt"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// BuildAuthRecord converts a login bundle into a persistable auth record
// (auth file "type": "zai"). Only the coding-plan API key is stored; the
// short-lived OAuth/business tokens are intentionally discarded.
func BuildAuthRecord(bundle *Bundle) (*cliproxyauth.Auth, error) {
	if bundle == nil || strings.TrimSpace(bundle.APIKey) == "" {
		return nil, fmt.Errorf("zai: login bundle is missing the API key")
	}
	baseURL := strings.TrimSpace(bundle.BaseURL)
	if baseURL == "" {
		baseURL = CodingPlanBaseURL
	}
	identity := bundle.User.Identity()
	metadata := map[string]any{
		"type":                         ProviderKey,
		cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
		cliproxyauth.AttributeAPIKey:   strings.TrimSpace(bundle.APIKey),
		"base_url":                     baseURL,
		"login_method":                 "zcode_cli_oauth",
		"timestamp":                    time.Now().UnixMilli(),
	}
	if bundle.User.Email != "" {
		metadata["email"] = bundle.User.Email
	}
	if bundle.User.Name != "" {
		metadata["name"] = bundle.User.Name
	}
	if bundle.User.ID != "" {
		metadata["user_id"] = bundle.User.ID
	}
	label := identity
	if label == "" {
		label = "Z.ai User"
	}
	fileName := fmt.Sprintf("%s-%s.json", ProviderKey, safeFilePart(identity))
	if identity == "" {
		fileName = fmt.Sprintf("%s-%d.json", ProviderKey, time.Now().UnixMilli())
	}
	return &cliproxyauth.Auth{
		ID:       fileName,
		Provider: ProviderKey,
		FileName: fileName,
		Label:    label,
		Metadata: metadata,
		Attributes: map[string]string{
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
			cliproxyauth.AttributeAPIKey:   strings.TrimSpace(bundle.APIKey),
			"base_url":                     baseURL,
		},
	}, nil
}

// ApplyAuthFileAttributes maps a "type": "zai" auth file onto the runtime
// attributes consumed by the OpenAI-compatible executor. Both hand-written
// API-key files ({"type":"zai","api_key":"..."}) and OAuth login files are
// supported; base_url defaults to the coding-plan endpoint.
func ApplyAuthFileAttributes(auth *cliproxyauth.Auth, metadata map[string]any) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	baseURL := firstNonEmpty(stringField(metadata, "base_url"), stringField(metadata, "base-url"))
	if baseURL == "" {
		baseURL = CodingPlanBaseURL
	}
	auth.Attributes["base_url"] = strings.TrimRight(baseURL, "/")
	if apiKey := firstNonEmpty(
		stringField(metadata, "api_key"),
		stringField(metadata, "api-key"),
		stringField(metadata, "apiKey"),
		stringField(metadata, "access_token"),
	); apiKey != "" {
		auth.Attributes[cliproxyauth.AttributeAPIKey] = apiKey
	}
	if label := firstNonEmpty(stringField(metadata, "email"), stringField(metadata, "name")); label != "" {
		if current := strings.TrimSpace(auth.Label); current == "" || strings.EqualFold(current, ProviderKey) {
			auth.Label = label
		}
	}
}

func safeFilePart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		if b.Len() >= 48 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == '@':
			b.WriteString("_at_")
		default:
			b.WriteByte('-')
		}
	}
	part := strings.Trim(b.String(), "-._")
	if part == "" {
		return "account"
	}
	return part
}
