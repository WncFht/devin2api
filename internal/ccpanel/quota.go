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
// 供应商的六路聚合。
func (h *Handler) adminStatus(w http.ResponseWriter, r *http.Request) {
	// 面板聚合多个上游调用，给足时间避免单个慢接口拖垮整体；
	// 与 ResponseHeaderTimeout 对齐，允许上游长时思考/排队。
	ctx, cancel := context.WithTimeout(r.Context(), 610*time.Second)
	defer cancel()
	respondOK(w, h.StatusReport(ctx))
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
		h.sampleQuota()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.sampleQuota()
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
// 逐个采——间隔默认 5 分钟，串行两次上游调用无并发必要。
func (h *Handler) sampleQuota() {
	for _, account := range h.quotaAccounts() {
		h.sampleAccountQuota(account.name, account.token)
	}
}

// quotaAccount 是配额采样的一个账号视角：name 落 quota_samples 的
// account 列，token 是该 lane 的当前凭据。
type quotaAccount struct {
	name  string
	token string
}

// quotaAccounts 返回本轮要采样的账号清单：号池经 SetPoolTokenFuncs
// 登记时逐号采（按名序输出稳定）；未登记回退面板首号凭据源、
// account 字段留空——与历史上无号池时的行格式一致。
func (h *Handler) quotaAccounts() []quotaAccount {
	funcs := map[string]func() string{}
	if h.poolTokenFuncs != nil {
		funcs = h.poolTokenFuncs()
	}
	if len(funcs) == 0 {
		return []quotaAccount{{token: h.tokenFunc()}}
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

// sampleAccountQuota 拉取一个账号的状态并写入一行配额快照。
func (h *Handler) sampleAccountQuota(account, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	user, plan, _, err := h.fetchUserStatusAs(ctx, token)
	if err != nil {
		slog.Warn("quota sample failed", "account", account, "error", err)
		return
	}
	h.quotaUserMu.Lock()
	if h.quotaUsers == nil {
		h.quotaUsers = map[string]map[string]any{}
	}
	h.quotaUsers[account] = map[string]any{
		"name":             strAny(user["name"]),
		"email":            strAny(user["email"]),
		"pro":              user["pro"],
		"teams_tier":       strAny(user["teams_tier"]),
		"plan_name":        strAny(plan["plan_name"]),
		"billing_strategy": strAny(plan["billing_strategy"]),
	}
	h.quotaUserMu.Unlock()
	if plan == nil {
		// 上游 200 但缺 planStatus：不写点也不报错会让曲线静默断档，
		// 留一行痕迹说明「拉到了但无配额数据」。
		slog.Warn("quota sample skipped: userStatus carried no planStatus", "account", account)
		return
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
}

// quotaHistoryCap 是单次读取的历史样本数上限；默认 5 分钟间隔下约覆盖
// 34 天。
const quotaHistoryCap = 10000

// readQuotaHistory 读 quota_samples 尾部 quotaHistoryCap 条（at 升序）。
// 按号分组的裁剪在 QuotaReport 侧做；库查询失败按无历史降级。
func (h *Handler) readQuotaHistory(ctx context.Context) []*store.QuotaSample {
	if h.store == nil {
		return nil
	}
	points, err := h.store.ListQuotaSamples(ctx, "", 0)
	if err != nil {
		slog.Warn("quota history read failed", "error", err)
		return nil
	}
	if len(points) > quotaHistoryCap {
		points = points[len(points)-quotaHistoryCap:]
	}
	return points
}

// forecast 用最近 lookback 窗口内的首尾两点差分估算燃烧速率与耗尽时刻。
// 配额只剩百分比语义：日配额在 daily_reset_at 重置，周配额同理；
// 「耗尽」指按当前速率在重置前把剩余百分比烧完。
func forecast(points []*store.QuotaSample, lookback time.Duration, pick func(*store.QuotaSample) float64, resetAt func(*store.QuotaSample) int64) map[string]any {
	if len(points) < 2 {
		return nil
	}
	last := points[len(points)-1]
	cutoff := last.At - int64(lookback.Seconds())
	first := points[0]
	for i := len(points) - 2; i >= 0; i-- {
		if points[i].At <= cutoff {
			break
		}
		first = points[i]
	}
	if first.At == last.At {
		return nil
	}
	hours := float64(last.At-first.At) / 3600
	rate := (pick(first) - pick(last)) / hours // 百分比/小时，消耗为正
	out := map[string]any{
		"window_hours":  hours,
		"remaining":     pick(last),
		"reset_at":      resetAt(last),
		"burn_per_hour": rate,
		"burn_per_day":  rate * 24,
	}
	if rate > 0 {
		hoursLeft := pick(last) / rate
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
	return out
}

// QuotaReport 返回配额历史曲线与按最近窗口燃烧速率外推的预测。
// 号池下每号配额独立：accounts 组按名给各自的曲线与预测，顶层
// points/daily/weekly 镜像尾点 At 最大（最新鲜）的那条序列作后
// 兼容视图——单号部署时与升级前输出逐字段一致（历史无 account
// 字段的行归入 "default" 桶，与隐式单 lane 同名自然合流）。
func (h *Handler) QuotaReport(ctx context.Context) map[string]any {
	byAccount := map[string][]*store.QuotaSample{}
	for _, point := range h.readQuotaHistory(ctx) {
		name := point.Account
		if name == "" {
			name = "default"
		}
		byAccount[name] = append(byAccount[name], point)
	}
	reportFor := func(series []*store.QuotaSample) map[string]any {
		return map[string]any{
			"points": series,
			"daily":  forecast(series, 24*time.Hour, func(p *store.QuotaSample) float64 { return floatOr0(p.DailyRemaining) }, func(p *store.QuotaSample) int64 { return p.DailyResetAt }),
			"weekly": forecast(series, 7*24*time.Hour, func(p *store.QuotaSample) float64 { return floatOr0(p.WeeklyRemaining) }, func(p *store.QuotaSample) int64 { return p.WeeklyResetAt }),
		}
	}
	names := make([]string, 0, len(byAccount))
	for name := range byAccount {
		names = append(names, name)
	}
	slices.Sort(names)
	h.quotaUserMu.Lock()
	users := make(map[string]map[string]any, len(h.quotaUsers))
	for name, u := range h.quotaUsers {
		users[name] = u
	}
	h.quotaUserMu.Unlock()
	accounts := make(map[string]any, len(names))
	for _, name := range names {
		report := reportFor(byAccount[name])
		// user 是采样顺带取回的身份快照：只对确有该号记录的 lane
		// 投影，重启后首个采样点落盘前的缺席交给前端渲染成未知。
		if u, ok := users[name]; ok {
			report["user"] = u
		}
		accounts[name] = report
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

// floatOr0 解引用配额指针；nil（上游未上报）按 0 参与差分，与此前
// 字段缺席落 0 的口径一致。
func floatOr0(v *float64) float64 {
	if v == nil {
		return 0
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
