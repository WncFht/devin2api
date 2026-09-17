package common

import (
	"net/http"
	"strings"
)

// sessionAffinityHeaders 是客户端显式声明会话身份的头，按优先级排列。
// 头是调用方的显式意图，恒赢于 body 侧提取（metadata.user_id /
// prompt_cache_key|user）——显式声明与会话内容推断冲突时以声明为准。
// X-Client-Request-Id 刻意不采：请求唯一 ID 每轮都变，当亲和键反而
// 破坏会话归并。X-Claude-Code-Agent-Id 不进链：子代理与父会话同 lane
// （共享上游缓存谱系），不该给它独立键。
var sessionAffinityHeaders = []string{
	"X-Claude-Code-Session-Id",
	"X-Session-ID",
	"X-Session-Affinity",
	"X-Conversation-Id",
	"X-Thread-Id",
}

// SessionKeyFromHeader 按优先级链取首个非空会话亲和头；全空返回 ""，
// 调用方回落到 body 提取逻辑。
func SessionKeyFromHeader(header http.Header) string {
	for _, name := range sessionAffinityHeaders {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}
