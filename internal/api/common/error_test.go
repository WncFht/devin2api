// 本文件验证 Connect 错误码到 OpenAI / Anthropic error type 的映射。
package common

import "testing"

func TestOpenAIErrorType(t *testing.T) {
	cases := []struct {
		message string
		want    string
	}{
		{"invalid_argument: model does not support image", "invalid_request_error"},
		{"resource_exhausted: rate limit exceeded", "rate_limit_error"},
		{"permission_denied: not allowed", "permission_error"},
		{"not_found: model missing", "not_found_error"},
		{"unavailable: upstream offline", "server_error"},
		{"some unknown error", "server_error"},
	}
	for _, c := range cases {
		if got := OpenAIErrorType(c.message); got != c.want {
			t.Fatalf("OpenAIErrorType(%q) = %q, want %q", c.message, got, c.want)
		}
	}
}

func TestAnthropicErrorType(t *testing.T) {
	cases := []struct {
		message string
		want    string
	}{
		{"invalid_argument: bad request", "invalid_request_error"},
		{"resource_exhausted: rate limit exceeded", "rate_limit_error"},
		{"permission_denied: not allowed", "permission_error"},
		{"not_found: model missing", "not_found_error"},
		{"unavailable: upstream offline", "api_error"},
		{"some unknown error", "api_error"},
	}
	for _, c := range cases {
		if got := AnthropicErrorType(c.message); got != c.want {
			t.Fatalf("AnthropicErrorType(%q) = %q, want %q", c.message, got, c.want)
		}
	}
}
