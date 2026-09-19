// 本文件是 /admin/accounts 的批量面：GET /admin/accounts/export 把生效
// 账号集（非墓碑）导成一份可移植文件，POST /admin/accounts/import 把同
// 形状文件按 name upsert 进 overlay 行。导入整批原子——任一条件非法
// 或重推失败即全部回滚。
package ccpanel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/WncFht/devin2api/internal/accounts"
	"github.com/WncFht/devin2api/internal/store"
)

// accountExportEntry 是批量文件里一个账号条目的形状：yaml/json 键与
// config.DevinAccountConfig 对齐，外加三个行级字段——credentials_content
// （导出时把 credentials_file 的文件内容内联，跨机迁移不依赖原路径）、
// disabled 与 notes（config 无对应物，粘回 config.yaml 需剔除）。
// priority/max_rpm 指针语义同写端点：缺席即「无行覆盖」。
type accountExportEntry struct {
	Name               string `yaml:"name" json:"name"`
	Token              string `yaml:"token,omitempty" json:"token,omitempty"`
	CredentialsFile    string `yaml:"credentials_file,omitempty" json:"credentials_file,omitempty"`
	CredentialsContent string `yaml:"credentials_content,omitempty" json:"credentials_content,omitempty"`
	APIKey             string `yaml:"api_key,omitempty" json:"api_key,omitempty"`
	Disabled           bool   `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	Priority           *int64 `yaml:"priority,omitempty" json:"priority,omitempty"`
	MaxRPM             *int64 `yaml:"max_rpm,omitempty" json:"max_rpm,omitempty"`
	Notes              string `yaml:"notes,omitempty" json:"notes,omitempty"`
}

// accountsExportHeader 是导出 yaml 的文件头：交代用途与 config 粘贴的
// 字段边界。
const accountsExportHeader = `# devin-2api accounts export — POST /admin/accounts/import 的回灌素材。
# 字段与 config.yaml 的 devin.accounts 同构；credentials_content 是
# credentials.toml 全文内联（跨机迁移不依赖原路径），disabled/notes 是
# 面板行级字段——粘进 config.yaml 前需剔除这两个键。
`

// adminExportAccounts 实现 GET /admin/accounts/export：生效账号集（非
// 墓碑）导成可移植文件——默认 yaml，?format=json 出 JSON。credentials_file
// 型账号把文件内容内联成 credentials_content（读不到文件才回落原路径），
// tombstoned 不导：其 config 声明仍在操作员手里，导出它会在导入侧复活。
func (h *Handler) adminExportAccounts(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	accounts, err := h.accountOps.Effective(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	entries := make([]accountExportEntry, 0, len(accounts))
	for _, acc := range accounts {
		if acc.Source == store.AccountSourceTombstoned {
			continue
		}
		e := accountExportEntry{
			Name:     acc.Name,
			Token:    acc.Token,
			APIKey:   acc.APIKey,
			Disabled: acc.Disabled,
			Notes:    acc.Notes,
		}
		if acc.CredentialsFile != "" {
			if data, readErr := os.ReadFile(acc.CredentialsFile); readErr == nil {
				e.CredentialsContent = string(data)
			} else {
				e.CredentialsFile = acc.CredentialsFile
			}
		}
		if acc.Priority != 0 {
			v := int64(acc.Priority)
			e.Priority = &v
		}
		if acc.MaxRPM != 0 {
			v := int64(acc.MaxRPM)
			e.MaxRPM = &v
		}
		entries = append(entries, e)
	}
	payload := struct {
		Accounts []accountExportEntry `yaml:"accounts" json:"accounts"`
	}{Accounts: entries}
	if r.URL.Query().Get("format") == "json" {
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="devin-accounts.json"`)
		_, _ = w.Write(data)
		return
	}
	var buf bytes.Buffer
	buf.WriteString(accountsExportHeader)
	data, err := yaml.Marshal(payload)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	buf.Write(data)
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="devin-accounts.yaml"`)
	_, _ = w.Write(buf.Bytes())
}

// adminImportAccounts 实现 POST /admin/accounts/import：body 是 yaml 或
// json（yaml 解析器单入口吃两种——JSON 是 YAML 1.2 子集），形状取
// {accounts: [...]} 或裸账号列表。每条是该名的期望全态：新名建行、
// config 名建覆盖行、墓碑名复活；任一条目非法即整批 400 不落库。
func (h *Handler) adminImportAccounts(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) || h.accountOps.Import == nil {
		respondError(w, http.StatusServiceUnavailable, "account ops unavailable")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var wrapped struct {
		Accounts []accountExportEntry `yaml:"accounts"`
	}
	var entries []accountExportEntry
	if err := yaml.Unmarshal(body, &wrapped); err == nil && len(wrapped.Accounts) > 0 {
		entries = wrapped.Accounts
	} else if err := yaml.Unmarshal(body, &entries); err != nil || len(entries) == 0 {
		respondError(w, http.StatusBadRequest, "payload must be {accounts: [...]} or a bare account list (yaml/json)")
		return
	}
	// 逐条快检：名正则/批内重名/file 与 content 互斥/配额下界——错误
	// 带条目序号，整批拒绝（ops 侧还会再过一遍整表干跑校验）。
	seen := make(map[string]bool, len(entries))
	ins := make([]accounts.AccountWrite, 0, len(entries))
	for i, e := range entries {
		name := strings.TrimSpace(e.Name)
		if !accountNamePattern.MatchString(name) {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("accounts[%d]: invalid account name %q", i, e.Name))
			return
		}
		if seen[name] {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("accounts[%d]: duplicate name %q in payload", i, name))
			return
		}
		seen[name] = true
		if e.CredentialsFile != "" && e.CredentialsContent != "" {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("accounts[%d]: credentials_file and credentials_content are mutually exclusive", i))
			return
		}
		if e.Priority != nil && *e.Priority < 0 {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("accounts[%d]: priority must be >= 0", i))
			return
		}
		if e.MaxRPM != nil && *e.MaxRPM < 0 {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("accounts[%d]: max_rpm must be >= 0", i))
			return
		}
		var notes *string
		if e.Notes != "" {
			notes = &e.Notes
		}
		ins = append(ins, accounts.AccountWrite{
			Name:               name,
			Token:              e.Token,
			CredentialsFile:    e.CredentialsFile,
			CredentialsContent: e.CredentialsContent,
			APIKey:             e.APIKey,
			Disabled:           e.Disabled,
			Priority:           e.Priority,
			MaxRPM:             e.MaxRPM,
			Notes:              notes,
		})
	}
	resolved, err := h.accountOps.Import(r.Context(), ins)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	snap := h.accountSnapshots(r.Context(), "")
	views := make([]map[string]any, 0, len(resolved))
	for i := range resolved {
		snap.usage[resolved[i].Name] = h.accountUsage(r.Context(), resolved[i].Name)
		views = append(views, buildAccountView(&resolved[i], snap))
	}
	respondOK(w, map[string]any{"imported": views})
}
