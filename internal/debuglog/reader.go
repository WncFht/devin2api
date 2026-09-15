// 本文件实现调试日志的读取面：全局索引倒读、单请求目录详情与文件内容、
// 进行中请求的活快照。写入面见 recorder.go/index.go。
//
// 这些接口服务两个消费者：面板的请求浏览页，以及 agent 直接 curl
// /panel/api/requests* 做程序化排障——所有返回都是 JSON 可消费结构。
package debuglog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// indexTailBytes 限定 ListRequests 只读 index.jsonl 尾部；
// 4MB 约覆盖一万条请求记录，更早的历史用 grep 查原文件。
const indexTailBytes = 4 << 20

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
	// Files 是目录内目前已落盘的文件清单。
	Files []RequestFileInfo `json:"files"`
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
		snap := recorder.snapshot()
		snap.Files = listRequestFiles(recorder.directory)
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// ListResult 是 ListRequests 的返回体：命中条目加历史截断信号。
type ListResult struct {
	// Entries 是命中的请求摘要，新的在前。
	Entries []IndexEntry `json:"entries"`
	// HasMore 表示 index.jsonl 在读取窗口之外还有更早历史
	// （文件超过尾部读取上限，或未读窗口内仍有剩余行）。
	HasMore bool `json:"has_more"`
	// IndexTailStart 是本轮索引尾部读取窗覆盖到的最早一行 started_at；
	// 配合 filter.Since 可判断时间窗覆盖是否完整——它比 since 还晚，
	// 说明尾部窗口边界落在请求窗口内部，窗内条目可能被截断。
	IndexTailStart string `json:"index_tail_start,omitempty"`
}

// RequestFilter 是请求列表的结构化筛选条件；零值匹配全部。
type RequestFilter struct {
	// StatusClass 按状态码段过滤："2xx"/"4xx"/"5xx"。
	StatusClass string
	// Status 按状态表达式过滤（同类网关式），逗号分隔为 OR；
	// 单项支持精确码(499)、段位(4xx)、比较(>=400/<300)、取反(!200/!2xx)。
	Status string
	// Result 按结果过滤：completed/failed/disconnected/aborted。
	Result string
	// Model 按请求或响应模型精确过滤。
	Model string
	// ErrorStage 按失败阶段精确过滤。
	ErrorStage string
	// Since 只保留开始时间晚于该时刻的请求；零值不限。
	Since time.Time
	// Until 只保留开始时间早于该时刻的请求；零值不限。
	// 矩阵格子下钻用它把列表钉在一个历史窗口内，而不是从现在往回滚。
	Until time.Time
	// Query 保留原有子串匹配：命中 dir/model/key_hash/client_request_id/path。
	Query string
}

// statusCond 是编译后的单项状态码条件；多个条件之间是 OR。
type statusCond struct {
	neg bool // "!" 前缀对整个单项取反
	op  byte // '=' 精确码, 'x' 段位(百位), 'g' '>=', 'l' '<=', '>' '>', '<' '<'
	val int
}

func (c statusCond) ok(code int) bool {
	var m bool
	switch c.op {
	case 'x':
		m = code/100 == c.val
	case 'g':
		m = code >= c.val
	case 'l':
		m = code <= c.val
	case '>':
		m = code > c.val
	case '<':
		m = code < c.val
	default:
		m = code == c.val
	}
	return m != c.neg
}

// parseStatusExpr 把状态表达式编译成条件列表；空串返回 nil（不过滤）。
// 任一单项非法时返回恒不匹配的哨兵——表达式是用户输入，
// 宁可显式空结果也不静默退化成无过滤。
func parseStatusExpr(s string) []statusCond {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []statusCond
	for _, part := range strings.Split(s, ",") {
		c, ok := parseStatusTerm(strings.TrimSpace(part))
		if !ok {
			return []statusCond{{op: '=', val: -1}}
		}
		out = append(out, c)
	}
	return out
}

// parseStatusTerm 解析单项：可选 "!" 前缀 + Nxx 段位 / 比较符三位码 / 裸三位码。
func parseStatusTerm(t string) (statusCond, bool) {
	c := statusCond{op: '='}
	if strings.HasPrefix(t, "!") {
		c.neg = true
		t = t[1:]
	}
	for _, p := range []struct {
		pre string
		op  byte
	}{{">=", 'g'}, {"<=", 'l'}, {">", '>'}, {"<", '<'}} {
		if strings.HasPrefix(t, p.pre) {
			c.op = p.op
			t = t[len(p.pre):]
			break
		}
	}
	// 段位写法 "4xx" 只在精确语义下成立（">=4xx" 无意义）。
	if len(t) == 3 && t[1:] == "xx" && t[0] >= '0' && t[0] <= '9' {
		if c.op != '=' {
			return c, false
		}
		c.op, c.val = 'x', int(t[0]-'0')
		return c, true
	}
	n, err := strconv.Atoi(t)
	if err != nil || n < 100 || n > 999 {
		return c, false
	}
	c.val = n
	return c, true
}

