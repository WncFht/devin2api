// /admin/accounts 的操作面编排：行写入+重推+回滚的多步提交。
package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/WncFht/devin2api/internal/ccpanel"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/store"
)

// Ops 装配 /admin/accounts 的操作面：各闭包全部在 rt 锁下跑——
// 行写入、Apply 重推与失败回滚是一条多步提交，和 reload 共用同一把
// 串行化锁才不跟热更交错。写动作同一骨架：预检（存在性/状态/干跑
// 整表校验）→ 行写入 → Apply → 失败按写前快照回滚行 → 成功回该名
// 生效视图。
func (rt *Runtime) Ops(settings *ccpanel.PanelSettings) ccpanel.AccountOps {
	configDir := filepath.Dir(rt.configPath)
	push := func(ctx context.Context) ([]store.ResolvedAccount, error) {
		resolved, _, err := rt.Apply(ctx, rt.Config(), settings)
		return resolved, err
	}
	return ccpanel.AccountOps{
		Effective: func(ctx context.Context) ([]store.ResolvedAccount, error) {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			return rt.db.EffectiveAccounts(ctx, rt.Config().Devin.Accounts)
		},
		Create: func(ctx context.Context, in ccpanel.AccountWrite) (*store.ResolvedAccount, error) {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			cfg := rt.Config()
			if strings.TrimSpace(cfg.Devin.BaseURL) == "" || strings.TrimSpace(cfg.Devin.Model) == "" {
				return nil, errors.New("devin.base_url / devin.model required before adding accounts")
			}
			rows, err := rt.db.ListAccounts(ctx)
			if err != nil {
				return nil, err
			}
			// config 声明名、活行与墓碑行（含死墓碑）同撞「已存在」：
			// 声明名的覆盖要走 Update，墓碑名的单出口是 restore。
			if findAccountRow(rows, in.Name) != nil || declaredAccount(cfg, in.Name) {
				return nil, fmt.Errorf("account %q: %w", in.Name, store.ErrAccountExists)
			}
			row := &store.AccountRow{
				Name:            in.Name,
				Token:           strings.TrimSpace(in.Token),
				CredentialsFile: strings.TrimSpace(in.CredentialsFile),
				Disabled:        in.Disabled,
				Priority:        in.Priority,
				MaxRPM:          in.MaxRPM,
			}
			if in.Notes != nil {
				row.Notes = *in.Notes
			}
			// credentials_content 是粘贴上传：先证明能解出 token 再落盘
			// 到状态目录管理位，行存绝对路径。落盘先于干跑——合成校验
			// 要按 credentials_file 口径重读它；后续步骤失败留下的是
			// 未被引用的文件，惰性无害。
			if in.CredentialsContent != "" {
				if config.TokenFromCredentialsContent([]byte(in.CredentialsContent)) == "" {
					return nil, errors.New("credentials_content carries no windsurf_api_key")
				}
				path, err := writeAccountCredentialsFile(rt.stateDir, in.Name, in.CredentialsContent)
				if err != nil {
					return nil, fmt.Errorf("write credentials_content: %w", err)
				}
				row.CredentialsFile = path
			}
			// 干跑整表校验先于行写入：合成集非法（零凭据/重名/重
			// token/文件不可解）直接拒绝，库里不留脏行。
			synthesized, err := config.ResolveAccounts(candidateConfigs(
				store.MergeAccounts(cfg.Devin.Accounts, append(rows, row))), configDir)
			if err != nil {
				return nil, err
			}
			// 行存锚定后的绝对路径：merge 不做二次锚定，加载期「相对
			// 锚 configDir」的规则要在行写入侧复刻。
			if row.CredentialsFile != "" {
				row.CredentialsFile = synthesizedAccount(synthesized, in.Name).CredentialsFile
			}
			if err := rt.db.UpsertAccount(ctx, row); err != nil {
				return nil, err
			}
			resolved, err := push(ctx)
			if err != nil {
				rollbackAccountRow(ctx, rt.db, in.Name, nil)
				return nil, err
			}
			return findResolved(resolved, in.Name), nil
		},
		Update: func(ctx context.Context, name string, patch ccpanel.AccountPatch) (*store.ResolvedAccount, error) {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			cfg := rt.Config()
			rows, err := rt.db.ListAccounts(ctx)
			if err != nil {
				return nil, err
			}
			acc := findResolved(store.MergeAccounts(cfg.Devin.Accounts, rows), name)
			if acc == nil {
				return nil, fmt.Errorf("account %q: %w", name, store.ErrAccountNotFound)
			}
			if acc.Source == store.AccountSourceTombstoned {
				return nil, fmt.Errorf("account %q: %w", name, store.ErrAccountTombstoned)
			}
			oldRow := findAccountRow(rows, name)
			row := &store.AccountRow{Name: name}
			if oldRow != nil {
				*row = *oldRow
			}
			// 指针字段区分缺席与显式空：显式 "" 落 NULL 即「清行覆盖」
			// （config 名回落 config 值），缺席不动旧值。priority/max_rpm
			// 的 0 是真实覆盖值——指针语义表达不了「清回 NULL」。
			if patch.Token != nil {
				row.Token = strings.TrimSpace(*patch.Token)
			}
			if patch.CredentialsFile != nil {
				row.CredentialsFile = strings.TrimSpace(*patch.CredentialsFile)
			}
			if patch.Disabled != nil {
				row.Disabled = *patch.Disabled
			}
			if patch.Priority != nil {
				row.Priority = patch.Priority
			}
			if patch.MaxRPM != nil {
				row.MaxRPM = patch.MaxRPM
			}
			if patch.Notes != nil {
				row.Notes = *patch.Notes
			}
			// credentials_content 同 Create：显式空串清 credentials_file
			// 覆盖（不落盘），非空先验 token 再写管理位。落盘先于干跑，
			// 失败残留的孤儿文件惰性无害。
			if patch.CredentialsContent != nil {
				if *patch.CredentialsContent == "" {
					row.CredentialsFile = ""
				} else {
					if config.TokenFromCredentialsContent([]byte(*patch.CredentialsContent)) == "" {
						return nil, errors.New("credentials_content carries no windsurf_api_key")
					}
					path, err := writeAccountCredentialsFile(rt.stateDir, name, *patch.CredentialsContent)
					if err != nil {
						return nil, fmt.Errorf("write credentials_content: %w", err)
					}
					row.CredentialsFile = path
				}
			}
			synthesized, err := config.ResolveAccounts(candidateConfigs(
				store.MergeAccounts(cfg.Devin.Accounts, replaceAccountRow(rows, row))), configDir)
			if err != nil {
				return nil, err
			}
			if row.CredentialsFile != "" {
				row.CredentialsFile = synthesizedAccount(synthesized, name).CredentialsFile
			}
			if err := rt.db.UpsertAccount(ctx, row); err != nil {
				return nil, err
			}
			resolved, err := push(ctx)
			if err != nil {
				rollbackAccountRow(ctx, rt.db, name, oldRow)
				return nil, err
			}
			return findResolved(resolved, name), nil
		},
		Delete: func(ctx context.Context, name string) (*store.ResolvedAccount, error) {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			cfg := rt.Config()
			rows, err := rt.db.ListAccounts(ctx)
			if err != nil {
				return nil, err
			}
			acc := findResolved(store.MergeAccounts(cfg.Devin.Accounts, rows), name)
			if acc == nil || acc.Source == store.AccountSourceTombstoned {
				// 墓碑的单出口是 restore；死墓碑与无名同归 not found。
				return nil, fmt.Errorf("account %q: %w", name, store.ErrAccountNotFound)
			}
			oldRow := findAccountRow(rows, name)
			if acc.ConfigDeclared {
				// config 名物理删会在下一次 merge 里复活成 config 源
				// ——只能立墓碑压住；覆盖字段原样保留（restore 原样
				// 复活，disabled/凭据覆盖不丢）。
				tomb := &store.AccountRow{Name: name}
				if oldRow != nil {
					*tomb = *oldRow
				}
				tomb.Deleted = true
				if err := rt.db.UpsertAccount(ctx, tomb); err != nil {
					return nil, err
				}
			} else if err := rt.db.DeleteAccount(ctx, name); err != nil {
				return nil, err
			}
			if _, err := push(ctx); err != nil {
				rollbackAccountRow(ctx, rt.db, name, oldRow)
				return nil, err
			}
			// 回「删除前」视图：handler 只读 ConfigDeclared 挑响应形态
			// （tombstoned:true vs deleted:true），它在删除前后同值。
			return acc, nil
		},
		Restore: func(ctx context.Context, name string) (*store.ResolvedAccount, error) {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			cfg := rt.Config()
			rows, err := rt.db.ListAccounts(ctx)
			if err != nil {
				return nil, err
			}
			acc := findResolved(store.MergeAccounts(cfg.Devin.Accounts, rows), name)
			if acc == nil {
				return nil, fmt.Errorf("account %q: %w", name, store.ErrAccountNotFound)
			}
			if acc.Source != store.AccountSourceTombstoned {
				return nil, fmt.Errorf("account %q: %w", name, store.ErrAccountNotTombstoned)
			}
			oldRow := findAccountRow(rows, name)
			row := *oldRow
			row.Deleted = false
			if err := rt.db.UpsertAccount(ctx, &row); err != nil {
				return nil, err
			}
			resolved, err := push(ctx)
			if err != nil {
				rollbackAccountRow(ctx, rt.db, name, oldRow)
				return nil, err
			}
			return findResolved(resolved, name), nil
		},
		ClearCooldown: func(name string) bool {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			return rt.pool.ClearCooldown(name)
		},
		TokenOf: func(ctx context.Context, name string) (string, error) {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			return ResolveToken(ctx, rt.db, rt.Config().Devin.Accounts, name)
		},
		// CredentialOf 解析一次写输入将生效的凭据（verify 探测用）：
		// content 直解；file 走整表校验的锚定/现读合成（~/ 展开、相对
		// 锚 configDir），file 优先于 token——与 TokenOf/池侧 lane 的
		// 「文件是自愈源、字面量是兜底」同口径。纯解析不落盘。
		CredentialOf: func(in ccpanel.AccountWrite) (string, error) {
			if in.CredentialsContent != "" {
				if token := config.TokenFromCredentialsContent([]byte(in.CredentialsContent)); token != "" {
					return token, nil
				}
				return "", errors.New("credentials_content carries no windsurf_api_key")
			}
			if in.CredentialsFile != "" {
				synthesized, err := config.ResolveAccounts([]config.DevinAccountConfig{{
					Name: in.Name, CredentialsFile: in.CredentialsFile,
				}}, configDir)
				if err != nil {
					return "", err
				}
				return synthesized[0].Token, nil
			}
			if in.Token != "" {
				return in.Token, nil
			}
			return "", errors.New("one of token/credentials_file/credentials_content is required")
		},
	}
}

