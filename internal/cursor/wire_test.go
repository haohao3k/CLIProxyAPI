package cursor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func frame(flag byte, payload []byte) []byte {
	out := []byte{flag, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	return append(out, payload...)
}

func chatChunk(content, thinking string) []byte {
	var msg []byte
	msg = appendString(msg, 1, content)
	if thinking != "" {
		var th []byte
		th = appendString(th, 1, thinking)
		msg = appendMessage(msg, 25, th)
	}
	var b []byte
	return appendMessage(b, 2, msg)
}

func TestGenerateChatBodyFraming(t *testing.T) {
	body, err := GenerateChatBody([]Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}}, "claude-4-sonnet")
	if err != nil {
		t.Fatalf("GenerateChatBody() error = %v", err)
	}
	if len(body) < 5 || body[0] != 0 {
		t.Fatalf("expected uncompressed frame, got flag %v", body[:1])
	}
	if n := int(binary.BigEndian.Uint32(body[1:5])); n != len(body)-5 {
		t.Fatalf("frame length %d, payload %d", n, len(body)-5)
	}
	for _, want := range []string{"be brief", "hi", "claude-4-sonnet"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("payload missing %q", want)
		}
	}

	many := []Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}, {Role: "user", Content: "c"}}
	gz, err := GenerateChatBody(many, "m")
	if err != nil {
		t.Fatalf("GenerateChatBody() error = %v", err)
	}
	if gz[0] != flagCompressed {
		t.Fatalf("expected compressed flag for %d messages", len(many))
	}
}

func TestReadFramesContentAndEndStreamError(t *testing.T) {
	var stream []byte
	stream = append(stream, frame(0, chatChunk("Hel", "think"))...)
	stream = append(stream, frame(0, chatChunk("lo", ""))...)
	stream = append(stream, frame(flagEndStream, []byte(`{"error":{"code":"resource_exhausted","message":"slow down","details":[{"debug":{"error":"ERROR_RATE_LIMITED","details":{"title":"Too many","detail":"Retry later"}}}]}}`))...)

	var text, reasoning strings.Builder
	err := ReadFrames(bytes.NewReader(stream), func(th, content string) {
		reasoning.WriteString(th)
		text.WriteString(content)
	})
	if text.String() != "Hello" || reasoning.String() != "think" {
		t.Fatalf("text=%q reasoning=%q", text.String(), reasoning.String())
	}
	var se *StreamError
	if !errors.As(err, &se) {
		t.Fatalf("expected StreamError, got %v", err)
	}
	if se.HTTPStatus() != 429 || se.Reason != "ERROR_RATE_LIMITED" || se.Detail != "Retry later" || se.Title != "Too many" {
		t.Fatalf("unexpected stream error: %+v", se)
	}
}

func TestReadFramesCleanEndAndTruncation(t *testing.T) {
	ok := append(frame(0, chatChunk("x", "")), frame(flagEndStream, []byte(`{}`))...)
	if err := ReadFrames(bytes.NewReader(ok), func(string, string) {}); err != nil {
		t.Fatalf("clean stream error = %v", err)
	}
	if err := ReadFrames(bytes.NewReader(nil), func(string, string) {}); err != nil {
		t.Fatalf("empty stream error = %v", err)
	}
	truncated := frame(0, chatChunk("x", ""))[:3]
	if err := ReadFrames(bytes.NewReader(truncated), func(string, string) {}); err == nil {
		t.Fatalf("expected error for truncated header")
	}
	huge := []byte{0, 0xff, 0xff, 0xff, 0xff}
	if err := ReadFrames(bytes.NewReader(huge), func(string, string) {}); err == nil {
		t.Fatalf("expected error for oversized frame")
	}
}

func TestParseEndStreamNonJSON(t *testing.T) {
	err := parseEndStream([]byte("plain failure"))
	var se *StreamError
	if !errors.As(err, &se) || se.Detail != "plain failure" || se.HTTPStatus() != 502 {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestNormalizeToken(t *testing.T) {
	for in, want := range map[string]string{
		"tok":                 "tok",
		"user_1%3A%3Atok":     "tok",
		"user_1::tok":         "tok",
		"  user_1::tok  ":     "tok",
		"user_1%3A%3A  tok  ": "tok",
	} {
		if got := NormalizeToken(in); got != want {
			t.Fatalf("NormalizeToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeAvailableModels(t *testing.T) {
	var model []byte
	model = appendString(model, 1, "claude-4-sonnet")
	var data []byte
	data = appendMessage(data, 2, model)
	data = appendString(data, 1, "gpt-5")
	names, err := DecodeAvailableModels(data)
	if err != nil {
		t.Fatalf("DecodeAvailableModels() error = %v", err)
	}
	if strings.Join(names, ",") != "claude-4-sonnet,gpt-5" {
		t.Fatalf("names = %v", names)
	}
}
