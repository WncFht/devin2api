// 本文件实现单次 HTTP 请求的分阶段调试日志目录和 JSON/JSONL 写盘。
//
// Package debuglog 负责记录兼容 API 请求在 HTTP、中间模型和供应商协议之间的转换过程。
// 所有写盘作业经每请求一个有界任务队列交给单 worker 串行执行——
// 事件顺序即入队顺序，热路径只承担一次 channel send；队列满时丢弃并计数，
// 观测系统自身降级不拖垮请求。生命周期管理（保留期/总量清理）见 cleaner.go。
package debuglog

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leookun/devin-2api/internal/llm"
)

// writeQueueSize 是单请求写任务的排队上限；流式帧在万级以下时绰绰有余。
const writeQueueSize = 4096

// Manager 在固定 logs 根目录下为每次请求创建独立 recorder，并持有
// 全局索引（index.jsonl）与后台清理器。
type Manager struct {
	// root 是所有请求日志目录的根路径；空值表示禁用调试日志。
	root string
	// now 返回当前时间；测试会固定它以验证同秒目录分配。
	now func() time.Time
	// mutex 串行化目录分配、index.jsonl 追加和 activeDirs 维护。
	mutex sync.Mutex
	// activeDirs 记录仍有进行中请求的目录名→recorder，清理器必须跳过；
	// 存指针是为了 ActiveRequests 能直出进行中请求的活快照。
	activeDirs map[string]*Recorder
	// retentionDays 是请求目录保留天数；<=0 不按时间清理。
	retentionDays int
	// maxBytes 是 logs 根目录总量上限；<=0 不按大小清理。
	maxBytes int64
	// indexFile/indexWriter 是跨请求索引（index.jsonl）的持久句柄。
	indexFile   *os.File
	indexWriter *bufio.Writer
	// cleanerStop/cleanerDone 控制后台清理协程生命周期；nil 表示未启动。
	cleanerStop chan struct{}
	cleanerDone chan struct{}
	// droppedTotal 汇总各请求被丢弃的写任务数，供 Stats 暴露。
	droppedTotal atomic.Uint64
}

// RequestMeta 是创建请求日志时已经确定的 HTTP 元信息。
type RequestMeta struct {
	// Method 是 HTTP 请求方法。
	Method string
	// Path 是 HTTP 请求路径。
	Path string
	// API 是入口协议标识（openai-chat、openai-responses、responses-ws、anthropic）。
	API string
	// ClientIP 是下游客户端地址（不含端口）。
	ClientIP string
	// UserAgent 是下游客户端声明的 UA。
	UserAgent string
	// KeyHash 是客户端凭据的 SHA-256 前 8 字节十六进制——
	// 用于按 key 关联请求，不明文落盘。
	KeyHash string
	// ClientRequestID 是客户端自带的关联 ID（X-Request-Id/X-Session-Id），
	// 让调用方能用自己的 ID 检索本次请求。
	ClientRequestID string
}

// Completion 是请求结束时写入 meta.json 的结果摘要。
type Completion struct {
	// StatusCode 是最终 HTTP 状态码。
	StatusCode int
	// Result 是 completed、failed 或 disconnected。
	Result string
	// Model 是实际发给上游的模型标识（别名解析后）。
	Model string
	// RequestedModel 是客户端原始请求的模型名（可能命中别名）。
	RequestedModel string
	// ResponseModel 是上游响应声明的模型；为空表示上游未声明。
	ResponseModel string
	// ModelMismatch 表示上游声明模型与实际请求模型不一致。
	ModelMismatch bool
	// Provider 是实际生成响应的供应商标识。
	Provider string
	// Stream 表示请求是否使用流式响应。
	Stream bool
	// UpstreamRequestID 是上游为本次调用分配的追踪标识，报障时可引用。
	UpstreamRequestID string
	// Usage 是上游报告的最终 token 用量；失败或未上报时为零值。
	Usage llm.Usage
}

