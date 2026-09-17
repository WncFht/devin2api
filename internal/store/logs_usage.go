// 本文件是 /admin/usage 与 runtime-metrics 延迟组的聚合读路径——
// 旧 debuglog.usageAggregator（内存 replay 索引尾部）的 SQL 版。
// 聚合源头从「256MB 索引尾部回放」升级为 logs 全表：条数/维度计数由
// GROUP BY 一次算出，分位数仍按旧口径取最近 N 条样本在 Go 侧排序
// （SQLite 无原生分位数；窗口函数把「每桶/每维最近 N 条」压成一次扫描）。
package store

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// logDecodeCond 是「可信流式」条目的 SQL 判定（旧 decodeWindow 同款）：
// 完成态 + 有首帧 + 有输出 token + 表面速率不超物理上限。400 tok/s 是
// 可信边界——上游（swe-2 走 Devin 协议）常把整段结果集中在末帧突发
// 下发，duration−TTFB≈0 除出的速率是测量伪影；真实 decode 峰值约 300+。
const logDecodeCond = `result = 'completed' AND first_upstream_ms IS NOT NULL AND output_tokens > 0
		AND duration_ms - first_upstream_ms > 0
		AND output_tokens * 1000 <= 400 * (duration_ms - first_upstream_ms)`

// logOwnerCase 与 debuglog.ErrorOwner 同一判定链的 SQL 版（store 不能
// 反向依赖 debuglog 的阶段常量，'http_read'/'http_decode' 字面量对应
// ErrStageHTTPRead/HTTPDecode，两边必须同步改）。
const logOwnerCase = `CASE
		WHEN status_code = 429 OR rate_limited != 0 THEN 'business_limited'
		WHEN result IN ('disconnected', 'aborted') THEN 'client'
		WHEN status_code < 400 AND result != 'failed' THEN 'none'
		WHEN error_stage IN ('http_read', 'http_decode') THEN 'client'
		ELSE 'upstream' END`

// dayBoundsMS 返回 t 所在本地日的毫秒闭开区间界（[起点, 次日起点)），
// 供 today 谓词替代不可索引的 logDayExpr 等值比较。
func dayBoundsMS(t time.Time) (int64, int64) {
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return start.UnixMilli(), start.AddDate(0, 0, 1).UnixMilli()
}

// logDayExpr 是本地时区日期键：strftime 的 'localtime' 修饰符走连接
// 配置的 _loc=Local——与旧 Go 侧 started.Local().Format 同一时区源。
const logDayExpr = `strftime('%Y-%m-%d', time/1000, 'unixepoch', 'localtime')`

// usageTotalsCols 是 UsageTotals 15 个字段的聚合列清单（顺序即
// usageTotalsDests 的目标顺序）。全部 SUM 套 COALESCE：无 GROUP BY 的
// 整表查询在空表上 SUM 出 NULL，分组查询里无害。
const usageTotalsCols = `
	COUNT(*),
	COALESCE(SUM(CASE WHEN result IN ('disconnected', 'aborted') THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN result NOT IN ('disconnected', 'aborted') AND (status_code >= 400 OR result = 'failed') THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status_code = 429 OR rate_limited != 0 THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN ` + logOwnerCase + ` = 'client' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN ` + logOwnerCase + ` = 'upstream' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(cache_write_tokens), 0),
	COALESCE(SUM(reasoning_tokens), 0),
	COALESCE(SUM(total_tokens), 0),
	COALESCE(SUM(credit_cost), 0),
	COALESCE(SUM(CASE WHEN ` + logDecodeCond + ` THEN duration_ms - first_upstream_ms ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN ` + logDecodeCond + ` THEN output_tokens ELSE 0 END), 0)`

