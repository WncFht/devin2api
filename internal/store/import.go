package store

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// legacyToken/legacyTokenFile 是 auth_tokens.json 的读写形状——
// store 不能 import authtoken（方向相反），这里自持最小镜像。
// JSON tag 与文件时代 authtoken.Token/tokenFile 逐字段一致（含
// omitempty 分布），导入（Unmarshal）与导出（Marshal）共用。
type legacyTokenFile struct {
	NextID int64          `json:"next_id"`
	Tokens []*legacyToken `json:"tokens"`
}

type legacyToken struct {
	ID             int64   `json:"id"`
	Hash           string  `json:"token"`
	Description    string  `json:"description"`
	CreatedAt      string  `json:"created_at"`
	ExpiresAt      *int64  `json:"expires_at,omitempty"`
	LastUsedAt     *int64  `json:"last_used_at,omitempty"`
	IsActive       bool    `json:"is_active"`
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

	CostUsedMicroUSD     int64 `json:"cost_used_micro_usd"`
	CostLimitMicroUSD    int64 `json:"cost_limit_micro_usd"`
	DailyUsedMicroUSD    int64 `json:"cost_daily_used_micro_usd"`
	DailyLimitMicroUSD   int64 `json:"cost_daily_limit_micro_usd"`
	DailyPeriodStart     int64 `json:"cost_daily_period_start"`
	MonthlyUsedMicroUSD  int64 `json:"cost_monthly_used_micro_usd"`
	MonthlyLimitMicroUSD int64 `json:"cost_monthly_limit_micro_usd"`
	MonthlyPeriodStart   int64 `json:"cost_monthly_period_start"`
	Cost5hUsedMicroUSD   int64 `json:"cost_5h_used_micro_usd"`
	Cost5hLimitMicroUSD  int64 `json:"cost_5h_limit_micro_usd"`
	Cost5hAnchor         int64 `json:"cost_5h_anchor"`
	WeeklyUsedMicroUSD   int64 `json:"cost_weekly_used_micro_usd"`
	WeeklyLimitMicroUSD  int64 `json:"cost_weekly_limit_micro_usd"`
	WeeklyPeriodStart    int64 `json:"cost_weekly_period_start"`

	AllowedModels  []string `json:"allowed_models,omitempty"`
	MaxConcurrency int      `json:"max_concurrency"`
	MaxRPM         int      `json:"max_rpm"`
}

// probeClientRequestID 镜像 debuglog.ProbeClientRequestID——store 是被
// debuglog 导入的下层包，不能反向引用常量。仅剩 importIndex 一处消费：
// legacy index.jsonl 行不带 log_source，分类规则（面板探活归
// manual_test）属文件格式翻译，随 importer 退役一起删。
const probeClientRequestID = "panel-probe"

