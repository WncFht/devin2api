// 本文件实现跨请求索引：logs/index.jsonl 每完成一个请求追加一行摘要。
//
// 有了索引后，定位请求从「遍历目录逐个翻 meta.json」变成一次 grep；
// 面板也可直接消费它渲染请求列表。
package debuglog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
)

// IndexEntry 是 index.jsonl 中一行请求的摘要。
// 字段选择面向「grep 定位 + 面板列表」两个用途。
type IndexEntry struct {
	Dir        string `json:"dir"`
	StartedAt  string `json:"started_at"`
	DurationMS int64  `json:"duration_ms"`
	// FirstUpstreamMS/FirstClientMS 用指针区分「未发生」（nil，省略）
	// 与「即时发生」（0ms）；int 零值会掩盖这两种语义。
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
	}
	entry := IndexEntry{
		Dir:               filepath.Base(recorder.directory),
		StartedAt:         recorder.startedAt.Format(time.RFC3339Nano),
		DurationMS:        time.Since(recorder.startedAt).Milliseconds(),
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
	manager.usage.add(entry)
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
