// 本文件实现调试日志的读取面：单请求目录详情与文件内容、进行中
// 请求的活快照。日志行检索与请求 payload 都落在 store（logs 表 +
// debug_files/debug_chunks 两表）；写入面见 recorder.go/index.go。
//
// 这些接口服务两个消费者：面板的请求浏览页，以及 agent 直接 curl
// /admin/logs* 与 /admin/debug-logs/{id} 做程序化排障——所有返回都是 JSON 可消费结构。
package debuglog

import (
	"context"
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

// requestDirPattern 约束请求目录名，防止伪造目录名探测库内其它行。
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
	// Files 列出目录内全部可读文件（含 attachments 名下文件）。
	Files []RequestFileInfo `json:"files"`
	// Summary 是 meta.json 解码后的 typed 视图；缺失或损坏时为 null。
	// 消费方要字段语义时读它，不必再对 Meta 做匿名 struct 解码。
	Summary *MetaSummary `json:"summary,omitempty"`
}

// Detail 读取一个已完成或进行中请求目录的 meta.json 与文件清单。
// dir 必须匹配请求目录命名模式。目录名只在 claim 落库那一刻起算存在；
// 文件清单为空且不在活跃集（日志行在而 payload 已被淘汰）时回
// os.ErrNotExist——恢复「目录已删」的 404 语义而不是 200 空数据。
func (manager *Manager) Detail(dir string) (*RequestDetail, error) {
	if manager == nil || !requestDirPattern.MatchString(dir) {
		return nil, os.ErrNotExist
	}
	var files []RequestFileInfo
	if manager.store != nil {
		list, err := manager.store.DebugFileList(context.Background(), dir)
		if err != nil {
			return nil, err
		}
		for _, f := range list {
			files = append(files, RequestFileInfo{Name: f.Name, Size: f.Size})
		}
	}
	if len(files) == 0 {
		manager.mutex.Lock()
		_, active := manager.activeDirs[dir]
		manager.mutex.Unlock()
		if !active {
			return nil, os.ErrNotExist
		}
	}
	detail := &RequestDetail{Dir: dir, Files: files}
	if manager.store != nil {
		if data, _, ok, err := manager.store.DebugFile(context.Background(), dir, MetaFile, fileReadCap); err == nil && ok && json.Valid(data) {
			detail.Meta = json.RawMessage(data)
			var summary MetaSummary
			if json.Unmarshal(data, &summary) == nil {
				detail.Summary = &summary
			}
		}
	}
	return detail, nil
}

// ReadFile 读取请求目录内指定文件；超过 fileReadCap 时返回截断前缀。
// total 返回文件真实大小，便于调用方提示「已截断」。name 允许顶层文件
// 或 attachments/ 下一层文件，其余路径一律拒绝。
func (manager *Manager) ReadFile(dir, name string) (data []byte, total int64, truncated bool, err error) {
	if manager == nil || manager.store == nil || !requestDirPattern.MatchString(dir) || !validFileRelPath(name) {
		return nil, 0, false, os.ErrNotExist
	}
	data, total, ok, err := manager.store.DebugFile(context.Background(), dir, name, fileReadCap)
	if err != nil {
		return nil, 0, false, err
	}
	if !ok {
		return nil, 0, false, os.ErrNotExist
	}
	return data, total, total > int64(len(data)), nil
}

// validFileRelPath 校验目录内文件名的合法形状：顶层文件或
// attachments/ 下一层，拒绝空段、穿越段与反斜杠。入库后名字只是行键，
// 校验的意义收窄为「只允许面板契约里的两种形态」，不再承担防路径穿越。
func validFileRelPath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return len(parts) == 1 || (len(parts) == 2 && parts[0] == AttachmentsDir)
}

// DevinRequestStages 列出请求目录内全部上游 wire 请求文件名——首个请求加
// attemptN/searchN 分片，按名字字典序返回。census 类消费者与面板的
// req_body 拼接都经它枚举，重试写进上游的 wire 形态才不会逃出覆盖统计。
func (manager *Manager) DevinRequestStages(dir string) ([]string, error) {
	if manager == nil || manager.store == nil || !requestDirPattern.MatchString(dir) {
		return nil, os.ErrNotExist
	}
	names, err := manager.store.DebugFileNames(context.Background(), dir)
	if err != nil {
		return nil, err
	}
	var stages []string
	for _, name := range names {
		if strings.HasPrefix(name, devinRequestStageStem) && strings.HasSuffix(name, ".json") {
			stages = append(stages, name)
		}
	}
	return stages, nil
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
// JSONL 内容按 flush 批提交，文件清单可能略滞后于实际收到的事件。
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
// 活跃请求先查内存——claim 行虽即时落库，meta.json 内容要等首个写
// 任务跑完才有 started_at，毫秒级查询窗口内只能靠 recorder 记的时刻。
func (manager *Manager) FindDirByStartedAt(ms int64) (string, bool) {
	if manager == nil {
		return "", false
	}
	manager.mutex.Lock()
	var matched string
	for dir, recorder := range manager.activeDirs {
		if recorder.startedAt.UnixMilli() == ms && (matched == "" || dir < matched) {
			matched = dir
		}
	}
	manager.mutex.Unlock()
	if matched != "" {
		return matched, true
	}
	if manager.store == nil {
		return "", false
	}
	ctx := context.Background()
	dirs, err := manager.store.DebugDirsByPrefix(ctx, time.UnixMilli(ms).Format("20060102-150405"))
	if err != nil {
		return "", false
	}
	for _, dir := range dirs {
		if !requestDirPattern.MatchString(dir) {
			continue
		}
		data, _, ok, err := manager.store.DebugFile(ctx, dir, MetaFile, fileReadCap)
		if err != nil || !ok {
			continue
		}
		var meta MetaSummary
		if json.Unmarshal(data, &meta) != nil {
			continue
		}
		started, err := time.Parse(time.RFC3339Nano, meta.StartedAt)
		if err == nil && started.UnixMilli() == ms {
			return dir, true
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
