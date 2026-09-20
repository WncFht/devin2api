// 本文件是面板内部的通用小工具：枚举名缩短、any 类型提取、截断、
// 上游 token 字面值兜底脱敏。
package ccpanel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// remoteIP 返回请求来源 IP（去端口）；登录限速按它归并。
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// maskToken 对回显给面板的日志字节做字面值兜底脱敏：写路径的 secretKey
// 名单只能覆盖结构化键名，token 若出现在自由文本（请求 body 原文、上游
// 错误文案）里会漏出，读路径再按最近见过的 token 字面值过一遍——
// 自愈轮换后旧 token 仍可能躺在旧请求目录里。号池下脱敏集合收编全部
// lane 的当前凭据：漏遮任一号的 token 都是泄露。
func (h *Handler) maskToken(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	tokens := make([]string, 0, 2)
	tokens = append(tokens, h.tokenFunc())
	if ps, ok := h.poolSnapshot(); ok {
		for _, fn := range ps.TokenFuncs {
			tokens = append(tokens, fn())
		}
	}
	for _, token := range h.maskTokenSet(tokens) {
		if token == "" {
			continue
		}
		// token 含 JSON 转义字符时在 JSON 文本（meta.json 的字符串值）里以
		// 转义形态出现，只换原始字节会静默漏遮——先替换 json.Marshal
		// 产出的转义形态再替换原始形态：顺序不能反，以 \ 结尾的 token
		// 原始形态是转义形态的前缀，先吃原始形态会留下孤立反斜杠，
		// 既漏遮又破坏 JSON 转义。不含转义字符的 token 两形态恒等，
		// 跳过 marshal 与第二次扫描。
		if strings.IndexFunc(token, jsonEscapable) >= 0 {
			if escaped, err := json.Marshal(token); err == nil {
				if esc := escaped[1 : len(escaped)-1]; !bytes.Equal(esc, []byte(token)) && bytes.Contains(data, esc) {
					data = bytes.ReplaceAll(data, esc, []byte("<redacted>"))
				}
			}
		}
		// Contains 预扫跳过未命中：ReplaceAll 未命中也返回全量拷贝，
		// 绝大多数 payload 不含 token，逐 token 白扫一份 MB 级字节。
		if bytes.Contains(data, []byte(token)) {
			data = bytes.ReplaceAll(data, []byte(token), []byte("<redacted>"))
		}
	}
	return data
}

// jsonEscapable 报告 rune 是否会被 json.Marshal 默认（HTML 安全）转义——
// 与 encoding/json 的 escape 表一致，maskToken 据此决定是否要算转义形态。
func jsonEscapable(r rune) bool {
	return r < 0x20 || r == '"' || r == '\\' || r == '<' || r == '>' || r == '&' ||
		r == '\u2028' || r == '\u2029'
}

// maskTokenSet 把本批新见过的 token 记入 recentTokens 环（去重、保留
// 最近 16 个——号池下每号各占若干槽），返回脱敏要覆盖的字面值全集：
// 常驻播种集合与环的并集，同一把锁取齐。
func (h *Handler) maskTokenSet(tokens []string) []string {
	h.tokenMu.Lock()
	defer h.tokenMu.Unlock()
	for _, token := range tokens {
		if token == "" || slices.Contains(h.recentTokens, token) {
			continue
		}
		h.recentTokens = append([]string{token}, h.recentTokens...)
	}
	if len(h.recentTokens) > 16 {
		h.recentTokens = h.recentTokens[:16]
	}
	out := make([]string, 0, len(h.recentTokens)+len(h.seedTokens))
	out = append(out, h.recentTokens...)
	for token := range h.seedTokens {
		out = append(out, token)
	}
	return out
}

// shortEnum 剥掉生成枚举名的长前缀（ExaCodeiumCommonPb_X_），只留可读尾段。
// 表按精确到宽泛排序；全部落空时取最后一个 "_" 之后的段。
func shortEnum(full string) string {
	if full == "" {
		return ""
	}
	for _, p := range []string{
		"ExaCodeiumCommonPb_ModelProvider_MODEL_PROVIDER_",
		"ExaCodeiumCommonPb_APIProvider_API_PROVIDER_",
		"ExaCodeiumCommonPb_ModelPricingType_MODEL_PRICING_TYPE_",
		"ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_",
		"ExaCodeiumCommonPb_ModelDimensionKind_MODEL_DIMENSION_KIND_",
		"ExaCodeiumCommonPb_StatusLevel_STATUS_LEVEL_",
		"ExaCodeiumCommonPb_ModelStatus_MODEL_STATUS_",
		"ExaCodeiumCommonPb_TeamsTier_TEAMS_TIER_",
		"ExaCodeiumCommonPb_BillingStrategy_BILLING_STRATEGY_",
		"ExaCodeiumCommonPb_GracePeriodStatus_GRACE_PERIOD_STATUS_",
		"ExaCodeiumCommonPb_TransactionStatus_TRANSACTION_STATUS_",
		"ExaCodeiumCommonPb_Model_",
		"MODEL_PROVIDER_", "API_PROVIDER_", "MODEL_PRICING_TYPE_", "MODEL_COST_TIER_",
		"MODEL_DIMENSION_KIND_", "STATUS_LEVEL_", "MODEL_STATUS_", "TEAMS_TIER_", "BILLING_STRATEGY_",
		"GRACE_PERIOD_STATUS_", "TRANSACTION_STATUS_",
		"MODEL_",
	} {
		if idx := strings.Index(full, p); idx >= 0 {
			return full[idx+len(p):]
		}
	}
	if i := strings.LastIndex(full, "_"); i >= 0 && i+1 < len(full) {
		return full[i+1:]
	}
	return full
}

// strAny 返回首个非空字符串形态值：上游 protobuf JSON 里同一字段
// 在不同版本出现为 string/number，逐个候选取第一个可用的。
func strAny(vals ...any) string {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case json.Number:
			return t.String()
		case float64:
			return fmt.Sprintf("%.0f", t)
		default:
			s := fmt.Sprint(t)
			if s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}

// boolAny 返回首个可判真的值：bool 原样、字符串按 "true"/"1" 判真。
func boolAny(vals ...any) bool {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t
		case string:
			return t == "true" || t == "1"
		}
	}
	return false
}

// numAny 返回首个数值形态值（数值类型原样透传，非空字符串也接受——
// 上游偶发把数字序列化成字符串）。用于 JSON 里键名漂移的数值字段。
func numAny(vals ...any) any {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case float64, float32, int, int32, int64, json.Number:
			return t
		case string:
			if t != "" {
				return t
			}
		}
	}
	return nil
}

// rfc3339Any 把 Connect-JSON 的 Timestamp 字段（RFC3339 字符串）归一成
// UTC RFC3339；解析失败保留原文——外部输入边界上原样暴露比吞掉更可排障。
func rfc3339Any(vals ...any) string {
	s := strAny(vals...)
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return s
}

// truncate 把 s 截到至多 n 字节并以 "..." 结尾；不切在多字节 rune 中间，
// 不超原样返回。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
