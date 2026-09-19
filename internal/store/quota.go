package store

import (
	"context"
)

// QuotaSample 是 quota_samples 一行的领域形状，也是 /admin/quota
// 响应 points 的线格式（与被取代的 quota.jsonl 行格式一致）；
// Daily/WeeklyRemaining 指针语义保留——NULL/nil 表示「上游没报」，
// 0 表示「真到 0」，故该两字段不带 omitempty。
type QuotaSample struct {
	At                        int64    `json:"at"` // unix 秒
	Account                   string   `json:"account,omitempty"`
	DailyRemaining            *float64 `json:"daily_remaining"`
	WeeklyRemaining           *float64 `json:"weekly_remaining"`
	DailyResetAt              int64    `json:"daily_reset_at,omitempty"`
	WeeklyResetAt             int64    `json:"weekly_reset_at,omitempty"`
	PromptCredits             float64  `json:"prompt_credits,omitempty"`
	FlowCredits               float64  `json:"flow_credits,omitempty"`
	FlexCredits               float64  `json:"flex_credits,omitempty"`
	ACUConsumed               float64  `json:"acu_consumed,omitempty"`
	ACULimit                  float64  `json:"acu_limit,omitempty"`
	UsedPromptCredits         float64  `json:"used_prompt_credits,omitempty"`
	UsedFlowCredits           float64  `json:"used_flow_credits,omitempty"`
	UsedFlexCredits           float64  `json:"used_flex_credits,omitempty"`
	GracePeriodStatus         string   `json:"grace_period_status,omitempty"`
	GracePeriodEnd            int64    `json:"grace_period_end,omitempty"`
	WasReducedByOrphanedUsage bool     `json:"was_reduced_by_orphaned_usage,omitempty"`
	TopUpEnabled              bool     `json:"top_up_enabled,omitempty"`
	TopUpTransactionStatus    string   `json:"top_up_transaction_status,omitempty"`
	OverageBalanceMicros      int64    `json:"overage_balance_micros,omitempty"`
}

// InsertQuotaSample 追加一条配额快照。(account,at) 有唯一索引——
// 同秒撞车（导入与实时采样并行、间隔配成 <1s）时丢新点不报错，
// 与导入路径的 OR IGNORE 口径一致。
func (s *Store) InsertQuotaSample(ctx context.Context, q *QuotaSample) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO quota_samples(
		at, account, daily_remaining, weekly_remaining, daily_reset_at, weekly_reset_at,
		prompt_credits, flow_credits, flex_credits, acu_consumed, acu_limit,
		used_prompt_credits, used_flow_credits, used_flex_credits,
		grace_period_status, grace_period_end, was_reduced_by_orphaned_usage,
		top_up_enabled, top_up_transaction_status, overage_balance_micros
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		q.At, q.Account, q.DailyRemaining, q.WeeklyRemaining, q.DailyResetAt, q.WeeklyResetAt,
		q.PromptCredits, q.FlowCredits, q.FlexCredits, q.ACUConsumed, q.ACULimit,
		q.UsedPromptCredits, q.UsedFlowCredits, q.UsedFlexCredits,
		q.GracePeriodStatus, q.GracePeriodEnd, q.WasReducedByOrphanedUsage,
		q.TopUpEnabled, q.TopUpTransactionStatus, q.OverageBalanceMicros)
	return err
}

// ListQuotaSamples 按时间升序返回某账号 since 之后的快照，至多 limit
// 条（取最新者——SQL 倒序截断后 Go 侧反转回升序）；limit<=0 不限。
// account 为空串时返回全部账号（兼容单号时代无 account 字段的行）。
func (s *Store) ListQuotaSamples(ctx context.Context, account string, since int64, limit int) ([]*QuotaSample, error) {
	if limit <= 0 {
		limit = -1
	}
	query := `SELECT at, account, daily_remaining, weekly_remaining, daily_reset_at, weekly_reset_at,
		prompt_credits, flow_credits, flex_credits, acu_consumed, acu_limit,
		used_prompt_credits, used_flow_credits, used_flex_credits,
		grace_period_status, grace_period_end, was_reduced_by_orphaned_usage,
		top_up_enabled, top_up_transaction_status, overage_balance_micros
		FROM quota_samples WHERE at>=?`
	args := []any{since}
	if account != "" {
		query += ` AND account=?`
		args = append(args, account)
	}
	query += ` ORDER BY at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*QuotaSample
	for rows.Next() {
		var q QuotaSample
		if err := rows.Scan(&q.At, &q.Account, &q.DailyRemaining, &q.WeeklyRemaining,
			&q.DailyResetAt, &q.WeeklyResetAt,
			&q.PromptCredits, &q.FlowCredits, &q.FlexCredits, &q.ACUConsumed, &q.ACULimit,
			&q.UsedPromptCredits, &q.UsedFlowCredits, &q.UsedFlexCredits,
			&q.GracePeriodStatus, &q.GracePeriodEnd, &q.WasReducedByOrphanedUsage,
			&q.TopUpEnabled, &q.TopUpTransactionStatus, &q.OverageBalanceMicros); err != nil {
			return nil, err
		}
		out = append(out, &q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// quotaSampleKeep 是 quota_samples 的全局行数帽——对齐文件时代
// ~4MB quota.jsonl ≈ 2 万行的保留边界。
const quotaSampleKeep = 20000

// PruneQuotaSamples 把 quota_samples 截到最新 quotaSampleKeep 行
// （同秒并列按 id 取新者，帽是精确界）。返回删除行数。保留集谓词
// 对分片删除稳定：被删的都是集外旧行，帽内成员资格不随分片漂移。
func (s *Store) PruneQuotaSamples(ctx context.Context) (int64, error) {
	return s.deleteRowsChunked(ctx, "quota_samples",
		`id NOT IN (SELECT id FROM quota_samples ORDER BY at DESC, id DESC LIMIT ?)`,
		quotaSampleKeep)
}
