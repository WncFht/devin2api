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
//   - stderr.log 等顶层文件不属于请求目录，留盘不管；logs 表行、
//     quota_samples 行数与 freelist 回收归 store.Maintain（main.go
//     ticker 驱动），与这里的目录保留是两条独立轴。
package debuglog

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"time"
)

// cleanerInterval 是清理周期。三个维度全是粗粒度策略（小时级负载剥离、
// 天级目录淘汰、GB 级总量软上限），不需要分钟级精度；周期放大到 5 分钟
// 可把每轮的全表聚合摊薄到可忽略。
const cleanerInterval = 5 * time.Minute

// 负载层的成员判定已下推成 DeleteDebugPayloadsBefore 的名字谓词
//（devinRequestStageStem+"." 前缀圈出 03 主文件与全部重试/搜索分片、
// 04/06 精确名、attachments/ 前缀）——剥离名单与该谓词同源维护。

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
// 顺序：剥离超龄负载 → 删超龄目录 → 总量超限从最旧淘汰（受保护的
// 失败目录与活跃目录除外）。
// 目录名内嵌 "20060102-150405" 时间戳：字典序界即时间界。
func (manager *Manager) cleanOnce() int {
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

	// 负载剥离：一条集合 DELETE 替代逐目录枚举+删除的 N+1（一轮
	// ~900 目录曾付 1800+ 查询）。bound 钳到活跃集最小名之下，
	// 等价旧实现的逐目录活跃跳过。
	if policy.PayloadHours > 0 {
		payloadBound := now.Add(-time.Duration(policy.PayloadHours) * time.Hour).Format("20060102-150405")
		if minActive != "" && minActive < payloadBound {
			payloadBound = minActive
		}
		if err := manager.store.DeleteDebugPayloadsBefore(ctx, payloadBound,
			[]string{StageDevinResponse, StageHTTPResponse},
			[]string{devinRequestStageStem + ".", AttachmentsDir + "/"}); err != nil {
			manager.ioErrors.Add(1)
			slog.Warn("debuglog: strip payloads failed", "error", err)
		}
	}

	// errorDirs 提前取：龄删豁免与容量淘汰共用同一保护集口径。
	errorDirs := map[string]bool{}
	if policy.KeepErrorDirs > 0 {
		if errorDirs, err = manager.store.DebugDirsContaining(ctx, ErrorFile); err != nil {
			manager.ioErrors.Add(1)
			slog.Warn("debuglog: list error dirs failed", "error", err)
			errorDirs = map[string]bool{}
		}
	}

	// 按龄删除：界名压到活跃集最小名之下，等价旧实现「跳过活跃目录」。
	// 受保护的失败目录同样豁免——「最近 N 个失败目录受保护」对龄删
	// 与容量淘汰同语义，不再只豁免后者。
	if policy.Days > 0 {
		bound := now.Add(-time.Duration(policy.Days) * 24 * time.Hour).Format("20060102-150405")
		if minActive != "" && minActive < bound {
			bound = minActive
		}
		var deletable []string
		for _, dir := range dirs {
			if dir < bound {
				deletable = append(deletable, dir)
			}
		}
		protected := manager.protectedErrorDirs(ctx, deletable, errorDirs, policy.KeepErrorDirs)
		var exclude []string
		for dir := range protected {
			exclude = append(exclude, dir)
		}
		if len(deletable) > len(exclude) {
			if err := manager.store.DeleteDebugDirsBefore(ctx, bound, exclude); err != nil {
				manager.ioErrors.Add(1)
				slog.Warn("debuglog: delete expired dirs failed", "error", err)
			} else {
				removed += len(deletable) - len(exclude)
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
	protected := manager.protectedErrorDirs(ctx, candidates, errorDirs, policy.KeepErrorDirs)
	for _, dir := range candidates {
		if totalBytes <= maxBytes {
			break
		}
		if protected[dir] {
			continue
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

// errorSigLongRun 折叠签名里的长数字串与 hex id（request id/时间戳）：
// 同一场风暴的消息只差这些尾部实例值，折叠后才共享签名、共享保护名额。
// 模型名里的短版本号段（"5-3"）不命中——不同模型/不同错误仍是不同签名。
var errorSigLongRun = regexp.MustCompile(`[0-9a-fA-F]{8,}|[0-9]{4,}`)

// protectedErrorDirs 计算淘汰豁免集：从新到旧挑含 error.json 的目录，
// 总量 cap 是 keepErrorDirs，且同一错误签名（stage+归一化消息）最多占
// keepErrorDirs/16（至少 4）个名额——一场同签名风暴不再能把保护窗
// 挤满、把稀有失败的现场顶出去。无 logs 归因行的目录（413/行已先删）
// 各自独占签名，不参与归并。龄删与容量淘汰两处调用共用同一豁免口径。
func (manager *Manager) protectedErrorDirs(ctx context.Context, candidates []string, errorDirs map[string]bool, keepErrorDirs int) map[string]bool {
	protected := map[string]bool{}
	if keepErrorDirs <= 0 {
		return protected
	}
	sigs, err := manager.store.DebugErrorSignatures(ctx)
	if err != nil {
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: list error signatures failed", "error", err)
		sigs = map[string][2]string{}
	}
	perSigCap := keepErrorDirs / 16
	if perSigCap < 4 {
		perSigCap = 4
	}
	sigCount := map[string]int{}
	for i := len(candidates) - 1; i >= 0 && len(protected) < keepErrorDirs; i-- {
		dir := candidates[i]
		if !errorDirs[dir] {
			continue
		}
		sig, ok := sigs[dir]
		sigKey := dir // 无归因行的目录按各自独立签名处理
		if ok {
			sigKey = sig[0] + "\x00" + errorSigLongRun.ReplaceAllString(sig[1], "#")
		}
		if sigCount[sigKey] >= perSigCap {
			continue
		}
		sigCount[sigKey]++
		protected[dir] = true
	}
	return protected
}
