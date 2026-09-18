package ccpanel

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/store"
)

// AccountOps 是 /admin/accounts 的操作面：账号集合的读写要跨 store
// 行、config 声明集、devinPool 热应用与 settings 覆盖重放协调，
// 实现由装配层（main）提供，面板只持有接口（ConfigOps 先例）。
// 聚合视图组装留在面板——lane/gate/warm/quota/usage 全是面板已有
// 快照源，ops 只给身份与动作。
type AccountOps struct {
	// Effective 返回当前生效集（config 声明 ∪ 活行 − 墓碑）。
	Effective func(ctx context.Context) ([]store.ResolvedAccount, error)
	// Create/Update/Delete/Restore 各自动作内部已完成「行写入 +
	// ApplyConfigs 重推 + 回滚」，返回重推后的单号生效视图。
	Create  func(ctx context.Context, in AccountWrite) (*store.ResolvedAccount, error)
	Update  func(ctx context.Context, name string, patch AccountPatch) (*store.ResolvedAccount, error)
	Delete  func(ctx context.Context, name string) (*store.ResolvedAccount, error)
	Restore func(ctx context.Context, name string) (*store.ResolvedAccount, error)
	// Import 批量 upsert：每条 AccountWrite 是该名的「期望全态」——
	// 行级字段按入参整体覆盖（缺席 yaml 键即零值，token/credentials_file/
	// api_key 传空即清行覆盖、config 名回落 config 值）。整批原子：
	// 干跑整表校验或任一写入失败即全部回滚，成功返回被触账号的生效视图。
	Import func(ctx context.Context, entries []AccountWrite) ([]store.ResolvedAccount, error)
	// ClearCooldown 清该名 lane 的池侧冷却；无活 lane 返 false。
	ClearCooldown func(name string) bool
	// TokenOf 解析该名生效凭据（行值→config 值→credentials_file
	// 现读，不经 lane；disabled 可解，tombstoned 不可解）。
	TokenOf func(ctx context.Context, name string) (string, error)
	// CredentialOf 解出一次写操作将生效的凭据（create verify 探测用）：
	// content 直解、file 按 configDir 锚定后读、token 原样——与
	// Create/Update 的持久化口径一致。nil 时 verify 探测不可用。
	CredentialOf func(in AccountWrite) (string, error)
}

// AccountWrite 是建号输入：Token/CredentialsFile/APIKey 至少其一；
// CredentialsContent 是 credentials.toml 全文粘贴（ops 落盘成
// 管理目录下的 <name>.toml 并置 CredentialsFile），与 CredentialsFile
// 互斥。APIKey 是 Devin 平台 durable key（cog_*）——lane 用它在上游
// 判死时现场铸 session token。Verify 为 true 时 handler 先以上游探测
// 验证凭据再建行。
type AccountWrite struct {
	Name               string
	Token              string
	CredentialsFile    string
	CredentialsContent string
	APIKey             string
	Disabled           bool
	Verify             bool
	Priority           *int64
	MaxRPM             *int64
	Notes              *string
}

// AccountPatch 是改号输入：指针字段区分缺席与显式空——显式空串是
// 「清行覆盖」（config 名回落 config 值），不是「不变」。
// CredentialsFile 与 CredentialsContent 互斥（同现 400）；显式空
// CredentialsContent 等价于清 credentials_file 覆盖。
type AccountPatch struct {
	Token              *string
	CredentialsFile    *string
	CredentialsContent *string
	APIKey             *string
	Disabled           *bool
	Priority           *int64
	MaxRPM             *int64
	Notes              *string
}

// accountOpsUnavailable 在操作面未接线时回 503（tokensUnavailable 先例）。
func (h *Handler) accountOpsUnavailable(w http.ResponseWriter) bool {
	if h.accountOps != nil {
		return false
	}
	respondError(w, http.StatusServiceUnavailable, "account ops unavailable")
	return true
}

// accountSnapshots 是一次聚合组装用的全部运行时快照：快照源各取一次
// 按名分发，列表路径不必逐号重查 quota 历史（SQL）与活跃请求集。
// 无该名条目一律缺席（map 零值），由 view 落成 null。
// quotaOnly 非空时只读该名的样本序列——单号视图（写端点回包）不必为
// 一条序列扫全表；空串走全量 QuotaReport（列表路径一次取齐）。
type accountSnapshots struct {
	laneStates map[string]devin.LaneState
	gates      map[string]devin.GateStats
	warms      map[string]devin.WarmStats
	inflight   map[string]int
	quota      map[string]any            // QuotaReport 的 accounts 子表
	usage      map[string]map[string]any // 逐号 usage 投影，由调用方按名填
}