// match 判断一条索引行是否满足筛选条件；conds 由 ListRequests 统一编译传入，
// f.Query 同处已归一为小写（子串比较不再逐条 ToLower）。
func (f RequestFilter) match(e IndexEntry, conds []statusCond) bool {
	if conds != nil {
		ok := false
		for _, c := range conds {
			if c.ok(e.StatusCode) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if f.StatusClass != "" {
		class := e.StatusCode / 100
		want := int(f.StatusClass[0] - '0')
		if len(f.StatusClass) != 3 || f.StatusClass[1:] != "xx" || class != want {
			return false
		}
	}
	if f.Result != "" && e.Result != f.Result {
		return false
	}
	if f.Model != "" && e.Model != f.Model && e.RequestedModel != f.Model && e.ResponseModel != f.Model {
		return false
	}
	if f.ErrorStage != "" && e.ErrorStage != f.ErrorStage {
		return false
	}
	if !f.Since.IsZero() || !f.Until.IsZero() {
		started, err := time.Parse(time.RFC3339Nano, e.StartedAt)
		if err != nil {
			return false
		}
		if !f.Since.IsZero() && !started.After(f.Since) {
			return false
		}
		if !f.Until.IsZero() && !started.Before(f.Until) {
			return false
		}
	}
	if f.Query != "" {
		haystack := e.Dir + " " + e.Method + " " + e.Path + " " + e.Model + " " +
			e.RequestedModel + " " + e.ResponseModel + " " + e.KeyHash + " " +
			e.ClientRequestID + " " + e.ErrorStage + " " + e.Result
		if !strings.Contains(strings.ToLower(haystack), f.Query) {
			return false
		}
	}
	return true
}

// listIndexCache 缓存 index.jsonl 尾部窗口的全量解析结果，键是文件的
// (size,mtime)——索引只在请求完成追加或超容量重写时变化，命中期间 ListRequests
// 只剩内存筛选。entries 按文件序（旧→新）存储，调用方倒序取新。
type listIndexCache struct {
	key         string
	entries     []IndexEntry
	windowBytes int64 // 缓存窗口覆盖的字节数（=当时 tailRead 长度），供 hasMore 判断
}

// ListRequests 返回 index.jsonl 中最新 limit 条请求摘要（新的在前）。
// 索引在目录被清理后仍保留记录，因此列表是完整历史，Detail 才可能 404。
// 结构化筛选走 filter；HasMore 提示更早历史只存在于原文件中。
func (manager *Manager) ListRequests(limit int, filter RequestFilter) ListResult {
	if manager == nil || manager.root == "" || limit <= 0 {
		return ListResult{}
	}
	path := filepath.Join(manager.root, IndexFile)
	info, err := os.Stat(path)
	if err != nil {
		return ListResult{}
	}
	key := strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
	manager.listCacheMu.Lock()
	if manager.listCache.key != key {
		data, err := TailRead(path, indexTailBytes)
		if err != nil {
			manager.listCacheMu.Unlock()
			return ListResult{}
		}
		lines := bytes.Split(data, []byte{'\n'})
		all := make([]IndexEntry, 0, len(lines))
		for _, ln := range lines {
			if len(ln) == 0 {
				continue
			}
			var e IndexEntry
			if json.Unmarshal(ln, &e) == nil {
				all = append(all, e)
			}
		}
		manager.listCache = listIndexCache{key: key, entries: all, windowBytes: int64(len(data))}
	}
	cached := manager.listCache
	manager.listCacheMu.Unlock()

	entries := make([]IndexEntry, 0, limit)
	conds := parseStatusExpr(filter.Status)
	// q 筛选的 needle 与大小写形态对全循环不变——先归一再扫，
	// 免得每条候选行各做一次 ToLower(f.Query)。
	filter.Query = strings.ToLower(filter.Query)
	// scannedAll 为 false 表示窗口内还有没扫到的行（limit 用尽），更早历史必然存在。
	scannedAll := true
	for i := len(cached.entries) - 1; i >= 0; i-- {
		if len(entries) >= limit {
			scannedAll = false
			break
		}
		if filter.match(cached.entries[i], conds) {
			entries = append(entries, cached.entries[i])
		}
	}
	// 文件比读取窗口大 → 窗口外还有历史；窗口内未扫完 → 同理。
	hasMore := info.Size() > cached.windowBytes
	result := ListResult{Entries: entries, HasMore: hasMore || !scannedAll}
	if len(cached.entries) > 0 {
		result.IndexTailStart = cached.entries[0].StartedAt
	}
	return result
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
