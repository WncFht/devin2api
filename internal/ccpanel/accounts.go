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
// names 是视图要投出的账号名集合：usage 走 store 批量聚合（一条调用
// 全覆盖）；quota 在单名时只读该名的样本序列（单号视图不为一条序列
// 扫全表），其余走全量 quota report 一次取齐。
type accountSnapshots struct {
	laneStates map[string]devin.LaneState
	gates      map[string]devin.GateStats
	warms      map[string]devin.WarmStats
	inflight   map[string]int
	quota      map[string]any            // quota report 的 accounts 子表
	usage      map[string]map[string]any // 逐号 usage 投影
}

func (h *Handler) accountSnapshots(ctx context.Context, names []string) accountSnapshots {
	snap := accountSnapshots{
		laneStates: map[string]devin.LaneState{},
		gates:      map[string]devin.GateStats{},
		warms:      map[string]devin.WarmStats{},
		inflight:   map[string]int{},
		quota:      map[string]any{},
		usage:      map[string]map[string]any{},
	}
	// lane/gate/warm 三表同来自一次池快照：同一切面按名分发，无活
	// lane 的号（disabled/tombstoned/未接线）自然缺席。
	if ps, ok := h.poolSnapshot(); ok {
		for name, ls := range ps.Accounts {
			snap.laneStates[name] = ls.State
			snap.gates[name] = ls.Gate
			snap.warms[name] = ls.Warm
		}
	}
	// inflight 按终局 lane 名分桶：上游请求未发出或 failover 途中的
	// 请求 Account 为空，不落入任何号；disabled 排空期 lane 快照已撤
	// 但在途计数仍按名可见（契约：inflight 顶层字段，不随 lane 消失）。
	for _, ar := range h.debug.ActiveRequests() {
		if ar.Account != "" {
			snap.inflight[ar.Account]++
		}
	}
	if len(names) == 1 {
		if h.store != nil {
			series, err := h.store.ListQuotaSamples(ctx, names[0], 0, quotaHistoryCap)
			if err != nil {
				slog.Warn("quota history read failed", "account", names[0], "error", err)
			} else if len(series) > 0 {
				snap.quota[names[0]] = h.quotaSub().seriesReport(names[0], series)
			}
		}
	} else if accounts, ok := h.quotaSub().report(ctx)["accounts"].(map[string]any); ok {
		snap.quota = accounts
	}
	// usage 是观测字段不挡视图：store 未接线或批量查询失败时整组缺席，
	// view 侧落成 null。
	if h.store != nil {
		if usage, err := h.store.AccountsUsage(ctx, names); err != nil {
			slog.Warn("account usage query failed", "err", err)
		} else {
			for name, row := range usage {
				snap.usage[name] = accountUsageView(row)
			}
		}
	}
	return snap
}

// accountView 把一条生效账号投影成契约单号视图（列表项同形）：
// 身份字段 + lane/gate/warm 快照 + inflight + quota 摘要 + usage。
// 写端点回包与 GET 列表共用同一投影，schema 只有这一处来源。
func (h *Handler) accountView(ctx context.Context, acc *store.ResolvedAccount) map[string]any {
	return buildAccountView(acc, h.accountSnapshots(ctx, []string{acc.Name}))
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
	names := make([]string, len(accounts))
	for i := range accounts {
		names[i] = accounts[i].Name
	}
	snap := h.accountSnapshots(r.Context(), names)
	views := make([]map[string]any, 0, len(accounts))
	for i := range accounts {
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
