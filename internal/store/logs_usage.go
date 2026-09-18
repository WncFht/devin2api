// 本文件是 /admin/usage 与 runtime-metrics 延迟组的聚合读路径——
// 旧 debuglog.usageAggregator（内存 replay 索引尾部）的 SQL 版。
// 聚合源头从「256MB 索引尾部回放」升级为 logs 全表：条数/维度计数由
// GROUP BY 一次算出，分位数仍按旧口径取最近 N 条样本在 Go 侧排序
// （SQLite 无原生分位数；窗口函数把「每桶/每维最近 N 条」压成一次扫描）。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
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
		WHEN result = 'rejected' THEN 'none'
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
}

// MarshalJSON 稀疏编码单点：只发非零字段。面板 8 天网格的绝大多数桶
// 只有 requests 与少数字段非零——15 个定长字段全发会让 points 段占到
// 响应的 82-95%。缺失键在消费侧（stats.js sumUsageTotals 的
// Number(p[k])||0）按 0 处理，合计口径不变。at 恒发——零值过滤对它
// 无意义且是切片的唯一谓词键。
func (p UsageMinPoint) MarshalJSON() ([]byte, error) {
	type plain UsageMinPoint
	raw, err := json.Marshal(plain(p))
	if err != nil {
		return nil, err
	}
	var m map[string]int64
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k, v := range m {
		if v == 0 && k != "at" {
			delete(m, k)
		}
	}
	return json.Marshal(m)
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
	WindowStart string          `json:"window_start"` // 窗口内最早一条日志的时间
	Entries     int64           `json:"entries"`      // 窗口内参与聚合的行数
	Today       UsageTotals     `json:"today"`
	Window      UsageTotals     `json:"window"`
	Days        []UsageDayRow   `json:"days"`   // 新在前
	Points      []UsageMinPoint `json:"points"` // 旧到新，10 分钟粒度，仅非零桶（稀疏）
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
	// AttemptCauses 是被放弃 lane 尝试的 日×lane×cause 聚合
	//（lane_attempt_causes 表 31 天窗口直读，旧到新）：区分真实
	// failover 发送（connect code）与本地闸门幻影换号（local_gate:*，
	// 零上游成本）。表自 0007 迁移起累计，之前的历史不可回填。
	AttemptCauses []LaneAttemptCause `json:"attempt_causes,omitempty"`
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

// recentSamples 返回某列最近 usageSampleCapacity 条样本（分位数只需要
// 多重集，与旧蓄水池「最近 N 条入样」同口径）。account 非空时按读侧
// 折叠口径过滤单 lane（'default' 命中 ”+'default' 两群）。
func (s *Store) recentSamples(ctx context.Context, column string, nonNull bool, account string) ([]int64, error) {
	// rejected 行是管线前拒绝的留存记录，无时长/token——进样本只会
	// 把分位数拉向 0，全部聚合口径一致剔除。
	query := `SELECT ` + column + ` FROM logs WHERE log_source != 'rejected'`
	var args []any
	if nonNull {
		query += ` AND ` + column + ` IS NOT NULL`
	}
	if account != "" {
		query += ` AND ` + logAccountExpr + ` = ?`
		args = append(args, account)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	rows, err := s.ro.QueryContext(ctx, query, append(args, usageSampleCapacity)...)
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
	dur, err := s.recentSamples(ctx, "duration_ms", false, "")
	if err != nil {
		return nil, err
	}
	ttfb, err := s.recentSamples(ctx, "first_upstream_ms", true, "")
	if err != nil {
		return nil, err
	}
	return map[string]LatencyStats{
		"duration": latencyStatsOf(dur),
		"ttfb":     latencyStatsOf(ttfb),
	}, nil
}

// usagePoints 装配 8 天 10 分钟粒度序列：每桶 totals 走 GROUP BY。
// 范围谓词走 minute_bucket（time/60000 物化列）：time/600000>=S ⟺
// minute_bucket>=S*10，idx_logs_minute_* 前缀索引即刻生效。sc 为
// 零值时全量——per-account 10 分钟趋势走逐号调用
// （LogScope{Account: lane}），lane 数个位数无压力。
// totals 在 scope 无 account 维时走格子 UNION 补尾（cells.go 读侧
// 约定）；account 维度在格子里不存在，整条留在原始行。
func (s *Store) usagePoints(ctx context.Context, currentSlot int64, sc LogScope) ([]UsageMinPoint, error) {
	minSlot := currentSlot - usageMinBuckets + 1
	minBucket := minSlot * 10
	scopeWhere, scopeArgs := sc.where()
	var totalRows *sql.Rows
	var err error
	if sc.Account == "" {
		slotLo := cellSlotLo(minBucket * 60000)
		tail, tailArgs := cellTailPred(slotLo, math.MaxInt64)
		cellScope, cellArgs := sc.cellWhere()
		args := append([]any{slotLo}, cellArgs...)
		args = append(args, minBucket)
		args = append(args, tailArgs...)
		args = append(args, scopeArgs...)
		totalRows, err = s.ro.QueryContext(ctx,
			`SELECT slot,`+cellSumList(cellUsageCols)+` FROM (
				SELECT slot, `+cellUsageCols+` FROM log_cells WHERE slot >= ?`+cellScope+`
				UNION ALL SELECT time/600000, `+cellRowList(cellUsageCols)+` FROM logs
				WHERE minute_bucket >= ?`+tail+scopeWhere+`) GROUP BY slot`, args...)
	} else {
		totalRows, err = s.ro.QueryContext(ctx,
			`SELECT time/600000 AS slot,`+usageTotalsCols+` FROM logs WHERE minute_bucket >= ?`+scopeWhere+` GROUP BY slot`,
			append([]any{minBucket}, scopeArgs...)...)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = totalRows.Close() }()
	totals := map[int64]UsageTotals{}
	for totalRows.Next() {
		var slot int64
		var t UsageTotals
		if err := totalRows.Scan(append([]any{&slot}, usageTotalsDests(&t)...)...); err != nil {
			return nil, err
		}
		totals[slot] = t
	}
	// Close 只回收迭代器不报中途错误——迭代中断（ctx 取消/IO 故障）
	// 走 Err()，漏检会把截断结果当完整聚合。
	if err := totalRows.Err(); err != nil {
		return nil, err
	}

	// 零值桶不发：网格内绝大多数桶无流量，消费侧只做 at 过滤+字段求和，
	// 略零桶不改口径。GROUP BY 只产出有行的槽，totals 缺席即零桶。
	points := make([]UsageMinPoint, 0, len(totals))
	for slot := minSlot; slot <= currentSlot; slot++ {
		if t, ok := totals[slot]; ok {
			points = append(points, UsageMinPoint{At: slot * 600, UsageTotals: t})
		}
	}
	return points, nil
}

// dimAggs 按维度聚合窗口内行：totals + 时长和 + TTFB 样本均值 + 末次
// started_at。cellDim 非空（emodel/key_hash）时走格子 UNION 补尾，
// last_key 打包串的 MAX 等价于「最大 id 行的 started_at」（cells.go
// 登记表约定）；cellDim 为空（account 维，格子里没有该列）时退回
// 原始行——那里 MAX(id) 所在行的 bare column 绑定同一语义。
func (s *Store) dimAggs(ctx context.Context, cellDim, dimExpr string, minBucket int64) ([]DimensionAgg, error) {
	var rows *sql.Rows
	var err error
	if cellDim != "" {
		slotLo := cellSlotLo(minBucket * 60000)
		tail, tailArgs := cellTailPred(slotLo, math.MaxInt64)
		args := append([]any{slotLo, minBucket}, tailArgs...)
		rows, err = s.ro.QueryContext(ctx, `
			SELECT dim,`+cellSumList(cellUsageCols)+`,
				COALESCE(SUM(sd),0), SUM(nt), COALESCE(SUM(st),0), SUBSTR(MAX(lk),22)
			FROM (
				SELECT `+cellDim+` AS dim, `+cellUsageCols+`,
					sum_dur_all AS sd, n_ttfb AS nt, sum_ttfb AS st, last_key AS lk
				FROM log_cells WHERE slot >= ? AND `+cellDim+` != ''
				UNION ALL
				SELECT `+dimExpr+`, `+cellRowList(cellUsageCols)+`, duration_ms,
					first_upstream_ms IS NOT NULL, COALESCE(first_upstream_ms,0),
					printf('%020d', id)||'|'||started_at
				FROM logs WHERE minute_bucket >= ?`+tail+` AND log_source != 'rejected' AND `+dimExpr+` != ''
			) GROUP BY dim`, args...)
	} else {
		rows, err = s.ro.QueryContext(ctx, `
			SELECT dim,`+usageTotalsCols+`,
				COALESCE(SUM(duration_ms), 0),
				SUM(first_upstream_ms IS NOT NULL),
				COALESCE(SUM(first_upstream_ms), 0),
				SUBSTR(MAX(printf('%020d', id)||'|'||started_at),22)
			FROM (SELECT `+dimExpr+` AS dim, * FROM logs)
			WHERE dim != '' AND log_source != 'rejected' AND minute_bucket >= ? GROUP BY dim`, minBucket)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DimensionAgg
	for rows.Next() {
		var d DimensionAgg
		var sumDur, nTTFB, sumTTFB int64
		var lastAt string
		dests := append([]any{&d.Name}, usageTotalsDests(&d.UsageTotals)...)
		dests = append(dests, &sumDur, &nTTFB, &sumTTFB, &lastAt)
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
// 独立取数。逐模型走 idx_logs_emodel_id 的 ORDER BY id DESC LIMIT
// 索引扫描——成本只与样本量挂钩；旧窗口函数实现是全表排序，成本
// 随保留期线性退化。
func (s *Store) modelTokenSamples(ctx context.Context, models []string) (inTok, outTok map[string][]int64, err error) {
	inTok = map[string][]int64{}
	outTok = map[string][]int64{}
	sample := func(query, model string) ([]int64, error) {
		rows, err := s.ro.QueryContext(ctx, query, model, dimensionSampleCapacity)
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
	for _, m := range models {
		in, err := sample(`SELECT input_tokens FROM logs WHERE `+logEModelExpr+` = ? ORDER BY id DESC LIMIT ?`, m)
		if err != nil {
			return nil, nil, err
		}
		out, err := sample(`SELECT output_tokens FROM logs WHERE `+logEModelExpr+` = ? AND output_tokens > 0 ORDER BY id DESC LIMIT ?`, m)
		if err != nil {
			return nil, nil, err
		}
		if len(in) > 0 {
			inTok[m] = in
		}
		if len(out) > 0 {
			outTok[m] = out
		}
	}
	return inTok, outTok, nil
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
	// 聚合窗口收敛到最近 usageMaxDays 天：旧内存聚合器本来也只覆盖
	// 索引尾部窗口；无界全表扫会把整个保留期（90 天）的行数线性摊进
	// 每次面板轮询，minute_bucket 下界把成本钉在窗口体积上。
	// 整格覆盖段走 log_cells 的格子 SUM，水位外行与窗底不满一格的
	// 边带行走原始 logs——UNION 两侧是无重无漏的划分（cells.go 读侧约定）。
	minBucket := time.Now().AddDate(0, 0, -usageMaxDays).UnixMilli() / 60000
	slotLo := cellSlotLo(minBucket * 60000)
	tail, tailArgs := cellTailPred(slotLo, math.MaxInt64)
	winTailArgs := append([]any{minBucket}, tailArgs...)
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(c),0), MIN(t) FROM (
			SELECT req AS c, min_time AS t FROM log_cells WHERE slot >= ?
			UNION ALL SELECT 1, time FROM logs WHERE minute_bucket >= ?`+tail+` AND log_source != 'rejected')`,
		append([]any{slotLo}, winTailArgs...)...).Scan(&snap.Entries, &minMS); err != nil {
		return snap, err
	}
	if minMS.Valid {
		snap.WindowStart = time.UnixMilli(minMS.Int64).Format(time.RFC3339)
	} else {
		snap.WindowStart = time.Time{}.Format(time.RFC3339)
	}

	if err := scanUsageTotals(s.ro.QueryRowContext(ctx,
		`SELECT `+cellSumList(cellUsageCols)+` FROM (
			SELECT `+cellUsageCols+` FROM log_cells WHERE slot >= ?
			UNION ALL SELECT `+cellRowList(cellUsageCols)+` FROM logs
			WHERE minute_bucket >= ?`+tail+` AND log_source != 'rejected')`,
		append([]any{slotLo}, winTailArgs...)...), &snap.Window); err != nil {
		return snap, err
	}
	// 今日单列查询而非从 days 里挑：31 天上限外若有未来日期的行，
	// today 也不该被挤掉。本地日界在 Go 侧算好打成毫秒界——
	// strftime(localtime) 谓词不可索引，time 范围可走 idx_logs_time_status。
	dayStart, dayEnd := dayBoundsMS(time.Now())
	daySlotLo, daySlotHi := cellSlotLo(dayStart), dayEnd/600000
	dayTail, dayTailArgs := cellTailPred(daySlotLo, daySlotHi)
	dayArgs := append([]any{daySlotLo, daySlotHi, dayStart, dayEnd}, dayTailArgs...)
	if err := scanUsageTotals(s.ro.QueryRowContext(ctx,
		`SELECT `+cellSumList(cellUsageCols)+` FROM (
			SELECT `+cellUsageCols+` FROM log_cells WHERE slot >= ? AND slot < ?
			UNION ALL SELECT `+cellRowList(cellUsageCols)+` FROM logs
			WHERE time >= ? AND time < ?`+dayTail+` AND log_source != 'rejected')`,
		dayArgs...), &snap.Today); err != nil {
		return snap, err
	}

	// 逐日聚合（新在前，上限 usageMaxDays）；kept 记录保留日键，
	// ModelDays 只投影同日键集合（旧快照语义）。
	dayRows, err := s.ro.QueryContext(ctx,
		`SELECT day,`+cellSumList(cellUsageCols)+` FROM (
			SELECT day, `+cellUsageCols+` FROM log_cells WHERE slot >= ?
			UNION ALL SELECT `+logDayExpr+`, `+cellRowList(cellUsageCols)+` FROM logs
			WHERE minute_bucket >= ?`+tail+` AND log_source != 'rejected')
		GROUP BY day ORDER BY day DESC LIMIT ?`,
		append(append([]any{slotLo}, winTailArgs...), usageMaxDays)...)
	if err != nil {
		return snap, err
	}
	defer func() { _ = dayRows.Close() }()
	kept := map[string]bool{}
	for dayRows.Next() {
		var row UsageDayRow
		if err := dayRows.Scan(append([]any{&row.Date}, usageTotalsDests(&row.UsageTotals)...)...); err != nil {
			return snap, err
		}
		snap.Days = append(snap.Days, row)
		kept[row.Date] = true
	}
	if err := dayRows.Err(); err != nil {
		return snap, err
	}

	mdayRows, err := s.ro.QueryContext(ctx,
		`SELECT emodel, day,`+cellSumList(cellUsageCols)+` FROM (
			SELECT emodel, day, `+cellUsageCols+` FROM log_cells WHERE slot >= ? AND emodel != ''
			UNION ALL SELECT `+logEModelExpr+`, `+logDayExpr+`, `+cellRowList(cellUsageCols)+` FROM logs
			WHERE minute_bucket >= ?`+tail+` AND log_source != 'rejected' AND `+logEModelExpr+` != '')
		GROUP BY emodel, day`,
		append([]any{slotLo}, winTailArgs...)...)
	if err != nil {
		return snap, err
	}
	defer func() { _ = mdayRows.Close() }()
	modelDays := map[string]map[string]UsageTotals{}
	for mdayRows.Next() {
		var emodel, day string
		var t UsageTotals
		if err := mdayRows.Scan(append([]any{&emodel, &day}, usageTotalsDests(&t)...)...); err != nil {
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
	if err := mdayRows.Err(); err != nil {
		return snap, err
	}
	if len(modelDays) > 0 {
		snap.ModelDays = modelDays
	}

	stageRows, err := s.ro.QueryContext(ctx,
		`SELECT stage, SUM(req) FROM (
			SELECT stage, req FROM log_err_cells WHERE slot >= ?
			UNION ALL SELECT error_stage, 1 FROM logs
			WHERE error_stage != '' AND minute_bucket >= ?`+tail+` AND log_source != 'rejected')
		GROUP BY stage`,
		append([]any{slotLo}, winTailArgs...)...)
	if err != nil {
		return snap, err
	}
	defer func() { _ = stageRows.Close() }()
	for stageRows.Next() {
		var stage string
		var n int64
		if err := stageRows.Scan(&stage, &n); err != nil {
			return snap, err
		}
		snap.ErrorStages[stage] = n
	}
	if err := stageRows.Err(); err != nil {
		return snap, err
	}

	// 维度行：perModel 带 token 分位数，perKey 没有（旧版 keys 的
	// inTokSamples 是 nil 不入样）。分位数样本逐模型走索引取数，
	// 输入清单即 dimAggs 已聚合出的窗口内模型集合。
	models, err := s.dimAggs(ctx, "emodel", logEModelExpr, minBucket)
	if err != nil {
		return snap, err
	}
	modelNames := make([]string, 0, len(models))
	for _, m := range models {
		modelNames = append(modelNames, m.Name)
	}
	inTok, outTok, err := s.modelTokenSamples(ctx, modelNames)
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
	keys, err := s.dimAggs(ctx, "key_hash", "key_hash", minBucket)
	if err != nil {
		return snap, err
	}
	snap.Models = sortDimsByRequests(models)
	snap.Keys = sortDimsByRequests(keys)

	if snap.Points, err = s.usagePoints(ctx, time.Now().Unix()/600, LogScope{}); err != nil {
		return snap, err
	}
	if snap.Duration, err = s.latencyOf(ctx, "duration_ms", false, ""); err != nil {
		return snap, err
	}
	if snap.TTFB, err = s.latencyOf(ctx, "first_upstream_ms", true, ""); err != nil {
		return snap, err
	}
	if snap.RateLimitEvents, err = s.rateLimitEvents(ctx); err != nil {
		return snap, err
	}
	sinceDay := time.Now().AddDate(0, 0, -usageMaxDays).Format("2006-01-02")
	if snap.AttemptCauses, err = s.LaneAttemptCauses(ctx, sinceDay); err != nil {
		return snap, err
	}
	return snap, nil
}

// latencyOf 是 recentSamples + latencyStatsOf 的组合；account 非空时
// 样本限定该 lane（读侧折叠口径同 LogScope.Account）。
func (s *Store) latencyOf(ctx context.Context, column string, nonNull bool, account string) (LatencyStats, error) {
	samples, err := s.recentSamples(ctx, column, nonNull, account)
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
// 外层谓词+ORDER BY id DESC 由部分索引 idx_logs_limited_id 供序：
// 只扫命中行的 id 序，429 稀少时也不再近似全表扫。
func (s *Store) rateLimitEvents(ctx context.Context) ([]RateLimitEvent, error) {
	// 相关子查询里 endExpr 必须带 logs. 限定：裸列名会被内层 l2 遮蔽。
	// 内层窗口条件整数除法等价改写为 l2.time 的毫秒闭区间
	// （s/1000 > E-60 ⟺ s >= (E-59)*1000；s/1000 <= E ⟺ s <= E*1000+999），
	// 从全表 COUNT 变成 idx_logs_time_status 范围扫。
	const endExpr = `logs.time/1000 + logs.duration_ms/1000`
	rows, err := s.ro.QueryContext(ctx, `
		SELECT `+endExpr+`, `+logEModelExpr+`, error_stage,
			(SELECT COUNT(*) FROM logs l2
				WHERE l2.time >= (`+endExpr+` - 59) * 1000
					AND l2.time <= (`+endExpr+`) * 1000 + 999
					AND l2.log_source != 'rejected')
		FROM logs WHERE (status_code = 429 OR rate_limited != 0) AND log_source != 'rejected'
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

// AccountAggs 按上游账号 lane 聚合窗口内 logs：totals + avg TTFB + 末次
// 时刻——P2 /admin/accounts 逐号维度表的支点。” 历史行折叠进
// 'default' 桶（logAccountExpr），与 LogScope.Account、QuotaReport
// 的读侧口径一致，不会出现 ”/'default' 幽灵分桶。
func (s *Store) AccountAggs(ctx context.Context) ([]DimensionAgg, error) {
	minBucket := time.Now().AddDate(0, 0, -usageMaxDays).UnixMilli() / 60000
	return s.dimAggs(ctx, "", logAccountExpr, minBucket)
}

// AccountUsageToday 是单账号本地日界内的原始计数。
type AccountUsageToday struct {
	Requests int64 // 全部完成行（含 499）
	OK       int64 // 2xx——success_rate 分子
	Non499   int64 // 非 499（ccLoad total 口径，success_rate 分母）
	Tokens   int64 // total_tokens 和
}

// AccountUsageRow 是单账号 usage 块的原始量集合——内部行类型不进
// wire，供 P2 /admin/accounts 的 usage 字段（{rpm_now, tps_now,
// ttfb_avg, ttfb_p50, ttfb_p90, cache_rate, today:{requests,
// success_rate, tokens}}）投影消费。口径全部复用既有读路径：
//   - Recent：LogRecentWindow 60s 完成窗——rpm_now=Recent.Req（非 499
//     完成数，同 LogRecentRPM），tps_now=Recent.OutTok*1000/Recent.GenMS
//     （同 stats recentBlock 速度列），cache_rate=Recent.CrTok/
//     (Recent.InTok+Recent.CrTok+Recent.CwTok)（同 recentBlock
//     cache_pct）。
//   - TTFB：最近 usageSampleCapacity 条非空 first_upstream_ms 样本
//     （同 LogLatency["ttfb"] 口径）——TTFB.P50/P90 直取，TTFBAvgMS
//     是同一样本集的均值，与分位同源。
//   - Today：本地日界内计数——success_rate=Today.OK/Today.Non499。
type AccountUsageRow struct {
	Recent    LogRecentAgg
	Today     AccountUsageToday
	TTFB      LatencyStats
	TTFBAvgMS float64
}

// AccountUsage 返回单账号的 usage 原始量；account 走读侧折叠口径
// （'default' 命中 ”+'default' 两群）。三个分量各一条 SQL——TTFB
// 分位须取样本集在 Go 侧排序，无法并进聚合扫描；逐号 N 次调用的
// 成本随 lane 数线性，个位数无压力。
func (s *Store) AccountUsage(ctx context.Context, account string) (*AccountUsageRow, error) {
	row := &AccountUsageRow{}
	var err error
	if row.Recent, err = s.LogRecentWindow(ctx, 60, LogScope{Account: account}); err != nil {
		return nil, err
	}
	scopeWhere, scopeArgs := LogScope{Account: account}.where()
	dayStart, dayEnd := dayBoundsMS(time.Now())
	if err = s.ro.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code != 499 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(total_tokens), 0)
		FROM logs WHERE time >= ? AND time < ?`+scopeWhere,
		append([]any{dayStart, dayEnd}, scopeArgs...)...).Scan(
		&row.Today.Requests, &row.Today.OK, &row.Today.Non499, &row.Today.Tokens); err != nil {
		return nil, err
	}
	samples, err := s.recentSamples(ctx, "first_upstream_ms", true, account)
	if err != nil {
		return nil, err
	}
	row.TTFB = latencyStatsOf(samples)
	if len(samples) > 0 {
		var sum int64
		for _, v := range samples {
			sum += v
		}
		row.TTFBAvgMS = float64(sum) / float64(len(samples))
	}
	return row, nil
}
