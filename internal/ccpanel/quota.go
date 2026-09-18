// 本文件实现配额快照采样与燃烧速率预测，以及 /admin/quota、/admin/status
// 两个端点。
//
// /admin/status 每次都会取回日/周配额剩余百分比与重置时间，但看完即弃。
// 这里按固定间隔把快照写入 quota_samples 表，面板据此画出配额曲线，
// 并用最近窗口的消耗速率外推耗尽时刻——回答「按现在的用法还能撑多久」。
package ccpanel

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

// adminQuota 实现 GET /admin/quota：日/周配额历史曲线与燃烧速率预测。
// 账户计费数据只对 admin 开放。
func (h *Handler) adminQuota(w http.ResponseWriter, r *http.Request) {
	respondOK(w, h.QuotaReport(r.Context()))
}

// adminStatus 实现 GET /admin/status：上游账户/plan/容量/IDE/模型状态/
// 供应商的六路聚合。整页结果进 statusSnapshot 的短 TTL 缓存。
func (h *Handler) adminStatus(w http.ResponseWriter, r *http.Request) {
	respondOK(w, h.statusSnapshot(r.Context()))
}

// statusSnapshot 返回 TTL 内的 StatusReport 缓存；并发收敛与超时兜底
// 由 statusCache（ttlCache）承担。等待方断连吃 ctx 取消——映射为
// fetch_error 键回给前端，与 StatusReport 单路失败落 *_error 键的
// 既定语义一致。
func (h *Handler) statusSnapshot(ctx context.Context) map[string]any {
	snap, err := h.statusCache.Get(ctx)
	if err != nil {
		return map[string]any{"fetch_error": err.Error()}
	}
	return snap
}

// 配额快照的行类型是 store.QuotaSample——表行与 /admin/quota 的
// points 线格式共用一个形状（字段 JSON tag 与被取代的 quota.jsonl
// 行一致）。Daily/WeeklyRemaining 是 *float64：保留「上游没报」
// （nil/NULL）与「真到 0」的区分，耗尽时刻前端仍能画出 0%。

// SetQuotaInterval 设定后台配额采样周期；interval<=0 或持久层未注入时
// 停采。可被重复调用（配置 reload 热路径）：cancel 旧协程按新间隔重起，
// 变更点多采一个点——无害，反而给曲线留了变更标记。
// 采样失败只记一行进程日志，不影响面板与请求链路。
func (h *Handler) SetQuotaInterval(interval time.Duration) {
	h.quotaMu.Lock()
	defer h.quotaMu.Unlock()
	// 记录最近一次请求值（含停采的 <=0）：ticker 起跑后自身不暴露周期，
	// 面板设置页回读生效值要靠这个簿记。
	h.quotaInterval = interval
	if h.quotaCancel != nil {
		h.quotaCancel()
		h.quotaCancel = nil
	}
	if interval <= 0 || h.store == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.quotaCancel = cancel
	go func() {
		h.sampleQuota(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.sampleQuota(ctx)
			}
		}
	}()
}

// QuotaInterval 返回最近一次 SetQuotaInterval 请求的采样周期（<=0 表示
// 已停采），供面板设置页回读生效值。
func (h *Handler) QuotaInterval() time.Duration {
	h.quotaMu.Lock()
	defer h.quotaMu.Unlock()
	return h.quotaInterval
}

// sampleQuota 对每个账号各拉取一次状态并把 plan_status 快照写入
// quota_samples（每行带 account 字段，两号曲线分开画）。账号间按名序
// 逐个采——间隔默认 5 分钟，串行两次上游调用无并发必要。ctx 是采样
// 协程的生命周期：SetQuotaInterval 停采/重起会打断在途轮次。
func (h *Handler) sampleQuota(ctx context.Context) {
	for _, account := range h.quotaAccounts() {
		if ctx.Err() != nil {
			return
		}
		h.sampleAccountQuota(ctx, account.name, account.token)
	}
}

