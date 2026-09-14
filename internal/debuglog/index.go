// 本文件实现跨请求索引：logs/index.jsonl 每完成一个请求追加一行摘要。
//
// 有了索引后，定位请求从「遍历目录逐个翻 meta.json」变成一次 grep；
// 面板也可直接消费它渲染请求列表。
package debuglog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
)

// indexFileCap 是 index.jsonl 的体积上限；超限后保留尾部一半重写。
// 取值与启动回放窗口一致，保证聚合重建永远覆盖全文件。
// var 而非 const：测试临时缩小它来覆盖截断路径。
var indexFileCap int64 = usageReplayTailBytes

// IndexEntry 是 index.jsonl 中一行请求的摘要。
// 字段选择面向「grep 定位 + 面板列表」两个用途。
type IndexEntry struct {
	Dir        string `json:"dir"`
	StartedAt  string `json:"started_at"`
	DurationMS int64  `json:"duration_ms"`
	// 延迟分解字段用指针区分「未发生」（nil，省略）与「即时发生」（0ms）；
	// int 零值会掩盖这两种语义。五段口径见 recorder.go 同名字段注释：
	// ready→sent 本地投影、sent→open 建流往返、open→first_upstream 上游
	// 思考 TTFT、first_upstream→first_client 代理编码下发。
	RequestReadyMS    *int64 `json:"request_ready_ms,omitempty"`
	UpstreamSentMS    *int64 `json:"upstream_sent_ms,omitempty"`
	UpstreamOpenMS    *int64 `json:"upstream_open_ms,omitempty"`
	FirstUpstreamMS   *int64 `json:"first_upstream_ms,omitempty"`
	FirstClientMS     *int64 `json:"first_client_ms,omitempty"`
	API               string `json:"api,omitempty"`
	Method            string `json:"method"`
	Path              string `json:"path"`
	StatusCode        int    `json:"status_code"`
	Result            string `json:"result"`
	RequestedModel    string `json:"requested_model,omitempty"`
	Model             string `json:"model,omitempty"`
	ResponseModel     string `json:"response_model,omitempty"`
	ModelMismatch     bool   `json:"model_mismatch,omitempty"`
	Stream            bool   `json:"stream"`
	InputTokens       int64  `json:"input_tokens,omitempty"`
	OutputTokens      int64  `json:"output_tokens,omitempty"`
	CacheReadTokens   int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens  int64  `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   int64  `json:"reasoning_tokens,omitempty"`
	TotalTokens       int64  `json:"total_tokens,omitempty"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	ClientIP          string `json:"client_ip,omitempty"`
	KeyHash           string `json:"key_hash,omitempty"`
	// ClientRequestID 是客户端自带的关联 ID（X-Request-Id 等），
	// 让调用方能用自己的 ID 反查本次请求。
	ClientRequestID string `json:"client_request_id,omitempty"`
	// ErrorStage 是首个失败阶段（http_decode/provider_stream/http_stream 等），
	// 让 grep 直接定位失败发生在哪一层。
	ErrorStage    string `json:"error_stage,omitempty"`
	DroppedEvents uint64 `json:"dropped_events,omitempty"`
	// RetryAfterSeconds 是上游限流给出的 reset 秒数 hint，
	// 供聚合区分「有退避提示的限流」与「裸限流」；非限流请求为 0。
	RetryAfterSeconds int64 `json:"retry_after_seconds,omitempty"`
	// RateLimited 标记本请求被限流语义终结（上游 429 / 本地闸门 / 流内
	// 限流错误事件）。HTTP 状态码认不全限流——流内下发的限流仍是 200，
	// 责任归因与限流采样用本字段而不是 status_code。
	RateLimited bool `json:"rate_limited,omitempty"`
	// Retries 是上游重发次数（attempt2+，token 自愈/空响应/transport
	// 重开）；明细在同目录 meta.json 的 retry_attempts 与 04 的
	// retry_attempt 分界行。0 表示一次发送完成。
	Retries int `json:"retries,omitempty"`
	// PrematureEndTurn 标记「工具结果之后模型纯文本 end_turn」的可疑收尾，
	// 供 grep 统计该模型行为的真实频率（见 Completion 同名字段）。
	PrematureEndTurn bool `json:"premature_end_turn,omitempty"`
	// Repairs 是请求投影为上游 wire 格式时的静默修复动作总数
	//（重排/降级/剥离/指纹改写），明细在同名 meta.json 字段。
	Repairs int `json:"repairs,omitempty"`
}