// Recorder 保存单次请求的目录、开始时间和异步写队列。
type Recorder struct {
	// manager 回指所属 Manager，Complete 时写索引并释放目录保护。
	manager *Manager
	// directory 是本次请求的日志目录。
	directory string
	// startedAt 是 HTTP 请求进入应用的时间。
	startedAt time.Time
	// requestMeta 保存创建时的 HTTP 元信息。
	requestMeta RequestMeta
	// mutex 保护 closed 标志与入队决策；worker 自身状态无锁。
	mutex sync.Mutex
	// closed 表示 Complete 已关闭队列，之后入队请求直接计入丢弃。
	closed bool
	// tasks 是待执行写任务的有界队列；满时丢弃而非阻塞调用方。
	tasks chan writeTask
	// writerDone 在 worker 排空队列并关闭文件后关闭。
	writerDone chan struct{}
	// dropped 是本次请求被丢弃的写任务数。
	dropped atomic.Uint64
	// firstUpstreamMS/firstClientMS 是首上游事件/首客户端内容字节的
	// 相对毫秒数；-1 表示尚未发生。区分「上游慢」与「网关编码慢」。
	firstUpstreamMS atomic.Int64
	firstClientMS   atomic.Int64

	// 以下字段仅由写 worker 访问，无需加锁：
	// sequences 保存每个 JSONL 文件各自的递增序号。
	sequences map[string]int
	// attachmentByHash 用于复用在多个转换阶段重复出现的同一附件。
	attachmentByHash map[string]attachmentReference
	// attachmentCount 是附件文件名的递增编号。
	attachmentCount int
	// jsonlFiles 保存已打开的 JSONL 文件，避免每帧重复 open/close。
	jsonlFiles map[string]*jsonlFile
	// errorWritten 保证 error.json 只保留首个错误（最先失败点最有诊断价值）。
	errorWritten bool
	// errorStage 记录首个错误的阶段名，随索引落盘供按失败点检索。
	errorStage string
}

// writeTask 是交给写 worker 的一次作业，worker 内串行执行。
type writeTask func()

// jsonlFile 保存单个已打开的 JSONL 文件句柄及其缓冲写。
type jsonlFile struct {
	file   *os.File
	writer *bufio.Writer
}

// JSONLRecord 是一个 JSONL 文件中的统一行信封。
type JSONLRecord struct {
	// Seq 是当前文件内从 1 开始的顺序号。
	Seq int `json:"seq"`
	// Time 是事件发生时带时区的 RFC3339Nano 时间。
	Time string `json:"time"`
	// ElapsedMS 是相对请求进入时间的毫秒数。
	ElapsedMS int64 `json:"elapsed_ms"`
	// Event 是协议事件名；没有独立事件名时省略。
	Event string `json:"event,omitempty"`
	// Data 是本行记录的结构化内容。
	Data any `json:"data"`
}

// contextKey 是 request context 中 recorder 的私有键类型。
type contextKey struct{}

// attachmentReference 是 JSON 中替代图片 base64 正文的附件引用。
type attachmentReference struct {
	// File 是相对于请求日志目录的附件路径。
	File string `json:"file"`
	// MIMEType 是附件的媒体类型。
	MIMEType string `json:"mime_type"`
	// Size 是解码后二进制内容的字节数。
	Size int `json:"size"`
	// SHA256 是附件内容的 SHA-256 十六进制摘要。
	SHA256 string `json:"sha256"`
}

// NewManager 创建写入指定 logs 根目录的管理器；空路径返回禁用状态的管理器。
// retentionDays 和 maxTotalMB 控制后台清理：<=0 表示对应维度不限制。
func NewManager(root string, retentionDays int, maxTotalMB int64) *Manager {
	manager := &Manager{
		root:          root,
		now:           time.Now,
		activeDirs:    make(map[string]*Recorder),
		retentionDays: retentionDays,
		maxBytes:      maxTotalMB << 20,
	}
	if root != "" && (retentionDays > 0 || maxTotalMB > 0) {
		manager.cleanerStop = make(chan struct{})
		manager.cleanerDone = make(chan struct{})
		go manager.runCleaner()
	}
	return manager
}

// Close 停止后台清理并关闭索引文件句柄；进程退出前调用一次。
func (manager *Manager) Close() {
	if manager == nil {
		return
	}
	if manager.cleanerStop != nil {
		close(manager.cleanerStop)
		<-manager.cleanerDone
	}
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if manager.indexWriter != nil {
		_ = manager.indexWriter.Flush()
		_ = manager.indexFile.Close()
		manager.indexWriter = nil
		manager.indexFile = nil
	}
}

