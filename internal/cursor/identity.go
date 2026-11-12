package cursor

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/google/uuid"
)

const (
	// APIHost is the legacy Cursor AIServer origin (used for image-containing requests).
	APIHost = "https://api2.cursor.sh"
	// apiHostHeader is the Host header Cursor's own client sends to api2.
	apiHostHeader = "api2.cursor.sh"

	// AgentHost is the Cursor AgentService HTTP/2 origin used by current Cursor IDE releases.
	AgentHost = "https://agent.api5.cursor.sh"
	// AgentHostHeader is the Host/:authority header for AgentService.
	AgentHostHeader = "agent.api5.cursor.sh"

	// ChatEndpoint is the legacy Connect-RPC path for a unified chat stream.
	ChatEndpoint = APIHost + "/aiserver.v1.ChatService/StreamUnifiedChatWithTools"
	// AvailableModelsEndpoint lists models on legacy AIServer.
	AvailableModelsEndpoint = APIHost + "/aiserver.v1.AiService/AvailableModels"

	// AgentRunEndpoint is the bidirectional Connect-RPC path for Cursor AgentService.
	AgentRunEndpoint = AgentHost + "/agent.v1.AgentService/Run"
	// GetUsableModelsEndpoint returns the live account-specific model picker catalog.
	GetUsableModelsEndpoint = AgentHost + "/agent.v1.AgentService/GetUsableModels"

	// DefaultClientVersion is the Cursor IDE client version reported to AgentService/AIServer.
	DefaultClientVersion = "3.12.17"
	// DefaultClientCommit is the matching Cursor IDE commit hash for DefaultClientVersion.
	DefaultClientCommit = "0fb762053c34788bb7760d5673f8a6d4c8589d50"

	// UserAgent is the Cursor editor user agent used by the login endpoints.
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Cursor/0.48.6 Chrome/132.0.6834.210 Electron/34.3.4 Safari/537.36"
)

// Hashed64Hex returns the sha256 hex digest of input+salt.
// Used for x-client-key and the machine IDs inside the checksum.
func Hashed64Hex(input, salt string) string {
	h := sha256.Sum256([]byte(input + salt))
	return hex.EncodeToString(h[:])
}

// obfuscateBytes mirrors the Jyh cipher byte obfuscation from the Cursor IDE client
// used inside the x-cursor-checksum value.
func obfuscateBytes(b []byte) {
	t := byte(165)
	for i := range b {
		b[i] = (b[i] ^ t) + byte(i%256)
		t = b[i]
	}
}

// Checksum builds the x-cursor-checksum header value for the given token.
func Checksum(token string) string {
	return ChecksumWithMachineID(token, "")
}

// ChecksumWithMachineID builds the x-cursor-checksum header value (Jyh cipher:
// URL-safe unpadded base64 of obfuscated 6-byte big-endian floor(unixMillis/1e6)
// concatenated with machineID).
func ChecksumWithMachineID(token, machineID string) string {
	cleanToken := NormalizeToken(token)
	effectiveMachineID := strings.TrimSpace(machineID)
	if effectiveMachineID == "" {
		effectiveMachineID = Hashed64Hex(cleanToken, "machineId")
	}

	ts := uint64(time.Now().UnixMilli() / 1_000_000)
	b := []byte{
		byte(ts >> 40),
		byte(ts >> 32),
		byte(ts >> 24),
		byte(ts >> 16),
		byte(ts >> 8),
		byte(ts),
	}
	obfuscateBytes(b)
	return base64.RawURLEncoding.EncodeToString(b) + effectiveMachineID
}

func cursorClientOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

func cursorClientArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x64"
}