// appendIndex 在请求完成后把摘要写入 index.jsonl。
// 每行一次 Flush：索引是排障证据，崩溃后也不能丢尾巴。
func (manager *Manager) appendIndex(recorder *Recorder, completion *Completion) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if manager.indexWriter == nil {
		file, err := os.OpenFile(filepath.Join(manager.root, "index.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			manager.ioErrors.Add(1)
			return
		}
		manager.indexFile = file
		manager.indexWriter = bufio.NewWriter(file)
		if info, statErr := file.Stat(); statErr == nil {
			manager.indexBytes = info.Size()
		}
	}
	entry := IndexEntry{
		Dir:               filepath.Base(recorder.directory),
		StartedAt:         recorder.startedAt.Format(time.RFC3339Nano),
		DurationMS:        time.Since(recorder.startedAt).Milliseconds(),
		RequestReadyMS:    optionalLatency(recorder.requestReadyMS.Load()),
		UpstreamSentMS:    optionalLatency(recorder.upstreamSentMS.Load()),
		UpstreamOpenMS:    optionalLatency(recorder.upstreamOpenMS.Load()),
		FirstUpstreamMS:   optionalLatency(recorder.firstUpstreamMS.Load()),
		FirstClientMS:     optionalLatency(recorder.firstClientMS.Load()),
		API:               recorder.requestMeta.API,
		Method:            recorder.requestMeta.Method,
		Path:              recorder.requestMeta.Path,
		StatusCode:        completion.StatusCode,
		Result:            completion.Result,
		RequestedModel:    completion.RequestedModel,
		Model:             completion.Model,
		ResponseModel:     completion.ResponseModel,
		ModelMismatch:     completion.ModelMismatch,
		Stream:            completion.Stream,
		InputTokens:       completion.Usage.Input,
		OutputTokens:      completion.Usage.Output,
		CacheReadTokens:   completion.Usage.CacheRead,
		CacheWriteTokens:  completion.Usage.CacheWrite,
		ReasoningTokens:   reasoningTokens(completion.Usage),
		TotalTokens:       completion.Usage.TotalTokens,
		UpstreamRequestID: completion.UpstreamRequestID,
		ClientIP:          recorder.requestMeta.ClientIP,
		KeyHash:           recorder.requestMeta.KeyHash,
		ClientRequestID:   recorder.requestMeta.ClientRequestID,
		ErrorStage:        recorder.errorStage,
		DroppedEvents:     recorder.dropped.Load(),
		RetryAfterSeconds: recorder.retryAfterSeconds.Load(),
		RateLimited:       recorder.rateLimited.Load(),
		Retries:           len(recorder.retryAttempts()),
		PrematureEndTurn:  completion.PrematureEndTurn,
	}
	if repairs := recorder.repairs.Load(); repairs != nil {
		entry.Repairs = repairs.Total()
	}
	data, err := json.Marshal(entry)
	if err != nil {
		manager.ioErrors.Add(1)
		return
	}
	if _, err := manager.indexWriter.Write(data); err != nil {
		manager.ioErrors.Add(1)
		return
	}
	_ = manager.indexWriter.WriteByte('\n')
	if err := manager.indexWriter.Flush(); err != nil {
		manager.ioErrors.Add(1)
		return
	}
	manager.indexBytes += int64(len(data) + 1)
	// 回放快照落定前完成的请求不单独入账：其索引行已在回放快照内，
	// 由回放统一计入；落定后的行快照不可见，必须由实时路径累加——
	// 闸门保证任一行恰入账一次（见 NewManager 的回放协程）。
	if manager.indexSnapshotted {
		manager.usage.add(entry)
	}
	if manager.indexBytes > indexFileCap {
		manager.truncateIndexLocked()
	}
}

// truncateIndexLocked 把 index.jsonl 截到尾部一半大小；调用方持有 mutex。
// 截断失败只记 ioErrors：写入器重置为惰性重开，索引继续追加不受影响。
func (manager *Manager) truncateIndexLocked() {
	if err := manager.indexWriter.Flush(); err != nil {
		manager.ioErrors.Add(1)
	}
	_ = manager.indexFile.Close()
	manager.indexWriter = nil
	manager.indexFile = nil
	path := filepath.Join(manager.root, "index.jsonl")
	data, err := tailRead(path, indexFileCap/2)
	if err == nil {
		// 截断点可能落在行中间，丢弃首行残段。
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			data = data[idx+1:]
		}
		err = os.WriteFile(path, data, 0o600)
	}
	if err != nil {
		manager.ioErrors.Add(1)
		return
	}
	manager.indexBytes = int64(len(data))
}

// reasoningTokens 展开 Usage.Reasoning 指针为整数值。
func reasoningTokens(usage llm.Usage) int64 {
	if usage.Reasoning == nil {
		return 0
	}
	return *usage.Reasoning
}

// optionalLatency 把 -1 哨兵转成 nil，其余原样透传（含合法的 0ms）。
func optionalLatency(ms int64) *int64 {
	if ms < 0 {
		return nil
	}
	return &ms
}

// releaseDir 把目录移出活跃集合，允许清理器回收它。
func (manager *Manager) releaseDir(directory string) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	delete(manager.activeDirs, filepath.Base(directory))
}
