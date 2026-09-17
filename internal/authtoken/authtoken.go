// Package authtoken 实现下游 API 令牌仓：auth_tokens 表（internal/store）
// 持久化、/v1 准入用的哈希解析、并发槽与费用限额计数。契约对齐 ccLoad 的
// AuthToken：JSON 形状逐字段一致，明文令牌只在创建时返回一次，
// 存库与列表输出均为 SHA-256 全哈希（hex 64）。
//
// 与 ccLoad 的一处有意偏差：不支持「直接出示哈希」双路径——哈希在
// 面板列表里可见，若接受哈希当凭据，看过面板的人就拿到可用令牌。
// 索引关联用截断哈希：logs 表的 key_hash = sha256(明文)[:8字节]
// 即 Token.Hash 的前 16 个 hex 字符。
package authtoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

// Token 是一条下游访问令牌，序列化形状对齐 ccLoad model.AuthToken。
// Hash 保存 sha256(明文) 的全 hex；明文不落盘。
type Token struct {
	ID          int64     `json:"id"`
	Hash        string    `json:"token"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   *int64    `json:"expires_at,omitempty"`
	LastUsedAt  *int64    `json:"last_used_at,omitempty"`
	IsActive    bool      `json:"is_active"`

	SuccessCount   int64   `json:"success_count"`
	FailureCount   int64   `json:"failure_count"`
	StreamAvgTTFB  float64 `json:"stream_avg_ttfb"`
	NonStreamAvgRT float64 `json:"non_stream_avg_rt"`
	StreamCount    int64   `json:"stream_count"`
	NonStreamCount int64   `json:"non_stream_count"`

	PromptTokensTotal        int64   `json:"prompt_tokens_total"`
	CompletionTokensTotal    int64   `json:"completion_tokens_total"`
	CacheReadTokensTotal     int64   `json:"cache_read_tokens_total"`
	CacheCreationTokensTotal int64   `json:"cache_creation_tokens_total"`
	TotalCostUSD             float64 `json:"total_cost_usd"`
	EffectiveCostUSD         float64 `json:"effective_cost_usd"`

	// 费用窗口用微美元整数计数（1 USD = 1e6）。窗口起点是服务器本地
	// 日历日/自然月 0 点的 Unix 毫秒；起点不匹配当前周期时该窗口
	// 用量视为 0（与 ccLoad CurrentPeriodCostUsed 同义）。
	CostUsedMicroUSD     int64 `json:"cost_used_micro_usd"`
	CostLimitMicroUSD    int64 `json:"cost_limit_micro_usd"`
	DailyUsedMicroUSD    int64 `json:"cost_daily_used_micro_usd"`
	DailyLimitMicroUSD   int64 `json:"cost_daily_limit_micro_usd"`
	DailyPeriodStart     int64 `json:"cost_daily_period_start"`
	MonthlyUsedMicroUSD  int64 `json:"cost_monthly_used_micro_usd"`
	MonthlyLimitMicroUSD int64 `json:"cost_monthly_limit_micro_usd"`
	MonthlyPeriodStart   int64 `json:"cost_monthly_period_start"`

	// 5h/weekly 是锚定滚动窗口（区别于日历日/自然月）：锚点为窗口
	// 过期后的首次记账时刻，now > anchor+窗口长即过期，过期窗口用量
	// 读取时视为 0——与日历窗口同为懒惰重置，只是起点由流量决定。
	Cost5hUsedMicroUSD      int64 `json:"cost_5h_used_micro_usd"`
	Cost5hLimitMicroUSD     int64 `json:"cost_5h_limit_micro_usd"`
	Cost5hAnchor            int64 `json:"cost_5h_anchor"`
	CostWeeklyUsedMicroUSD  int64 `json:"cost_weekly_used_micro_usd"`
	CostWeeklyLimitMicroUSD int64 `json:"cost_weekly_limit_micro_usd"`
	CostWeeklyPeriodStart   int64 `json:"cost_weekly_period_start"`

	AllowedModels []string `json:"allowed_models,omitempty"`
	// MaxConcurrency 是同时在途请求上限（占槽/还槽）；MaxRPM 是每分钟
	// 请求数上限（固定分钟桶计数，重启归零不持久化）。0 均为不限制。
	MaxConcurrency int `json:"max_concurrency"`
	MaxRPM         int `json:"max_rpm"`
	// Class 是请求类标记：fg（前台，默认）经速率闸门按原规则准入；
	// bg（后台，无人值守批跑）在同一条闸门上叠加动态预留约束——
	// 语义见 docs/gate-classes.md。空值按 fg 处理。
	Class string `json:"class"`

	inflight  int64 // 在途并发计数，不序列化
	rpmBucket int64 // 当前 RPM 计数的分钟桶（unix 秒/60），不序列化
	rpmCount  int64 // 当前分钟桶内已计请求数，不序列化
}

// AnonymousHash 是空明文的存储哈希：Hash 等于它的行即「匿名通道」——
// 未携带凭据的 /v1 请求按该行准入。明文为空串，没有可出示的令牌值。
var AnonymousHash = HashToken("")

// 令牌请求类取值：fg 是默认与全部存量语义，bg 在速率闸门里走动态
// 预留约束。ValidateClass 是管理面输入校验；NormalizeClass 是持久化
// 前的兜底归一——空值/未知值都落成 fg，保证库里只有两值。
const (
	ClassFG = "fg"
	ClassBG = "bg"
)

// ValidateClass 报告 class 是否合法取值（fg/bg/空——空表示默认 fg）。
func ValidateClass(class string) bool {
	return class == "" || class == ClassFG || class == ClassBG
}

// NormalizeClass 把空值与非法值归一成 fg——写库前的兜底，列默认值同向。
func NormalizeClass(class string) string {
	if class == ClassBG {
		return ClassBG
	}
	return ClassFG
}

// IsAnonymous 报告该令牌是不是匿名通道行。
func (t *Token) IsAnonymous() bool { return t.Hash == AnonymousHash }

// View 是令牌对外的 JSON 形状（ccLoad authTokenJSON 同形）：
// 内部 micro 字段折算成 *_usd 浮点；PeakRPM/AvgRPM/RecentRPM 是
// 纯响应字段（时间窗覆盖层写入，不落库）。
type View struct {
	ID                       int64     `json:"id"`
	Token                    string    `json:"token"`
	Description              string    `json:"description"`
	CreatedAt                time.Time `json:"created_at"`
	ExpiresAt                *int64    `json:"expires_at,omitempty"`
	LastUsedAt               *int64    `json:"last_used_at,omitempty"`
	IsActive                 bool      `json:"is_active"`
	SuccessCount             int64     `json:"success_count"`
	FailureCount             int64     `json:"failure_count"`
	StreamAvgTTFB            float64   `json:"stream_avg_ttfb"`
	NonStreamAvgRT           float64   `json:"non_stream_avg_rt"`
	StreamCount              int64     `json:"stream_count"`
	NonStreamCount           int64     `json:"non_stream_count"`
	PromptTokensTotal        int64     `json:"prompt_tokens_total"`
	CompletionTokensTotal    int64     `json:"completion_tokens_total"`
	CacheReadTokensTotal     int64     `json:"cache_read_tokens_total"`
	CacheCreationTokensTotal int64     `json:"cache_creation_tokens_total"`
	TotalCostUSD             float64   `json:"total_cost_usd"`
	EffectiveCostUSD         float64   `json:"effective_cost_usd"`
	CostUsedUSD              float64   `json:"cost_used_usd"`
	CostLimitUSD             float64   `json:"cost_limit_usd"`
	CostDailyUsedUSD         float64   `json:"cost_daily_used_usd"`
	CostDailyLimitUSD        float64   `json:"cost_daily_limit_usd"`
	CostMonthlyUsedUSD       float64   `json:"cost_monthly_used_usd"`
	CostMonthlyLimitUSD      float64   `json:"cost_monthly_limit_usd"`
	Cost5hUsedUSD            float64   `json:"cost_5h_used_usd"`
	Cost5hLimitUSD           float64   `json:"cost_5h_limit_usd"`
	CostWeeklyUsedUSD        float64   `json:"cost_weekly_used_usd"`
	CostWeeklyLimitUSD       float64   `json:"cost_weekly_limit_usd"`
	PeakRPM                  float64   `json:"peak_rpm,omitempty"`
	AvgRPM                   float64   `json:"avg_rpm,omitempty"`
	RecentRPM                float64   `json:"recent_rpm,omitempty"`
	AllowedModels            []string  `json:"allowed_models,omitempty"`
	MaxConcurrency           int       `json:"max_concurrency"`
	MaxRPM                   int       `json:"max_rpm"`
	Class                    string    `json:"class"`
	// Anonymous 标记该行是匿名通道（空明文占位行）——面板据此显示
	// "(anonymous)" 而非掩码哈希，且不展示任何可复制的凭据。
	Anonymous bool `json:"anonymous,omitempty"`
}

// API 返回对外视图：micro 窗口折算成 USD，窗口周期不匹配时用量归 0。
func (t *Token) API() View {
	now := time.Now()
	dayStart, monthStart := periodStarts(now)
	dailyUsed := t.DailyUsedMicroUSD
	if t.DailyPeriodStart != dayStart {
		dailyUsed = 0
	}
	monthlyUsed := t.MonthlyUsedMicroUSD
	if t.MonthlyPeriodStart != monthStart {
		monthlyUsed = 0
	}
	return View{
		ID:                       t.ID,
		Token:                    t.Hash,
		Description:              t.Description,
		CreatedAt:                t.CreatedAt,
		ExpiresAt:                t.ExpiresAt,
		LastUsedAt:               t.LastUsedAt,
		IsActive:                 t.IsActive,
		SuccessCount:             t.SuccessCount,
		FailureCount:             t.FailureCount,
		StreamAvgTTFB:            t.StreamAvgTTFB,
		NonStreamAvgRT:           t.NonStreamAvgRT,
		StreamCount:              t.StreamCount,
		NonStreamCount:           t.NonStreamCount,
		PromptTokensTotal:        t.PromptTokensTotal,
		CompletionTokensTotal:    t.CompletionTokensTotal,
		CacheReadTokensTotal:     t.CacheReadTokensTotal,
		CacheCreationTokensTotal: t.CacheCreationTokensTotal,
		TotalCostUSD:             t.TotalCostUSD,
		EffectiveCostUSD:         t.EffectiveCostUSD,
		CostUsedUSD:              float64(t.CostUsedMicroUSD) / 1e6,
		CostLimitUSD:             float64(t.CostLimitMicroUSD) / 1e6,
		CostDailyUsedUSD:         float64(dailyUsed) / 1e6,
		CostDailyLimitUSD:        float64(t.DailyLimitMicroUSD) / 1e6,
		CostMonthlyUsedUSD:       float64(monthlyUsed) / 1e6,
		CostMonthlyLimitUSD:      float64(t.MonthlyLimitMicroUSD) / 1e6,
		Cost5hUsedUSD:            float64(anchoredWindowUsed(t.Cost5hUsedMicroUSD, t.Cost5hAnchor, cost5hWindow, now)) / 1e6,
		Cost5hLimitUSD:           float64(t.Cost5hLimitMicroUSD) / 1e6,
		CostWeeklyUsedUSD:        float64(anchoredWindowUsed(t.CostWeeklyUsedMicroUSD, t.CostWeeklyPeriodStart, costWeeklyWindow, now)) / 1e6,
		CostWeeklyLimitUSD:       float64(t.CostWeeklyLimitMicroUSD) / 1e6,
		AllowedModels:            t.AllowedModels,
		MaxConcurrency:           t.MaxConcurrency,
		MaxRPM:                   t.MaxRPM,
		Class:                    NormalizeClass(t.Class),
		Anonymous:                t.IsAnonymous(),
	}
}

// 锚定滚动窗口（5h/weekly）的窗口长。
const (
	cost5hWindow     = 5 * time.Hour
	costWeeklyWindow = 7 * 24 * time.Hour
)

// anchoredWindowExpired 报告锚定滚动窗口是否已过期：无锚点（尚未记账）
// 或 now 超过 anchor+window 即过期。
func anchoredWindowExpired(anchor int64, window time.Duration, now time.Time) bool {
	return anchor <= 0 || now.UnixMilli() > anchor+window.Milliseconds()
}

// anchoredWindowUsed 返回锚定滚动窗口的当前用量：过期窗口按 0 计——
// 与日历窗口同为懒惰重置。
func anchoredWindowUsed(used, anchor int64, window time.Duration, now time.Time) int64 {
	if anchoredWindowExpired(anchor, window, now) {
		return 0
	}
	return used
}

// KeyHash 返回该令牌在 logs 表里的 key_hash（全哈希前 16 hex）。
func (t *Token) KeyHash() string {
	if len(t.Hash) < 16 {
		return ""
	}
	return t.Hash[:16]
}

// IsExpired 报告令牌是否已过 expires_at（空/0 永不过期）。
func (t *Token) IsExpired() bool {
	return t.ExpiresAt != nil && *t.ExpiresAt > 0 && time.Now().UnixMilli() > *t.ExpiresAt
}

// IsValid 报告令牌当前可不可用（启用且未过期）。
func (t *Token) IsValid() bool { return t.IsActive && !t.IsExpired() }

// IsModelAllowed 按 allowed_models 白名单判定，空表表示不限制，匹配不分大小写。
func (t *Token) IsModelAllowed(model string) bool {
	if len(t.AllowedModels) == 0 {
		return true
	}
	for _, m := range t.AllowedModels {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	return false
}

// HasCostLimit 报告是否配置了任一费用限额。
func (t *Token) HasCostLimit() bool {
	return t.CostLimitMicroUSD > 0 || t.DailyLimitMicroUSD > 0 || t.MonthlyLimitMicroUSD > 0 ||
		t.Cost5hLimitMicroUSD > 0 || t.CostWeeklyLimitMicroUSD > 0
}

// ValidateUsageLimits 校验限额字段非负；带费用限额的令牌必须同时设
// max_concurrency>0——ccLoad 用这条不变量把「预检-记账」窗口期的超额
// 请求数限制在并发上限内，本服务沿用同一约束。
func (t *Token) ValidateUsageLimits() error {
	if t.CostLimitMicroUSD < 0 || t.DailyLimitMicroUSD < 0 || t.MonthlyLimitMicroUSD < 0 ||
		t.Cost5hLimitMicroUSD < 0 || t.CostWeeklyLimitMicroUSD < 0 {
		return errors.New("cost limits must be >= 0")
	}
	if t.MaxConcurrency < 0 {
		return errors.New("max_concurrency must be >= 0")
	}
	if t.MaxRPM < 0 {
		return errors.New("max_rpm must be >= 0")
	}
	if t.HasCostLimit() && t.MaxConcurrency <= 0 {
		return errors.New("cost-limited auth token requires max_concurrency > 0")
	}
	return nil
}

// CostLimitState 报告当前各窗口的用量/限额与是否超额；window 返回
// 超额的窗口名（5h|daily|weekly|monthly|total），供错误信息区分口径。
func (t *Token) CostLimitState(now time.Time) (used, limit int64, window string, exceeded bool) {
	dayStart, monthStart := periodStarts(now)
	for _, c := range []struct {
		name    string
		used    int64
		limit   int64
		expired bool
	}{
		{"5h", t.Cost5hUsedMicroUSD, t.Cost5hLimitMicroUSD,
			anchoredWindowExpired(t.Cost5hAnchor, cost5hWindow, now)},
		{"daily", t.DailyUsedMicroUSD, t.DailyLimitMicroUSD, t.DailyPeriodStart != dayStart},
		{"weekly", t.CostWeeklyUsedMicroUSD, t.CostWeeklyLimitMicroUSD,
			anchoredWindowExpired(t.CostWeeklyPeriodStart, costWeeklyWindow, now)},
		{"monthly", t.MonthlyUsedMicroUSD, t.MonthlyLimitMicroUSD, t.MonthlyPeriodStart != monthStart},
		{"total", t.CostUsedMicroUSD, t.CostLimitMicroUSD, false},
	} {
		used := c.used
		if c.expired {
			used = 0
		}
		if c.limit > 0 && used >= c.limit {
			return used, c.limit, c.name, true
		}
	}
	return 0, 0, "", false
}

// clone 返回令牌的深拷贝：指针字段复制指向值、AllowedModels 拷底层
// 数组；inflight/rpm* 瞬态字段随结构体值拷贝带过。Store 对外只给
// 快照——仓内对象不出 s.mu，准入热路径与管理面改单互不竞态。
func (t *Token) clone() *Token {
	c := *t
	if t.ExpiresAt != nil {
		v := *t.ExpiresAt
		c.ExpiresAt = &v
	}
	if t.LastUsedAt != nil {
		v := *t.LastUsedAt
		c.LastUsedAt = &v
	}
	c.AllowedModels = slices.Clone(t.AllowedModels)
	return &c
}

func usdToMicro(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	return int64(usd * 1e6)
}

// periodStarts 返回本地日历日与自然月 0 点的 Unix 毫秒。
func periodStarts(now time.Time) (dayStart, monthStart int64) {
	loc := now.Location()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	return day.UnixMilli(), month.UnixMilli()
}

// Result 是一次完成请求回写令牌统计的输入。StatusCode 取最终下发码：
// 499（客户端取消）整次跳过不回写——ccLoad 同口径（取消不进任何计数）。
type Result struct {
	StatusCode       int
	Stream           bool
	FirstByteSec     float64 // 上游首字节秒（无则 0）
	DurationSec      float64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64 // 目录价估算标准成本
}

// rowFromToken 把内存态 Token 投影成持久化行；瞬态字段
// （inflight/rpmBucket/rpmCount）不进列。
func rowFromToken(t *Token) *store.TokenRow {
	return &store.TokenRow{
		ID: t.ID, Token: t.Hash, Description: t.Description,
		CreatedAt: t.CreatedAt.UnixMilli(),
		ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, IsActive: t.IsActive,
		SuccessCount: t.SuccessCount, FailureCount: t.FailureCount,
		StreamAvgTTFB: t.StreamAvgTTFB, NonStreamAvgRT: t.NonStreamAvgRT,
		StreamCount: t.StreamCount, NonStreamCount: t.NonStreamCount,
		PromptTokensTotal: t.PromptTokensTotal, CompletionTokensTotal: t.CompletionTokensTotal,
		CacheReadTokensTotal: t.CacheReadTokensTotal, CacheCreationTokensTotal: t.CacheCreationTokensTotal,
		TotalCostUSD: t.TotalCostUSD, EffectiveCostUSD: t.EffectiveCostUSD,
		CostUsedMicroUSD: t.CostUsedMicroUSD, CostLimitMicroUSD: t.CostLimitMicroUSD,
		DailyUsedMicroUSD: t.DailyUsedMicroUSD, DailyLimitMicroUSD: t.DailyLimitMicroUSD,
		DailyPeriodStart:    t.DailyPeriodStart,
		MonthlyUsedMicroUSD: t.MonthlyUsedMicroUSD, MonthlyLimitMicroUSD: t.MonthlyLimitMicroUSD,
		MonthlyPeriodStart: t.MonthlyPeriodStart,
		Cost5hUsedMicroUSD: t.Cost5hUsedMicroUSD, Cost5hLimitMicroUSD: t.Cost5hLimitMicroUSD,
		Cost5hAnchor:       t.Cost5hAnchor,
		WeeklyUsedMicroUSD: t.CostWeeklyUsedMicroUSD, WeeklyLimitMicroUSD: t.CostWeeklyLimitMicroUSD,
		WeeklyPeriodStart: t.CostWeeklyPeriodStart,
		AllowedModels:     t.AllowedModels,
		MaxConcurrency:    t.MaxConcurrency, MaxRPM: t.MaxRPM,
		Class: NormalizeClass(t.Class),
	}
}

// tokenFromRow 由持久化行重建内存 Token；瞬态字段取零值。
func tokenFromRow(r *store.TokenRow) *Token {
	return &Token{
		ID: r.ID, Hash: r.Token, Description: r.Description,
		CreatedAt: time.UnixMilli(r.CreatedAt),
		ExpiresAt: r.ExpiresAt, LastUsedAt: r.LastUsedAt, IsActive: r.IsActive,
		SuccessCount: r.SuccessCount, FailureCount: r.FailureCount,
		StreamAvgTTFB: r.StreamAvgTTFB, NonStreamAvgRT: r.NonStreamAvgRT,
		StreamCount: r.StreamCount, NonStreamCount: r.NonStreamCount,
		PromptTokensTotal: r.PromptTokensTotal, CompletionTokensTotal: r.CompletionTokensTotal,
		CacheReadTokensTotal: r.CacheReadTokensTotal, CacheCreationTokensTotal: r.CacheCreationTokensTotal,
		TotalCostUSD: r.TotalCostUSD, EffectiveCostUSD: r.EffectiveCostUSD,
		CostUsedMicroUSD: r.CostUsedMicroUSD, CostLimitMicroUSD: r.CostLimitMicroUSD,
		DailyUsedMicroUSD: r.DailyUsedMicroUSD, DailyLimitMicroUSD: r.DailyLimitMicroUSD,
		DailyPeriodStart:    r.DailyPeriodStart,
		MonthlyUsedMicroUSD: r.MonthlyUsedMicroUSD, MonthlyLimitMicroUSD: r.MonthlyLimitMicroUSD,
		MonthlyPeriodStart: r.MonthlyPeriodStart,
		Cost5hUsedMicroUSD: r.Cost5hUsedMicroUSD, Cost5hLimitMicroUSD: r.Cost5hLimitMicroUSD,
		Cost5hAnchor:           r.Cost5hAnchor,
		CostWeeklyUsedMicroUSD: r.WeeklyUsedMicroUSD, CostWeeklyLimitMicroUSD: r.WeeklyLimitMicroUSD,
		CostWeeklyPeriodStart: r.WeeklyPeriodStart,
		AllowedModels:         r.AllowedModels,
		MaxConcurrency:        r.MaxConcurrency, MaxRPM: r.MaxRPM,
		Class: NormalizeClass(r.Class),
	}
}

// writeTask 是排进持久化队列的一条写：run 由单 worker 顺序执行；
// done 非空时提交方同步等结果（管理面写保留「同步返回写库错误」的
// 语义），nil 即火忘（统计回写）。
type writeTask struct {
	run  func(context.Context) error
	done chan error
}

// Store 管理 auth_tokens 表与内存索引。所有变更写穿透到表（单行
// upsert/delete，替代整文件重写），但落库不在 s.mu 内联执行——s.mu
// 同时守护 Resolve/Acquire/AllowRPM 等准入检查，一次 sqlite 抖动会
// 瞬堵所有新请求鉴权。全部写经 writes 队列由单 worker 顺序落盘：
// 入队发生在 s.mu 内，队列序即旧的锁内写序——先排的统计快照不会
// 覆盖后到的管理面写；管理面写的提交方同样在释锁后才等落库结果。
// 统计写是全量行快照，丢弃中间帧由下一帧自愈。
// LastUsedAt 只在内存里更新，随本行下一次写回顺带持久化。
// 返回 *Token 的方法（Resolve/Get/Lookup*/List/Ensure）一律给
// 深拷贝快照：签发后管理员改单不影响在途请求，这就是正确语义。
type Store struct {
	mu     sync.Mutex
	db     *store.Store
	byHash map[string]*Token
	byID   map[int64]*Token
	// byKeyHash 以 logs 表的 key_hash（16 hex 截断）为键，是
	// LookupByKeyHash 的倒排——日志行投影逐行调用，线性扫描是隐性热点。
	byKeyHash map[string]*Token
	// writes 是全部 auth_tokens 写的串行队列；writeDone 在 worker
	// 排空退出时关闭；closed 由 Close 置位，之后的入队被拒绝。
	// droppedPersist 记队列满丢弃的统计写（s.mu 内计数）。
	writes         chan writeTask
	writeDone      chan struct{}
	closed         bool
	droppedPersist int
}

// New 从 auth_tokens 表水合全部行建内存索引；旧 auth_tokens.json 的迁移
// 由 store.ImportLegacy 在建仓前完成，这里不再接触文件。
func New(st *store.Store) (*Store, error) {
	s := &Store{
		db:        st,
		byHash:    map[string]*Token{},
		byID:      map[int64]*Token{},
		byKeyHash: map[string]*Token{},
		writes:    make(chan writeTask, 1024),
		writeDone: make(chan struct{}),
	}
	rows, err := st.ListTokens(context.Background())
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		t := tokenFromRow(r)
		s.byHash[t.Hash] = t
		s.byID[t.ID] = t
		s.byKeyHash[t.KeyHash()] = t
	}
	go s.writeWorker()
	return s, nil
}

// writeWorker 顺序消费 writes 队列直落库；通道关闭后把剩余写排空再退。
// run 只碰 db（快照在入队时已固化），不回头拿 s.mu——与等待 done 的
// 提交方无锁互依。
func (s *Store) writeWorker() {
	defer close(s.writeDone)
	for task := range s.writes {
		err := task.run(context.Background())
		if task.done != nil {
			task.done <- err
		} else if err != nil {
			slog.Warn("persist token write failed", "error", err)
		}
	}
}

// submitSync 把一条管理面写排进队列并返回完成句柄；调用方持 s.mu
// 入队（队列序=内存变更序），释锁后才阻塞读 done 等落库结果——
// 等库不持锁，sqlite 抖动不会冻住准入路径。仓已关闭时返回 nil。
func (s *Store) submitSync(run func(context.Context) error) chan error {
	if s.closed {
		return nil
	}
	done := make(chan error, 1)
	s.writes <- writeTask{run: run, done: done}
	return done
}

// submitStats 排一条统计快照写（火忘）：队列满即丢弃——快照是全量
// 行，下一笔 AddResult 自然补齐，不能让 sqlite 积压回灌准入锁。
// 调用方须持 s.mu。
func (s *Store) submitStats(run func(context.Context) error) {
	if s.closed {
		return
	}
	select {
	case s.writes <- writeTask{run: run}:
	default:
		if s.droppedPersist%100 == 0 {
			slog.Warn("token stats persist queue full, dropping writes", "dropped", s.droppedPersist+1)
		}
		s.droppedPersist++
	}
}

// Close 关闭写队列并等 worker 排空在途与已排队的写——优雅退出不丢
// 统计回写（被 SIGKILL 截断的按计数器口径可丢）。之后到达的管理面写
// 报错、统计写丢弃。
func (s *Store) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.writes)
	}
	s.mu.Unlock()
	<-s.writeDone
}

// HashToken 计算明文令牌的存储哈希（sha256 全 hex）。
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Resolve 按明文解析出有效令牌：哈希命中 + 启用 + 未过期，返回快照。
// 空明文同路径解析——命中哈希为 sha256("") 的匿名通道行即按该令牌
// 准入（仓内无此行时照旧 miss）。命中即刷新 LastUsedAt（写仓内真
// 对象，随本行后续写回固化；返回的快照含新值）。
func (s *Store) Resolve(plain string) (*Token, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byHash[HashToken(plain)]
	if !ok || !t.IsValid() {
		return nil, false
	}
	now := time.Now().UnixMilli()
	if t.LastUsedAt == nil || now > *t.LastUsedAt {
		t.LastUsedAt = &now
	}
	return t.clone(), true
}

// Get 按 ID 取令牌，返回快照。
func (s *Store) Get(id int64) (*Token, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	return t.clone(), true
}

// LookupByKeyHash 按 logs 表的 key_hash（哈希前 16 hex）反查令牌，
// 返回快照，供日志行投影 auth_token_id/description。已删除的令牌
// 查不到，调用方按未知处理。
func (s *Store) LookupByKeyHash(keyHash string) (*Token, bool) {
	if len(keyHash) != 16 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byKeyHash[keyHash]
	if !ok {
		return nil, false
	}
	return t.clone(), true
}

// LookupByKeyHashes 是 LookupByKeyHash 的批量版：一次持锁查多个
// key_hash，命中键映射到令牌快照；未命中或长度非法的键不进图。
// 日志行批量投影用它替代逐行独占 mu。
func (s *Store) LookupByKeyHashes(keyHashes []string) map[string]*Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*Token, len(keyHashes))
	for _, kh := range keyHashes {
		if len(kh) != 16 {
			continue
		}
		if t, ok := s.byKeyHash[kh]; ok {
			out[kh] = t.clone()
		}
	}
	return out
}

// List 返回按 ID 排序的全部令牌快照。
func (s *Store) List() []*Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Token, 0, len(s.byID))
	for _, t := range s.byID {
		out = append(out, t.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Create 生成 64 字符 hex 明文令牌并入库；明文经返回值给出，只此一次。
// id 由 auth_tokens 的自增主键分配——落库拿到 id 才进索引：明文尚未
// 交付调用方，提前可解析没有意义。随机哈希不会与既有行冲突，无需占位。
func (s *Store) Create(t *Token) (plain string, err error) {
	if err := t.ValidateUsageLimits(); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	plain = hex.EncodeToString(raw)
	t.Hash = HashToken(plain)
	t.CreatedAt = time.Now()
	stored := t.clone()
	row := rowFromToken(stored)
	var id int64
	s.mu.Lock()
	done := s.submitSync(func(ctx context.Context) error {
		var err error
		id, err = s.db.InsertToken(ctx, row)
		return err
	})
	s.mu.Unlock()
	if done == nil {
		return "", errors.New("auth token store closed")
	}
	if err := <-done; err != nil {
		return "", err
	}
	t.ID = id
	stored.ID = id
	s.mu.Lock()
	s.byHash[stored.Hash] = stored
	s.byID[stored.ID] = stored
	s.byKeyHash[stored.KeyHash()] = stored
	s.mu.Unlock()
	return plain, nil
}

// Ensure 按明文播种：哈希已存在时原样返回 (existing, false, nil)，
// 否则把 t 入库并返回 (t, true, nil)。幂等——调用方不区分「刚建」
// 与「早就有」。匿名通道用 plain="" 播种。
// 入队的同一临界区占住 byHash/byKeyHash 位防同哈希并发插入；自增 id
// 落库后才进 byID——窗口内 Resolve 命中的快照带 id 0，Acquire/
// AddResult 按「已删」旁路放行，等价于行刚创建尚未可见。
func (s *Store) Ensure(plain string, t *Token) (*Token, bool, error) {
	if err := t.ValidateUsageLimits(); err != nil {
		return nil, false, err
	}
	hash := HashToken(plain)
	s.mu.Lock()
	if existing, ok := s.byHash[hash]; ok {
		c := existing.clone()
		s.mu.Unlock()
		return c, false, nil
	}
	t.Hash = hash
	t.CreatedAt = time.Now()
	stored := t.clone()
	row := rowFromToken(stored)
	var id int64
	done := s.submitSync(func(ctx context.Context) error {
		var err error
		id, err = s.db.InsertToken(ctx, row)
		return err
	})
	if done == nil {
		s.mu.Unlock()
		return nil, false, errors.New("auth token store closed")
	}
	s.byHash[hash] = stored
	s.byKeyHash[stored.KeyHash()] = stored
	s.mu.Unlock()
	if err := <-done; err != nil {
		s.mu.Lock()
		delete(s.byHash, hash)
		delete(s.byKeyHash, stored.KeyHash())
		s.mu.Unlock()
		return nil, false, err
	}
	s.mu.Lock()
	stored.ID = id
	s.byID[id] = stored
	s.mu.Unlock()
	t.ID = id
	return t, true, nil
}

// Update 覆盖写一条令牌：调用方先 Get 拿快照、改副本、再回传。
// 锁内先校验传入副本，失败则内存态原样返回错误；通过后原子换索引
// （inflight/rpm 瞬态计数从旧对象继承）并入队落库，释锁再等写库
// 结果——等库不持锁。落库失败时内存已换，与改前同口径：内存为准，
// 行随该令牌下一次写回收敛。
func (s *Store) Update(t *Token) error {
	s.mu.Lock()
	if err := t.ValidateUsageLimits(); err != nil {
		s.mu.Unlock()
		return err
	}
	old, ok := s.byID[t.ID]
	if !ok {
		s.mu.Unlock()
		return errors.New("token not found")
	}
	stored := t.clone()
	stored.inflight = old.inflight
	stored.rpmBucket = old.rpmBucket
	stored.rpmCount = old.rpmCount
	delete(s.byHash, old.Hash)
	delete(s.byKeyHash, old.KeyHash())
	s.byHash[stored.Hash] = stored
	s.byID[stored.ID] = stored
	s.byKeyHash[stored.KeyHash()] = stored
	row := rowFromToken(stored)
	done := s.submitSync(func(ctx context.Context) error {
		return s.db.UpsertToken(ctx, row)
	})
	s.mu.Unlock()
	if done == nil {
		return errors.New("auth token store closed")
	}
	return <-done
}

// Delete 移除令牌；不存在时按成功处理（幂等删除）。
// 索引摘除与入队在同一临界区，释锁后才等落库结果。
func (s *Store) Delete(id int64) error {
	s.mu.Lock()
	t, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.byHash, t.Hash)
	delete(s.byKeyHash, t.KeyHash())
	delete(s.byID, id)
	done := s.submitSync(func(ctx context.Context) error {
		return s.db.DeleteToken(ctx, id)
	})
	s.mu.Unlock()
	if done == nil {
		return nil // 仓已关：行已出内存索引，幂等删除语义不变
	}
	return <-done
}

// Acquire 占用一个令牌并发槽；到顶返回 (active, limit, false)。
// 成功时调用方必须在请求结束时配对 Release。
func (s *Store) Acquire(id int64) (active, limit int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, exists := s.byID[id]
	if !exists {
		return 0, 0, true // 令牌在准入后被删：不占槽放行，AddResult 按 ID 找不到即弃
	}
	if t.MaxConcurrency > 0 && t.inflight >= int64(t.MaxConcurrency) {
		return t.inflight, int64(t.MaxConcurrency), false
	}
	t.inflight++
	return t.inflight, int64(t.MaxConcurrency), true
}

// Release 归还并发槽。
func (s *Store) Release(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.byID[id]; ok && t.inflight > 0 {
		t.inflight--
	}
}

// AllowRPM 按令牌 MaxRPM 计数一次请求：固定分钟桶（unix 秒/60）内
// 已计数达到上限返回 (used, limit, false)。桶随分钟翻页自然归零，
// 进程重启整体归零（计数不持久化）。令牌被删或 MaxRPM<=0 放行。
func (s *Store) AllowRPM(id int64) (used, limit int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, exists := s.byID[id]
	if !exists || t.MaxRPM <= 0 {
		return 0, 0, true
	}
	bucket := time.Now().Unix() / 60
	if bucket != t.rpmBucket {
		t.rpmBucket = bucket
		t.rpmCount = 0
	}
	if t.rpmCount >= int64(t.MaxRPM) {
		return t.rpmCount, int64(t.MaxRPM), false
	}
	t.rpmCount++
	return t.rpmCount, int64(t.MaxRPM), true
}

// AddResult 回写一次完成请求的统计与费用窗口，并把该行写回表——与文件
// 时代同为每请求持久化；写库失败不阻塞请求收尾（内存态仍在，行写是
// 全量快照，下次成功写自动收敛）。口径对齐 ccLoad updateTokenStats：
// 499 整次跳过；token/费用只在 2xx 时累加；TTFB/RT 均值与流式计数对
// 全部非 499 行更新（失败流也计入均值样本）。
func (s *Store) AddResult(id int64, r Result) {
	if r.StatusCode == 499 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return
	}
	now := time.Now().UnixMilli()
	t.LastUsedAt = &now
	if r.Stream {
		t.StreamCount++
		t.StreamAvgTTFB += (r.FirstByteSec - t.StreamAvgTTFB) / float64(t.StreamCount)
	} else {
		t.NonStreamCount++
		t.NonStreamAvgRT += (r.DurationSec - t.NonStreamAvgRT) / float64(t.NonStreamCount)
	}
	if r.StatusCode >= 200 && r.StatusCode < 300 {
		t.SuccessCount++
		t.PromptTokensTotal += r.InputTokens
		t.CompletionTokensTotal += r.OutputTokens
		t.CacheReadTokensTotal += r.CacheReadTokens
		t.CacheCreationTokensTotal += r.CacheWriteTokens
		micro := usdToMicro(r.CostUSD)
		t.TotalCostUSD += r.CostUSD
		t.EffectiveCostUSD += r.CostUSD // 本服务无渠道倍率，effective=total
		t.CostUsedMicroUSD += micro
		dayStart, monthStart := periodStarts(time.Now())
		if t.DailyPeriodStart != dayStart {
			t.DailyPeriodStart = dayStart
			t.DailyUsedMicroUSD = 0
		}
		t.DailyUsedMicroUSD += micro
		if t.MonthlyPeriodStart != monthStart {
			t.MonthlyPeriodStart = monthStart
			t.MonthlyUsedMicroUSD = 0
		}
		t.MonthlyUsedMicroUSD += micro
		if anchoredWindowExpired(t.Cost5hAnchor, cost5hWindow, time.Now()) {
			t.Cost5hAnchor = now
			t.Cost5hUsedMicroUSD = 0
		}
		t.Cost5hUsedMicroUSD += micro
		if anchoredWindowExpired(t.CostWeeklyPeriodStart, costWeeklyWindow, time.Now()) {
			t.CostWeeklyPeriodStart = now
			t.CostWeeklyUsedMicroUSD = 0
		}
		t.CostWeeklyUsedMicroUSD += micro
	} else {
		t.FailureCount++
	}
	row := rowFromToken(t)
	s.submitStats(func(ctx context.Context) error {
		return s.db.UpsertToken(ctx, row)
	})
}

// Empty 报告仓内是否一个令牌都没有；开放模式判定用——仓空时 /v1
// 不校验凭据，一旦有任一行（含匿名通道）即转为要求凭据。
func (s *Store) Empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID) == 0
}

// CostLimitState 透传令牌的限额检查（含周期校正）。
func (s *Store) CostLimitState(id int64) (used, limit int64, window string, exceeded bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return 0, 0, "", false
	}
	return t.CostLimitState(time.Now())
}
