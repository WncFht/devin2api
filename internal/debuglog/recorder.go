// 本文件实现单次 HTTP 请求的分阶段调试日志目录和 JSON/JSONL 写盘。
//
// Package debuglog 负责记录兼容 API 请求在 HTTP、中间模型和供应商协议之间的转换过程。
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
	"time"
)

// Manager 在固定 logs 根目录下为每次请求创建独立 recorder。
type Manager struct {
	// root 是所有请求日志目录的根路径；空值表示禁用调试日志。
	root string
	// now 返回当前时间；测试会固定它以验证同秒目录分配。
	now func() time.Time
	// mutex 保证同一秒并发请求的目录后缀分配不会冲突。
	mutex sync.Mutex
}

// RequestMeta 是创建请求日志时已经确定的 HTTP 元信息。
type RequestMeta struct {
	// Method 是 HTTP 请求方法。
	Method string
	// Path 是 HTTP 请求路径。
	Path string
}

// Completion 是请求结束时写入 meta.json 的结果摘要。
type Completion struct {
	// StatusCode 是最终 HTTP 状态码。
	StatusCode int
	// Result 是 completed、failed 或 disconnected。
	Result string
	// Model 是请求使用的模型标识。
	Model string
	// Provider 是实际生成响应的供应商标识。
	Provider string
	// Stream 表示请求是否使用流式响应。
	Stream bool
}

// Recorder 保存单次请求的目录、开始时间和各 JSONL 文件序号。
type Recorder struct {
	// directory 是本次请求的日志目录。
	directory string
	// startedAt 是 HTTP 请求进入应用的时间。
	startedAt time.Time
	// requestMeta 保存创建时的 HTTP 元信息。
	requestMeta RequestMeta
	// mutex 串行化同一请求内的文件写入和附件编号。
	mutex sync.Mutex
	// sequences 保存每个 JSONL 文件各自的递增序号。
	sequences map[string]int
	// attachmentByHash 用于复用在多个转换阶段重复出现的同一附件。
	attachmentByHash map[string]attachmentReference
	// attachmentCount 是附件文件名的递增编号。
	attachmentCount int
	// jsonlFiles 保存已打开的 JSONL 文件，避免每帧重复 open/sync/close。
	jsonlFiles map[string]*jsonlFile
	// pendingWrites 跟踪异步 WriteJSON goroutine，Complete 时统一等待。
	pendingWrites sync.WaitGroup
}

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

// NewManager 创建写入指定 logs 根目录的管理器；空路径会返回禁用状态的管理器。
func NewManager(root string) *Manager {
	return &Manager{root: root, now: time.Now}
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
			directory:        directory,
			startedAt:        now,
			requestMeta:      meta,
			sequences:        make(map[string]int),
			attachmentByHash: make(map[string]attachmentReference),
			jsonlFiles:       make(map[string]*jsonlFile),
		}
		recorder.writeMeta(nil)
		return recorder
	}
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

// WriteJSON 将一个阶段快照写为格式化 JSON 文件。
// 序列化和写盘在后台 goroutine 中执行，不阻塞请求主线程；Complete 时统一等待完成。
func (recorder *Recorder) WriteJSON(name string, value any) {
	if recorder == nil || !validLogName(name, ".json") {
		return
	}
	recorder.pendingWrites.Add(1)
	go func() {
		defer recorder.pendingWrites.Done()
		recorder.mutex.Lock()
		defer recorder.mutex.Unlock()
		value = recorder.sanitize(value)
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return
		}
		data = append(data, '\n')
		_ = os.WriteFile(filepath.Join(recorder.directory, name), data, 0o600)
	}()
}

// AppendJSONL 将一个有序事件追加到指定 JSONL 文件。
func (recorder *Recorder) AppendJSONL(name, event string, value any) {
	if recorder == nil || !validLogName(name, ".jsonl") {
		return
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
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
}

// AppendValueJSONL 将一个结构化值直接追加为 JSONL 行，不添加事件信封。
func (recorder *Recorder) AppendValueJSONL(name string, value any) {
	if recorder == nil || !validLogName(name, ".jsonl") {
		return
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	data, err := json.Marshal(recorder.sanitize(value))
	if err != nil {
		return
	}
	recorder.appendJSONL(name, data)
}

// WriteError 写入请求失败的阶段和错误摘要。
func (recorder *Recorder) WriteError(stage string, err error) {
	if recorder == nil || err == nil {
		return
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	path := filepath.Join(recorder.directory, "error.json")
	if _, statErr := os.Stat(path); statErr == nil {
		return
	}
	value := recorder.sanitize(map[string]any{
		"stage":      stage,
		"message":    err.Error(),
		"elapsed_ms": time.Since(recorder.startedAt).Milliseconds(),
	})
	data, marshalErr := json.MarshalIndent(value, "", "  ")
	if marshalErr != nil {
		return
	}
	_ = os.WriteFile(path, append(data, '\n'), 0o600)
}

// Complete 使用最终状态重写 meta.json。
func (recorder *Recorder) Complete(completion Completion) {
	if recorder == nil {
		return
	}
	// 等待所有异步 WriteJSON 完成后再刷盘关闭，避免文件内容不完整。
	recorder.pendingWrites.Wait()
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	// 请求结束时统一刷盘并关闭 JSONL，避免每帧 file.Sync() 带来的延迟。
	for _, f := range recorder.jsonlFiles {
		_ = f.writer.Flush()
		_ = f.file.Close()
	}
	recorder.jsonlFiles = nil
	recorder.writeMeta(&completion)
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
	if recorder.jsonlFiles == nil {
		recorder.jsonlFiles = make(map[string]*jsonlFile)
	}
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
	if completion != nil {
		finishedAt := time.Now()
		meta["finished_at"] = finishedAt.Format(time.RFC3339Nano)
		meta["duration_ms"] = finishedAt.Sub(recorder.startedAt).Milliseconds()
		meta["status_code"] = completion.StatusCode
		meta["result"] = completion.Result
		meta["model"] = completion.Model
		meta["provider"] = completion.Provider
		meta["stream"] = completion.Stream
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
