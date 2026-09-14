// 本文件是面板内部的通用小工具：枚举名缩短、any 类型提取、截断。
package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
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
// 错误文案）里会漏出，读路径再按当前 token 字面值过一遍。
func (h *Handler) maskToken(data []byte) []byte {
	token := h.tokenFunc()
	if token == "" || len(data) == 0 {
		return data
	}
	return bytes.ReplaceAll(data, []byte(token), []byte("<redacted>"))
}

// shortEnum 剥掉生成枚举名的长前缀（ExaCodeiumCommonPb_X_），只留可读尾段。
// prefix 参数是调用方知道的确切前缀；前面的表是兜底匹配。
func shortEnum(full, prefix string) string {
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
		"ExaCodeiumCommonPb_Model_",
		"MODEL_PROVIDER_", "API_PROVIDER_", "MODEL_PRICING_TYPE_", "MODEL_COST_TIER_",
		"MODEL_DIMENSION_KIND_", "STATUS_LEVEL_", "MODEL_STATUS_", "TEAMS_TIER_", "BILLING_STRATEGY_",
		"MODEL_",
		prefix,
	} {
		if p == "" {
			continue
		}
		if idx := strings.Index(full, p); idx >= 0 {
			return full[idx+len(p):]
		}
	}
	if i := strings.LastIndex(full, "_"); i >= 0 && i+1 < len(full) {
		return full[i+1:]
	}
	return full
}

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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