// candidateConfigs 投影写入前干跑校验的账号集：非墓碑条目全部参与
// （含停用——停用是静止 lane，enable 即转正，凭据合法性必须在写入时
// 证明，否则坏行能落库、等 enable 才爆）。
func candidateConfigs(resolved []store.ResolvedAccount) []config.DevinAccountConfig {
	out := make([]config.DevinAccountConfig, 0, len(resolved))
	for _, acc := range resolved {
		if acc.Source == store.AccountSourceTombstoned {
			continue
		}
		out = append(out, config.DevinAccountConfig{
			Name: acc.Name, Token: acc.Token, CredentialsFile: acc.CredentialsFile,
			Priority: acc.Priority, MaxRPM: acc.MaxRPM,
		})
	}
	return out
}

// writeAccountCredentialsFile 把粘贴的 credentials.toml 落进状态目录
// 的 account-credentials/<name>.toml（0600 凭据件、0700 目录）；
// name 已过账号名正则，路径无注入面。返回绝对路径供行 CredentialsFile
// 置位。同名的覆盖写语义顺带给 Update 复用（改内容=改文件）。
func writeAccountCredentialsFile(stateDir, name, content string) (string, error) {
	dir := filepath.Join(stateDir, "account-credentials")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// findAccountRow 按名找库行（含墓碑）；无行返回 nil。
func findAccountRow(rows []*store.AccountRow, name string) *store.AccountRow {
	for _, row := range rows {
		if row.Name == name {
			return row
		}
	}
	return nil
}

// synthesizedAccount 按名找干跑校验后的条目；调用方保证名在候选集
// 内（刚写入的行非墓碑必在），找不到是装配 bug，直接暴露。
func synthesizedAccount(synthesized []config.DevinAccountConfig, name string) config.DevinAccountConfig {
	for _, acc := range synthesized {
		if acc.Name == name {
			return acc
		}
	}
	return config.DevinAccountConfig{}
}

// replaceAccountRow 返回把 rows 里同名行换成 row（无同名则追加）的新
// 切片——构造「写入后」的行集喂干跑校验，不碰库里旧行。
func replaceAccountRow(rows []*store.AccountRow, row *store.AccountRow) []*store.AccountRow {
	out := make([]*store.AccountRow, 0, len(rows)+1)
	replaced := false
	for _, existing := range rows {
		if existing.Name == row.Name {
			out = append(out, row)
			replaced = true
		} else {
			out = append(out, existing)
		}
	}
	if !replaced {
		out = append(out, row)
	}
	return out
}

// rollbackAccountRow 在重推失败后把行写回 apply 前快照（快照 nil 即
// 「写前无行」→ 物理删）。回滚失败只告警：重推错误本身已回传给
// 操作者，残留行会在下一次成功重推时被同一规则重新评价。
func rollbackAccountRow(ctx context.Context, db *store.Store, name string, old *store.AccountRow) {
	var err error
	if old == nil {
		err = db.DeleteAccount(ctx, name)
	} else {
		err = db.UpsertAccount(ctx, old)
	}
	if err != nil {
		slog.Warn("account row rollback failed", "name", name, "error", err)
	}
}

// declaredAccount 报 name 是否被 config.yaml 声明——声明名即便没有
// overlay 行也占着「已存在」语义：面板建同名号必须走 Update 覆盖
// 路径，不能 Create 出第二条身份。
func declaredAccount(cfg config.Config, name string) bool {
	for _, acc := range cfg.Devin.Accounts {
		if acc.Name == name {
			return true
		}
	}
	return false
}
