// 本文件实现调试日志的生命周期管理：分层保留请求目录。
//
// 设计要点（参照 CLIProxyAPI log_dir_cleaner 与同类网关的调试日志短 TTL）：
//   - 后台 ticker 周期执行，失败只告警不中断；
//   - activeDirs 中仍在写入的目录永不删除；
//   - 大体积负载（上游响应/客户端 SSE/附件）先剥离，证据文件
//     （meta/error/01/02/05）留满整周期——磁盘大头是负载，
//     留小文件不影响排障入口；
//   - 容量淘汰时保护最近 N 个失败目录（含 error.json），成功请求先删；
//   - index.jsonl、quota.jsonl、stderr.log 等顶层文件不属于请求目录。
package debuglog

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// cleanerInterval 是清理周期。三个维度全是粗粒度策略（小时级负载剥离、
// 天级目录淘汰、GB 级总量软上限），不需要分钟级精度；周期放大到 5 分钟
// 可把每轮的全树 dirSize 遍历（每目录一次 Walk）摊薄到可忽略。
const cleanerInterval = 5 * time.Minute

// payloadNames 是「负载层」文件：体积大、只在近距排障时需要。
// 超时后被剥离，meta.json/error.json/01/02/05 等证据继续保留。
var payloadNames = []string{
	"03-devin-request.json",
	"04-devin-response.jsonl",
	"06-http-response.jsonl",
	"attachments",
}

// runCleaner 是后台清理协程：按 ticker 周期执行 retention 检查，
// 收到 stop 信号时退出。Close 通过 cleanerDone 等它结束。
func (manager *Manager) runCleaner() {
	defer close(manager.cleanerDone)
	ticker := time.NewTicker(cleanerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-manager.cleanerStop:
			return
		case <-ticker.C:
			if removed := manager.cleanOnce(); removed > 0 {
				slog.Debug("debuglog: cleaned old request logs", "removed_dirs", removed)
			}
		}
	}
}

// requestDir 是清理决策所需的目录摘要。
type requestDir struct {
	name     string
	size     int64
	modTime  time.Time
	hasError bool // 含 error.json，失败现场
}

// cleanOnce 执行一轮清理，返回删除的目录数。
// 顺序：活跃目录跳过 → 剥离超龄负载 → 删超龄目录 → 总量超限从最旧淘汰
// （受保护的失败目录除外）。
func (manager *Manager) cleanOnce() int {
	entries, err := os.ReadDir(manager.root)
	if err != nil {
		return 0
	}
	manager.mutex.Lock()
	active := make(map[string]struct{}, len(manager.activeDirs))
	for name := range manager.activeDirs {
		active[name] = struct{}{}
	}
	manager.mutex.Unlock()

	policy := manager.policy
	maxBytes := policy.MaxTotalMB << 20
	now := time.Now()
	ageCutoff := now.Add(-time.Duration(policy.Days) * 24 * time.Hour)
	payloadCutoff := now.Add(-time.Duration(policy.PayloadHours) * time.Hour)

	var dirs []requestDir
	var totalBytes int64
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue // index.jsonl 等顶层文件不属于请求目录
		}
		if _, ok := active[entry.Name()]; ok {
			continue // 请求仍在写入，保护其证据完整性
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(manager.root, entry.Name())
		if policy.PayloadHours > 0 && info.ModTime().Before(payloadCutoff) {
			stripPayload(full)
		}
		if policy.Days > 0 && info.ModTime().Before(ageCutoff) {
			if os.RemoveAll(full) == nil {
				removed++
				continue
			}
		}
		dir := requestDir{name: entry.Name(), modTime: info.ModTime()}
		if maxBytes > 0 {
			dir.size = dirSize(full)
			totalBytes += dir.size
			if policy.KeepErrorDirs > 0 {
				if _, statErr := os.Stat(filepath.Join(full, "error.json")); statErr == nil {
					dir.hasError = true
				}
			}
		}
		dirs = append(dirs, dir)
	}

	// 总量超限后从最旧的目录开始回收，直到回到上限内。
	// 最近 policy.KeepErrorDirs 个失败目录受保护：失败现场恰恰是日后最想回看的。
	if maxBytes > 0 && totalBytes > maxBytes {
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].modTime.Before(dirs[j].modTime) })
		errorCount := 0
		for _, dir := range dirs {
			if dir.hasError {
				errorCount++
			}
		}
		// dirs 按旧到新排序：前 deletableErrors 个失败目录仍可淘汰，
		// 末尾 KeepErrorDirs 个失败目录豁免。
		deletableErrors := errorCount - policy.KeepErrorDirs
		for _, dir := range dirs {
			if totalBytes <= maxBytes {
				break
			}
			if dir.hasError {
				if deletableErrors > 0 {
					deletableErrors--
				} else {
					continue
				}
			}
			if os.RemoveAll(filepath.Join(manager.root, dir.name)) == nil {
				totalBytes -= dir.size
				removed++
			}
		}
	}
	return removed
}

// stripPayload 删除目录内的大体积负载文件，保留证据层文件。
// 返回释放的字节数；文件本就不存在不是错误。
func stripPayload(dir string) int64 {
	var freed int64
	for _, name := range payloadNames {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		size := info.Size()
		if info.IsDir() {
			size = dirSize(path)
		}
		if os.RemoveAll(path) == nil {
			freed += size
		}
	}
	return freed
}

// dirSize 递归汇总目录字节数；失败文件按 0 计。
func dirSize(root string) int64 {
	var size int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	return size
}
