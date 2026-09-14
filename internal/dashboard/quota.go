// 本文件实现配额快照采样与燃烧速率预测。
//
// /panel/api/status 每次都会取回日/周配额剩余百分比与重置时间，但看完即弃。
// 这里按固定间隔把快照追加到 logs/quota.jsonl，面板据此画出配额曲线，
// 并用最近窗口的消耗速率外推耗尽时刻——回答「按现在的用法还能撑多久」。
package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	DailyRemaining  float64 `json:"daily_remaining,omitempty"`
	WeeklyRemaining float64 `json:"weekly_remaining,omitempty"`
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
		DailyRemaining:  floatAny(plan["daily_quota_remaining"]),
		WeeklyRemaining: floatAny(plan["weekly_quota_remaining"]),
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
	}
	data, err := json.Marshal(point)
	if err != nil {
		return
	}
	if info, statErr := os.Stat(path); statErr == nil && info.Size() > quotaFileCap {
		truncateQuotaFile(path)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = file.Write(append(data, '\n'))
}

// truncateQuotaFile 保留文件尾部一半重写，防止多年运行无限增长。
func truncateQuotaFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return
	}
	start := info.Size() / 2
	data := make([]byte, info.Size()-start)
	if _, err = file.ReadAt(data, start); err != nil {
		_ = file.Close()
		return
	}
	_ = file.Close()
	// 丢弃首行残段（截断点可能落在行中间）。
	if idx := strings.IndexByte(string(data), '\n'); idx >= 0 {
		data = data[idx+1:]
	}
	_ = os.WriteFile(path, data, 0o600)
}

// quotaHistoryCap 是单次读取的历史行数上限；默认 5 分钟间隔下约覆盖 34 天。
const quotaHistoryCap = 10000

// readQuotaHistory 读取 quota.jsonl 全部有效行（尾部 quotaHistoryCap 条）。
func (h *Handler) readQuotaHistory() []quotaPoint {
	if h.debugManager == nil || h.debugManager.Root() == "" {
		return nil
	}
	path := filepath.Join(h.debugManager.Root(), quotaFileName)
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	var points []quotaPoint
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var point quotaPoint
		if json.Unmarshal(scanner.Bytes(), &point) == nil {
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

// apiQuota 返回配额历史与燃烧速率预测。
func (h *Handler) apiQuota(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	points := h.readQuotaHistory()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"points": points,
		"daily":  forecast(points, 24*time.Hour, func(p quotaPoint) float64 { return p.DailyRemaining }, func(p quotaPoint) int64 { return p.DailyResetAt }),
		"weekly": forecast(points, 7*24*time.Hour, func(p quotaPoint) float64 { return p.WeeklyRemaining }, func(p quotaPoint) int64 { return p.WeeklyResetAt }),
	})
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
