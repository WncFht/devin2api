// Package authtoken 实现下游 API 令牌仓：auth_tokens.json 持久化、
// /v1 准入用的哈希解析、并发槽与费用限额计数。契约对齐 ccLoad 的
// AuthToken：JSON 形状逐字段一致，明文令牌只在创建时返回一次，
// 存库与列表输出均为 SHA-256 全哈希（hex 64）。
//
// 与 ccLoad 的一处有意偏差：不支持「直接出示哈希」双路径——哈希在
// 面板列表里可见，若接受哈希当凭据，看过面板的人就拿到可用令牌。
// 索引关联用截断哈希：index.jsonl 的 key_hash = sha256(明文)[:8字节]
// 即 Token.Hash 的前 16 个 hex 字符。
package authtoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
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

	AllowedModels []string `json:"allowed_models,omitempty"`
	// 渠道限制字段只为契约兼容而存：本服务只有一条合成上游（id=1），
	// allow 列表不含 1 或 deny 列表含 1 时该令牌全部请求被拒。
	AllowedChannelIDs      []int64 `json:"allowed_channel_ids,omitempty"`
	ChannelRestrictionMode string  `json:"channel_restriction_mode,omitempty"`
	MaxConcurrency         int     `json:"max_concurrency"`

	inflight int64 // 在途并发计数，不序列化
}

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
	PeakRPM                  float64   `json:"peak_rpm,omitempty"`
	AvgRPM                   float64   `json:"avg_rpm,omitempty"`
	RecentRPM                float64   `json:"recent_rpm,omitempty"`
	AllowedModels            []string  `json:"allowed_models,omitempty"`
	AllowedChannelIDs        []int64   `json:"allowed_channel_ids,omitempty"`
	ChannelRestrictionMode   string    `json:"channel_restriction_mode,omitempty"`
	MaxConcurrency           int       `json:"max_concurrency"`
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
	mode := t.ChannelRestrictionMode
	if mode == "" {
		mode = "allow"
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
		AllowedModels:            t.AllowedModels,
		AllowedChannelIDs:        t.AllowedChannelIDs,
		ChannelRestrictionMode:   mode,
		MaxConcurrency:           t.MaxConcurrency,
	}
}

// KeyHash 返回该令牌在 index.jsonl 里的 key_hash（全哈希前 16 hex）。
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

// channelAllowed 执行渠道限制：合成渠道恒为 id=1，allow 空表不限制。
func (t *Token) channelAllowed() bool {
	if len(t.AllowedChannelIDs) == 0 {
		return true
	}
	listed := false
	for _, id := range t.AllowedChannelIDs {
		if id == 1 {
			listed = true
			break
		}
	}
	return listed != (t.ChannelRestrictionMode == "deny")
}

// HasCostLimit 报告是否配置了任一费用限额。
func (t *Token) HasCostLimit() bool {
	return t.CostLimitMicroUSD > 0 || t.DailyLimitMicroUSD > 0 || t.MonthlyLimitMicroUSD > 0
}

// ValidateUsageLimits 校验限额字段非负；带费用限额的令牌必须同时设
// max_concurrency>0——ccLoad 用这条不变量把「预检-记账」窗口期的超额
// 请求数限制在并发上限内，本服务沿用同一约束。
func (t *Token) ValidateUsageLimits() error {
	if t.CostLimitMicroUSD < 0 || t.DailyLimitMicroUSD < 0 || t.MonthlyLimitMicroUSD < 0 {
		return errors.New("cost limits must be >= 0")
	}
	if t.MaxConcurrency < 0 {
		return errors.New("max_concurrency must be >= 0")
	}
	if t.HasCostLimit() && t.MaxConcurrency <= 0 {
		return errors.New("cost-limited auth token requires max_concurrency > 0")
	}
	return nil
}

