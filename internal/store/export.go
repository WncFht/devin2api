// 本文件是 ImportLegacy 的逆方向：把库内状态写回文件时代布局
// （logs/index.jsonl、auth_tokens.json、models.json、
// panel-settings.json、quota.jsonl、gate-state*.json）。
//
// 服务的两类逃生场景：(a) 回滚文件版二进制且要保住 sqlite 窗口期
// 写入——auth_tokens 的面板令牌变更是唯一真权限损失；(b) DB 损坏
// 或误删时的取证重建。入口是主二进制的 -export-legacy flag：服务
// 起不来恰是最需要导出的场景，admin 端点那时不可用。
package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExportReport 汇总一次 ExportLegacy 的落盘结果。
type ExportReport struct {
	// Written 是实际落盘的文件路径——目标被现役文件占用时改道
	// <name>.exported，此处反映真实路径。
	Written []string
	// Notices 是需要操作员知晓的旁注（目标被占改道 .exported 等）。
	Notices []string
	// DebugRows 是 debug_files+debug_chunks 的残留行数。调试目录的
	// 文件还原不在本工具范围；>0 时调用方应提示「回滚后请求详情页
	// 只剩 DB 副本」这一取证缺口。
	DebugRows int
}

// ExportLegacy 把各表逐源写回文件时代格式。逐源独立——单源失败不
// 阻断其余，全部错误经 errors.Join 汇总返回（调用方据此定退出码）。
// stateDir/logRoot 与 ImportLegacy 同义：整文件 JSON 落 stateDir，
// jsonl 与 gate-state 落 logRoot（缺失时由调用方建好）。
func (s *Store) ExportLegacy(ctx context.Context, stateDir, logRoot string) (*ExportReport, error) {
	rep := &ExportReport{}
	var errs []error
	// run 执行单文件源：fn 返回实际路径与改道旁注，错误记名汇总。
	run := func(name string, fn func(context.Context) (string, string, error)) {
		actual, notice, err := fn(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("export %s: %w", name, err))
			return
		}
		rep.Written = append(rep.Written, actual)
		if notice != "" {
			rep.Notices = append(rep.Notices, notice)
		}
	}
	run("index", func(ctx context.Context) (string, string, error) {
		return s.exportIndex(ctx, filepath.Join(logRoot, "index.jsonl"))
	})
	run("auth_tokens", func(ctx context.Context) (string, string, error) {
		return s.exportTokens(ctx, filepath.Join(stateDir, "auth_tokens.json"))
	})
	run("models", func(ctx context.Context) (string, string, error) {
		return s.exportModels(ctx, filepath.Join(stateDir, "models.json"))
	})
	run("panel_settings", func(ctx context.Context) (string, string, error) {
		return s.exportSettings(ctx, filepath.Join(stateDir, "panel-settings.json"))
	})
	run("quota", func(ctx context.Context) (string, string, error) {
		return s.exportQuota(ctx, filepath.Join(logRoot, "quota.jsonl"))
	})
	// gate-state 是多文件源（逐 lane 一文件），自收自支。
	if paths, notices, err := s.exportGateStates(ctx, logRoot); err != nil {
		errs = append(errs, fmt.Errorf("export gate_state: %w", err))
	} else {
		rep.Written = append(rep.Written, paths...)
		rep.Notices = append(rep.Notices, notices...)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM debug_files)+(SELECT COUNT(*) FROM debug_chunks)`).
		Scan(&rep.DebugRows); err != nil {
		errs = append(errs, fmt.Errorf("export debug probe: %w", err))
	}
	return rep, errors.Join(errs...)
}

// exportIndex 把 logs 表按时间序写成 index.jsonl。导出列即读路径
// logColumns——id/log_source/upstream_protocol/minute_bucket 是库内
// 派生列，不进文件格式（重导入时照常再生）。
func (s *Store) exportIndex(ctx context.Context, target string) (string, string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+logColumns+` FROM logs ORDER BY time, id`)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = rows.Close() }()
	return writeExportAtomic(target, func(w *bufio.Writer) error {
		enc := json.NewEncoder(w)
		for rows.Next() {
			r, err := scanLogRow(rows)
			if err != nil {
				return err
			}
			if err := enc.Encode(logRowToLegacyIndexEntry(r)); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// exportTokens 把 auth_tokens 表写成 auth_tokens.json。next_id 不进库，
// 导出按 max(id)+1 重建——语义只需大于全部现存 id（文件时代装载时
// next_id<=0 会被忽略，等价于从头计）。
func (s *Store) exportTokens(ctx context.Context, target string) (string, string, error) {
	tokens, err := s.ListTokens(ctx)
	if err != nil {
		return "", "", err
	}
	f := legacyTokenFile{NextID: 1, Tokens: make([]*legacyToken, 0, len(tokens))}
	for _, t := range tokens {
		if t.ID >= f.NextID {
			f.NextID = t.ID + 1
		}
		f.Tokens = append(f.Tokens, tokenRowToLegacyToken(t))
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", "", err
	}
	return writeExportAtomic(target, writeAll(data))
}

// exportModels 把 model_registry 写成 models.json。updated_at 是库内
// 审计列，文件格式不承载。
func (s *Store) exportModels(ctx context.Context, target string) (string, string, error) {
	entries, err := s.ListModels(ctx)
	if err != nil {
		return "", "", err
	}
	f := legacyRegistryFile{Models: map[string]struct {
		RedirectModel string `json:"redirect_model,omitempty"`
		Disabled      bool   `json:"disabled,omitempty"`
	}{}}
	for _, e := range entries {
		f.Models[e.Model] = struct {
			RedirectModel string `json:"redirect_model,omitempty"`
			Disabled      bool   `json:"disabled,omitempty"`
		}{RedirectModel: e.RedirectModel, Disabled: e.Disabled}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", "", err
	}
	return writeExportAtomic(target, writeAll(data))
}

// exportSettings 把 settings 表写成 panel-settings.json。updated_at 原样
// 回写——文件时代它就是 unix 秒口径（面板写入路径保持一致），库不另作解释。
func (s *Store) exportSettings(ctx context.Context, target string) (string, string, error) {
	values, updated, err := s.ListSettings(ctx)
	if err != nil {
		return "", "", err
	}
	data, err := json.MarshalIndent(legacySettingsFile{Values: values, Updated: updated}, "", "  ")
	if err != nil {
		return "", "", err
	}
	return writeExportAtomic(target, writeAll(data))
}

// exportQuota 把 quota_samples 按时间序写成 quota.jsonl；QuotaSample 的
// JSON tag 即文件时代行格式。id 参与排序保证同 at 的稳定次序。
func (s *Store) exportQuota(ctx context.Context, target string) (string, string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, account, daily_remaining, weekly_remaining,
		daily_reset_at, weekly_reset_at, prompt_credits, flow_credits, flex_credits,
		acu_consumed, acu_limit, used_prompt_credits, used_flow_credits, used_flex_credits,
		grace_period_status, grace_period_end, was_reduced_by_orphaned_usage,
		top_up_enabled, top_up_transaction_status
		FROM quota_samples ORDER BY at, id`)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = rows.Close() }()
	return writeExportAtomic(target, func(w *bufio.Writer) error {
		enc := json.NewEncoder(w)
		for rows.Next() {
			var q QuotaSample
			if err := rows.Scan(&q.At, &q.Account, &q.DailyRemaining, &q.WeeklyRemaining,
				&q.DailyResetAt, &q.WeeklyResetAt,
				&q.PromptCredits, &q.FlowCredits, &q.FlexCredits, &q.ACUConsumed, &q.ACULimit,
				&q.UsedPromptCredits, &q.UsedFlowCredits, &q.UsedFlexCredits,
				&q.GracePeriodStatus, &q.GracePeriodEnd, &q.WasReducedByOrphanedUsage,
				&q.TopUpEnabled, &q.TopUpTransactionStatus); err != nil {
				return err
			}
			if err := enc.Encode(&q); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// exportGateStates 把 runtime_state 的 gate:* 键拆回 gate-state*.json：
// gate:default → gate-state.json，gate:<lane> → gate-state-<lane>.json；
// value 即文件原文（导入时原样入库），逐字节写回。
func (s *Store) exportGateStates(ctx context.Context, logRoot string) ([]string, []string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT "key", value FROM runtime_state WHERE "key" LIKE 'gate:%' ORDER BY "key"`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var paths, notices []string
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, nil, err
		}
		lane := strings.TrimPrefix(key, "gate:")
		name := "gate-state.json"
		if lane != "default" {
			name = "gate-state-" + lane + ".json"
		}
		actual, notice, err := writeExportAtomic(filepath.Join(logRoot, name), writeAll([]byte(value)))
		if err != nil {
			return nil, nil, err
		}
		paths = append(paths, actual)
		if notice != "" {
			notices = append(notices, notice)
		}
	}
	return paths, notices, rows.Err()
}

// writeExportAtomic 把 write 的产出经 tmp+rename 原子落到 target。
// target 已存在时改道 <target>.exported（返回 notice 非空，由调用方
// 透传给操作员）——本工具绝不覆盖可能是现役状态的文件；.exported 是
// 工具自身产物，重复导出直接覆盖它。
func writeExportAtomic(target string, write func(w *bufio.Writer) error) (actual, notice string, err error) {
	actual = target
	if _, statErr := os.Stat(target); statErr == nil {
		actual = target + ".exported"
		notice = fmt.Sprintf("%s occupied; wrote %s instead", target, actual)
	} else if !os.IsNotExist(statErr) {
		return "", "", statErr
	}
	tmp := actual + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	werr := write(bw)
	if ferr := bw.Flush(); werr == nil {
		werr = ferr
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return "", "", werr
	}
	if err := os.Rename(tmp, actual); err != nil {
		_ = os.Remove(tmp)
		return "", "", err
	}
	return actual, notice, nil
}

// writeAll 适配整文件 JSON 源：一次性字节写进导出流。
func writeAll(data []byte) func(w *bufio.Writer) error {
	return func(w *bufio.Writer) error {
		_, err := w.Write(data)
		return err
	}
}

// logRowToLegacyIndexEntry 把读路径的 LogRow 投影回文件时代行形状；
// started_at 归一为 RFC3339Nano（与写入侧 Format 同源，round-trip 无损）。
func logRowToLegacyIndexEntry(r *LogRow) *legacyIndexEntry {
	return &legacyIndexEntry{
		Dir:               r.Dir,
		StartedAt:         r.StartedAt.Format(time.RFC3339Nano),
		DurationMS:        r.DurationMS,
		RequestReadyMS:    r.RequestReadyMS,
		UpstreamSentMS:    r.UpstreamSentMS,
		UpstreamOpenMS:    r.UpstreamOpenMS,
		FirstUpstreamMS:   r.FirstUpstreamMS,
		FirstClientMS:     r.FirstClientMS,
		API:               r.API,
		Method:            r.Method,
		Path:              r.Path,
		StatusCode:        r.StatusCode,
		Result:            r.Result,
		RequestedModel:    r.RequestedModel,
		Model:             r.Model,
		ResponseModel:     r.ResponseModel,
		ModelMismatch:     r.ModelMismatch,
		Stream:            r.Stream,
		InputTokens:       r.InputTokens,
		OutputTokens:      r.OutputTokens,
		CacheReadTokens:   r.CacheReadTokens,
		CacheWriteTokens:  r.CacheWriteTokens,
		ReasoningTokens:   r.ReasoningTokens,
		TotalTokens:       r.TotalTokens,
		CreditCost:        r.CreditCost,
		UpstreamRequestID: r.UpstreamRequestID,
		ClientIP:          r.ClientIP,
		KeyHash:           r.KeyHash,
		ClientRequestID:   r.ClientRequestID,
		ErrorStage:        r.ErrorStage,
		ErrorMessage:      r.ErrorMessage,
		DroppedEvents:     r.DroppedEvents,
		RetryAfterSeconds: r.RetryAfterSeconds,
		RateLimited:       r.RateLimited,
		Retries:           r.Retries,
		Account:           r.Account,
		AccountSwitches:   r.AccountSwitches,
		PrematureEndTurn:  r.PrematureEndTurn,
		Repairs:           r.Repairs,
		ConnReused:        r.ConnReused,
		ConnIdleMS:        r.ConnIdleMS,
	}
}

// tokenRowToLegacyToken 把 TokenRow 投影回文件时代令牌形状；created_at
// 从 unix 毫秒还原为 RFC3339Nano（文件时代是 time.Time 直 marshal，
// 毫秒以外的亚毫秒精度在导入时已被截断，不可恢复）。
func tokenRowToLegacyToken(t *TokenRow) *legacyToken {
	return &legacyToken{
		ID:             t.ID,
		Hash:           t.Token,
		Description:    t.Description,
		CreatedAt:      time.UnixMilli(t.CreatedAt).UTC().Format(time.RFC3339Nano),
		ExpiresAt:      t.ExpiresAt,
		LastUsedAt:     t.LastUsedAt,
		IsActive:       t.IsActive,
		SuccessCount:   t.SuccessCount,
		FailureCount:   t.FailureCount,
		StreamAvgTTFB:  t.StreamAvgTTFB,
		NonStreamAvgRT: t.NonStreamAvgRT,
		StreamCount:    t.StreamCount,
		NonStreamCount: t.NonStreamCount,

		PromptTokensTotal:        t.PromptTokensTotal,
		CompletionTokensTotal:    t.CompletionTokensTotal,
		CacheReadTokensTotal:     t.CacheReadTokensTotal,
		CacheCreationTokensTotal: t.CacheCreationTokensTotal,
		TotalCostUSD:             t.TotalCostUSD,
		EffectiveCostUSD:         t.EffectiveCostUSD,

		CostUsedMicroUSD:     t.CostUsedMicroUSD,
		CostLimitMicroUSD:    t.CostLimitMicroUSD,
		DailyUsedMicroUSD:    t.DailyUsedMicroUSD,
		DailyLimitMicroUSD:   t.DailyLimitMicroUSD,
		DailyPeriodStart:     t.DailyPeriodStart,
		MonthlyUsedMicroUSD:  t.MonthlyUsedMicroUSD,
		MonthlyLimitMicroUSD: t.MonthlyLimitMicroUSD,
		MonthlyPeriodStart:   t.MonthlyPeriodStart,
		Cost5hUsedMicroUSD:   t.Cost5hUsedMicroUSD,
		Cost5hLimitMicroUSD:  t.Cost5hLimitMicroUSD,
		Cost5hAnchor:         t.Cost5hAnchor,
		WeeklyUsedMicroUSD:   t.WeeklyUsedMicroUSD,
		WeeklyLimitMicroUSD:  t.WeeklyLimitMicroUSD,
		WeeklyPeriodStart:    t.WeeklyPeriodStart,

		AllowedModels:  t.AllowedModels,
		MaxConcurrency: t.MaxConcurrency,
		MaxRPM:         t.MaxRPM,
	}
}