// UsageTotals 是一组请求的基础累计量（JSON 形状与旧 usageTotals 一致）。
type UsageTotals struct {
	Requests     int64 `json:"requests"`
	Errors       int64 `json:"errors"`       // status>=400 或 result=failed
	Disconnected int64 `json:"disconnected"` // 含 aborted
	// RateLimited 是上游返回 429 的次数。本地并发拒绝在 recorder 创建前
	// 返回、不进 logs 表，故此处纯为上游限流语义。
	RateLimited int64 `json:"rate_limited"`
	// ClientFaults/UpstreamFaults 是 errorOwner 归因计数：调用方责任
	//（断连/中断/请求体阶段失败）与服务端责任（上游错误与代理自身
	// 失败）分列——SLA 口径只把后者算作失分。
	ClientFaults   int64 `json:"client_faults"`
	UpstreamFaults int64 `json:"upstream_faults"`
	InputTokens    int64 `json:"input_tokens"`
	OutputTokens   int64 `json:"output_tokens"`
	CacheRead      int64 `json:"cache_read_tokens"`
	CacheWrite     int64 `json:"cache_write_tokens"`
	Reasoning      int64 `json:"reasoning_tokens"`
	TotalTokens    int64 `json:"total_tokens"`
	// CreditCost 是上游权威计费读数的累计（替代纯 token 估算的 quota 口径）。
	CreditCost int64 `json:"credit_cost,omitempty"`
	// GenMS/GenOut 是 decode 速率的分母分子：只累计「可信流式」条目
	//（见 logDecodeCond），前端用 gen_tokens/gen_ms 求 tok/s。
	GenMS  int64 `json:"gen_ms,omitempty"`
	GenOut int64 `json:"gen_tokens,omitempty"`
}

// sqlScanner 抽象 *sql.Row/*sql.Rows 的 Scan。
type sqlScanner interface {
	Scan(dest ...any) error
}

// usageTotalsDests 按 usageTotalsCols 顺序返回扫描目标。
func usageTotalsDests(t *UsageTotals) []any {
	return []any{&t.Requests, &t.Disconnected, &t.Errors, &t.RateLimited,
		&t.ClientFaults, &t.UpstreamFaults, &t.InputTokens, &t.OutputTokens,
		&t.CacheRead, &t.CacheWrite, &t.Reasoning, &t.TotalTokens, &t.CreditCost,
		&t.GenMS, &t.GenOut}
}

// scanUsageTotals 把 usageTotalsCols 形态的行扫进 t（单列行查询用）。
func scanUsageTotals(row sqlScanner, t *UsageTotals) error {
	return row.Scan(usageTotalsDests(t)...)
}

// usageSampleCapacity 是延迟分位数样本容量（最近 N 条完成请求）。
const usageSampleCapacity = 4096

// usageMinBuckets 是细粒度趋势保留的 10 分钟桶数（8 天）。
const usageMinBuckets = 6 * 24 * 8

// usageMinSampleCap 是单桶内保留的延迟样本上限。
const usageMinSampleCap = 256

// usageMaxDays 是天聚合输出的天数上限。
const usageMaxDays = 31

// dimensionSampleCapacity 是单维度行 token 样本容量。
const dimensionSampleCapacity = 2048

// rateLimitEventCap 是限流事件保留条数。
const rateLimitEventCap = 256

// UsageMinPoint 是输出给面板的 10 分钟粒度数据点。
type UsageMinPoint struct {
	At int64 `json:"at"` // 桶起点 unix 秒
	UsageTotals
	DurP95  int64 `json:"duration_p95_ms"`
	AvgTTFB int64 `json:"avg_ttfb_ms"`
	TTFBP95 int64 `json:"ttfb_p95_ms"`
}

// UsageDayRow 是单日聚合。
type UsageDayRow struct {
	Date string `json:"date"`
	UsageTotals
}

