// 本文件提供三个前端请求解码共用的微策略：可选正整数字段归一、会话缓存键
// 选择、SystemPrompt 拼接与未知 role 降级。同一判定散在各前端抄写会随
// 演进漂移，下沉单源。
package common

import (
	"encoding/json"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
)

// PositiveIntOrDrop 归一可选正整数字段（max_tokens/top_k 一类）：nil 缺席
// 返回 nil；正数返回原指针交给 IR；显式非正值是客户端给了但不生效的输入，
// 记 marker 进 dropped 透出而非静默吞掉。
func PositiveIntOrDrop(value *int, dropped *[]string, marker string) *int {
	if value == nil {
		return nil
	}
	if *value > 0 {
		return value
	}
	*dropped = append(*dropped, marker)
	return nil
}

// SessionKey 选会话缓存键：prompt_cache_key 优先，空则退回 user——
// 上游按会话键命中 prompt cache，chat/responses 两前端同口径。
func SessionKey(promptCacheKey, user string) string {
	if promptCacheKey != "" {
		return promptCacheKey
	}
	return user
}

// AppendSystemPrompt 把一段系统提示词并进现有 SystemPrompt：两段都非空
// 时用单个 \n 分隔——chat 多 system 消息、anthropic system 块数组、
// responses system/developer message 三处同口径。
func AppendSystemPrompt(existing, text string) string {
	if existing != "" && text != "" {
		return existing + "\n" + text
	}
	return existing + text
}

// DemotedRoleMessage 给不识 role 的消息造降级 USER 文本：整条静默丢弃
// 会丢上下文，role 标记加内容原文保住信息（与 responses 前端对未知
// input item type 的口径一致）。content 是消息原文 content 字段——
// 纯字符串剥引号取文本，其余形态（part 数组/缺席）原样 JSON。
func DemotedRoleMessage(role string, content json.RawMessage) llm.UserMessage {
	var text string
	if json.Unmarshal(content, &text) != nil {
		text = string(content)
	}
	return llm.UserMessage{
		Content:     []llm.Content{llm.TextContent{Text: "[message role=" + role + "]\n" + text}},
		TimestampMS: time.Now().UnixMilli(),
	}
}