// Stats 返回日志管道自身的运行指标：丢弃数、活跃请求目录数。
// 观测系统自己的健康状况也应可观测（参考 ccLoad 的 drop/backlog 计数）。
func (manager *Manager) Stats() map[string]any {
	if manager == nil {
		return nil
	}
	manager.mutex.Lock()
	active := len(manager.activeDirs)
	manager.mutex.Unlock()
	return map[string]any{
		"log_root":            manager.root,
		"active_request_dirs": active,
		"dropped_log_events":  manager.droppedTotal.Load(),
		"retention_days":      manager.retentionDays,
		"max_total_mb":        manager.maxBytes >> 20,
	}
}

// Start 为一个 HTTP 请求创建按进入秒命名的独立日志目录。
func (manager *Manager) Start(meta RequestMeta) *Recorder {
	if manager == nil || manager.root == "" {
		return nil
	}
	now := manager.now()
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if err := os.MkdirAll(manager.root, 0o700); err != nil {
		return nil
	}
	base := now.Format("20060102-150405")
	for suffix := 1; ; suffix++ {
		name := base
		if suffix > 1 {
			name = fmt.Sprintf("%s-%02d", base, suffix)
		}
		directory := filepath.Join(manager.root, name)
		if err := os.Mkdir(directory, 0o700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return nil
		}
		recorder := &Recorder{
			manager:          manager,
			directory:        directory,
			startedAt:        now,
			requestMeta:      meta,
			tasks:            make(chan writeTask, writeQueueSize),
			writerDone:       make(chan struct{}),
			sequences:        make(map[string]int),
			attachmentByHash: make(map[string]attachmentReference),
			jsonlFiles:       make(map[string]*jsonlFile),
		}
		recorder.firstUpstreamMS.Store(-1)
		recorder.firstClientMS.Store(-1)
		manager.activeDirs[name] = recorder
		go recorder.runWriter()
		recorder.writeMeta(nil)
		return recorder
	}
}

// DirectoryPath 返回本请求的日志目录绝对路径；禁用态 recorder 为空串。
func (recorder *Recorder) DirectoryPath() string {
	if recorder == nil {
		return ""
	}
	return recorder.directory
}

// WithRecorder 将本次请求 recorder 放入 context 供供应商 adapter 使用。
func WithRecorder(ctx context.Context, recorder *Recorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, recorder)
}

// FromContext 返回当前请求 recorder；未启用日志时返回 nil。
func FromContext(ctx context.Context) *Recorder {
	recorder, _ := ctx.Value(contextKey{}).(*Recorder)
	return recorder
}

// enqueue 把一个写任务交给 worker；队列满或已关闭时丢弃并计数。
func (recorder *Recorder) enqueue(task writeTask) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if recorder.closed {
		recorder.dropped.Add(1)
		return
	}
	select {
	case recorder.tasks <- task:
	default:
		recorder.dropped.Add(1)
	}
}

// runWriter 是单请求写协程：串行执行任务，保证 JSONL 事件序与入队序一致；
// tasks 关闭后排空残余任务，统一刷盘并关闭所有 JSONL 文件。
func (recorder *Recorder) runWriter() {
	for task := range recorder.tasks {
		task()
	}
	for _, f := range recorder.jsonlFiles {
		_ = f.writer.Flush()
		_ = f.file.Close()
	}
	recorder.jsonlFiles = nil
	close(recorder.writerDone)
}

// NoteUpstreamLatency 记录首个上游事件到达的相对毫秒数（幂等，只记第一次）。
func (recorder *Recorder) NoteUpstreamLatency() {
	if recorder == nil {
		return
	}
	recorder.firstUpstreamMS.CompareAndSwap(-1, time.Since(recorder.startedAt).Milliseconds())
}

// NoteClientLatency 记录首个下发给客户端的内容字节的相对毫秒数。
// SSE 保活注释不计——它是链路保活不是内容。
func (recorder *Recorder) NoteClientLatency() {
	if recorder == nil {
		return
	}
	recorder.firstClientMS.CompareAndSwap(-1, time.Since(recorder.startedAt).Milliseconds())
}

// WriteJSON 将一个阶段快照排入队列，由 worker 序列化并写为格式化 JSON 文件。
func (recorder *Recorder) WriteJSON(name string, value any) {
	if recorder == nil || !validLogName(name, ".json") {
		return
	}
	recorder.enqueue(func() {
		value = recorder.sanitize(value)
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return
		}
		data = append(data, '\n')
		_ = os.WriteFile(filepath.Join(recorder.directory, name), data, 0o600)
	})
}