// DimensionAgg 是按模型或 key 哈希聚合的行。
type DimensionAgg struct {
	Name string `json:"name"`
	UsageTotals
	LastAt      string  `json:"last_at,omitempty"`
	AvgDuration float64 `json:"avg_duration_ms"`
	AvgTTFB     float64 `json:"avg_ttfb_ms"`
	// InTok/OutTok 分位数描述该维度的请求体量分布：计费与上下文窗口
	// 占用都跟长度强相关，均值会掩盖长尾。
	InTokP50  int64 `json:"input_p50,omitempty"`
	InTokP95  int64 `json:"input_p95,omitempty"`
	OutTokP50 int64 `json:"output_p50,omitempty"`
	OutTokP95 int64 `json:"output_p95,omitempty"`
}

// LatencyStats 是样本算出的延迟分布。
type LatencyStats struct {
	Samples int64 `json:"samples"`
	P50     int64 `json:"p50"`
	P90     int64 `json:"p90"`
	P95     int64 `json:"p95"`
	P99     int64 `json:"p99"`
	Max     int64 `json:"max"`
}

// RateLimitEvent 是一次 429 的采样：发生时刻（≈请求完成时刻）、模型、
// 以及该时刻前 60s 内启动的请求数。stage 区分来源——rate_gate 是本地
// 闸门快败（RPM 读数为到达速率，含被拒），其余是上游真 429。
type RateLimitEvent struct {
	At    int64  `json:"at"` // unix 秒
	Model string `json:"model,omitempty"`
	RPM   int64  `json:"rpm"` // At 前 60s 内启动的请求数（含本请求）
	Stage string `json:"stage,omitempty"`
}

// UsageSnapshot 是聚合结果的完整快照（JSON 形状与旧版一致）。
type UsageSnapshot struct {
	WindowStart string          `json:"window_start"` // 最早一条日志的时间
	Entries     int64           `json:"entries"`      // 参与聚合的行数
	Today       UsageTotals     `json:"today"`
	Window      UsageTotals     `json:"window"`
	Days        []UsageDayRow   `json:"days"`   // 新在前
	Points      []UsageMinPoint `json:"points"` // 旧到新，10 分钟粒度，含零值桶
	Models      []DimensionAgg  `json:"models"`
	Keys        []DimensionAgg  `json:"keys"`
	// ModelDays 是 模型×自然日 的 totals 矩阵，面板的时间范围选择器
	// 用它把模型表过滤到所选窗口。
	ModelDays   map[string]map[string]UsageTotals `json:"model_days,omitempty"`
	ErrorStages map[string]int64                  `json:"error_stages"`
	Duration    LatencyStats                      `json:"duration"`
	TTFB        LatencyStats                      `json:"ttfb"`
	// RateLimitEvents 是最近的上游 429 采样（旧到新），供面板推算限流阈值。
	RateLimitEvents []RateLimitEvent `json:"rate_limit_events,omitempty"`
}

