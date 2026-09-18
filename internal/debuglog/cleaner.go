// 本文件实现调试日志的生命周期管理：分层保留调试 payload。
//
// 设计要点（参照 CLIProxyAPI log_dir_cleaner 与同类网关的调试日志短 TTL）：
//   - 后台 ticker 周期执行，失败只告警不中断；
//   - activeDirs 中仍在写入的目录永不删除——删除界按活跃集的最小名钳位，
//     等价旧实现的逐目录跳过（目录名内嵌时间戳，字典序即时间序）；
//   - 大体积负载（上游响应/客户端 SSE/附件）先剥离，证据文件
//     （meta/error/01/02/05）留满整周期——库里的大头是负载，
//     留小文件不影响排障入口；
//   - 容量淘汰分两相：超限先把最旧目录剥到 meta/error 归因锚点
//     （payload 放掉、排障入口保住），剥载仍回不到限内才整目录删除；
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

// payloadReconcileInterval 是 payload 计数器的周期对账间隔：计数器按
// 提交增量维护、设计上零漂移，仍保留低频全量对账兜底漏记账路径——
// 漂移告警本身比静默修正更有信息量。每小时一轮，成本是每 12 个
// 清理 tick 多一次聚合扫。
const payloadReconcileInterval = time.Hour

// 负载层的成员判定已下推成 DeleteDebugPayloadsBefore 的名字谓词
//（devinRequestStageStem+"." 前缀圈出 03 主文件与全部重试/搜索分片、
// 04/06 精确名、attachments/ 前缀）——剥离名单与该谓词同源维护。

// capacityAnchorFiles 是容量淘汰剥载相保留的归因锚点：meta.json 记
// 结果/模型/延迟/token/终局 lane，error.json 记首失败点（keep_error_dirs
// 的失败目录识别也靠它——锚点在则保护身份在）——合计 ~1KB/dir，
// 留住即保住排障入口。与 WriteDebugBatch StripDirs 的锚点名单一致。
var capacityAnchorFiles = []string{MetaFile, ErrorFile}

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
			if removed, stripped := manager.cleanOnce(); removed+stripped > 0 {
				slog.Debug("debuglog: cleaned old request logs", "removed_dirs", removed, "stripped_dirs", stripped)
			}
		}
	}
}