// AppendJSONL 将一个有序事件追加到指定 JSONL 文件。
func (recorder *Recorder) AppendJSONL(name, event string, value any) {
	if recorder == nil || !validLogName(name, ".jsonl") {
		return
	}
	recorder.enqueue(func() {
		recorder.sequences[name]++
		record := JSONLRecord{
			Seq:       recorder.sequences[name],
			Time:      time.Now().Format(time.RFC3339Nano),
			ElapsedMS: time.Since(recorder.startedAt).Milliseconds(),
			Event:     event,
			Data:      recorder.sanitize(value),
		}
		data, err := json.Marshal(record)
		if err != nil {
			return
		}
		recorder.appendJSONL(name, data)
	})
}

// AppendValueJSONL 将一个结构化值直接追加为 JSONL 行，不添加事件信封。
func (recorder *Recorder) AppendValueJSONL(name string, value any) {
	if recorder == nil || !validLogName(name, ".jsonl") {
		return
	}
	recorder.enqueue(func() {
		data, err := json.Marshal(recorder.sanitize(value))
		if err != nil {
			return
		}
		recorder.appendJSONL(name, data)
	})
}

// WriteError 写入请求失败的阶段和错误摘要；只保留首个错误。
func (recorder *Recorder) WriteError(stage string, err error) {
	if recorder == nil || err == nil {
		return
	}
	recorder.enqueue(func() {
		if recorder.errorWritten {
			return
		}
		recorder.errorWritten = true
		recorder.errorStage = stage
		value := recorder.sanitize(map[string]any{
			"stage":      stage,
			"message":    err.Error(),
			"elapsed_ms": time.Since(recorder.startedAt).Milliseconds(),
		})
		data, marshalErr := json.MarshalIndent(value, "", "  ")
		if marshalErr != nil {
			return
		}
		_ = os.WriteFile(filepath.Join(recorder.directory, "error.json"), append(data, '\n'), 0o600)
	})
}

// Complete 关闭写队列、等待残余任务排空，然后写终态 meta.json、
// 追加全局索引行并释放目录的清理保护。
func (recorder *Recorder) Complete(completion Completion) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	if !recorder.closed {
		recorder.closed = true
		close(recorder.tasks)
	}
	recorder.mutex.Unlock()
	<-recorder.writerDone
	recorder.writeMeta(&completion)
	recorder.manager.appendIndex(recorder, &completion)
	recorder.manager.releaseDir(recorder.directory)
	recorder.manager.droppedTotal.Add(recorder.dropped.Load())
}

func (recorder *Recorder) appendJSONL(name string, data []byte) {
	jf, err := recorder.getJSONLFile(name)
	if err != nil {
		return
	}
	_, _ = jf.writer.Write(data)
	_ = jf.writer.WriteByte('\n')
}

func (recorder *Recorder) getJSONLFile(name string) (*jsonlFile, error) {
	if f, ok := recorder.jsonlFiles[name]; ok {
		return f, nil
	}
	file, err := os.OpenFile(filepath.Join(recorder.directory, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f := &jsonlFile{file: file, writer: bufio.NewWriter(file)}
	recorder.jsonlFiles[name] = f
	return f, nil
}

func (recorder *Recorder) writeMeta(completion *Completion) {
	meta := map[string]any{
		"started_at": recorder.startedAt.Format(time.RFC3339Nano),
		"method":     recorder.requestMeta.Method,
		"path":       recorder.requestMeta.Path,
	}
	if recorder.requestMeta.API != "" {
		meta["api"] = recorder.requestMeta.API
	}
	client := map[string]any{}
	if recorder.requestMeta.ClientIP != "" {
		client["ip"] = recorder.requestMeta.ClientIP
	}
	if recorder.requestMeta.UserAgent != "" {
		client["user_agent"] = recorder.requestMeta.UserAgent
	}
	if recorder.requestMeta.KeyHash != "" {
		client["key_hash"] = recorder.requestMeta.KeyHash
	}
	if recorder.requestMeta.ClientRequestID != "" {
		client["request_id"] = recorder.requestMeta.ClientRequestID
	}
	if len(client) > 0 {
		meta["client"] = client
	}
	if first := recorder.firstUpstreamMS.Load(); first >= 0 {
		meta["first_upstream_ms"] = first
	}
	if first := recorder.firstClientMS.Load(); first >= 0 {
		meta["first_client_ms"] = first
	}
	if dropped := recorder.dropped.Load(); dropped > 0 {
		meta["dropped_events"] = dropped
	}
	if completion != nil {
		finishedAt := time.Now()
		meta["finished_at"] = finishedAt.Format(time.RFC3339Nano)
		meta["duration_ms"] = finishedAt.Sub(recorder.startedAt).Milliseconds()
		meta["status_code"] = completion.StatusCode
		meta["result"] = completion.Result
		meta["model"] = completion.Model
		meta["provider"] = completion.Provider
		meta["stream"] = completion.Stream
		if completion.RequestedModel != "" {
			meta["requested_model"] = completion.RequestedModel
		}
		if completion.ResponseModel != "" {
			meta["response_model"] = completion.ResponseModel
		}
		if completion.ModelMismatch {
			meta["model_mismatch"] = true
		}
		if completion.UpstreamRequestID != "" {
			meta["upstream_request_id"] = completion.UpstreamRequestID
		}
		if completion.Usage != (llm.Usage{}) {
			meta["usage"] = map[string]any{
				"input":       completion.Usage.Input,
				"output":      completion.Usage.Output,
				"cache_read":  completion.Usage.CacheRead,
				"cache_write": completion.Usage.CacheWrite,
				"total":       completion.Usage.TotalTokens,
			}
		}
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(recorder.directory, "meta.json"), append(data, '\n'), 0o600)
	}
}

func validLogName(name, extension string) bool {
	return filepath.Base(name) == name && strings.HasSuffix(name, extension)
}

func (recorder *Recorder) sanitize(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return map[string]any{"serialization_error": err.Error()}
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return map[string]any{"serialization_error": err.Error()}
	}
	return recorder.sanitizeValue(generic)
}

