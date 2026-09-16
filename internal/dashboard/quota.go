// 本文件实现配额快照采样与燃烧速率预测。
//
// /panel/api/status 每次都会取回日/周配额剩余百分比与重置时间，但看完即弃。
// 这里按固定间隔把快照追加到 logs/quota.jsonl，面板据此画出配额曲线，
// 并用最近窗口的消耗速率外推耗尽时刻——回答「按现在的用法还能撑多久」。
package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/debuglog"
)

// quotaFileName 是配额历史文件名，位于日志根目录下。
const quotaFileName = "quota.jsonl"

// quotaFileCap 是配额文件体积上限；超限后保留尾部一半重写。
// 每条约 200B，4MB 约覆盖 2 万条（默认 5 分钟一条 ≈ 69 天）。
const quotaFileCap = 4 << 20

// quotaPoint 是一次配额快照。
type quotaPoint struct {
	// At 是采样时刻（unix 秒）。
	At int64 `json:"at"`
	// DailyRemaining/WeeklyRemaining 是日/周配额剩余百分比（0-100）。
	// 指针保留「上游没报」与「真到 0」的区分：omitempty 会把 0% 序列化成
	// 缺席，恰恰在耗尽时刻让前端什么都不显示。
	DailyRemaining  *float64 `json:"daily_remaining"`
	WeeklyRemaining *float64 `json:"weekly_remaining"`
	// DailyResetAt/WeeklyResetAt 是日/周配额重置时刻（unix 秒）。
	DailyResetAt  int64 `json:"daily_reset_at,omitempty"`
	WeeklyResetAt int64 `json:"weekly_reset_at,omitempty"`
	// 以下为零散额度字段，原样透传便于面板展示。
	PromptCredits float64 `json:"prompt_credits,omitempty"`
	FlowCredits   float64 `json:"flow_credits,omitempty"`
	FlexCredits   float64 `json:"flex_credits,omitempty"`
	ACUConsumed   float64 `json:"acu_consumed,omitempty"`
	ACULimit      float64 `json:"acu_limit,omitempty"`
	UsedPrompt    float64 `json:"used_prompt_credits,omitempty"`
	UsedFlow      float64 `json:"used_flow_credits,omitempty"`
	UsedFlex      float64 `json:"used_flex_credits,omitempty"`
	// 宽限与自动加额状态：配额烧穿后不是立即断供，grace_period_status
	// 为 ACTIVE 时宽限期计数中，grace_period_end 是宽限截止（unix 秒）；
	// top_up_* 记录自动加额开关与最近一次加额交易状态。
	GracePeriodStatus string `json:"grace_period_status,omitempty"`
	GracePeriodEnd    int64  `json:"grace_period_end,omitempty"`
	OrphanedUsageCut  bool   `json:"was_reduced_by_orphaned_usage,omitempty"`
	TopUpEnabled      bool   `json:"top_up_enabled,omitempty"`
	TopUpStatus       string `json:"top_up_transaction_status,omitempty"`
}

// StartQuotaSampler 启动后台配额采样协程；interval<=0 或日志未启用时不启动。
// 采样失败只记一行进程日志，不影响面板与请求链路。
func (h *Handler) StartQuotaSampler(interval time.Duration) {
	if interval <= 0 || h.debugManager == nil || h.debugManager.Root() == "" {
		return
	}
	path := filepath.Join(h.debugManager.Root(), quotaFileName)
	go func() {
		h.sampleQuota(path)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			h.sampleQuota(path)
		}
	}()
}

