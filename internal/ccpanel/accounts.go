package ccpanel

import (
	"context"
	"crypto/sha256"
	"fmt"
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
	// ClearCooldown 清该名 lane 的池侧冷却；无活 lane 返 false。
	ClearCooldown func(name string) bool
	// TokenOf 解析该名生效凭据（行值→config 值→credentials_file
	// 现读，不经 lane；disabled 可解，tombstoned 不可解）。
	TokenOf func(ctx context.Context, name string) (string, error)
}

// AccountWrite 是建号输入：Token 与 CredentialsFile 至少其一。
type AccountWrite struct {
	Name            string
	Token           string
	CredentialsFile string
	Disabled        bool
}

// AccountPatch 是改号输入：指针字段区分缺席与显式空——显式空串是
// 「清行覆盖」（config 名回落 config 值），不是「不变」。
type AccountPatch struct {
	Token           *string
	CredentialsFile *string
	Disabled        *bool
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
type accountSnapshots struct {
	laneStates map[string]devin.LaneState
	gates      map[string]devin.GateStats
	warms      map[string]devin.WarmStats
	inflight   map[string]int
	quota      map[string]any // QuotaReport 的 accounts 子表
}

func (h *Handler) accountSnapshots(ctx context.Context) accountSnapshots {
	snap := accountSnapshots{
		laneStates: map[string]devin.LaneState{},
		gates:      map[string]devin.GateStats{},
		warms:      map[string]devin.WarmStats{},
		inflight:   map[string]int{},
		quota:      map[string]any{},
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
	if accounts, ok := h.QuotaReport(ctx)["accounts"].(map[string]any); ok {
		snap.quota = accounts
	}
	return snap
}

// accountView 把一条生效账号投影成契约单号视图（列表项同形）：
// 身份字段 + lane/gate/warm 快照 + inflight + quota 摘要 + usage。
// 写端点回包与 GET 列表共用同一投影，schema 只有这一处来源。
func (h *Handler) accountView(ctx context.Context, acc *store.ResolvedAccount) map[string]any {
	return buildAccountView(acc, h.accountSnapshots(ctx))
}

func buildAccountView(acc *store.ResolvedAccount, snap accountSnapshots) map[string]any {
	credential := "literal"
	if acc.CredentialsFile != "" {
		credential = "credentials_file"
	}
	tokenSHA := ""
	if acc.Token != "" {
		sum := sha256.Sum256([]byte(acc.Token))
		tokenSHA = fmt.Sprintf("sha256:%x", sum[:6])
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
		"credentials_file": acc.CredentialsFile,
		"lane":             nil,
		"gate":             nil,
		"warm":             nil,
		"inflight":         snap.inflight[acc.Name],
		"quota":            quota,
		"usage":            nil, // P2 聚合位，v1 恒 null
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
	snap := h.accountSnapshots(r.Context())
	views := make([]map[string]any, 0, len(accounts))
	for i := range accounts {
		views = append(views, buildAccountView(&accounts[i], snap))
	}
	writeEnvelope(w, http.StatusOK, apiResponse{
		Success: true,
		Data:    map[string]any{"accounts": views},
		Count:   len(views),
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
