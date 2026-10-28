package main

import "testing"

func TestStripModelPrefix(t *testing.T) {
	cases := []struct {
		model      string
		authPrefix string
		want       string
	}{
		{"cc/deepseek/deepseek-v4-flash", "cc", "deepseek/deepseek-v4-flash"},
		{"cc/zai-org/GLM-5.2", "cc", "zai-org/GLM-5.2"},
		{"commandcode-go/moonshotai/Kimi-K3", "", "moonshotai/Kimi-K3"},
		{"commandcode-go/cc/xai/grok-4.5", "cc", "xai/grok-4.5"},
		{"cc/cc/deepseek/deepseek-v4-flash", "cc", "deepseek/deepseek-v4-flash"},
		{"deepseek/deepseek-v4-flash", "cc", "deepseek/deepseek-v4-flash"},
		{"glm5.2", "cc", "glm5.2"}, // bare alias survives prefix stripping and resolves via catalog
		{"cc/", "cc", "cc/"},       // stripping would empty the id: keep as-is
		{"unknown/vendor-model", "", "unknown/vendor-model"},
	}
	for _, tc := range cases {
		if got := stripModelPrefix(tc.model, tc.authPrefix); got != tc.want {
			t.Fatalf("stripModelPrefix(%q, %q) = %q, want %q", tc.model, tc.authPrefix, got, tc.want)
		}
	}
}

func TestAcceptEncodingIsNotDeclared(t *testing.T) {
	body := &ccRequestBody{ThreadID: "t", Config: map[string]any{"workingDir": "/tmp/app"}}
	headers := buildUpstreamHeaders("k", "1.0.0", body)
	// The host HTTP bridge does not decompress error bodies; requesting only
	// identity encoding (by omitting the header) keeps upstream 4xx readable.
	if _, exists := headers["Accept-Encoding"]; exists {
		t.Fatal("Accept-Encoding must not be sent (raw bridge cannot decode br/gzip error bodies)")
	}
}
