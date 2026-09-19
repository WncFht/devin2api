// 本文件定义供应商适配器与中间 LLM 模型之间的边界。
//
// Package adapter 定义供应商适配器与中间 LLM 模型之间的边界。
package adapter

import (
	"context"
	"errors"

	"github.com/WncFht/devin2api/internal/llm"
)

// ErrStaleCatalog 是 ListModels 的陈旧兜底哨兵：目录刷新失败但旧缓存
// 尚在时，实现返回 (非空 models, 包裹本哨兵的 error)——models 是可用的
// 过期快照，error 让「本次刷新其实失败了」不被 nil 吞掉。按 err!=nil
// 即拒绝服务的调用方会把可用陈旧目录误当全损，必须 errors.Is 放行；
// 号池这类要记 lane 失败证据的调用方靠它区分「真没拉到」与「兜底成功」。
var ErrStaleCatalog = errors.New("adapter: serving stale model catalog")

// ModelInfo 是对外暴露的模型目录条目（OpenAI /v1/models 形状）。
type ModelInfo struct {
	// ID 是模型标识（OpenAI model id / Devin model_uid）。
	ID string
	// Created 是目录条目的 Unix 秒时间戳；未知时可为 0。
	Created int64
	// OwnedBy 是模型归属方展示名。
	OwnedBy string
	// SupportsImages 表示该模型是否支持多模态图片输入；目录未知时为 false。
	SupportsImages bool
	// SupportsDocuments 表示该模型是否支持文档输入（上游 modelFeatures
	// 的 supports_documents 能力位，独立于 supports_images）；目录未知时为 false。
	SupportsDocuments bool
	// SupportsVideo 表示该模型是否支持视频输入（上游 modelFeatures 的
	// supports_video 能力位；supports_video_urls 在本账号目录与其恒共线，
	// 未单列字段）；目录未知时为 false。
	SupportsVideo bool
	// SupportsToolCalls 表示该模型是否支持工具调用；目录未声明时为 false。
	SupportsToolCalls bool
	// SupportsParallelToolCalls 表示该模型是否支持同轮并行工具调用；目录未声明时为 false。
	SupportsParallelToolCalls bool
	// SupportsThinking 表示该模型是否产出 thinking 内容。
	SupportsThinking bool
	// PreserveThinking 表示该模型的 thinking 是否需要在后续轮次中原样回放。
	PreserveThinking bool
	// IsModelRouter 表示该 uid 是上游路由器而非具体模型：
	// 直连 GetChatMessage 会被上游以 unavailable 拒绝，需先 AssignModel 解析。
	IsModelRouter bool
	// ContextTokens 是上游声明的上下文窗口 token 数；未知为 0。
	ContextTokens int
	// MaxOutputTokens 是上游声明的单次输出 token 上限；未知为 0。
	MaxOutputTokens int
	// AliasOf 非空表示该条目是客户端别名而非真实 uid：以 ID 发出
	// 的请求实际改写到 AliasOf 运行，能力位继承自目标条目。
	AliasOf string
}

// Adapter 将供应商无关的请求上下文转换为具体供应商调用，并返回有序响应流。
type Adapter interface {
	// Stream 开始一次或多次助手响应的流式生成。
	Stream(context.Context, llm.RequestMessages) (llm.ResponseStream, error)
	// ListModels 返回当前账号可用的模型目录；失败时返回错误。刷新失败
	// 但旧缓存尚在时可返回非空 models 加 errors.Is(ErrStaleCatalog) 的
	// 错误——调用方应照旧下发 models，同时能拿到本次刷新失败的信号。
	ListModels(context.Context) ([]ModelInfo, error)
}
