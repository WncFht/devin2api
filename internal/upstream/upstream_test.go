// 本文件验证上游 transport 的认证头与指纹行为。
package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubRoundTripper 记录最近一次请求的 Header 并返回空响应。
type stubRoundTripper struct {
	header http.Header
}

// RoundTrip 捕获请求头。
func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.header = req.Header.Clone()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func newRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://server.codeium.com/rpc", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestBasicAuthTransportSuppressesUserAgent 验证 User-Agent 被显式置空——
// net/http 对空值 User-Agent 键的处理是整条头省略，消掉 connect-go 指纹。
func TestBasicAuthTransportSuppressesUserAgent(t *testing.T) {
	stub := &stubRoundTripper{}
	transport := NewBasicAuthTransport(stub, "tok")
	resp, err := transport.RoundTrip(newRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	values, present := stub.header["User-Agent"]
	if !present || len(values) != 1 || values[0] != "" {
		t.Fatalf("User-Agent = %#v (present=%v), want single empty value", values, present)
	}
	if got := stub.header.Get("Authorization"); got != "Basic tok-tok" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestBasicAuthTransportFuncRefreshesPerRequest 验证 tokenFunc 每次请求
// 重新求值——凭据自愈后无需重建 transport 即生效。
func TestBasicAuthTransportFuncRefreshesPerRequest(t *testing.T) {
	stub := &stubRoundTripper{}
	token := "old"
	transport := NewBasicAuthTransportFunc(stub, func() string { return token })
	resp, err := transport.RoundTrip(newRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	token = "new"
	resp, err = transport.RoundTrip(newRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := stub.header.Get("Authorization"); got != "Basic new-new" {
		t.Fatalf("Authorization = %q, want refreshed token", got)
	}
}

// TestBasicAuthTransportPreservesExistingAuthorization 验证已带 Authorization
// 的请求（Seat Bearer 形态）不被 Basic 覆盖。
func TestBasicAuthTransportPreservesExistingAuthorization(t *testing.T) {
	stub := &stubRoundTripper{}
	transport := NewBasicAuthTransport(stub, "tok")
	req := newRequest(t)
	req.Header.Set("Authorization", "Bearer seat-token")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := stub.header.Get("Authorization"); got != "Bearer seat-token" {
		t.Fatalf("Authorization = %q, want Bearer preserved", got)
	}
}