// quotaAccount 是配额采样的一个账号视角：name 落 quota_samples 的
// account 列，token 是该 lane 的当前凭据。
type quotaAccount struct {
	name  string
	token string
}

// quotaAccounts 返回本轮要采样的账号清单，按三种状态分别处置：
//   - poolTokenFuncs 未接线（nil）：回退面板首号凭据源的单号匿名
//     采样，account 字段留空——与历史上无号池时的行格式一致；
//   - 已接线但空池（返回空 map）：返回空清单整轮跳过——再往下走
//     tokenFunc→firstLane 会裸取下标 panic，且每周期写一条
//     account="" 的上游 401 失败行污染 default 桶；
//   - 有号：逐号采，按名序输出稳定。
func (h *Handler) quotaAccounts() []quotaAccount {
	if h.poolTokenFuncs == nil {
		return []quotaAccount{{token: h.tokenFunc()}}
	}
	funcs := h.poolTokenFuncs()
	if len(funcs) == 0 {
		return nil
	}
	names := make([]string, 0, len(funcs))
	for name := range funcs {
		names = append(names, name)
	}
	slices.Sort(names)
	accounts := make([]quotaAccount, 0, len(names))
	for _, name := range names {
		accounts = append(accounts, quotaAccount{name: name, token: funcs[name]()})
	}
	return accounts
}

// sampleAccountQuota 拉取一个账号的状态并写入一行配额快照；ctx 挂在
// 采样协程生命周期上，单号上限 120s。
func (h *Handler) sampleAccountQuota(ctx context.Context, account, token string) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if _, _, err := h.captureAccountQuota(ctx, account, token); err != nil {
		slog.Warn("quota sample failed", "account", account, "error", err)
	}
}

