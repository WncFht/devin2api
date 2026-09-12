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
	"strings"
	"time"
)

// indexTailBytes 限定 ListRequests 只读 index.jsonl 尾部；
// 4MB 约覆盖一万条请求记录，更早的历史用 grep 查原文件。
const indexTailBytes = 4 << 20

// fileReadCap 是单文件读取上限；超出时截断并在响应里标记 truncated。
const fileReadCap = 4 << 20

// requestDirPattern 约束请求目录名，防止路径穿越读取任意目录。
var requestDirPattern = regexp.MustCompile(`^\d{8}-\d{6}(-\d{2})?$`)

// ListRequests 返回 index.jsonl 中最新 limit 条请求摘要（新的在前）。
// 索引在目录被清理后仍保留记录，因此列表是完整历史，Detail 才可能 404。
func (manager *Manager) ListRequests(limit int) []IndexEntry {
	if manager == nil || manager.root == "" || limit <= 0 {
		return nil
	}
	data, err := tailRead(filepath.Join(manager.root, "index.jsonl"), indexTailBytes)
	if err != nil {
		return nil
	}
	lines := bytes.Split(data, []byte{'\n'})
	entries := make([]IndexEntry, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(entries) < limit; i-- {
		if len(lines[i]) == 0 {
			continue
		}
		var entry IndexEntry
		if json.Unmarshal(lines[i], &entry) == nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

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
	if data, err := os.ReadFile(filepath.Join(root, "meta.json")); err == nil && json.Valid(data) {
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
	return len(parts) == 1 || (len(parts) == 2 && parts[0] == "attachments")
}

// ActiveRequest 是一个仍在进行中的请求的可观测快照。
type ActiveRequest struct {
	// Dir 是请求日志目录名。
	Dir string `json:"dir"`
	// Meta 是创建时记录的 HTTP 元信息。
	Meta RequestMeta `json:"meta"`
	// StartedAt 是请求进入时间。
	StartedAt time.Time `json:"started_at"`
	// ElapsedMS 是快照时刻相对进入时间的毫秒数。
	ElapsedMS int64 `json:"elapsed_ms"`
	// DroppedEvents 是目前已被丢弃的写任务数。
	DroppedEvents uint64 `json:"dropped_events"`
	// Files 是目录内目前已落盘的文件清单。
	Files []RequestFileInfo `json:"files"`
}

// ActiveRequests 返回仍在写入的请求目录快照（ccLoad 的
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
	now := time.Now()
	out := make([]ActiveRequest, 0, len(recorders))
	for _, recorder := range recorders {
		out = append(out, ActiveRequest{
			Dir:           filepath.Base(recorder.directory),
			Meta:          recorder.requestMeta,
			StartedAt:     recorder.startedAt,
			ElapsedMS:     now.Sub(recorder.startedAt).Milliseconds(),
			DroppedEvents: recorder.dropped.Load(),
			Files:         listRequestFiles(recorder.directory),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// tailRead 读取文件末尾至多 max 字节；文件小于 max 时读全文。
// 返回内容从第一个完整行开始（丢弃被截断的首行由调用方按行解析时自然跳过）。
func tailRead(path string, max int64) ([]byte, error) {
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
