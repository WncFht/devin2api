// upstream 收敛 adapter 与 dashboard 共用的 Devin 上游 wire 辅助：
// 请求 metadata 构造与 Basic 认证 transport。
package upstream

import (
	"net/http"

	devinproto "local/devinproto"

	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/randid"
)

// BuildMetadata 构造上游请求的 Metadata 公共头。os 与 fingerprintBytes
// 由调用方按其模仿的客户端形态给出（chisel CLI 是 mac/366 字节指纹，
// Windsurf 形态是 win/32 字节，0 表示不带指纹）。F 仅为设备遥测，
// crypto/rand 理论性失败时省略该字段而非让请求失败。
func BuildMetadata(token, clientName, clientVersion, os string, fingerprintBytes int) *devinproto.ExaCodeiumCommonPb_Metadata {
	metadata := &devinproto.ExaCodeiumCommonPb_Metadata{
		ApiKey:           proto.String(token),
		ExtensionName:    proto.String(clientName),
		ExtensionVersion: proto.String(clientVersion),
		IdeName:          proto.String(clientName),
		IdeVersion:       proto.String(clientVersion),
		Locale:           proto.String("en"),
		Os:               proto.String(os),
	}
	if fingerprintBytes > 0 {
		if fingerprint, err := randid.Hex(fingerprintBytes); err == nil {
			metadata.F = proto.String(fingerprint)
		}
	}
	return metadata
}

// BasicAuthTransport 给上游请求补 "Basic <token>-<token>" 头；
// 已带 Authorization 的请求（如 Seat 的 Bearer）原样放行。
type BasicAuthTransport struct {
	base      http.RoundTripper
	tokenFunc func() string
}

// NewBasicAuthTransportFunc 包装 base，使出站请求默认携带 Basic
// token-token 认证；token 每次请求重新求值——unauthenticated 触发的
// 凭据自愈不需要重建 transport。
func NewBasicAuthTransportFunc(base http.RoundTripper, tokenFunc func() string) *BasicAuthTransport {
	return &BasicAuthTransport{base: base, tokenFunc: tokenFunc}
}

// RoundTrip 实现 http.RoundTripper。
func (t *BasicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	// ApiServer Connect 客户端沿用 Basic token-token；Seat 单独走 Bearer。
	if clone.Header.Get("Authorization") == "" {
		token := t.tokenFunc()
		clone.Header.Set("Authorization", "Basic "+token+"-"+token)
	}
	// 真实 Devin CLI 抓包不发送 User-Agent；connect-go 默认会带
	// "connect-go/<ver>"，置空串使 net/http 整体省略该头，消掉
	// 「自称 chisel 但 UA 是 connect-go」这个可抓的指纹破绽。
	clone.Header.Set("User-Agent", "")
	return t.base.RoundTrip(clone)
}
