package store

import (
	"context"
)

// QuotaSample 是 quota_samples 一行的领域形状，字段镜像
// ccpanel.quotaPoint；Daily/WeeklyRemaining 指针语义保留——
// NULL 表示「上游没报」，0 表示「真到 0」。
type QuotaSample struct {
	At                        int64 // unix 秒
	Account                   string
	DailyRemaining            *float64
	WeeklyRemaining           *float64
	DailyResetAt              int64
	WeeklyResetAt             int64
	PromptCredits             float64
	FlowCredits               float64
	FlexCredits               float64
	ACUConsumed               float64
	ACULimit                  float64
	UsedPromptCredits         float64
	UsedFlowCredits           float64
	UsedFlexCredits           float64
	GracePeriodStatus         string
	GracePeriodEnd            int64
	WasReducedByOrphanedUsage bool
	TopUpEnabled              bool
	TopUpTransactionStatus    string
}

// InsertQuotaSample 追加一条配额快照。
func (s *Store) InsertQuotaSample(ctx context.Context, q *QuotaSample) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO quota_samples(
		at, account, daily_remaining, weekly_remaining, daily_reset_at, weekly_reset_at,
		prompt_credits, flow_credits, flex_credits, acu_consumed, acu_limit,
		used_prompt_credits, used_flow_credits, used_flex_credits,
		grace_period_status, grace_period_end, was_reduced_by_orphaned_usage,
		top_up_enabled, top_up_transaction_status
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		q.At, q.Account, q.DailyRemaining, q.WeeklyRemaining, q.DailyResetAt, q.WeeklyResetAt,
		q.PromptCredits, q.FlowCredits, q.FlexCredits, q.ACUConsumed, q.ACULimit,
		q.UsedPromptCredits, q.UsedFlowCredits, q.UsedFlexCredits,
		q.GracePeriodStatus, q.GracePeriodEnd, q.WasReducedByOrphanedUsage,
		q.TopUpEnabled, q.TopUpTransactionStatus)
	return err
}

// ListQuotaSamples 按时间升序返回某账号 since 之后的快照；
// account 为空串时返回全部账号（兼容单号时代无 account 字段的行）。
func (s *Store) ListQuotaSamples(ctx context.Context, account string, since int64) ([]*QuotaSample, error) {
	query := `SELECT at, account, daily_remaining, weekly_remaining, daily_reset_at, weekly_reset_at,
		prompt_credits, flow_credits, flex_credits, acu_consumed, acu_limit,
		used_prompt_credits, used_flow_credits, used_flex_credits,
		grace_period_status, grace_period_end, was_reduced_by_orphaned_usage,
		top_up_enabled, top_up_transaction_status
		FROM quota_samples WHERE at>=?`
	args := []any{since}
	if account != "" {
		query += ` AND account=?`
		args = append(args, account)
	}
	query += ` ORDER BY at`
	rows, err := s.db.QueryContext(ctx, query, args...)
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
			&q.TopUpEnabled, &q.TopUpTransactionStatus); err != nil {
			return nil, err
		}
		out = append(out, &q)
	}
	return out, rows.Err()
}
