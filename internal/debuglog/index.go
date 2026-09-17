// 本文件实现跨请求索引：logs/index.jsonl 每完成一个请求追加一行摘要。
//
// 有了索引后，定位请求从「遍历目录逐个翻 meta.json」变成一次 grep；
// 面板也可直接消费它渲染请求列表。
package debuglog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

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
	RequestReadyMS   *int64 `json:"request_ready_ms,omitempty"`
	UpstreamSentMS   *int64 `json:"upstream_sent_ms,omitempty"`
	UpstreamOpenMS   *int64 `json:"upstream_open_ms,omitempty"`
	FirstUpstreamMS  *int64 `json:"first_upstream_ms,omitempty"`
	FirstClientMS    *int64 `json:"first_client_ms,omitempty"`
	API              string `json:"api,omitempty"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	StatusCode       int    `json:"status_code"`
	Result           string `json:"result"`
	RequestedModel   string `json:"requested_model,omitempty"`
	Model            string `json:"model,omitempty"`
	ResponseModel    string `json:"response_model,omitempty"`
	ModelMismatch    bool   `json:"model_mismatch,omitempty"`
	Stream           bool   `json:"stream"`
	InputTokens      int64  `json:"input_tokens,omitempty"`
	OutputTokens     int64  `json:"output_tokens,omitempty"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64  `json:"reasoning_tokens,omitempty"`
	TotalTokens      int64  `json:"total_tokens,omitempty"`
	// CreditCost 是上游帧上报的权威计费读数（单请求口径，可加总）；
	// Committed* 系账户快照读数不进索引——逐请求保留见 meta.json。
	CreditCost        int64  `json:"credit_cost,omitempty"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	ClientIP          string `json:"client_ip,omitempty"`
	KeyHash           string `json:"key_hash,omitempty"`
	// ClientRequestID 是客户端自带的关联 ID（X-Request-Id 等），
	// 让调用方能用自己的 ID 反查本次请求。
	ClientRequestID string `json:"client_request_id,omitempty"`
	// ErrorStage 是首个失败阶段（http_decode/provider_stream/http_stream 等），
	// 让 grep 直接定位失败发生在哪一层。只在终结性失败（result!=completed）
	// 时落盘：中途被重试救回的错误仍留在目录 error.json 与 retry_attempts
	// 里，进索引会把「发生过失败」与「请求失败」混成一桶。
	ErrorStage string `json:"error_stage,omitempty"`
	// ErrorMessage 是首个失败的错误文案（与 error.json 的 message 同源，
	// 截断至 errorMessageCap 字节）。目录被保留策略淘汰后，索引行仍能
	// 回答「为什么败」——此前只剩阶段名，归因必须靠目录在场。
	ErrorMessage  string `json:"error_message,omitempty"`
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
	// ConnReused 标记成功建流那次发送是否复用了 idle 连接；指针是为了
	// 区分「未记录」（nil，省略）与「复用失败新建」（false）——connect
	// 段偏高时靠它区分「握手成本」与「上游响应头延迟」。
	ConnReused *bool  `json:"conn_reused,omitempty"`
	ConnIdleMS *int64 `json:"conn_idle_ms,omitempty"`
}

// errorMessageCap 是 index 行 error_message 的截断字节数：保留首个失败
// 的可归因文本，又不让超大错误文案把索引行撑变形。
const errorMessageCap = 300

// appendIndex 在请求完成后把摘要写入 index.jsonl。
// 每行一次 Flush：索引是排障证据，进程崩溃也不能丢尾巴（Flush 只到
// 内核页缓存——断电级故障不在担保范围）。
// 条目序列化在锁外完成；indexMu 只罩住 index.jsonl 自身的 IO 与
// 快照闸门——索引磁盘停滞不堵目录分配（见 manager.mutex 注释）。
func (manager *Manager) appendIndex(recorder *Recorder, completion *Completion) {
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
		CreditCost:        creditCost(completion.Usage),
		UpstreamRequestID: completion.UpstreamRequestID,
		ClientIP:          recorder.requestMeta.ClientIP,
		KeyHash:           recorder.effectiveKeyHash(),
		ClientRequestID:   recorder.requestMeta.ClientRequestID,
		DroppedEvents:     recorder.dropped.Load(),
		RetryAfterSeconds: recorder.retryAfterSeconds.Load(),
		RateLimited:       recorder.rateLimited.Load(),
		Retries:           len(recorder.retryAttempts()),
		PrematureEndTurn:  completion.PrematureEndTurn,
	}
	if completion.Result != "completed" {
		// error 字段只对终结性失败出账：被重试救回的中间错误留在目录
		// error.json 与 meta.retry_attempts，不污染按失败点检索的口径。
		if stage, message := recorder.FirstError(); stage != "" {
			entry.ErrorStage = stage
			entry.ErrorMessage = truncateRunes(message, errorMessageCap)
		}
	}
	if conn := recorder.upstreamConn.Load(); conn != nil {
		entry.ConnReused = &conn.reused
		entry.ConnIdleMS = &conn.idleMS
	}
	if repairs := recorder.repairs.Load(); repairs != nil {
		entry.Repairs = repairs.Total()
	}
	data, err := json.Marshal(entry)
	if err != nil {
		manager.ioErrors.Add(1)
		return
	}
	manager.indexMu.Lock()
	defer manager.indexMu.Unlock()
	if manager.indexWriter == nil {
		file, err := os.OpenFile(filepath.Join(manager.root, IndexFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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
	if manager.indexSnapshotted.Load() {
		manager.usage.add(entry)
	}
	if manager.indexBytes > indexFileCap {
		manager.truncateIndexLocked()
	}
}

// truncateIndexLocked 把 index.jsonl 截到尾部一半大小；调用方持有 indexMu。
// 截断失败只记 ioErrors：写入器重置为惰性重开，索引继续追加不受影响。
func (manager *Manager) truncateIndexLocked() {
	if err := manager.indexWriter.Flush(); err != nil {
		manager.ioErrors.Add(1)
	}
	_ = manager.indexFile.Close()
	manager.indexWriter = nil
	manager.indexFile = nil
	path := filepath.Join(manager.root, IndexFile)
	kept, err := TruncateToTail(path, indexFileCap/2)
	if err != nil {
		manager.ioErrors.Add(1)
		return
	}
	manager.indexBytes = kept
}

// reasoningTokens 展开 Usage.Reasoning 指针为整数值。
func reasoningTokens(usage llm.Usage) int64 {
	if usage.Reasoning == nil {
		return 0
	}
	return *usage.Reasoning
}

// creditCost 展开 Usage.Costs 的单请求计费读数；上游未上报时为 0。
func creditCost(usage llm.Usage) int64 {
	if usage.Costs == nil {
		return 0
	}
	return usage.Costs.CreditCost
}

// optionalLatency 把 -1 哨兵转成 nil，其余原样透传（含合法的 0ms）。
func optionalLatency(ms int64) *int64 {
	if ms < 0 {
		return nil
	}
	return &ms
}

// truncateRunes 按字节截断到 cap，但不在多字节 rune 中间切断。
func truncateRunes(s string, cap int) string {
	if len(s) <= cap {
		return s
	}
	cut := cap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ScanIndex 从 offset 起增量扫描 index.jsonl 的完整行，逐条交给 fn。
// 文件被截断重建（size<offset，见 truncateIndexLocked）时先调 resetFn
// （可为 nil，调用方在此丢弃旧派生状态），随后从 0 重扫整个文件。
// 返回下一次调用应传入的偏移。
//
// 并发安全建立在写路径的 Flush 粒度上：appendIndex 每条都是
// 完整行+换行后一次 Flush，读者永远看不到半行；扫描不持锁，
// 截断恰好落在 stat 与读之间的极小窗口由「只处理到最后一个换行」兜底。
func (manager *Manager) ScanIndex(offset int64, resetFn func(), fn func(IndexEntry)) (int64, error) {
	if manager == nil || manager.root == "" {
		return 0, os.ErrNotExist
	}
	path := filepath.Join(manager.root, IndexFile)
	info, err := os.Stat(path)
	if err != nil {
		return offset, err
	}
	if info.Size() < offset {
		if resetFn != nil {
			resetFn()
		}
		offset = 0
	}
	if info.Size() == offset {
		return offset, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer func() { _ = file.Close() }()
	data := make([]byte, info.Size()-offset)
	n, readErr := file.ReadAt(data, offset)
	data = data[:n]
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return offset, readErr
	}
	// 只消费到最后一个换行符；残余尾巴留给下一次扫描。
	if cut := bytes.LastIndexByte(data, '\n'); cut < 0 {
		return offset, nil
	} else {
		data = data[:cut+1]
	}
	for line := range bytes.Lines(data) {
		if len(line) <= 1 {
			continue
		}
		var e IndexEntry
		if json.Unmarshal(line, &e) == nil {
			fn(e)
		}
	}
	return offset + int64(len(data)), nil
}

// releaseDir 把目录移出活跃集合，允许清理器回收它。
func (manager *Manager) releaseDir(directory string) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	delete(manager.activeDirs, filepath.Base(directory))
}