// uuidV5 replicates uuid v5 (SHA-1, name-based, RFC 4122 §4.3), which the Cursor
// client uses to derive x-session-id.
func uuidV5(namespace uuid.UUID, name string) uuid.UUID {
	h := sha1.New()
	h.Write(namespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	var u uuid.UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // variant RFC4122
	return u
}

// SessionID derives x-session-id as uuidv5(authToken, uuidv5.DNS).
func SessionID(token string) string {
	dns := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	return uuidV5(dns, token).String()
}

// NormalizeToken extracts the raw access token from a Cursor cookie. Cookies come
// in the `userId::token` and url-encoded `userId%3A%3Atoken` shapes; a bare token
// is returned unchanged.
func NormalizeToken(token string) string {
	token = strings.TrimSpace(token)
	if idx := strings.Index(token, "%3A%3A"); idx >= 0 {
		token = token[idx+len("%3A%3A"):]
	} else if idx := strings.Index(token, "::"); idx >= 0 {
		token = token[idx+len("::"):]
	}
	return strings.TrimSpace(token)
}

// ApplyHeaders sets the Cursor client headers on r for legacy AIServer calls.
func ApplyHeaders(r *http.Request, token string, streaming bool) {
	ApplyHeadersWithHost(r, token, streaming, apiHostHeader)
}

// ApplyHeadersWithHost is the same as ApplyHeaders but allows overriding the Host
// header, which is useful for tests or alternate Cursor API gateways.
func ApplyHeadersWithHost(r *http.Request, token string, streaming bool, host string) {
	token = NormalizeToken(token)
	r.Header.Set("authorization", "Bearer "+token)
	r.Header.Set("connect-protocol-version", "1")
	r.Header.Set("user-agent", "connect-es/1.6.1")
	r.Header.Set("x-cursor-checksum", Checksum(token))
	r.Header.Set("x-cursor-client-version", DefaultClientVersion)
	r.Header.Set("x-cursor-client-commit", DefaultClientCommit)
	r.Header.Set("x-cursor-client-type", "ide")
	r.Header.Set("x-cursor-client-os", cursorClientOS())
	r.Header.Set("x-cursor-client-arch", cursorClientArch())
	r.Header.Set("x-cursor-client-device-type", "desktop")
	r.Header.Set("x-cursor-config-version", uuid.NewString())
	r.Header.Set("x-cursor-timezone", "UTC")
	r.Header.Set("x-ghost-mode", "true")
	if host != "" {
		r.Host = host
	}
	if streaming {
		r.Header.Set("connect-accept-encoding", "gzip")
		r.Header.Set("connect-content-encoding", "gzip")
		r.Header.Set("content-type", "application/connect+proto")
		r.Header.Set("x-amzn-trace-id", "Root="+uuid.NewString())
		r.Header.Set("x-client-key", Hashed64Hex(token, ""))
		r.Header.Set("x-request-id", uuid.NewString())
		r.Header.Set("x-session-id", SessionID(token))
		return
	}
	r.Header.Set("accept-encoding", "gzip")
	r.Header.Set("content-type", "application/proto")
}

// ApplyAgentHeaders sets the Cursor IDE AgentService headers on r.
// When streaming is true (AgentService/Run), Connect streaming headers are sent;
// when streaming is false (AgentService/GetUsableModels), unary application/proto headers are sent.
func ApplyAgentHeaders(r *http.Request, token, machineID string, streaming bool) {
	ApplyAgentHeadersWithHost(r, token, machineID, streaming, AgentHostHeader)
}

// ApplyAgentHeadersWithHost sets Cursor AgentService headers with an optional Host override.
func ApplyAgentHeadersWithHost(r *http.Request, token, machineID string, streaming bool, host string) {
	cleanToken := NormalizeToken(token)
	r.Header.Set("authorization", "Bearer "+cleanToken)
	r.Header.Set("user-agent", "connect-es/1.6.1")
	r.Header.Set("x-amzn-trace-id", "Root="+uuid.NewString())
	r.Header.Set("x-client-key", Hashed64Hex(cleanToken, ""))
	r.Header.Set("x-cursor-checksum", ChecksumWithMachineID(cleanToken, machineID))
	r.Header.Set("x-cursor-client-version", DefaultClientVersion)
	r.Header.Set("x-cursor-client-commit", DefaultClientCommit)
	r.Header.Set("x-cursor-client-type", "ide")
	r.Header.Set("x-cursor-client-os", cursorClientOS())
	r.Header.Set("x-cursor-client-arch", cursorClientArch())
	r.Header.Set("x-cursor-client-device-type", "desktop")
	r.Header.Set("x-cursor-config-version", uuid.NewString())
	r.Header.Set("x-cursor-timezone", "UTC")
	r.Header.Set("x-ghost-mode", "true")
	r.Header.Set("x-request-id", uuid.NewString())
	r.Header.Set("x-session-id", SessionID(cleanToken))
	if host != "" {
		r.Host = host
	}
	if streaming {
		r.Header.Set("connect-accept-encoding", "gzip")
		r.Header.Set("connect-protocol-version", "1")
		r.Header.Set("content-type", "application/connect+proto")
		return
	}
	r.Header.Set("accept", "application/proto")
	r.Header.Set("content-type", "application/proto")
}
