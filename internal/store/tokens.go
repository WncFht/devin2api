package store

import (
	"context"
	"encoding/json"
	"strings"
)

// TokenRow 是 auth_tokens 表一行的领域形状，字段镜像
// authtoken.Token 的持久化字段；瞬态字段（inflight/rpm*）不在此。
// 时间一律 unix 毫秒；allowed_models 序列化为 JSON 数组文本。
type TokenRow struct {
	ID                       int64
	Token                    string
	Description              string
	CreatedAt                int64
	ExpiresAt                *int64
	LastUsedAt               *int64
	IsActive                 bool
	SuccessCount             int64
	FailureCount             int64
	StreamAvgTTFB            float64
	NonStreamAvgRT           float64
	StreamCount              int64
	NonStreamCount           int64
	PromptTokensTotal        int64
	CompletionTokensTotal    int64
	CacheReadTokensTotal     int64
	CacheCreationTokensTotal int64
	TotalCostUSD             float64
	EffectiveCostUSD         float64
	CostUsedMicroUSD         int64
	CostLimitMicroUSD        int64
	DailyUsedMicroUSD        int64
	DailyLimitMicroUSD       int64
	DailyPeriodStart         int64
	MonthlyUsedMicroUSD      int64
	MonthlyLimitMicroUSD     int64
	MonthlyPeriodStart       int64
	Cost5hUsedMicroUSD       int64
	Cost5hLimitMicroUSD      int64
	Cost5hAnchor             int64
	WeeklyUsedMicroUSD       int64
	WeeklyLimitMicroUSD      int64
	WeeklyPeriodStart        int64
	AllowedModels            []string
	MaxConcurrency           int
	MaxRPM                   int
}

// tokenColumnList 是 auth_tokens 的全部列（含 id，37 列），
// INSERT/SELECT 共用同一份列清单，占位符数量由它派生。
var tokenColumnList = []string{
	"id", "token", "description", "created_at", "expires_at", "last_used_at", "is_active",
	"success_count", "failure_count", "stream_avg_ttfb", "non_stream_avg_rt", "stream_count", "non_stream_count",
	"prompt_tokens_total", "completion_tokens_total", "cache_read_tokens_total", "cache_creation_tokens_total",
	"total_cost_usd", "effective_cost_usd",
	"cost_used_microusd", "cost_limit_microusd",
	"cost_daily_used_microusd", "cost_daily_limit_microusd", "cost_daily_period_start",
	"cost_monthly_used_microusd", "cost_monthly_limit_microusd", "cost_monthly_period_start",
	"cost_5h_used_microusd", "cost_5h_limit_microusd", "cost_5h_anchor",
	"cost_weekly_used_microusd", "cost_weekly_limit_microusd", "cost_weekly_period_start",
	"allowed_models", "max_concurrency", "max_rpm",
}

var (
	tokenColumns   = strings.Join(tokenColumnList, ", ")
	tokenInsertAll = `INSERT OR REPLACE INTO auth_tokens(` + tokenColumns + `) VALUES(` +
		placeholders(len(tokenColumnList)) + `)`
	tokenInsertAuto = `INSERT INTO auth_tokens(` + strings.Join(tokenColumnList[1:], ", ") + `) VALUES(` +
		placeholders(len(tokenColumnList)-1) + `)`
)

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func tokenArgs(t *TokenRow, allowed string) []any {
	return []any{
		t.ID, t.Token, t.Description, t.CreatedAt, t.ExpiresAt, t.LastUsedAt, t.IsActive,
		t.SuccessCount, t.FailureCount, t.StreamAvgTTFB, t.NonStreamAvgRT, t.StreamCount, t.NonStreamCount,
		t.PromptTokensTotal, t.CompletionTokensTotal, t.CacheReadTokensTotal, t.CacheCreationTokensTotal,
		t.TotalCostUSD, t.EffectiveCostUSD,
		t.CostUsedMicroUSD, t.CostLimitMicroUSD,
		t.DailyUsedMicroUSD, t.DailyLimitMicroUSD, t.DailyPeriodStart,
		t.MonthlyUsedMicroUSD, t.MonthlyLimitMicroUSD, t.MonthlyPeriodStart,
		t.Cost5hUsedMicroUSD, t.Cost5hLimitMicroUSD, t.Cost5hAnchor,
		t.WeeklyUsedMicroUSD, t.WeeklyLimitMicroUSD, t.WeeklyPeriodStart,
		allowed, t.MaxConcurrency, t.MaxRPM,
	}
}

func scanToken(row interface{ Scan(...any) error }) (*TokenRow, error) {
	var t TokenRow
	var allowed string
	err := row.Scan(&t.ID, &t.Token, &t.Description, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.IsActive,
		&t.SuccessCount, &t.FailureCount, &t.StreamAvgTTFB, &t.NonStreamAvgRT, &t.StreamCount, &t.NonStreamCount,
		&t.PromptTokensTotal, &t.CompletionTokensTotal, &t.CacheReadTokensTotal, &t.CacheCreationTokensTotal,
		&t.TotalCostUSD, &t.EffectiveCostUSD,
		&t.CostUsedMicroUSD, &t.CostLimitMicroUSD,
		&t.DailyUsedMicroUSD, &t.DailyLimitMicroUSD, &t.DailyPeriodStart,
		&t.MonthlyUsedMicroUSD, &t.MonthlyLimitMicroUSD, &t.MonthlyPeriodStart,
		&t.Cost5hUsedMicroUSD, &t.Cost5hLimitMicroUSD, &t.Cost5hAnchor,
		&t.WeeklyUsedMicroUSD, &t.WeeklyLimitMicroUSD, &t.WeeklyPeriodStart,
		&allowed, &t.MaxConcurrency, &t.MaxRPM)
	if err != nil {
		return nil, err
	}
	if allowed != "" && allowed != "[]" {
		if err := json.Unmarshal([]byte(allowed), &t.AllowedModels); err != nil {
			return nil, err
		}
	}
	return &t, nil
}

// InsertToken 插入令牌行并返回自增 id；调用方持零 ID 传入。
func (s *Store) InsertToken(ctx context.Context, t *TokenRow) (int64, error) {
	allowed, err := json.Marshal(t.AllowedModels)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, tokenInsertAuto, tokenArgs(t, string(allowed))[1:]...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpsertToken 按 id 覆盖整行——Token 的窗口折叠等域逻辑都在
// 调用方内存里完成，本方法只是持久化快照（替代整文件重写）。
func (s *Store) UpsertToken(ctx context.Context, t *TokenRow) error {
	allowed, err := json.Marshal(t.AllowedModels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, tokenInsertAll, tokenArgs(t, string(allowed))...)
	return err
}

// ListTokens 返回全部令牌行（含停用），按 id 升序——与旧文件
// 的排序语义一致。
func (s *Store) ListTokens(ctx context.Context) ([]*TokenRow, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT `+tokenColumns+` FROM auth_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*TokenRow
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteToken 按 id 删除令牌行。
func (s *Store) DeleteToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_tokens WHERE id=?`, id)
	return err
}