// cleanOnce 执行一轮清理，返回整删的目录数与剥到锚点的目录数。
// 顺序：剥离超龄负载 → 删超龄目录 → 总量超限先剥载到锚点、仍超限
// 再整目录淘汰（受保护的失败目录与活跃目录除外）。
// 目录名内嵌 "20060102-150405" 时间戳：字典序界即时间界。
func (manager *Manager) cleanOnce() (removed, stripped int) {
	if manager.store == nil {
		return 0, 0
	}
	ctx := context.Background()
	dirs, err := manager.store.DebugDirs(ctx)
	if err != nil {
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: list debug dirs failed", "error", err)
		return 0, 0
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
		return removed, stripped
	}

	// 容量淘汰：总量超限后从最旧的目录开始回收，直到回到上限内。
	// 最近 policy.KeepErrorDirs 个失败目录受保护：失败现场恰恰是日后最想回看的。
	// 闸门读 payload 计数器（写/删路径随事务增减的内存镜像，与
	// DebugDirSizes 总量同口径）：它没超限时跳过全表聚合扫（5min
	// 一轮，在 GB 级库上是一大笔固定开销）。原 DBBytes 口径把库文件
	// +WAL 的物理尺寸当 payload 用，WAL 膨胀后闸门恒真、聚合白跑。
	reconcileDue := now.Sub(manager.lastPayloadReconcile) >= payloadReconcileInterval
	if manager.store.DebugPayloadBytes() <= maxBytes && !reconcileDue {
		return removed, stripped
	}
	sizes, strippable, err := manager.store.DebugDirSizesSplit(ctx, capacityAnchorFiles)
	if err != nil {
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: measure debug dirs failed", "error", err)
		return removed, stripped
	}
	// 对账：计数器是派生态，聚合出的存量是权威；漂移告警而非静默修
	// 正——恒非零漂移说明有写/删路径漏了记账。
	manager.lastPayloadReconcile = now
	var storedBytes, totalBytes int64
	var candidates []string
	for dir, size := range sizes {
		storedBytes += size
		if _, ok := active[dir]; ok {
			continue
		}
		totalBytes += size
		candidates = append(candidates, dir)
	}
	// CAS 共享 blob 不属于任何目录（refs 已按目录摊进 sizes），但占
	// 全局库存——对账与淘汰闸都把它加在目录合计之上，与 debugBytes
	// 计数器的四表口径对齐。目录先死、blob 由 mark-sweep 滞后收尸，
	// 闸读到的 blob 字节含已删目录的暂留份额，need 因此略偏高估——
	// 淘汰偏保守方向，收敛后口径精确。
	blobBytes, err := manager.store.DebugBlobBytes(ctx)
	if err != nil {
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: measure debug blobs failed", "error", err)
		return removed, stripped
	}
	storedBytes += blobBytes
	totalBytes += blobBytes
	if drift := manager.store.ReconcileDebugPayloadBytes(storedBytes); drift != 0 {
		slog.Warn("debuglog: payload byte counter drifted", "drift", drift, "stored_bytes", storedBytes)
	}
	if totalBytes <= maxBytes {
		return removed, stripped
	}
	sort.Strings(candidates) // 名序即时间序
	protected := manager.protectedErrorDirs(ctx, candidates, errorDirs, policy.KeepErrorDirs)
	// exclude = 保护集 ∪ 活跃目录：活跃目录名可能小于界（长流仍在写），
	// 谓词界圈不出它，只能名单豁免——等价旧实现的「不进 candidates」。
	// 剥载与整删两相共用同一豁免集。
	exclude := make([]string, 0, len(protected)+len(active))
	for dir := range protected {
		exclude = append(exclude, dir)
	}
	for dir := range active {
		exclude = append(exclude, dir)
	}
	need := totalBytes - maxBytes

	// 第一相：剥载到归因锚点。最旧目录先只留 meta/error——meta 记
	// 结果/模型/延迟/token/终局 lane，error 记首失败点，合计 ~1KB/dir；
	// 01/02/03/04/05/06/attachments 的 payload 大头全放。容量压力
	// 几乎全由 payload 构成（实测 01 一项即占持有量 ~59%），剥载
	// 一己之力回限内时一个目录都不用死。
	var freedStrip int64
	stripLast := -1
	for i, dir := range candidates {
		if freedStrip >= need {
			break
		}
		if protected[dir] {
			continue
		}
		freedStrip += strippable[dir]
		stripLast = i
	}
	if stripLast < 0 {
		return removed, stripped
	}
	stripBound := "\xff"
	if stripLast+1 < len(candidates) {
		stripBound = candidates[stripLast+1]
	}
	if err := manager.store.StripDebugDirsBefore(ctx, stripBound, capacityAnchorFiles, exclude); err != nil {
		// 剥载失败不转整删——同一存储故障下一相多半同样失败，
		// 等下一轮重试比抢删更稳。
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: strip dirs to anchor failed", "bound", stripBound, "error", err)
		return removed, stripped
	}
	stripped += stripLast + 1 - countProtectedBelow(candidates, protected, stripLast)
	remaining := totalBytes - freedStrip
	if remaining <= maxBytes {
		return removed, stripped
	}

	// 第二相：剥载仍不够才整目录删除。已剥目录按剥后残值（锚点
	// ~1KB）计 freed——它们的死几乎不贡献释放，walk 越过它们圈住
	// 仍带 payload 的目录；释放不出时锚点随目录一起死，与旧实现
	// 「最旧先死」语义一致。
	need2 := remaining - maxBytes
	var freed int64
	last, evicted := -1, 0
	for i, dir := range candidates {
		if freed >= need2 {
			break
		}
		if protected[dir] {
			continue
		}
		size := sizes[dir]
		if i <= stripLast {
			size -= strippable[dir]
		}
		freed += size
		last, evicted = i, evicted+1
	}
	if last < 0 {
		return removed, stripped
	}
	// 界取下一候选名（dir<bound 圈出 candidates[:last+1]——字典序界即
	// 删除范围）；删到候选末尾时没有更大名，"\xff" 越过一切目录名。
	bound := "\xff"
	if last+1 < len(candidates) {
		bound = candidates[last+1]
	}
	// 一条集合 DELETE 完成整轮淘汰，替代逐目录 autocommit——prod 每轮
	// ~700 次独立事务曾把唯一写连接占满，insertQ 排空停滞、分片溢出
	// 尾丢，丢弃突发正与淘汰 tick 聚簇。
	if err := manager.store.DeleteDebugDirsBefore(ctx, bound, exclude); err != nil {
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: evict dirs failed", "bound", bound, "error", err)
	} else {
		removed += evicted
	}
	return removed, stripped
}

// countProtectedBelow 数 candidates[:last+1] 里受保护的目录数——剥载
// 界内的保护目录不在 StripDebugDirsBefore 的删除范围（exclude 豁免），
// stripped 计数要扣掉它们。
func countProtectedBelow(candidates []string, protected map[string]bool, last int) int {
	n := 0
	for i := 0; i <= last && i < len(candidates); i++ {
		if protected[candidates[i]] {
			n++
		}
	}
	return n
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
	if keepErrorDirs <= 0 || len(errorDirs) == 0 {
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