func (h *Handler) accountSnapshots(ctx context.Context, quotaOnly string) accountSnapshots {
	snap := accountSnapshots{
		laneStates: map[string]devin.LaneState{},
		gates:      map[string]devin.GateStats{},
		warms:      map[string]devin.WarmStats{},
		inflight:   map[string]int{},
		quota:      map[string]any{},
		usage:      map[string]map[string]any{},
	}
	if h.accountLaneStates != nil {
		snap.laneStates = h.accountLaneStates()
	}
	if h.accountGateStats != nil {
		snap.gates = h.accountGateStats()
	}
	if h.accountWarmStats != nil {
		snap.warms = h.accountWarmStats()
	}
	// inflight 按终局 lane 名分桶：上游请求未发出或 failover 途中的
	// 请求 Account 为空，不落入任何号；disabled 排空期 lane 快照已撤
	// 但在途计数仍按名可见（契约：inflight 顶层字段，不随 lane 消失）。
	for _, ar := range h.debug.ActiveRequests() {
		if ar.Account != "" {
			snap.inflight[ar.Account]++
		}
	}
	if quotaOnly != "" {
		if h.store != nil {
			series, err := h.store.ListQuotaSamples(ctx, quotaOnly, 0, quotaHistoryCap)
			if err != nil {
				slog.Warn("quota history read failed", "account", quotaOnly, "error", err)
			} else if len(series) > 0 {
				snap.quota[quotaOnly] = h.quotaSeriesReport(quotaOnly, series)
			}
		}
	} else if accounts, ok := h.QuotaReport(ctx)["accounts"].(map[string]any); ok {
		snap.quota = accounts
	}
	return snap
}

// accountView 把一条生效账号投影成契约单号视图（列表项同形）：
// 身份字段 + lane/gate/warm 快照 + inflight + quota 摘要 + usage。
// 写端点回包与 GET 列表共用同一投影，schema 只有这一处来源。
func (h *Handler) accountView(ctx context.Context, acc *store.ResolvedAccount) map[string]any {
	snap := h.accountSnapshots(ctx, acc.Name)
	snap.usage[acc.Name] = h.accountUsage(ctx, acc.Name)
	return buildAccountView(acc, snap)
}

func buildAccountView(acc *store.ResolvedAccount, snap accountSnapshots) map[string]any {
	credential := "literal"
	if acc.CredentialsFile != "" {
		credential = "credentials_file"
	} else if acc.Token == "" && acc.APIKey != "" {
		credential = "api_key"
	}
	tokenSHA := ""
	if acc.Token != "" {
		sum := sha256.Sum256([]byte(acc.Token))
		tokenSHA = fmt.Sprintf("sha256:%x", sum[:6])
	}
	apiKeySHA := ""
	if acc.APIKey != "" {
		sum := sha256.Sum256([]byte(acc.APIKey))
		apiKeySHA = fmt.Sprintf("sha256:%x", sum[:6])
	}
	// quota 只取 daily/weekly/user 三键——points 曲线由前端另 join
	// /admin/quota；号无采样时 daily/weekly 落 null、user 键缺席。
	quota := map[string]any{"daily": nil, "weekly": nil}
	if report, ok := snap.quota[acc.Name].(map[string]any); ok {
		quota["daily"] = report["daily"]
		quota["weekly"] = report["weekly"]
		if user, ok := report["user"]; ok {
			quota["user"] = user
		}
	}
	view := map[string]any{
		"name":             acc.Name,
		"source":           acc.Source,
		"config_declared":  acc.ConfigDeclared,
		"has_override":     acc.Source == store.AccountSourceConfig && acc.HasRow,
		"credential":       credential,
		"disabled":         acc.Disabled,
		"token_sha":        tokenSHA,
		"api_key_sha":      apiKeySHA,
		"credentials_file": acc.CredentialsFile,
		"priority":         acc.Priority,
		"max_rpm":          acc.MaxRPM,
		"notes":            acc.Notes,
		"lane":             nil,
		"gate":             nil,
		"warm":             nil,
		"inflight":         snap.inflight[acc.Name],
		"quota":            quota,
		"usage":            snap.usage[acc.Name],
		"created_at":       acc.CreatedAt,
		"updated_at":       acc.UpdatedAt,
	}
	// lane/gate/warm 按名独立索引：无活 lane 的号（disabled/tombstoned/
	// 未接线）不会出现在任何一张快照里，三件自然全 null。
	if lane, ok := snap.laneStates[acc.Name]; ok {
		view["lane"] = lane
	}
	if gate, ok := snap.gates[acc.Name]; ok {
		view["gate"] = gate
	}
	if warm, ok := snap.warms[acc.Name]; ok {
		view["warm"] = warmStatsView(warm)
	}
	return view
}

