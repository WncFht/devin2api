package ccpanel

import (
	"context"
	"errors"
	"net/http"

	"github.com/WncFht/devin2api/internal/store"
)

// errNotImplemented 是骨架期占位错误：各切片交付真实实现后消失。
var errNotImplemented = errors.New("not implemented")

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

// adminAccounts 实现 GET /admin/accounts：身份（ops.Effective）+
// 运行时快照（lane/gate/warm/inflight）+ 配额摘要的聚合视图。
func (h *Handler) adminAccounts(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	respondError(w, http.StatusNotImplemented, "not implemented")
}

// adminCLICredentials 实现 GET /admin/accounts/cli-credentials：
// 发现链探针，只报存在性/可解析性，不回传凭据内容。
func (h *Handler) adminCLICredentials(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	respondError(w, http.StatusNotImplemented, "not implemented")
}