// CostLimitState 报告当前各窗口的用量/限额与是否超额；window 返回
// 超额的窗口名（daily|monthly|total），供错误信息区分口径。
func (t *Token) CostLimitState(now time.Time) (used, limit int64, window string, exceeded bool) {
	dayStart, monthStart := periodStarts(now)
	for _, c := range []struct {
		name   string
		used   int64
		limit  int64
		start  int64
		period int64
	}{
		{"daily", t.DailyUsedMicroUSD, t.DailyLimitMicroUSD, t.DailyPeriodStart, dayStart},
		{"monthly", t.MonthlyUsedMicroUSD, t.MonthlyLimitMicroUSD, t.MonthlyPeriodStart, monthStart},
		{"total", t.CostUsedMicroUSD, t.CostLimitMicroUSD, 0, 0},
	} {
		used := c.used
		if c.period != 0 && c.start != c.period {
			used = 0
		}
		if c.limit > 0 && used >= c.limit {
			return used, c.limit, c.name, true
		}
	}
	return 0, 0, "", false
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

// tokenFile 是 auth_tokens.json 的持久化形状；Token 的 micro 字段直接
// 序列化进文件（精确值），API 输出走 API() 折算。
type tokenFile struct {
	NextID int64    `json:"next_id"`
	Tokens []*Token `json:"tokens"`
}

// Store 管理 auth_tokens.json 与内存索引。所有变更写穿透落盘
// （文件 KB 级，请求完成频率低）；LastUsedAt 只在内存里更新，
// 随下一次落盘顺带持久化。
type Store struct {
	mu     sync.Mutex
	path   string
	nextID int64
	byHash map[string]*Token
	byID   map[int64]*Token
}

// New 加载 stateDir/auth_tokens.json；文件缺失以空仓起步，损坏时把
// 原文件改名留档后空仓起步（不静默吞掉坏数据）。
func New(stateDir string) (*Store, error) {
	s := &Store{
		path:   filepath.Join(stateDir, "auth_tokens.json"),
		nextID: 1,
		byHash: map[string]*Token{},
		byID:   map[int64]*Token{},
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f tokenFile
	if err := json.Unmarshal(data, &f); err != nil {
		_ = os.Rename(s.path, s.path+".corrupt")
		return s, nil
	}
	for _, t := range f.Tokens {
		if t == nil || t.Hash == "" {
			continue
		}
		s.byHash[t.Hash] = t
		s.byID[t.ID] = t
	}
	if f.NextID > 0 {
		s.nextID = f.NextID
	} else {
		for id := range s.byID {
			if id >= s.nextID {
				s.nextID = id + 1
			}
		}
	}
	return s, nil
}

// HashToken 计算明文令牌的存储哈希（sha256 全 hex）。
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Resolve 按明文解析出有效令牌：哈希命中 + 启用 + 未过期 + 渠道策略放行。
// 命中即刷新 LastUsedAt（内存态，随后续落盘固化）。
func (s *Store) Resolve(plain string) (*Token, bool) {
	if plain == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byHash[HashToken(plain)]
	if !ok || !t.IsValid() || !t.channelAllowed() {
		return nil, false
	}
	now := time.Now().UnixMilli()
	if t.LastUsedAt == nil || now > *t.LastUsedAt {
		t.LastUsedAt = &now
	}
	return t, true
}

// Get 按 ID 取令牌。
func (s *Store) Get(id int64) (*Token, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	return t, ok
}

// LookupByKeyHash 按 index.jsonl 的 key_hash（哈希前 16 hex）反查令牌，
// 供日志行投影 auth_token_id/description。已删除的令牌查不到，调用方
// 按未知处理。
func (s *Store) LookupByKeyHash(keyHash string) (*Token, bool) {
	if len(keyHash) != 16 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.byHash {
		if t.KeyHash() == keyHash {
			return t, true
		}
	}
	return nil, false
}

// List 返回按 ID 排序的全部令牌快照。
func (s *Store) List() []*Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Token, 0, len(s.byID))
	for _, t := range s.byID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Create 生成 64 字符 hex 明文令牌并入库；明文经返回值给出，只此一次。
func (s *Store) Create(t *Token) (plain string, err error) {
	if err := t.ValidateUsageLimits(); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	plain = hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	t.ID = s.nextID
	s.nextID++
	t.Hash = HashToken(plain)
	t.CreatedAt = time.Now()
	s.byHash[t.Hash] = t
	s.byID[t.ID] = t
	return plain, s.saveLocked()
}

// Update 覆盖写一条令牌（调用方先 Get 再改字段）。
func (s *Store) Update(t *Token) error {
	if err := t.ValidateUsageLimits(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[t.ID]; !ok {
		return errors.New("token not found")
	}
	delete(s.byHash, s.byID[t.ID].Hash)
	s.byHash[t.Hash] = t
	s.byID[t.ID] = t
	return s.saveLocked()
}

// Delete 移除令牌；不存在时按成功处理（幂等删除）。
func (s *Store) Delete(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return nil
	}
	delete(s.byHash, t.Hash)
	delete(s.byID, id)
	return s.saveLocked()
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

// AddResult 回写一次完成请求的统计与费用窗口，并落盘；落盘失败不阻塞
// 请求收尾（内存态仍在，下次写带全量）。口径对齐 ccLoad updateTokenStats：
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
	} else {
		t.FailureCount++
	}
	_ = s.saveLocked()
}

// Empty 报告仓内是否一个令牌都没有；开放模式判定用——未设 master key
// 且无令牌时 /v1 不校验凭据，一旦配了令牌即转为要求凭据。
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

// saveLocked 原子落盘（tmp+rename）；调用方必须持锁。
func (s *Store) saveLocked() error {
	f := tokenFile{NextID: s.nextID, Tokens: make([]*Token, 0, len(s.byID))}
	for _, t := range s.byID {
		f.Tokens = append(f.Tokens, t)
	}
	sort.Slice(f.Tokens, func(i, j int) bool { return f.Tokens[i].ID < f.Tokens[j].ID })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