// legacyIndexEntry 是 index.jsonl 一行的读写形状——JSON tag 与文件时代
// debuglog.IndexEntry 逐字段一致（含 omitempty 分布）；started_at 是
// RFC3339Nano。导入与导出共用，保证 round-trip 字段逐一对齐。
type legacyIndexEntry struct {
	Dir               string `json:"dir"`
	StartedAt         string `json:"started_at"`
	DurationMS        int64  `json:"duration_ms"`
	RequestReadyMS    *int64 `json:"request_ready_ms,omitempty"`
	UpstreamSentMS    *int64 `json:"upstream_sent_ms,omitempty"`
	UpstreamOpenMS    *int64 `json:"upstream_open_ms,omitempty"`
	FirstUpstreamMS   *int64 `json:"first_upstream_ms,omitempty"`
	FirstClientMS     *int64 `json:"first_client_ms,omitempty"`
	API               string `json:"api,omitempty"`
	Method            string `json:"method"`
	Path              string `json:"path"`
	StatusCode        int    `json:"status_code"`
	Result            string `json:"result"`
	RequestedModel    string `json:"requested_model,omitempty"`
	Model             string `json:"model,omitempty"`
	ResponseModel     string `json:"response_model,omitempty"`
	ModelMismatch     bool   `json:"model_mismatch,omitempty"`
	Stream            bool   `json:"stream"`
	InputTokens       int64  `json:"input_tokens,omitempty"`
	OutputTokens      int64  `json:"output_tokens,omitempty"`
	CacheReadTokens   int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens  int64  `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   int64  `json:"reasoning_tokens,omitempty"`
	TotalTokens       int64  `json:"total_tokens,omitempty"`
	CreditCost        int64  `json:"credit_cost,omitempty"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	ClientIP          string `json:"client_ip,omitempty"`
	KeyHash           string `json:"key_hash,omitempty"`
	ClientRequestID   string `json:"client_request_id,omitempty"`
	ErrorStage        string `json:"error_stage,omitempty"`
	ErrorMessage      string `json:"error_message,omitempty"`
	DroppedEvents     uint64 `json:"dropped_events,omitempty"`
	RetryAfterSeconds int64  `json:"retry_after_seconds,omitempty"`
	RateLimited       bool   `json:"rate_limited,omitempty"`
	Retries           int    `json:"retries,omitempty"`
	Account           string `json:"account,omitempty"`
	AccountSwitches   int    `json:"account_switches,omitempty"`
	PrematureEndTurn  bool   `json:"premature_end_turn,omitempty"`
	Repairs           int    `json:"repairs,omitempty"`
	ConnReused        *bool  `json:"conn_reused,omitempty"`
	ConnIdleMS        *int64 `json:"conn_idle_ms,omitempty"`
}

type legacyRegistryFile struct {
	Models map[string]struct {
		RedirectModel string `json:"redirect_model,omitempty"`
		Disabled      bool   `json:"disabled,omitempty"`
	} `json:"models"`
}

type legacySettingsFile struct {
	Values  map[string]string `json:"values"`
	Updated map[string]int64  `json:"updated,omitempty"`
}

