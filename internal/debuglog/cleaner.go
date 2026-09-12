// 本文件实现调试日志的生命周期管理：按保留天数和总量上限清理请求目录。
//
// 设计要点（参照 CLIProxyAPI log_dir_cleaner）：
//   - 后台 ticker 周期执行，失败只告警不中断；
//   - activeDirs 中仍在写入的目录永不删除；
//   - 按时间先清、再按总量从旧到新清；
//   - index.jsonl 是索引不是请求目录，不参与清理。
package debuglog

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// cleanerInterval 是清理周期；一分钟足够及时，也不会带来可感知的 IO 压力。
const cleanerInterval = time.Minute

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
	name    string
	size    int64
	modTime time.Time
}

// cleanOnce 执行一轮清理，返回删除的目录数。
// 活跃目录（请求仍在写入）永远跳过；先删超龄目录，再按总量从最旧开始删。
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

	cutoff := time.Now().Add(-time.Duration(manager.retentionDays) * 24 * time.Hour)
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
		if manager.retentionDays > 0 && info.ModTime().Before(cutoff) {
			if os.RemoveAll(full) == nil {
				removed++
				continue
			}
		}
		dir := requestDir{name: entry.Name(), modTime: info.ModTime()}
		if manager.maxBytes > 0 {
			dir.size = dirSize(full)
			totalBytes += dir.size
		}
		dirs = append(dirs, dir)
	}

	// 总量超限后从最旧的目录开始回收，直到回到上限内。
	if manager.maxBytes > 0 && totalBytes > manager.maxBytes {
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].modTime.Before(dirs[j].modTime) })
		for _, dir := range dirs {
			if totalBytes <= manager.maxBytes {
				break
			}
			if os.RemoveAll(filepath.Join(manager.root, dir.name)) == nil {
				totalBytes -= dir.size
				removed++
			}
		}
	}
	return removed
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
