// 本文件提供响应模型名的统一回显链。
package common

import "github.com/WncFht/devin2api/internal/llm"

// EchoModel 解析回显给客户端的模型名：优先请求原文（可能是别名），
// 其次上游声明的 actual uid，再到解析后的请求 uid，最后方言默认值。
// requested 为空表示调用方没有请求侧模型名可回显。
func EchoModel(requested string, message *llm.AssistantMessage, fallback string) string {
	if requested != "" {
		return requested
	}
	if message.ResponseModel != "" {
		return message.ResponseModel
	}
	if message.Model != "" {
		return message.Model
	}
	return fallback
}