func (recorder *Recorder) sanitizeValue(value any) any {
	switch value := value.(type) {
	case []any:
		for index := range value {
			value[index] = recorder.sanitizeValue(value[index])
		}
		return value
	case map[string]any:
		for key := range value {
			if secretKey(key) {
				value[key] = "<redacted>"
			}
		}
		if reference, ok := recorder.extractImage(value); ok {
			return reference
		}
		for key, item := range value {
			value[key] = recorder.sanitizeValue(item)
		}
		return value
	case string:
		if strings.HasPrefix(value, "data:image/") {
			if reference, ok := recorder.writeDataURL(value); ok {
				return reference
			}
		}
		return value
	default:
		return value
	}
}

func secretKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
	switch normalized {
	case "authorization", "cookie", "setcookie", "apikey", "accesskey", "token", "sessiontoken", "accesstoken", "refreshtoken", "bearertoken", "password", "clientsecret", "f", "devicefingerprint":
		return true
	default:
		return false
	}
}

func (recorder *Recorder) extractImage(value map[string]any) (attachmentReference, bool) {
	mimeType, _ := stringField(value, "mime_type", "mimeType", "MIMEType")
	encoded, _ := stringField(value, "data", "base64_data", "base64Data", "Data")
	if !strings.HasPrefix(mimeType, "image/") || encoded == "" {
		return attachmentReference{}, false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return attachmentReference{}, false
	}
	return recorder.writeAttachment(data, mimeType), true
}

func (recorder *Recorder) writeDataURL(value string) (attachmentReference, bool) {
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return attachmentReference{}, false
	}
	mimeType := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return attachmentReference{}, false
	}
	return recorder.writeAttachment(data, mimeType), true
}

func (recorder *Recorder) writeAttachment(data []byte, mimeType string) attachmentReference {
	hashBytes := sha256.Sum256(data)
	hash := hex.EncodeToString(hashBytes[:])
	if reference, ok := recorder.attachmentByHash[hash]; ok {
		return reference
	}
	recorder.attachmentCount++
	extension := imageExtension(mimeType)
	fileName := fmt.Sprintf("image-%03d%s", recorder.attachmentCount, extension)
	relativePath := filepath.Join("attachments", fileName)
	directory := filepath.Join(recorder.directory, "attachments")
	_ = os.MkdirAll(directory, 0o700)
	_ = os.WriteFile(filepath.Join(directory, fileName), data, 0o600)
	reference := attachmentReference{File: filepath.ToSlash(relativePath), MIMEType: mimeType, Size: len(data), SHA256: hash}
	recorder.attachmentByHash[hash] = reference
	return reference
}

func imageExtension(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		extensions, _ := mime.ExtensionsByType(mimeType)
		if len(extensions) > 0 {
			return extensions[0]
		}
		return ".bin"
	}
}

func stringField(value map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if text, ok := value[key].(string); ok {
			return text, true
		}
	}
	return "", false
}
