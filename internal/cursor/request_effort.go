package cursor

import (
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// RequestEffort returns the effort the Cursor executor derives for a client request: the effort
// fields of the client body, else those of the body translated to the OpenAI format, which is
// where shapes like Claude adaptive thinking or output_config.effort and Gemini thinkingConfig
// become reasoning_effort. A "(level)" suffix on the model is not included; the resolver reads it.
func RequestEffort(entryFormat, model string, raw []byte) string {
	if effort := ExtractPayloadEffort(raw); effort != "" {
		return effort
	}
	from := sdktranslator.FromString(strings.TrimSpace(entryFormat))
	to := sdktranslator.FromString("openai")
	if len(raw) == 0 || from == to {
		return ""
	}
	return ExtractPayloadEffort(sdktranslator.TranslateRequest(from, to, model, raw, false))
}