// accountUsage 取单号 usage 原始量并投影；store 未接线或查询失败落
// null（usage 是观测字段，不挡视图）。'default' 折叠口径在 store 内做。
func (h *Handler) accountUsage(ctx context.Context, name string) map[string]any {
	if h.store == nil {
		return nil
	}
	row, err := h.store.AccountUsage(ctx, name)
	if err != nil {
		slog.Warn("account usage query failed", "account", name, "err", err)
		return nil
	}
	return accountUsageView(row)
}

// accountUsageView 把 AccountUsageRow 投影成冻结形状
// {rpm_now,tps_now,ttfb_avg,ttfb_p50,ttfb_p90,cache_rate,today:{requests,
// success_rate,tokens}}。ttfb 单位 ms；比率字段分母为 0 落 null（「无
// 数据」与「真 0」区分，同 lane/gate 空值惯例），计数字段恒出数。
func accountUsageView(row *store.AccountUsageRow) map[string]any {
	var tpsNow, ttfbAvg, ttfbP50, ttfbP90, cacheRate, successRate any
	if row.Recent.GenMS > 0 {
		tpsNow = float64(row.Recent.OutTok) * 1000 / float64(row.Recent.GenMS)
	}
	if row.TTFB.Samples > 0 {
		ttfbAvg = row.TTFBAvgMS
		ttfbP50 = row.TTFB.P50
		ttfbP90 = row.TTFB.P90
	}
	if d := row.Recent.InTok + row.Recent.CrTok + row.Recent.CwTok; d > 0 {
		cacheRate = float64(row.Recent.CrTok) / float64(d)
	}
	if row.Today.Non499 > 0 {
		successRate = float64(row.Today.OK) / float64(row.Today.Non499)
	}
	return map[string]any{
		"rpm_now":    row.Recent.Req,
		"tps_now":    tpsNow,
		"ttfb_avg":   ttfbAvg,
		"ttfb_p50":   ttfbP50,
		"ttfb_p90":   ttfbP90,
		"cache_rate": cacheRate,
		"today": map[string]any{
			"requests":     row.Today.Requests,
			"success_rate": successRate,
			"tokens":       row.Today.Tokens,
		},
	}
}

// adminAccounts 实现 GET /admin/accounts：身份（ops.Effective）+
// 运行时快照（lane/gate/warm/inflight）+ 配额摘要的聚合视图。
func (h *Handler) adminAccounts(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	accounts, err := h.accountOps.Effective(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	snap := h.accountSnapshots(r.Context(), "")
	views := make([]map[string]any, 0, len(accounts))
	for i := range accounts {
		snap.usage[accounts[i].Name] = h.accountUsage(r.Context(), accounts[i].Name)
		views = append(views, buildAccountView(&accounts[i], snap))
	}
	writeEnvelope(w, http.StatusOK, apiResponse{
		Success: true,
		Data:    map[string]any{"accounts": views},
		Count:   intPtr(len(views)),
	})
}

// adminCLICredentials 实现 GET /admin/accounts/cli-credentials：
// 发现链探针，只报存在性/可解析性，不回传凭据内容。与 accountOps
// 无关——号池未接线时探针仍可用。
func (h *Handler) adminCLICredentials(w http.ResponseWriter, _ *http.Request) {
	for _, path := range config.DevinCredentialsPaths() {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		respondOK(w, map[string]any{
			"available":      true,
			"path":           path,
			"parsable":       config.TokenFromCredentialsFile(path) != "",
			"suggested_name": "default-cli",
		})
		return
	}
	respondOK(w, map[string]any{"available": false})
}