// ImportLegacy 把文件时代的持久化搬进库。触发条件是「源文件存在」
// 而不是一次性标记：分阶段落地期间旧写路径（D2–D6 切换前仍在写
// index.jsonl/auth_tokens.json 等）会在导入后重建同名文件，再次
// 调用必须把新文件也并进来。每个源在「数据行 + imported:<src>
// 标记」的同一事务内提交，导入全部幂等（dir 唯一索引、token 按
// token 列去重、(account,at) 配额去重、KV 覆盖），成功后原文件改名
// <name>.migrated。可重复调用，无文件时退化成几次 stat。
// imported:<src> 与 import_base_done 只写不读——纯粹的一次性留痕，
// 供排障确认「那次导入跑过」，不参与任何判定。
func (s *Store) ImportLegacy(ctx context.Context, stateDir, logRoot string) error {
	sources := []struct {
		name string
		path string
		run  func(ctx context.Context, path string) error
	}{
		{"index", filepath.Join(logRoot, "index.jsonl"), s.importIndex},
		{"auth_tokens", filepath.Join(stateDir, "auth_tokens.json"), s.importTokens},
		{"models", filepath.Join(stateDir, "models.json"), s.importModels},
		{"panel_settings", filepath.Join(stateDir, "panel-settings.json"), s.importSettings},
		{"quota", filepath.Join(logRoot, "quota.jsonl"), s.importQuota},
	}
	for _, src := range sources {
		if _, err := os.Stat(src.path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := src.run(ctx, src.path); err != nil {
			return fmt.Errorf("import %s: %w", src.name, err)
		}
		if err := os.Rename(src.path, src.path+".migrated"); err != nil {
			return fmt.Errorf("rename %s: %w", src.path, err)
		}
	}
	if err := s.importGateStates(ctx, logRoot); err != nil {
		return err
	}
	// importIndex 是唯一绕过双写的 logs 写入者：收尾把水位线之后
	// 落库的行补记进 rollup（无缺口时退化成一次空扫）。
	if err := s.ReconcileCells(ctx); err != nil {
		return err
	}
	return s.SetState(ctx, "import_base_done", time.Now().UTC().Format(time.RFC3339))
}

// importIndex 把 index.jsonl 逐行转成 logs 行；损坏行（截断尾部）
// 跳过，与文件时代 ScanIndex 的容忍语义一致。用 ReadBytes 行循环而
// 非 bufio.Scanner——Scanner 的行上限会把超长行变成整体中止
// （ImportLegacy 报错 → 文件不改名 → 重启再炸），坏行跳过语义
// 只在逐行容忍下成立。
func (s *Store) importIndex(ctx context.Context, path string) error {
	return s.withSourceTx(ctx, "index", func(tx *sql.Tx) error {
		f, err := os.Open(path)
		if err != nil {
			return skipMissing(err)
		}
		defer func() { _ = f.Close() }()
		reader := bufio.NewReaderSize(f, 64*1024)
		for {
			line, err := reader.ReadBytes('\n')
			var e legacyIndexEntry
			if len(line) > 0 && json.Unmarshal(line, &e) == nil {
				if started, perr := time.Parse(time.RFC3339Nano, e.StartedAt); perr == nil {
					source := "proxy"
					if e.ClientRequestID == probeClientRequestID {
						source = "manual_test"
					}
					ms := started.UnixMilli()
					// ON CONFLICT(dir)：dir 部分唯一索引把「标记丢失后的重跑」
					// 变成幂等空操作，不会卡死导入；WHERE 子句须与索引的
					// 部分谓词一致，SQLite 才认这个冲突目标。
					if _, err := tx.ExecContext(ctx, logsInsertSQL+` ON CONFLICT(dir) WHERE dir != '' DO NOTHING`,
						e.Dir, ms, ms/60000, started.Format(time.RFC3339Nano), e.DurationMS,
						e.RequestReadyMS, e.UpstreamSentMS, e.UpstreamOpenMS, e.FirstUpstreamMS, e.FirstClientMS,
						e.API, e.Method, e.Path, e.StatusCode, e.Result,
						e.RequestedModel, e.Model, e.ResponseModel, e.ModelMismatch, e.Stream,
						e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens, e.TotalTokens,
						e.CreditCost, e.UpstreamRequestID, e.ClientIP, e.KeyHash, e.ClientRequestID,
						e.ErrorStage, e.ErrorMessage, e.DroppedEvents, e.RetryAfterSeconds, e.RateLimited,
						e.Retries, e.Account, e.AccountSwitches, e.PrematureEndTurn, e.Repairs,
						e.ConnReused, e.ConnIdleMS, source); err != nil {
						return err
					}
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// importTokens 按 token 列去重合并：同 token 已在库就用库里的 id
// 覆盖字段；新 token 优先保留文件 id（sqlite_sequence 自动跟进到
// 最大显式 id，next_id 不必搬运），id 被别的 token 占用则交给
// autoincrement——分阶段落地期间旧仓重建文件的 id 空间与库不一致，
// 文件 id 只是参考。
func (s *Store) importTokens(ctx context.Context, path string) error {
	return s.withSourceTx(ctx, "auth_tokens", func(tx *sql.Tx) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return skipMissing(err)
		}
		var f legacyTokenFile
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, t := range f.Tokens {
			createdMS := int64(0)
			if t.CreatedAt != "" {
				if parsed, err := time.Parse(time.RFC3339Nano, t.CreatedAt); err == nil {
					createdMS = parsed.UnixMilli()
				}
			}
			allowed, err := json.Marshal(t.AllowedModels)
			if err != nil {
				return err
			}
			row := &TokenRow{
				ID: t.ID, Token: t.Hash, Description: t.Description, CreatedAt: createdMS,
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
				WeeklyUsedMicroUSD: t.WeeklyUsedMicroUSD, WeeklyLimitMicroUSD: t.WeeklyLimitMicroUSD,
				WeeklyPeriodStart: t.WeeklyPeriodStart,
				MaxConcurrency:    t.MaxConcurrency, MaxRPM: t.MaxRPM,
			}
			var existingID int64
			err = tx.QueryRowContext(ctx,
				`SELECT id FROM auth_tokens WHERE token=?`, t.Hash).Scan(&existingID)
			switch err {
			case nil:
				row.ID = existingID
				_, err = tx.ExecContext(ctx, tokenInsertAll, tokenArgs(row, string(allowed))...)
			case sql.ErrNoRows:
				var taken int64
				switch e := tx.QueryRowContext(ctx,
					`SELECT 1 FROM auth_tokens WHERE id=?`, t.ID).Scan(&taken); e {
				case nil:
					// 文件 id 被库里别的 token 占用：交给 autoincrement。
					_, err = tx.ExecContext(ctx, tokenInsertAuto, tokenArgs(row, string(allowed))[1:]...)
				case sql.ErrNoRows:
					_, err = tx.ExecContext(ctx, tokenInsertAll, tokenArgs(row, string(allowed))...)
				default:
					err = e
				}
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) importModels(ctx context.Context, path string) error {
	return s.withSourceTx(ctx, "models", func(tx *sql.Tx) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return skipMissing(err)
		}
		var f legacyRegistryFile
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		now := time.Now().UnixMilli()
		for name, e := range f.Models {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO model_registry(model, redirect_model, disabled, updated_at) VALUES(?,?,?,?)`,
				name, e.RedirectModel, e.Disabled, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) importSettings(ctx context.Context, path string) error {
	return s.withSourceTx(ctx, "panel_settings", func(tx *sql.Tx) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return skipMissing(err)
		}
		var f legacySettingsFile
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for k, v := range f.Values {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO settings("key", value, updated_at) VALUES(?,?,?)`,
				k, v, f.Updated[k]); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) importQuota(ctx context.Context, path string) error {
	return s.withSourceTx(ctx, "quota", func(tx *sql.Tx) error {
		f, err := os.Open(path)
		if err != nil {
			return skipMissing(err)
		}
		defer func() { _ = f.Close() }()
		reader := bufio.NewReaderSize(f, 16*1024)
		for {
			line, err := reader.ReadBytes('\n')
			var q QuotaSample
			if len(line) > 0 && json.Unmarshal(line, &q) == nil {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO quota_samples(
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
					q.TopUpEnabled, q.TopUpTransactionStatus, q.OverageBalanceMicros); err != nil {
					return err
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// importGateStates 把 gate-state.json 与 gate-state-<lane>.json
// 原样搬进 runtime_state（key=gate:<lane>，bare 文件记 default）；
// value 存文件原文 JSON，解码归 D6 的消费方。
func (s *Store) importGateStates(ctx context.Context, logRoot string) error {
	paths, err := filepath.Glob(filepath.Join(logRoot, "gate-state*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		lane := "default"
		base := filepath.Base(path)
		if rest, ok := strings.CutPrefix(base, "gate-state-"); ok {
			lane = strings.TrimSuffix(rest, ".json")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := s.SetState(ctx, GateStateKey(lane), string(data)); err != nil {
			return err
		}
		if err := os.Rename(path, path+".migrated"); err != nil {
			return fmt.Errorf("rename %s: %w", path, err)
		}
	}
	return nil
}

// withSourceTx 在单事务里跑 fn 再写 imported:<name> 标记；fn 返回
// skipMissing 归一的 nil（源文件缺席）时照样提交标记，语义是
// 「该源已处理，无需再理」。标记是只写不读的一次性留痕——重跑判定
// 靠源文件存在性而非标记，崩溃重试由数据行幂等兜底。
func (s *Store) withSourceTx(ctx context.Context, name string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO runtime_state("key", value, updated_at) VALUES(?,?,?)`,
		"imported:"+name, time.Now().UTC().Format(time.RFC3339), time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// skipMissing 把「源文件不存在」归一成 nil——缺席的源无需导入。
func skipMissing(err error) error {
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
