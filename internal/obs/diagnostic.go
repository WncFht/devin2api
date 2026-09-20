// 本文件把任意错误收敛成单行、有界、脱敏的诊断文本。
//
// 进程日志只承载白名单信号和脱敏后的截断摘要；完整的原始错误
// 细节由 per-request 调试日志承担。灵感来自 CLIProxyAPI 的
// SafeDiagnosticForLog——自由文本错误可能夹带 token/密钥。
package obs

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"

	"github.com/WncFht/devin2api/internal/logvocab"
)

const (
	// diagExcerptLimit 是保留的错误摘要长度上限（字符）。
	diagExcerptLimit = 300
)

var (
	// sensitiveAssignmentPattern 匹配 k=v / "k": "v" 形式的敏感赋值，保留键名。
	// 键名交替由 logvocab.SecretKeys 派生（'_' 展开成 [\s_-]* 分隔匹配原文），
	// 并入本正则专属的宽松项——名单与 debuglog 的 JSON 脱敏同源，增删只改
	// 叶子包一处。
	sensitiveAssignmentPattern = regexp.MustCompile(`(?i)(["']?(?:` + sensitiveKeyAlternation() + `)["']?\s*[:=]\s*)(?:(?:bearer|basic)\s+[^\s,;]+|"(?:\\.|[^"])*"|'(?:\\.|[^'])*'|[^\s,;&}\]]+)`)
	bearerPattern              = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[^\s,;]+`)
	urlUserinfoPattern         = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s@]+@`)
	httpStatusPattern          = regexp.MustCompile(`(?i)\bstatus(?:\s+code)?\s*[:=]?\s*([1-5][0-9]{2})\b`)
)

// sensitiveKeyAlternation 展开脱敏键名为 sensitiveAssignmentPattern 的
// 交替段：logvocab.SecretKeys 的规范形态把 '_' 换成 [\s_-]* 以匹配
// 原文键名（cookie 一项同时覆盖 set-cookie 子串命中，反过来
// set[\s_-]*cookie 也兜住键内带分隔的变体）。末尾并入本正则专属的
// 宽松项：id_token/proxy_authorization 是常见凭据形，credential/
// secret/fingerprint 是词缀兜底——自由文本诊断的口径宁宽勿漏，
// 这些项不进 JSON 键名名单（宽匹配会把客户端负载里的同名短键误伤）。
func sensitiveKeyAlternation() string {
	alternatives := make([]string, 0, len(logvocab.SecretKeys)+5)
	for _, key := range logvocab.SecretKeys {
		alternatives = append(alternatives, strings.ReplaceAll(key, "_", `[\s_-]*`))
	}
	alternatives = append(alternatives,
		`id[\s_-]*token`, `proxy[\s_-]*authorization`,
		`credential`, `secret`, `fingerprint`)
	return strings.Join(alternatives, "|")
}

// Diagnostic 把 err 收敛为单行有界文本：白名单信号 + 脱敏截断摘要。
// 永不原样输出自由文本——上游/provider 错误体可能携带凭据。
func Diagnostic(err error) string {
	if err == nil {
		return ""
	}
	signals := make([]string, 0, 4)
	appendSignal := func(s string) {
		for _, existing := range signals {
			if existing == s {
				return
			}
		}
		signals = append(signals, s)
	}

	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		appendSignal("unexpected_EOF")
	case errors.Is(err, io.EOF):
		appendSignal("EOF")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		appendSignal("timeout")
	}
	if errors.Is(err, context.Canceled) {
		appendSignal("canceled")
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			appendSignal("timeout")
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			appendSignal("dns")
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op != "" {
			appendSignal("net_" + opErr.Op)
		}
	}
	var connErr *connect.Error
	if errors.As(err, &connErr) {
		appendSignal("connect_" + connErr.Code().String())
	}
	if match := httpStatusPattern.FindStringSubmatch(err.Error()); len(match) == 2 {
		appendSignal("status_" + match[1])
	}

	// 摘要：压成单行、脱敏、按字符截断。
	excerpt := strings.Join(strings.Fields(err.Error()), " ")
	excerpt = urlUserinfoPattern.ReplaceAllString(excerpt, `${1}[REDACTED]@`)
	excerpt = sensitiveAssignmentPattern.ReplaceAllString(excerpt, `${1}"[REDACTED]"`)
	excerpt = bearerPattern.ReplaceAllString(excerpt, `${1} [REDACTED]`)
	excerpt = truncateRunes(excerpt, diagExcerptLimit)

	if len(signals) == 0 {
		return excerpt
	}
	return strings.Join(signals, " ") + " | " + excerpt
}

// truncateRunes 按字符数截断，避免在多字节 UTF-8 中间断开。
func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + "…"
}