// sampleQuota 拉取一次账户状态并把 plan_status 快照追加到 quota.jsonl。
func (h *Handler) sampleQuota(path string) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, plan, _, err := h.fetchUserStatus(ctx)
	if err != nil {
		slog.Warn("quota sample failed", "error", err)
		return
	}
	if plan == nil {
		// 上游 200 但缺 planStatus：不写点也不报错会把 quota.jsonl
		// 变成静默空文件，留一行痕迹说明「拉到了但无配额数据」。
		slog.Warn("quota sample skipped: userStatus carried no planStatus")
		return
	}
	point := quotaPoint{
		At:              time.Now().Unix(),
		DailyRemaining:  planFloat(plan, "daily_quota_remaining"),
		WeeklyRemaining: planFloat(plan, "weekly_quota_remaining"),
		DailyResetAt:    int64(floatAny(plan["daily_quota_reset"])),
		WeeklyResetAt:   int64(floatAny(plan["weekly_quota_reset"])),
		PromptCredits:   floatAny(plan["available_prompt_credits"]),
		FlowCredits:     floatAny(plan["available_flow_credits"]),
		FlexCredits:     floatAny(plan["available_flex_credits"]),
		ACUConsumed:     floatAny(plan["acu_consumed"]),
		ACULimit:        floatAny(plan["acu_limit"]),
		UsedPrompt:      floatAny(plan["used_prompt_credits"]),
		UsedFlow:        floatAny(plan["used_flow_credits"]),
		UsedFlex:        floatAny(plan["used_flex_credits"]),
		// plan["grace_period_status"] 已经 fetchUserStatus 的 shortEnum
		// 缩成尾段；grace_period_end 是归一后的 RFC3339，转回 unix 秒。
		GracePeriodStatus: strAny(plan["grace_period_status"]),
		GracePeriodEnd:    rfc3339Unix(plan["grace_period_end"]),
		OrphanedUsageCut:  boolAny(plan["was_reduced_by_orphaned_usage"]),
	}
	if tu, ok := plan["top_up_status"].(map[string]any); ok {
		point.TopUpEnabled = boolAny(tu["enabled"])
		point.TopUpStatus = strAny(tu["transaction_status"])
	}
	data, err := json.Marshal(point)
	if err != nil {
		return
	}
	if info, statErr := os.Stat(path); statErr == nil && info.Size() > quotaFileCap {
		if _, err := debuglog.TruncateToTail(path, quotaFileCap/2); err != nil {
			slog.Warn("quota history truncate failed", "error", err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("quota sample open failed", "error", err)
		return
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(append(data, '\n')); err != nil {
		slog.Warn("quota sample write failed", "error", err)
	}
}

// quotaHistoryCap 是单次读取的历史行数上限；默认 5 分钟间隔下约覆盖 34 天。
const quotaHistoryCap = 10000

// readQuotaHistory 读取 quota.jsonl 尾部 quotaHistoryCap 条有效行。
// 按行数上限折算字节上限（~256B/行）只读文件尾部，多年运行的文件不再整读；
// 截断点的首行残段解析失败自然跳过。
func (h *Handler) readQuotaHistory() []quotaPoint {
	if h.debugManager == nil || h.debugManager.Root() == "" {
		return nil
	}
	path := filepath.Join(h.debugManager.Root(), quotaFileName)
	data, err := debuglog.TailRead(path, quotaHistoryCap*256)
	if err != nil {
		return nil
	}
	var points []quotaPoint
	for line := range strings.Lines(string(data)) {
		var point quotaPoint
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &point) == nil {
			points = append(points, point)
		}
	}
	if len(points) > quotaHistoryCap {
		points = points[len(points)-quotaHistoryCap:]
	}
	return points
}

// forecast 用最近 lookback 窗口内的首尾两点差分估算燃烧速率与耗尽时刻。
// 配额只剩百分比语义：日配额在 daily_reset_at 重置，周配额同理；
// 「耗尽」指按当前速率在重置前把剩余百分比烧完。
func forecast(points []quotaPoint, lookback time.Duration, pick func(quotaPoint) float64, resetAt func(quotaPoint) int64) map[string]any {
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

// QuotaReport 返回配额历史曲线与按最近窗口燃烧速率外推的预测；
// 旧面板 /panel/api/quota 与移植面板 /admin/quota 共用。
func (h *Handler) QuotaReport() map[string]any {
	points := h.readQuotaHistory()
	return map[string]any{
		"points": points,
		"daily":  forecast(points, 24*time.Hour, func(p quotaPoint) float64 { return floatOr0(p.DailyRemaining) }, func(p quotaPoint) int64 { return p.DailyResetAt }),
		"weekly": forecast(points, 7*24*time.Hour, func(p quotaPoint) float64 { return floatOr0(p.WeeklyRemaining) }, func(p quotaPoint) int64 { return p.WeeklyResetAt }),
	}
}

// apiQuota 返回配额历史与燃烧速率预测。
func (h *Handler) apiQuota(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, h.QuotaReport())
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

// planFloat 取 planStatus 里的数值字段；键缺席返回 nil——quotaPoint 的
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
