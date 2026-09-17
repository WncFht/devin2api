// 本文件实现调试日志的生命周期管理：分层保留调试 payload。
//
// 设计要点（参照 CLIProxyAPI log_dir_cleaner 与同类网关的调试日志短 TTL）：
//   - 后台 ticker 周期执行，失败只告警不中断；
//   - activeDirs 中仍在写入的目录永不删除——删除界按活跃集的最小名钳位，
//     等价旧实现的逐目录跳过（目录名内嵌时间戳，字典序即时间序）；
//   - 大体积负载（上游响应/客户端 SSE/附件）先剥离，证据文件
//     （meta/error/01/02/05）留满整周期——库里的大头是负载，
//     留小文件不影响排障入口；
//   - 容量淘汰时保护最近 N 个失败目录（含 error.json），成功请求先删；
//   - stderr.log 等顶层文件不属于请求目录，留盘不管；logs 表行的时间
//     清理见 cleanLogRows（与 payload 保留是两条独立轴）。
package debuglog

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// cleanerInterval 是清理周期。三个维度全是粗粒度策略（小时级负载剥离、
// 天级目录淘汰、GB 级总量软上限），不需要分钟级精度；周期放大到 5 分钟
// 可把每轮的全表聚合摊薄到可忽略。
const cleanerInterval = 5 * time.Minute

// isPayloadName 判定目录内（按 debug 行键名）的「负载层」成员：体积大、
// 只在近距排障时需要。超时后被剥离，meta.json/error.json/01/02/05 等
// 证据继续保留。devinRequestStageStem+"." 前缀同时圈出 03 主文件与全部
// 重试分片（03-devin-request.attemptN.json）与托管搜索分片——精确名
// 匹配会漏掉分片，重试请求的大体积请求体将永不剥离。
func isPayloadName(name string) bool {
	if strings.HasPrefix(name, devinRequestStageStem+".") || strings.HasPrefix(name, AttachmentsDir+"/") {
		return true
	}
	switch name {
	case StageDevinResponse, StageHTTPResponse:
		return true
	}
	return false
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

// cleanOnce 执行一轮清理，返回删除的目录数。
// 顺序：logs 行按龄删除 → 剥离超龄负载 → 删超龄目录 →
// 总量超限从最旧淘汰（受保护的失败目录与活跃目录除外）。
// 目录名内嵌 "20060102-150405" 时间戳：字典序界即时间界。
func (manager *Manager) cleanOnce() int {
	manager.cleanLogRows()
	if manager.store == nil {
		return 0
	}
	ctx := context.Background()
	dirs, err := manager.store.DebugDirs(ctx)
	if err != nil {
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: list debug dirs failed", "error", err)
		return 0
	}
	manager.mutex.Lock()
	active := make(map[string]struct{}, len(manager.activeDirs))
	minActive := ""
	for name := range manager.activeDirs {
		active[name] = struct{}{}
		if minActive == "" || name < minActive {
			minActive = name
		}
	}
	manager.mutex.Unlock()

	policy := manager.Policy()
	now := time.Now()
	removed := 0

	// 负载剥离：逐目录筛出负载名删除。活跃目录整体跳过（请求还在写）。
	if policy.PayloadHours > 0 {
		payloadBound := now.Add(-time.Duration(policy.PayloadHours) * time.Hour).Format("20060102-150405")
		for _, dir := range dirs {
			if _, ok := active[dir]; ok || dir >= payloadBound {
				continue
			}
			names, err := manager.store.DebugFileNames(ctx, dir)
			if err != nil {
				manager.ioErrors.Add(1)
				slog.Warn("debuglog: list dir files failed", "dir", dir, "error", err)
				continue
			}
			var payloads []string
			for _, name := range names {
				if isPayloadName(name) {
					payloads = append(payloads, name)
				}
			}
			if err := manager.store.DeleteDebugPayloadFiles(ctx, dir, payloads); err != nil {
				manager.ioErrors.Add(1)
				slog.Warn("debuglog: strip payload failed", "dir", dir, "error", err)
			}
		}
	}

	// 按龄删除：界名压到活跃集最小名之下，等价旧实现「跳过活跃目录」。
	if policy.Days > 0 {
		bound := now.Add(-time.Duration(policy.Days) * 24 * time.Hour).Format("20060102-150405")
		if minActive != "" && minActive < bound {
			bound = minActive
		}
		deletables := 0
		for _, dir := range dirs {
			if dir < bound {
				deletables++
			}
		}
		if deletables > 0 {
			if err := manager.store.DeleteDebugDirsBefore(ctx, bound); err != nil {
				manager.ioErrors.Add(1)
				slog.Warn("debuglog: delete expired dirs failed", "error", err)
			} else {
				removed += deletables
			}
		}
	}

	maxBytes := policy.MaxTotalMB << 20
	if maxBytes <= 0 {
		return removed
	}

	// 容量淘汰：总量超限后从最旧的目录开始回收，直到回到上限内。
	// 最近 policy.KeepErrorDirs 个失败目录受保护：失败现场恰恰是日后最想回看的。
	sizes, err := manager.store.DebugDirSizes(ctx)
	if err != nil {
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: measure debug dirs failed", "error", err)
		return removed
	}
	errorDirs := map[string]bool{}
	if policy.KeepErrorDirs > 0 {
		if errorDirs, err = manager.store.DebugDirsContaining(ctx, ErrorFile); err != nil {
			manager.ioErrors.Add(1)
			slog.Warn("debuglog: list error dirs failed", "error", err)
			errorDirs = map[string]bool{}
		}
	}
	var totalBytes int64
	var candidates []string
	for dir, size := range sizes {
		if _, ok := active[dir]; ok {
			continue
		}
		totalBytes += size
		candidates = append(candidates, dir)
	}
	if totalBytes <= maxBytes {
		return removed
	}
	sort.Strings(candidates) // 名序即时间序
	errorCount := 0
	for _, dir := range candidates {
		if errorDirs[dir] {
			errorCount++
		}
	}
	// candidates 按旧到新排序：前 deletableErrors 个失败目录仍可淘汰，
	// 末尾 KeepErrorDirs 个失败目录豁免。
	deletableErrors := errorCount - policy.KeepErrorDirs
	for _, dir := range candidates {
		if totalBytes <= maxBytes {
			break
		}
		if errorDirs[dir] {
			if deletableErrors > 0 {
				deletableErrors--
			} else {
				continue
			}
		}
		if err := manager.store.DeleteDebugDir(ctx, dir); err != nil {
			manager.ioErrors.Add(1)
			slog.Warn("debuglog: evict dir failed", "dir", dir, "error", err)
			continue
		}
		totalBytes -= sizes[dir]
		removed++
	}
	return removed
}

// cleanLogRows 按 LogRowRetentionDays 删除 logs 表的过期行；失败记
// ioErrors 并告警——行清理是周期任务，一次失败不该静默漂移。
func (manager *Manager) cleanLogRows() {
	days := manager.LogRowRetentionDays()
	if manager.store == nil || days <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	if _, err := manager.store.DeleteLogsBefore(context.Background(), cutoff); err != nil {
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: clean log rows failed", "error", err)
	}
}
