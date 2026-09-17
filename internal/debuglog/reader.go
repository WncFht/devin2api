// 本文件实现调试日志的读取面：单请求目录详情与文件内容、进行中
// 请求的活快照。日志行检索已迁入 store（logs 表）；写入面见
// recorder.go/index.go。
//
// 这些接口服务两个消费者：面板的请求浏览页，以及 agent 直接 curl
// /admin/logs* 与 /admin/debug-logs/{id} 做程序化排障——所有返回都是 JSON 可消费结构。
package debuglog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// fileReadCap 是单文件读取上限；超出时截断并在响应里标记 truncated。
const fileReadCap = 4 << 20

// requestDirPattern 约束请求目录名，防止路径穿越读取任意目录。
// 同秒后缀按 %02d 生成、位数不设上限：同秒第 100+ 个请求会得到三位
// 后缀（-100），必须同样被接受。
var requestDirPattern = regexp.MustCompile(`^\d{8}-\d{6}(-\d{2,})?$`)

// RequestFileInfo 是请求目录内一个文件的清单项。
type RequestFileInfo struct {
	// Name 是相对请求目录的路径（顶层文件名或 attachments/xxx）。
	Name string `json:"name"`
	// Size 是文件字节数。
	Size int64 `json:"size"`
}

// RequestDetail 是单个请求目录的聚合视图：meta.json 原文加全部文件清单。
type RequestDetail struct {
	// Dir 是请求目录名。
	Dir string `json:"dir"`
	// Meta 是 meta.json 的原始 JSON；缺失或损坏时为 null。
	Meta json.RawMessage `json:"meta"`
	// Files 列出目录内全部可读文件（含 attachments 子目录）。
	Files []RequestFileInfo `json:"files"`
}

// Detail 读取一个已完成或进行中请求目录的 meta.json 与文件清单。
// dir 必须匹配请求目录命名模式，防止面板端点被用于遍历任意路径。
func (manager *Manager) Detail(dir string) (*RequestDetail, error) {
	if manager == nil || manager.root == "" || !requestDirPattern.MatchString(dir) {
		return nil, os.ErrNotExist
	}
	root := filepath.Join(manager.root, dir)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, os.ErrNotExist
	}
	detail := &RequestDetail{Dir: dir}
	if data, err := os.ReadFile(filepath.Join(root, MetaFile)); err == nil && json.Valid(data) {
		detail.Meta = json.RawMessage(data)
	}
	detail.Files = listRequestFiles(root)
	return detail, nil
}