// captureAccountQuota 是逐号配额采样内核：拉取该号 userStatus、更新
// quotaUsers 身份投影、把 planStatus 快照写入 quota_samples，返回
// 投影后的 (user, plan)。定时采样与手动刷新共用——后者把返回值回
// 显给操作者。plan 为 nil 表示上游 200 但未携带 planStatus：身份
// 投影照常更新，本轮只是无配额点可写，不算错误。
func (h *Handler) captureAccountQuota(ctx context.Context, account, token string) (user, plan map[string]any, err error) {
	rawUser, plan, _, err := h.fetchUserStatusAs(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	user = map[string]any{
		"name":             strAny(rawUser["name"]),
		"email":            strAny(rawUser["email"]),
		"pro":              rawUser["pro"],
		"teams_tier":       strAny(rawUser["teams_tier"]),
		"plan_name":        strAny(plan["plan_name"]),
		"billing_strategy": strAny(plan["billing_strategy"]),
	}
	h.quotaUserMu.Lock()
	if h.quotaUsers == nil {
		h.quotaUsers = map[string]map[string]any{}
	}
	h.quotaUsers[account] = user
	h.quotaUserMu.Unlock()
	if plan == nil {
		// 上游 200 但缺 planStatus：不写点也不报错会让曲线静默断档，
		// 留一行痕迹说明「拉到了但无配额数据」。
		slog.Warn("quota sample skipped: userStatus carried no planStatus", "account", account)
		return user, nil, nil
	}
	point := &store.QuotaSample{
		At:                time.Now().Unix(),
		Account:           account,
		DailyRemaining:    planFloat(plan, "daily_quota_remaining"),
		WeeklyRemaining:   planFloat(plan, "weekly_quota_remaining"),
		DailyResetAt:      int64(floatAny(plan["daily_quota_reset"])),
		WeeklyResetAt:     int64(floatAny(plan["weekly_quota_reset"])),
		PromptCredits:     floatAny(plan["available_prompt_credits"]),
		FlowCredits:       floatAny(plan["available_flow_credits"]),
		FlexCredits:       floatAny(plan["available_flex_credits"]),
		ACUConsumed:       floatAny(plan["acu_consumed"]),
		ACULimit:          floatAny(plan["acu_limit"]),
		UsedPromptCredits: floatAny(plan["used_prompt_credits"]),
		UsedFlowCredits:   floatAny(plan["used_flow_credits"]),
		UsedFlexCredits:   floatAny(plan["used_flex_credits"]),
		// overage_balance_micros 是 micros 粒度的欠费账本（负值=负债），
		// 比整数百分比细得多——付费燃烧走 overage 通道时百分比不动它动。
		OverageBalanceMicros: int64(floatAny(plan["overage_balance_micros"])),
		// plan["grace_period_status"] 已经 fetchUserStatus 的 shortEnum
		// 缩成尾段；grace_period_end 是归一后的 RFC3339，转回 unix 秒。
		GracePeriodStatus:         strAny(plan["grace_period_status"]),
		GracePeriodEnd:            rfc3339Unix(plan["grace_period_end"]),
		WasReducedByOrphanedUsage: boolAny(plan["was_reduced_by_orphaned_usage"]),
	}
	if tu, ok := plan["top_up_status"].(map[string]any); ok {
		point.TopUpEnabled = boolAny(tu["enabled"])
		point.TopUpTransactionStatus = strAny(tu["transaction_status"])
	}
	if err := h.store.InsertQuotaSample(ctx, point); err != nil {
		slog.Warn("quota sample persist failed", "account", account, "error", err)
	}
	h.noteAccountQuotaSignal(account, plan)
	return user, plan, nil
}

// noteAccountQuotaSignal 把一次成功探测的日/周剩余百分比回灌给池侧
// 降权簿记；两键俱缺时不喂——weekly 缺报按 0 喂会把 lane 误判进降权档。
func (h *Handler) noteAccountQuotaSignal(account string, plan map[string]any) {
	if h.accountQuotaSignal == nil || plan == nil {
		return
	}
	daily := planFloat(plan, "daily_quota_remaining")
	weekly := planFloat(plan, "weekly_quota_remaining")
	if daily == nil || weekly == nil {
		return
	}
	h.accountQuotaSignal(account, *daily, *weekly)
}

// refreshAccountQuota 即采一次指定账号配额：与定时采样共用
// captureAccountQuota 内核（拉 userStatus、更新 quotaUsers 投影、
// 落 quota_samples 行），把 {account, user, plan} 回给
// /admin/accounts/{name}/quota/refresh 作响应体——plan 直出
// fetchUserStatusAs 归一化后的 planStatus 子集，与采样落库的字段名
// 同口径。上游失败返回 error（handler 映 502）；plan 为 nil 表示
// 上游没报 planStatus。与定时采样同秒撞 (account,at) 唯一索引时
// INSERT OR IGNORE 静默丢点，不算失败。
func (h *Handler) refreshAccountQuota(ctx context.Context, account, token string) (map[string]any, error) {
	user, plan, err := h.captureAccountQuota(ctx, account, token)
	if err != nil {
		return nil, err
	}
	return map[string]any{"account": account, "user": user, "plan": plan}, nil
}

// quotaHistoryCap 是单次读取的历史样本数上限；默认 5 分钟间隔下约覆盖
// 34 天。
const quotaHistoryCap = 10000

// readQuotaHistory 读 quota_samples 尾部 quotaHistoryCap 条（at 升序，
// 截断下推 SQL LIMIT）。按号分组的裁剪在 QuotaReport 侧做；库查询失败
// 按无历史降级。
func (h *Handler) readQuotaHistory(ctx context.Context) []*store.QuotaSample {
	if h.store == nil {
		return nil
	}
	points, err := h.store.ListQuotaSamples(ctx, "", 0, quotaHistoryCap)
	if err != nil {
		slog.Warn("quota history read failed", "error", err)
		return nil
	}
	return points
}

// forecast 用最近 lookback 窗口内的逐相邻样本差分估算燃烧速率与耗尽
// 时刻：分子分母同步累计——只把「两端都报了数且 remaining 未上升」的
// 相邻段计入消耗与时长；remaining 上升的相邻段是周期重置边界，跳过
// （首尾两点差分遇到跨重置窗口会把回满错算成负消耗，烧着却报烧不完）；
// pick 返回 NaN 表示「上游没报」，含 NaN 端点的段不可测、不计入——
// 把 nil 当 0% 会伪造一次烧到 0 的差分。
// 配额只剩百分比语义：日配额在 daily_reset_at 重置，周配额同理；
// 「耗尽」指按当前速率在重置前把剩余百分比烧完。
func forecast(points []*store.QuotaSample, lookback time.Duration, pick func(*store.QuotaSample) float64, resetAt func(*store.QuotaSample) int64) map[string]any {
	if len(points) < 2 {
		return nil
	}
	last := points[len(points)-1]
	cutoff := last.At - int64(lookback.Seconds())
	start := 0
	for i := len(points) - 2; i >= 0; i-- {
		if points[i].At <= cutoff {
			break
		}
		start = i
	}
	var consumed float64
	var measured int64
	for i := start + 1; i < len(points); i++ {
		prev, cur := pick(points[i-1]), pick(points[i])
		if math.IsNaN(prev) || math.IsNaN(cur) || cur > prev {
			continue
		}
		consumed += prev - cur
		measured += points[i].At - points[i-1].At
	}
	if measured <= 0 {
		return nil
	}
	hours := float64(measured) / 3600
	rate := consumed / hours // 百分比/小时，消耗为正
	out := map[string]any{
		"window_hours":  hours,
		"reset_at":      resetAt(last),
		"burn_per_hour": rate,
		"burn_per_day":  rate * 24,
	}
	if rem := pick(last); !math.IsNaN(rem) {
		out["remaining"] = rem
		if rate > 0 {
			hoursLeft := rem / rate
			exhaustedAt := last.At + int64(hoursLeft*3600)
			// 外推的耗尽时刻越过重置点就没有物理意义：配额在 reset_at
			// 先回满，本周期烧不完——报 survives_until_reset 而非一个
			// 不可能发生的 exhausted_at。reset_at 未知或已过期时无法
			// 判定边界，按原样报 exhausted_at。
			if reset := resetAt(last); reset > last.At && exhaustedAt > reset {
				out["survives_until_reset"] = true
			} else {
				out["exhausted_at"] = exhaustedAt
			}
			out["hours_left"] = hoursLeft
		}
	}
	return out
}

// QuotaReport 返回配额历史曲线与按最近窗口燃烧速率外推的预测。
// 号池下每号配额独立：accounts 组按名给各自的曲线与预测（冻结序列
// 只留曲线与 stale 标记，见 quotaSeriesReport），顶层
// points/daily/weekly 镜像尾点 At 最大（最新鲜）的那条序列作后
// 兼容视图——单号部署时与升级前输出逐字段一致（历史无 account
// 字段的行归入 "default" 桶，与隐式单 lane 同名自然合流）。
func (h *Handler) QuotaReport(ctx context.Context) map[string]any {
	byAccount := map[string][]*store.QuotaSample{}
	for _, point := range h.readQuotaHistory(ctx) {
		name := point.Account
		if name == "" {
			// logs 逐号聚合同样按 COALESCE(NULLIF(account,''),'default')
			// 对齐此口径——''/default/真名三群取值两侧折叠一致。
			name = "default"
		}
		byAccount[name] = append(byAccount[name], point)
	}
	names := make([]string, 0, len(byAccount))
	for name := range byAccount {
		names = append(names, name)
	}
	slices.Sort(names)
	accounts := make(map[string]any, len(names))
	for _, name := range names {
		accounts[name] = h.quotaSeriesReport(name, byAccount[name])
	}
	out := map[string]any{"accounts": accounts}
	// 镜像跟随最新鲜的序列而非名序首个：被移出号池的号曲线停更，
	// 名序首个可能恰是那条冻住的序列，兼容视图会定格在旧数据上。
	// 尾点 At 相同取名序靠前者——names 已排序，先到最大值的胜出。
	freshest, freshestAt := "", int64(-1)
	for _, name := range names {
		if at := byAccount[name][len(byAccount[name])-1].At; at > freshestAt {
			freshest, freshestAt = name, at
		}
	}
	if freshest != "" {
		mirror := accounts[freshest].(map[string]any)
		out["points"] = mirror["points"]
		out["daily"] = mirror["daily"]
		out["weekly"] = mirror["weekly"]
	}
	return out
}

// quotaSeriesStaleFloor 是冻结序列判定的龄期下限：尾点距今超过
// max(3×采样周期, quotaSeriesStaleFloor) 的序列视为冻结。
const quotaSeriesStaleFloor = time.Hour

// quotaSeriesReport 用一条样本序列构建单号报告：points 曲线 + 日/周
// forecast，并并入该号的身份快照（采样顺带取回；只对确有记录的 lane
// 投影，重启后首个采样点落盘前的缺席交给前端渲染成未知）。单号视图
// （accounts 写端点回包）也走它，免去为一条序列扫全表。
// 冻结序列（尾点过旧：lane 被移出号池、采样停摆、历史空串行归入
// default 的遗留桶）不喂 forecast——它的外推锚在死尾点上，耗尽
// 时刻会落进过去；只留曲线并打 stale 标记，消费方据此判读。bound
// 随采样周期缩放、下限一小时：进程重启或短暂停采造成的缺口不误判，
// 单号部署下持续写入的 account 空串（→default）活跃序列不受影响。
func (h *Handler) quotaSeriesReport(name string, series []*store.QuotaSample) map[string]any {
	staleAfter := quotaSeriesStaleFloor
	if scaled := 3 * h.QuotaInterval(); scaled > staleAfter {
		staleAfter = scaled
	}
	stale := time.Now().Unix()-series[len(series)-1].At > int64(staleAfter.Seconds())
	report := map[string]any{
		"points": series,
		"stale":  stale,
		"daily":  nil,
		"weekly": nil,
	}
	if !stale {
		report["daily"] = forecast(series, 24*time.Hour, func(p *store.QuotaSample) float64 { return remainingOrNaN(p.DailyRemaining) }, func(p *store.QuotaSample) int64 { return p.DailyResetAt })
		report["weekly"] = forecast(series, 7*24*time.Hour, func(p *store.QuotaSample) float64 { return remainingOrNaN(p.WeeklyRemaining) }, func(p *store.QuotaSample) int64 { return p.WeeklyResetAt })
	}
	h.quotaUserMu.Lock()
	if u, ok := h.quotaUsers[name]; ok {
		report["user"] = u
	}
	h.quotaUserMu.Unlock()
	return report
}

// floatAny 把 fetchUserStatus 产出的宽松数值统一成 float64。
func floatAny(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

// planFloat 取 planStatus 里的数值字段；键缺席返回 nil——QuotaSample 的
// 指针字段靠它保住「未上报」与「0%」的区分。
func planFloat(plan map[string]any, key string) *float64 {
	v, ok := plan[key]
	if !ok {
		return nil
	}
	f := floatAny(v)
	return &f
}

// floatOr0 解引用配额指针，nil（上游未上报）按 0 返回——只用于展示性
// 读取；forecast 差分走 remainingOrNaN，那里必须把「没报」与「真到 0」
// 区分开。
func floatOr0(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// remainingOrNaN 解引用配额指针，nil 编码为 NaN——forecast 靠它识别
// 「这段不可测」而不把缺席伪造成 0%（那会谎报一次烧尽的差分）。
func remainingOrNaN(v *float64) float64 {
	if v == nil {
		return math.NaN()
	}
	return *v
}

// rfc3339Unix 把 planStatus 里归一化后的 RFC3339 时刻转回 unix 秒；
// 缺席或畸形记 0——omitempty 让快照里该键消失，与「未上报」口径一致。
func rfc3339Unix(v any) int64 {
	t, err := time.Parse(time.RFC3339, strAny(v))
	if err != nil {
		return 0
	}
	return t.Unix()
}
