package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestSlugifyWorkingDir(t *testing.T) {
	if got := slugifyWorkingDir("/Users/x/My_Project 2024"); got != "my-project-2024" {
		t.Fatalf("slug = %q", got)
	}
	if got := slugifyWorkingDir(""); got != "commandcode-proxy" {
		t.Fatalf("empty slug = %q", got)
	}
	long := "/" + strings.Repeat("a", 80)
	if got := slugifyWorkingDir(long); len(got) > 40 {
		t.Fatalf("slug not clamped: %d", len(got))
	}
}

func TestBuildUpstreamHeadersFingerprint(t *testing.T) {
	body := &ccRequestBody{ThreadID: "thread-abc", Config: map[string]any{"workingDir": "/tmp/demo-app"}}
	headers := buildUpstreamHeaders("secret-key", "9.9.9", body)
	required := []string{
		"User-Agent", "Authorization", "x-cli-environment", "x-command-code-version",
		"x-session-id", "x-co-flag", "x-taste-learning", "x-project-slug", "traceparent", "Content-Type",
	}
	for _, name := range required {
		if len(headers[name]) == 0 {
			t.Fatalf("missing header %s", name)
		}
	}
	if headers["Authorization"][0] != "Bearer secret-key" {
		t.Fatal("bearer format")
	}
	if !strings.HasPrefix(headers["User-Agent"][0], "commandcode-cli/9.9.9 Node.js/") {
		t.Fatalf("user agent = %q", headers["User-Agent"][0])
	}
	if headers["x-session-id"][0] != "thread-abc" {
		t.Fatal("session id must match threadId")
	}
	if headers["x-project-slug"][0] != "demo-app" {
		t.Fatalf("slug = %q", headers["x-project-slug"][0])
	}
	if !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`).MatchString(headers["traceparent"][0]) {
		t.Fatalf("traceparent = %q", headers["traceparent"][0])
	}
}

func TestSanitizeErrorText(t *testing.T) {
	out := sanitizeErrorText("key cc-SUPERsecret \x00bell:\x07 value", "cc-SUPERsecret")
	if strings.Contains(out, "cc-SUPERsecret") {
		t.Fatal("api key not redacted")
	}
	if strings.ContainsAny(out, "\x00\x07") {
		t.Fatal("control chars not stripped")
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatal("redaction marker missing")
	}
}

func TestNewUUIDShape(t *testing.T) {
	id := newUUID()
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("uuid v4 shape: %q", id)
	}
}

func TestUpstreamFailureEnvelopeFromStatus(t *testing.T) {
	env := executorFailureEnvelope(&upstreamFailure{Status: 401, Message: "CC API 401: invalid key"})
	var parsed envelope
	if err := json.Unmarshal(env, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Error == nil || parsed.Error.HTTPStatus != 401 {
		t.Fatalf("status passthrough: %+v", parsed.Error)
	}
	env = executorFailureEnvelope(&upstreamFailure{Status: 0, Message: "net down"})
	_ = json.Unmarshal(env, &parsed)
	if parsed.Error.HTTPStatus != 502 {
		t.Fatalf("transport failure should map to 502: %+v", parsed.Error)
	}
}
