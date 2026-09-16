package devin

import "testing"

// TestDomainBlocked 钉住 blocked_domains 的 host 后缀匹配：上游对屏蔽域
// 无对应字段，结果侧过滤是唯一防线——精确域、子域、大小写与形似域
// （notexample.com）的边界都在此固定。
func TestDomainBlocked(t *testing.T) {
	blocked := []string{"Example.COM", " bad-space.com "}
	cases := []struct {
		url  string
		want bool
	}{
		{"https://example.com/x", true},
		{"https://sub.example.com/x", true},
		{"https://EXAMPLE.com", true},
		{"https://bad-space.com/", true},
		{"https://notexample.com/x", false},
		{"https://example.com.evil.com/x", false},
		{"https://go.dev/dl/", false},
		{"not a url", false},
	}
	for _, c := range cases {
		if got := domainBlocked(c.url, blocked); got != c.want {
			t.Errorf("domainBlocked(%q) = %v, want %v", c.url, got, c.want)
		}
	}
	if domainBlocked("https://example.com", nil) {
		t.Error("empty blocked list must not block")
	}
}