// latencyStatsOf 返回样本的分位数摘要；空样本返回零值。
// pick(q)=sorted[int(q*(n-1))]，与旧 sampleRing.stats 同公式。
func latencyStatsOf(samples []int64) LatencyStats {
	if len(samples) == 0 {
		return LatencyStats{}
	}
	sorted := make([]int64, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(q float64) int64 {
		return sorted[int(q*float64(len(sorted)-1))]
	}
	return LatencyStats{
		Samples: int64(len(sorted)),
		P50:     pick(0.50),
		P90:     pick(0.90),
		P95:     pick(0.95),
		P99:     pick(0.99),
		Max:     sorted[len(sorted)-1],
	}
}

// sampleSummary 返回样本的均值与 p95；空样本返回零值。
func sampleSummary(samples []int64) (avg, p95 int64) {
	if len(samples) == 0 {
		return 0, 0
	}
	sorted := make([]int64, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	return sum / int64(len(sorted)), sorted[int(0.95*float64(len(sorted)-1))]
}

// recentSamples 返回某列最近 usageSampleCapacity 条样本（分位数只需要
// 多重集，与旧蓄水池「最近 N 条入样」同口径）。
func (s *Store) recentSamples(ctx context.Context, column string, nonNull bool) ([]int64, error) {
	query := `SELECT ` + column + ` FROM logs`
	if nonNull {
		query += ` WHERE ` + column + ` IS NOT NULL`
	}
	query += ` ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, usageSampleCapacity)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// LogLatency 返回全局延迟分位数摘要（duration/ttfb 两行）——
// /admin/runtime-metrics 轮询只消费这两行。
func (s *Store) LogLatency(ctx context.Context) (map[string]LatencyStats, error) {
	dur, err := s.recentSamples(ctx, "duration_ms", false)
	if err != nil {
		return nil, err
	}
	ttfb, err := s.recentSamples(ctx, "first_upstream_ms", true)
	if err != nil {
		return nil, err
	}
	return map[string]LatencyStats{
		"duration": latencyStatsOf(dur),
		"ttfb":     latencyStatsOf(ttfb),
	}, nil
}

// usagePoints 装配 8 天 10 分钟粒度序列：每桶 totals 走 GROUP BY，
// 延迟样本走窗口函数取每桶最近 usageMinSampleCap 条（等同旧环形
// 蓄水池的「留最新 N 个」语义）。范围谓词走 minute_bucket（time/60000
// 物化列）：time/600000>=S ⟺ minute_bucket>=S*10，idx_logs_minute_*
// 前缀索引即刻生效。
func (s *Store) usagePoints(ctx context.Context, currentSlot int64) ([]UsageMinPoint, error) {
	minSlot := currentSlot - usageMinBuckets + 1
	minBucket := minSlot * 10
	totalRows, err := s.db.QueryContext(ctx,
		`SELECT time/600000 AS slot,`+usageTotalsCols+` FROM logs WHERE minute_bucket >= ? GROUP BY slot`, minBucket)
	if err != nil {
		return nil, err
	}
	totals := map[int64]UsageTotals{}
	for totalRows.Next() {
		var slot int64
		var t UsageTotals
		if err := totalRows.Scan(append([]any{&slot}, usageTotalsDests(&t)...)...); err != nil {
			_ = totalRows.Close()
			return nil, err
		}
		totals[slot] = t
	}
	if err := totalRows.Close(); err != nil {
		return nil, err
	}

	// 每桶的 dur/ttfb 样本各取最近 cap 条：durs 按 id 倒序前 N；
	// ttfb 只在非空行里取前 N（旧 pushSample 只入非 nil 值）。两个环
	// 独立计数，所以取回行后要按各自的 rn 再闸一次。
	sampleRows, err := s.db.QueryContext(ctx, `
		SELECT slot, duration_ms, first_upstream_ms, rn_dur, rn_ttfb FROM (
			SELECT time/600000 AS slot, duration_ms, first_upstream_ms,
				ROW_NUMBER() OVER (PARTITION BY time/600000 ORDER BY id DESC) AS rn_dur,
				ROW_NUMBER() OVER (PARTITION BY time/600000, first_upstream_ms IS NOT NULL ORDER BY id DESC) AS rn_ttfb
			FROM logs WHERE minute_bucket >= ?
		) WHERE rn_dur <= ? OR (first_upstream_ms IS NOT NULL AND rn_ttfb <= ?)`, minBucket, usageMinSampleCap, usageMinSampleCap)
	if err != nil {
		return nil, err
	}
	type samples struct {
		durs  []int64
		ttfbs []int64
	}
	perSlot := map[int64]*samples{}
	for sampleRows.Next() {
		var slot, dur, rnDur, rnTTFB int64
		var fup sql.NullInt64
		if err := sampleRows.Scan(&slot, &dur, &fup, &rnDur, &rnTTFB); err != nil {
			_ = sampleRows.Close()
			return nil, err
		}
		ss := perSlot[slot]
		if ss == nil {
			ss = &samples{}
			perSlot[slot] = ss
		}
		if rnDur <= usageMinSampleCap {
			ss.durs = append(ss.durs, dur)
		}
		if fup.Valid && rnTTFB <= usageMinSampleCap {
			ss.ttfbs = append(ss.ttfbs, fup.Int64)
		}
	}
	if err := sampleRows.Close(); err != nil {
		return nil, err
	}

	points := make([]UsageMinPoint, 0, usageMinBuckets)
	for slot := minSlot; slot <= currentSlot; slot++ {
		p := UsageMinPoint{At: slot * 600}
		if t, ok := totals[slot]; ok {
			p.UsageTotals = t
		}
		if ss := perSlot[slot]; ss != nil {
			_, p.DurP95 = sampleSummary(ss.durs)
			p.AvgTTFB, p.TTFBP95 = sampleSummary(ss.ttfbs)
		}
		points = append(points, p)
	}
	return points, nil
}

// dimAggs 按维度表达式（emodel 或 key_hash）聚合：totals + 时长和 +
// TTFB 样本均值 + 末次 started_at（MAX(id) 所在行的裸列取自该行——
// SQLite 保证 bare column 绑定到唯一 min/max 聚合的达成行，等价于
// 旧按完成序追加的「最后一行覆盖」语义）。
func (s *Store) dimAggs(ctx context.Context, dimExpr string) ([]DimensionAgg, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT dim,`+usageTotalsCols+`,
			COALESCE(SUM(duration_ms), 0),
			SUM(first_upstream_ms IS NOT NULL),
			COALESCE(SUM(first_upstream_ms), 0),
			MAX(id), started_at
		FROM (SELECT `+dimExpr+` AS dim, * FROM logs)
		WHERE dim != '' GROUP BY dim`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DimensionAgg
	for rows.Next() {
		var d DimensionAgg
		var sumDur, nTTFB, sumTTFB, maxID int64
		var lastAt string
		dests := append([]any{&d.Name}, usageTotalsDests(&d.UsageTotals)...)
		dests = append(dests, &sumDur, &nTTFB, &sumTTFB, &maxID, &lastAt)
		if err := rows.Scan(dests...); err != nil {
			return nil, err
		}
		if d.Requests > 0 {
			d.AvgDuration = float64(sumDur) / float64(d.Requests)
		}
		if nTTFB > 0 {
			d.AvgTTFB = float64(sumTTFB) / float64(nTTFB)
		}
		d.LastAt = lastAt
		out = append(out, d)
	}
	return out, rows.Err()
}

// modelTokenSamples 返回每个生效模型最近 dimensionSampleCapacity 条
// input_tokens 与（仅 output>0 行的）output_tokens 样本集；两个环
// 独立计数，取回行后按各自的 rn 再闸一次。
func (s *Store) modelTokenSamples(ctx context.Context) (inTok, outTok map[string][]int64, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT emodel, input_tokens, output_tokens, rn_in, rn_out FROM (
			SELECT emodel, input_tokens, output_tokens,
				ROW_NUMBER() OVER (PARTITION BY emodel ORDER BY id DESC) AS rn_in,
				ROW_NUMBER() OVER (PARTITION BY emodel, output_tokens > 0 ORDER BY id DESC) AS rn_out
			FROM (SELECT `+logEModelExpr+` AS emodel, id, input_tokens, output_tokens FROM logs)
			WHERE emodel != ''
		) WHERE rn_in <= ? OR (output_tokens > 0 AND rn_out <= ?)`,
		dimensionSampleCapacity, dimensionSampleCapacity)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	inTok = map[string][]int64{}
	outTok = map[string][]int64{}
	for rows.Next() {
		var emodel string
		var in, out, rnIn, rnOut int64
		if err := rows.Scan(&emodel, &in, &out, &rnIn, &rnOut); err != nil {
			return nil, nil, err
		}
		if rnIn <= dimensionSampleCapacity {
			inTok[emodel] = append(inTok[emodel], in)
		}
		if out > 0 && rnOut <= dimensionSampleCapacity {
			outTok[emodel] = append(outTok[emodel], out)
		}
	}
	return inTok, outTok, rows.Err()
}

// UsageStats 返回 logs 表全量聚合快照：窗口/今日/逐日累计、按模型/按
// key、模型×日矩阵、错误阶段、8 天 10 分钟粒度趋势、全局延迟分位数
// （最近 4096 条）与最近 256 个限流事件。
func (s *Store) UsageStats(ctx context.Context) (UsageSnapshot, error) {
	snap := UsageSnapshot{
		Days:        []UsageDayRow{},
		Points:      []UsageMinPoint{},
		Models:      []DimensionAgg{},
		Keys:        []DimensionAgg{},
		ErrorStages: map[string]int64{},
	}

	var minMS sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(time) FROM logs`).Scan(&snap.Entries, &minMS); err != nil {
		return snap, err
	}
	if minMS.Valid {
		snap.WindowStart = time.UnixMilli(minMS.Int64).Format(time.RFC3339)
	} else {
		snap.WindowStart = time.Time{}.Format(time.RFC3339)
	}

	if err := scanUsageTotals(s.db.QueryRowContext(ctx, `SELECT `+usageTotalsCols+` FROM logs`), &snap.Window); err != nil {
		return snap, err
	}
	// 今日单列查询而非从 days 里挑：31 天上限外若有未来日期的行，
	// today 也不该被挤掉。本地日界在 Go 侧算好打成毫秒界——
	// strftime(localtime) 谓词不可索引，time 范围可走 idx_logs_time。
	dayStart, dayEnd := dayBoundsMS(time.Now())
	if err := scanUsageTotals(s.db.QueryRowContext(ctx,
		`SELECT `+usageTotalsCols+` FROM logs WHERE time >= ? AND time < ?`,
		dayStart, dayEnd), &snap.Today); err != nil {
		return snap, err
	}

	// 逐日聚合（新在前，上限 usageMaxDays）；kept 记录保留日键，
	// ModelDays 只投影同日键集合（旧快照语义）。
	dayRows, err := s.db.QueryContext(ctx,
		`SELECT `+logDayExpr+` AS day,`+usageTotalsCols+` FROM logs GROUP BY day ORDER BY day DESC LIMIT ?`, usageMaxDays)
	if err != nil {
		return snap, err
	}
	kept := map[string]bool{}
	for dayRows.Next() {
		var row UsageDayRow
		if err := dayRows.Scan(append([]any{&row.Date}, usageTotalsDests(&row.UsageTotals)...)...); err != nil {
			_ = dayRows.Close()
			return snap, err
		}
		snap.Days = append(snap.Days, row)
		kept[row.Date] = true
	}
	if err := dayRows.Close(); err != nil {
		return snap, err
	}

	mdayRows, err := s.db.QueryContext(ctx,
		`SELECT emodel, day,`+usageTotalsCols+` FROM (
			SELECT `+logEModelExpr+` AS emodel, `+logDayExpr+` AS day, * FROM logs
		) WHERE emodel != '' GROUP BY emodel, day`)
	if err != nil {
		return snap, err
	}
	modelDays := map[string]map[string]UsageTotals{}
	for mdayRows.Next() {
		var emodel, day string
		var t UsageTotals
		if err := mdayRows.Scan(append([]any{&emodel, &day}, usageTotalsDests(&t)...)...); err != nil {
			_ = mdayRows.Close()
			return snap, err
		}
		if !kept[day] {
			continue
		}
		dm := modelDays[emodel]
		if dm == nil {
			dm = map[string]UsageTotals{}
			modelDays[emodel] = dm
		}
		dm[day] = t
	}
	if err := mdayRows.Close(); err != nil {
		return snap, err
	}
	if len(modelDays) > 0 {
		snap.ModelDays = modelDays
	}

	stageRows, err := s.db.QueryContext(ctx,
		`SELECT error_stage, COUNT(*) FROM logs WHERE error_stage != '' GROUP BY error_stage`)
	if err != nil {
		return snap, err
	}
	for stageRows.Next() {
		var stage string
		var n int64
		if err := stageRows.Scan(&stage, &n); err != nil {
			_ = stageRows.Close()
			return snap, err
		}
		snap.ErrorStages[stage] = n
	}
	if err := stageRows.Close(); err != nil {
		return snap, err
	}

	// 维度行：perModel 带 token 分位数，perKey 没有（旧版 keys 的
	// inTokSamples 是 nil 不入样）。
	models, err := s.dimAggs(ctx, logEModelExpr)
	if err != nil {
		return snap, err
	}
	inTok, outTok, err := s.modelTokenSamples(ctx)
	if err != nil {
		return snap, err
	}
	for i := range models {
		if st := latencyStatsOf(inTok[models[i].Name]); st.Samples > 0 {
			models[i].InTokP50, models[i].InTokP95 = st.P50, st.P95
		}
		if st := latencyStatsOf(outTok[models[i].Name]); st.Samples > 0 {
			models[i].OutTokP50, models[i].OutTokP95 = st.P50, st.P95
		}
	}
	keys, err := s.dimAggs(ctx, "key_hash")
	if err != nil {
		return snap, err
	}
	snap.Models = sortDimsByRequests(models)
	snap.Keys = sortDimsByRequests(keys)

	if snap.Points, err = s.usagePoints(ctx, time.Now().Unix()/600); err != nil {
		return snap, err
	}
	if snap.Duration, err = s.latencyOf(ctx, "duration_ms", false); err != nil {
		return snap, err
	}
	if snap.TTFB, err = s.latencyOf(ctx, "first_upstream_ms", true); err != nil {
		return snap, err
	}
	if snap.RateLimitEvents, err = s.rateLimitEvents(ctx); err != nil {
		return snap, err
	}
	return snap, nil
}

// latencyOf 是 recentSamples + latencyStatsOf 的组合。
func (s *Store) latencyOf(ctx context.Context, column string, nonNull bool) (LatencyStats, error) {
	samples, err := s.recentSamples(ctx, column, nonNull)
	if err != nil {
		return LatencyStats{}, err
	}
	return latencyStatsOf(samples), nil
}

// sortDimsByRequests 把维度行按请求数降序、名升序排序（旧 sortedAggs
// 口径）；空输入保持 [] 而非 nil，快照 JSON 才能发空数组。
func sortDimsByRequests(in []DimensionAgg) []DimensionAgg {
	out := append([]DimensionAgg{}, in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// rateLimitEvents 返回最近 rateLimitEventCap 个 429 事件（旧到新）：
// 每条的 rpm 是其完成时刻前 60s 内启动的请求数（含自身）——旧版按
// 完成序的 starts 窗口计数会漏掉「窗口内启动但完成更晚」的行，SQL
// 版按启动时刻精确计数，口径相同但覆盖更完整。
func (s *Store) rateLimitEvents(ctx context.Context) ([]RateLimitEvent, error) {
	// 相关子查询里 endExpr 必须带 logs. 限定：裸列名会被内层 l2 遮蔽。
	// 内层窗口条件整数除法等价改写为 l2.time 的毫秒闭区间
	// （s/1000 > E-60 ⟺ s >= (E-59)*1000；s/1000 <= E ⟺ s <= E*1000+999），
	// 从全表 COUNT 变成 idx_logs_time 范围扫。
	const endExpr = `logs.time/1000 + logs.duration_ms/1000`
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+endExpr+`, `+logEModelExpr+`, error_stage,
			(SELECT COUNT(*) FROM logs l2
				WHERE l2.time >= (`+endExpr+` - 59) * 1000
					AND l2.time <= (`+endExpr+`) * 1000 + 999)
		FROM logs WHERE status_code = 429 OR rate_limited != 0
		ORDER BY id DESC LIMIT ?`, rateLimitEventCap)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RateLimitEvent
	for rows.Next() {
		var e RateLimitEvent
		if err := rows.Scan(&e.At, &e.Model, &e.Stage, &e.RPM); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 查询取最新 N 条（倒序），输出还原成旧到新的到达序。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