// listRequestFiles 列出请求目录内全部常规文件（含一层 attachments 子目录）。
func listRequestFiles(root string) []RequestFileInfo {
	var files []RequestFileInfo
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		depth := strings.Count(rel, string(filepath.Separator))
		if info.IsDir() {
			if depth > 0 {
				return filepath.SkipDir // 只下钻一层子目录（attachments）
			}
			return nil
		}
		if info.Mode().IsRegular() {
			files = append(files, RequestFileInfo{Name: filepath.ToSlash(rel), Size: info.Size()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files
}

// ReadFile 读取请求目录内指定文件；超过 fileReadCap 时返回截断前缀。
// total 返回文件真实大小，便于调用方提示「已截断」。name 允许顶层文件
// 或 attachments/ 下一层文件，其余路径一律拒绝。
func (manager *Manager) ReadFile(dir, name string) (data []byte, total int64, truncated bool, err error) {
	if manager == nil || manager.root == "" || !requestDirPattern.MatchString(dir) || !validFileRelPath(name) {
		return nil, 0, false, os.ErrNotExist
	}
	path := filepath.Join(manager.root, dir, filepath.FromSlash(name))
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, false, os.ErrNotExist
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = file.Close() }()
	readSize := info.Size()
	if readSize > fileReadCap {
		readSize = fileReadCap
		truncated = true
	}
	data = make([]byte, readSize)
	if _, err = file.ReadAt(data, 0); err != nil {
		return nil, 0, false, err
	}
	return data, info.Size(), truncated, nil
}

// validFileRelPath 校验相对路径：顶层文件或 attachments/ 下一层，
// 拒绝穿越、绝对路径和非规范形式。
func validFileRelPath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if clean != name || strings.HasPrefix(clean, "..") {
		return false
	}
	parts := strings.Split(clean, "/")
	return len(parts) == 1 || (len(parts) == 2 && parts[0] == AttachmentsDir)
}

// ActiveRequest 是一个仍在进行中的请求的可观测快照。
type ActiveRequest struct {
	// Dir 是请求日志目录名。
	Dir string `json:"dir"`
	// Meta 是创建时记录的 HTTP 元信息。
	Meta RequestMeta `json:"meta"`
	// Model 是解码后的客户端请求模型名；未记录时为空。
	Model string `json:"model,omitempty"`
	// ResolvedModel 是别名/路由判定后实际发给上游的 uid；上游请求
	// 尚未发出时为空。
	ResolvedModel string `json:"resolved_model,omitempty"`
	// Retries 是已发生的上游重发次数；>0 表示请求正在或曾经重试。
	Retries int `json:"retries,omitempty"`
	// LastRetryCause 是最近一次重发的触发原因。
	LastRetryCause string `json:"last_retry_cause,omitempty"`
	// Account 是已选定服务本请求的上游账号（号池 lane 名）；上游请求
	// 尚未发出或还在 failover 途中时为空。
	Account string `json:"account,omitempty"`
	// AccountSwitches 是至今发生的号池 failover 换号次数（失败尝试数）。
	AccountSwitches int `json:"account_switches,omitempty"`
	// StartedAt 是请求进入时间。
	StartedAt time.Time `json:"started_at"`
	// ElapsedMS 是快照时刻相对进入时间的毫秒数。
	ElapsedMS int64 `json:"elapsed_ms"`
	// State 是请求所处阶段：waiting_upstream / receiving_upstream / streaming_client。
	State string `json:"state"`
	// FirstUpstreamMS 是首上游事件相对毫秒数；未发生为 null。
	FirstUpstreamMS *int64 `json:"first_upstream_ms"`
	// ClientBytes 是已下发给客户端的累计字节数。
	ClientBytes int64 `json:"client_bytes"`
	// QueuedEvents 是写队列中积压的任务数。
	QueuedEvents int `json:"queued_events"`
	// DroppedEvents 是目前已被丢弃的写任务数。
	DroppedEvents uint64 `json:"dropped_events"`
	// Abortable 表示请求 ctx 已挂接取消函数、可被面板中断。
	Abortable bool `json:"abortable"`
}

// ActiveRequests 返回仍在写入的请求目录快照（同类代理的
// active-requests/debug-log 同款能力：请求未结束就能看已收到的帧）。
// JSONL 内容经 bufio 缓冲，文件清单可能略滞后于实际收到的事件。
func (manager *Manager) ActiveRequests() []ActiveRequest {
	if manager == nil {
		return nil
	}
	manager.mutex.Lock()
	recorders := make([]*Recorder, 0, len(manager.activeDirs))
	for _, recorder := range manager.activeDirs {
		recorders = append(recorders, recorder)
	}
	manager.mutex.Unlock()
	out := make([]ActiveRequest, 0, len(recorders))
	for _, recorder := range recorders {
		out = append(out, recorder.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// FindDirByStartedAt 按请求开始时刻（epoch 毫秒）反查请求目录名。
// 目录名只有秒级精度（本地时区 20060102-150405 前缀 + 同秒 -NN 后缀），
// 同秒多个候选经各自 meta.json 的 started_at 精确比对消歧。
// 供 ccpanel 把日志行 id（started_at 毫秒戳）映射回调试目录。
func (manager *Manager) FindDirByStartedAt(ms int64) (string, bool) {
	if manager == nil || manager.root == "" {
		return "", false
	}
	base := time.UnixMilli(ms).Format("20060102-150405")
	entries, err := os.ReadDir(manager.root)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, base) || !requestDirPattern.MatchString(name) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(manager.root, name, MetaFile))
		if err != nil {
			continue
		}
		var meta struct {
			StartedAt string `json:"started_at"`
		}
		if json.Unmarshal(data, &meta) != nil {
			continue
		}
		started, err := time.Parse(time.RFC3339Nano, meta.StartedAt)
		if err == nil && started.UnixMilli() == ms {
			return name, true
		}
	}
	return "", false
}

// processLogTailBytes 是进程日志单次返回的尾部上限。
const processLogTailBytes = 256 << 10

// ReadProcessLog 返回进程日志（stderr.log）尾部内容与下一次增量拉取的偏移。
// offset>0 时从该偏移继续读（CLIProxyAPI GetLogs 的 cursor 模式简化版——
// 本服务日志不 rotate，偏移量天然单调有效）。
func (manager *Manager) ReadProcessLog(offset int64) (data []byte, next int64, err error) {
	if manager == nil || manager.root == "" {
		return nil, 0, os.ErrNotExist
	}
	path := filepath.Join(manager.root, StderrFile)
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()
	if offset > 0 && offset <= size {
		// 增量模式：从上次偏移读到现在。
		start := offset
		if size-offset > processLogTailBytes {
			start = size - processLogTailBytes
		}
		data = make([]byte, size-start)
		file, openErr := os.Open(path)
		if openErr != nil {
			return nil, 0, openErr
		}
		defer func() { _ = file.Close() }()
		if _, err = file.ReadAt(data, start); err != nil {
			return nil, 0, err
		}
		return data, size, nil
	}
	// 全量尾部模式：读最后 processLogTailBytes。
	data, err = TailRead(path, processLogTailBytes)
	if err != nil {
		return nil, 0, err
	}
	return data, size, nil
}

// TailRead 读取文件末尾至多 max 字节；文件小于 max 时读全文。
// 返回内容从第一个完整行开始（丢弃被截断的首行由调用方按行解析时自然跳过）。
func TailRead(path string, max int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if info.Size() > max {
		start = info.Size() - max
	}
	data := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(data, start); err != nil {
		return nil, err
	}
	return data, nil
}

// TruncateToTail 把 path 文件截到末尾至多 keep 字节：读取尾部、丢弃被截断
// 的首行残段后原地重写，返回实际保留的字节数。文件本就不超过 keep 时
// 不改写，直接返回其大小。供 JSONL 类追加日志（index/quota）的容量收口
// 共用——截断点落在行中间时残留半行对按行消费者是毒数据，必须丢弃。
func TruncateToTail(path string, keep int64) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if info.Size() <= keep {
		return info.Size(), nil
	}
	data, err := TailRead(path, keep)
	if err != nil {
		return 0, err
	}
	if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
		data = data[idx+1:]
	} else {
		// 整个尾部是一行残段，全丢弃留空文件。
		data = nil
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}
