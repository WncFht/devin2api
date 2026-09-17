// 本文件集中定义请求调试目录内的阶段文件名与 logs 根目录下的共享文件名。
//
// 这是一份跨包契约：app/adapter 是写入侧，cleaner/dashboard/protocensus
// 是读取侧，两处各自拼写字面量会随演进静默对不上（清理漏剥、面板读空）。
// 入库后这些「文件名」是 debug_files/debug_chunks 行的 name 键，形状不变。
package debuglog

import (
	"fmt"
)

const (
	// StageHTTPRequest 是客户端原始请求投影。
	StageHTTPRequest = "01-http-request.json"
	// StageRequestMessages 是中间模型投影。
	StageRequestMessages = "02-request-messages.json"
	// StageDevinRequest 是首个上游 wire 请求；重试分片见 StageDevinRequestAttempt。
	StageDevinRequest = "03-devin-request.json"
	// StageDevinResponse 是上游原始响应帧。
	StageDevinResponse = "04-devin-response.jsonl"
	// StageResponseEvents 是内部响应事件流。
	StageResponseEvents = "05-response-events.jsonl"
	// StageHTTPResponse 是下发客户端的 SSE 帧。
	StageHTTPResponse = "06-http-response.jsonl"

	// AttachmentsDir 是请求目录内的附件子目录名。
	AttachmentsDir = "attachments"
	// MetaFile 是请求元信息文件（创建时与完结时各写一次）。
	MetaFile = "meta.json"
	// ErrorFile 记录首个失败点；容量淘汰按它识别失败目录。
	ErrorFile = "error.json"
	// StderrFile 是进程 stderr 日志（slog 行），部署脚本负责重定向写入。
	StderrFile = "stderr.log"
	// BindFailureFile 记录最近一次 listen 绑定失败（reuseport 交接争抢等），
	// main 侧写、Stats 侧读，面板能直接看到「上次为什么没绑上」。
	BindFailureFile = "bind-failure.json"
)

// 错误阶段名是另一条轴的跨包契约：WriteError/writeLoggedError 的 stage
// 实参、IndexEntry.ErrorStage、error.json 的 stage 字段共用这组取值——
// 写侧在 app/adapter，读侧在 usage 聚合与面板 error_stage 筛选，
// 字面量漂移会让「按失败点检索」静默失配。
const (
	// ErrStageHTTPRead 是请求体读取失败（超限/连接中断）。
	ErrStageHTTPRead = "http_read"
	// ErrStageHTTPDecode 是请求体解码或中间层校验失败。
	ErrStageHTTPDecode = "http_decode"
	// ErrStageRequestBuild 是本地请求投影失败（tool_choice 指空等参数校验），
	// 请求未触达上游——既不是上游语义拒绝也不是传输断裂。
	ErrStageRequestBuild = "request_build"
	// ErrStageProviderStream 是上游流式响应中途失败（上游责任或语义拒绝）。
	ErrStageProviderStream = "provider_stream"
	// ErrStageHTTPStream 是下发客户端的 SSE 写出失败（非断连类）。
	ErrStageHTTPStream = "http_stream"
	// ErrStageResponseEvent 是内部事件投影为协议帧时的失败。
	ErrStageResponseEvent = "response_event"
	// ErrStageHTTPEncode 是响应体序列化失败。
	ErrStageHTTPEncode = "http_encode"
	// ErrStageClientDisconnected 是客户端断连/取消终止了请求。
	ErrStageClientDisconnected = "client_disconnected"
	// ErrStageDevinConnect 是上游语义拒绝（参数/权限/限流的 Connect 层错误）。
	ErrStageDevinConnect = "devin_connect"
	// ErrStageDevinTransport 是上游传输断裂（EOF/帧截断，非语义响应）。
	ErrStageDevinTransport = "devin_transport"
	// ErrStageRateGate 是本地速率闸门快败，请求未触达上游。
	ErrStageRateGate = "rate_gate"
	// ErrStageTokenLimit 是下游令牌准入拒绝（并发/模型白名单/费用限额），
	// 请求未触达上游；与管线前拒绝不同——它发生在读体解码后，留有调试目录。
	ErrStageTokenLimit = "token_limit"
	// ErrStageModelDisabled 是模型注册表准入拒绝（模型被停用），
	// 请求未触达上游；同样发生在解码后，留有调试目录。
	ErrStageModelDisabled = "model_disabled"
	// ErrStagePrePipeline 是管线前拒绝（鉴权 401/并发 429/排空 503/
	// WS 准入/读体中断）：请求从未进入处理管线，无调试目录，
	// logs 行仅作留存检索（log_source=rejected 与服役流量分域）。
	ErrStagePrePipeline = "pre_pipeline"
)

// devinRequestStageStem 是上游请求文件名的公共词干：首个请求是
// 03-devin-request.json，第 N 次重发是 03-devin-request.attemptN.json——
// 词干加 "." 前缀匹配可同时圈出主文件与全部重试分片。
const devinRequestStageStem = "03-devin-request"

// StageDevinRequestStem 导出 devinRequestStageStem：托管搜索的调用方
// 需要词干（不带 .json）派生自己的请求文件名。
const StageDevinRequestStem = devinRequestStageStem

// StageDevinRequestAttempt 返回第 attempt 次（attempt>=2）上游重发的
// 请求文件名；与首个请求的 StageDevinRequest 共享 devinRequestStageStem。
func StageDevinRequestAttempt(attempt int) string {
	return fmt.Sprintf("%s.attempt%d.json", devinRequestStageStem, attempt)
}

// StageDevinSearchStem 是托管搜索调用请求文件名的词干工厂：seq 为请求内
// 的搜索调用序号（跨续轮跳与跨域统一递增），返回词干如
// "03-devin-request.search3"，调用方拼 ".json" / ".attemptN.json"。
// Flow B 续轮里 chat 重发占用了 attemptN 编号空间，搜索调用必须用独立
// 词干才不覆盖 chat 请求记录；共享 devinRequestStageStem 前缀让
// payload 剥离与 DevinRequestStages 枚举自动覆盖这些文件。
const StageDevinSearchStem = devinRequestStageStem + ".search"
