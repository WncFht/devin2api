package store

import (
	"context"
	"errors"
	"sort"

	"github.com/WncFht/devin2api/internal/config"
)

// AccountRow 是 upstream_accounts 表一行的领域形状。token/
// credentials_file 可空（NULL→""）；deleted=1 是墓碑——压住 config
// 同名声明，等 config 撤名后由 GC 物理收掉。时间一律 unix 毫秒，
// 对齐 TokenRow 口径。
type AccountRow struct {
	Name            string
	Token           string
	CredentialsFile string // 写入时已锚定为绝对路径
	Disabled        bool
	Deleted         bool
	CreatedAt       int64
	UpdatedAt       int64
}

// ResolvedAccount 是 config 声明集与 overlay 行 merge 后的生效视图，
// 不进库。Source 推导：deleted=1 && ConfigDeclared → tombstoned；
// !deleted && ConfigDeclared（可有覆盖行）→ config；
// !deleted && !ConfigDeclared → panel。死墓碑（deleted && !declared）
// 不进视图，由 GCTombstonedAccounts 收。
type ResolvedAccount struct {
	Name            string
	Token           string // 生效值：行非空用行，否则 config 值
	CredentialsFile string // 同上逐字段覆盖
	Disabled        bool   // 恒取行值（config 无该字段）
	Source          string // "config" | "panel" | "tombstoned"
	ConfigDeclared  bool
	HasRow          bool // 是否存在 overlay 行（含墓碑）
	CreatedAt       int64
	UpdatedAt       int64
}

// 账号来源标记，按 source 语义进 JSON 契约。
const (
	AccountSourceConfig     = "config"
	AccountSourcePanel      = "panel"
	AccountSourceTombstoned = "tombstoned"
)

// MergeAccounts 是 overlay 生效集的纯函数：declared 顺序起步，同名行
// 逐字段覆盖（行 token/credentials_file 非空覆盖，disabled 恒取行值），
// deleted=1 的同名项标 tombstoned，未声明的活行追加为 panel。干跑
// 校验与读路径共用，不写库。
func MergeAccounts(declared []config.DevinAccountConfig, rows []*AccountRow) []ResolvedAccount {
	byName := make(map[string]*AccountRow, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	out := make([]ResolvedAccount, 0, len(declared)+len(rows))
	seen := make(map[string]bool, len(declared))
	for _, decl := range declared {
		seen[decl.Name] = true
		acc := ResolvedAccount{
			Name:            decl.Name,
			Token:           decl.Token,
			CredentialsFile: decl.CredentialsFile,
			Source:          AccountSourceConfig,
			ConfigDeclared:  true,
		}
		if row, ok := byName[decl.Name]; ok {
			acc.HasRow = true
			acc.Disabled = row.Disabled
			acc.CreatedAt = row.CreatedAt
			acc.UpdatedAt = row.UpdatedAt
			if row.Deleted {
				acc.Source = AccountSourceTombstoned
			} else {
				if row.Token != "" {
					acc.Token = row.Token
				}
				if row.CredentialsFile != "" {
					acc.CredentialsFile = row.CredentialsFile
				}
			}
		}
		out = append(out, acc)
	}
	var panel []*AccountRow
	for _, row := range rows {
		if row.Deleted || seen[row.Name] {
			continue
		}
		panel = append(panel, row)
	}
	// panel 名按 created_at 再按 name 排，与 /admin/accounts 契约排序一致。
	sort.Slice(panel, func(i, j int) bool {
		if panel[i].CreatedAt != panel[j].CreatedAt {
			return panel[i].CreatedAt < panel[j].CreatedAt
		}
		return panel[i].Name < panel[j].Name
	})
	for _, row := range panel {
		out = append(out, ResolvedAccount{
			Name:            row.Name,
			Token:           row.Token,
			CredentialsFile: row.CredentialsFile,
			Disabled:        row.Disabled,
			Source:          AccountSourcePanel,
			HasRow:          true,
			CreatedAt:       row.CreatedAt,
			UpdatedAt:       row.UpdatedAt,
		})
	}
	return out
}

var errAccountsNotImplemented = errors.New("store: upstream_accounts not implemented")

// ListAccounts 返回全部行含墓碑，ORDER BY created_at, name。
func (s *Store) ListAccounts(ctx context.Context) ([]*AccountRow, error) {
	return nil, errAccountsNotImplemented
}

// GetAccount 按名取行；不存在时 ok=false。
func (s *Store) GetAccount(ctx context.Context, name string) (row *AccountRow, ok bool, err error) {
	return nil, false, errAccountsNotImplemented
}

// UpsertAccount INSERT ... ON CONFLICT(name) DO UPDATE；created_at 只在
// 首插写，updated_at 恒刷新。
func (s *Store) UpsertAccount(ctx context.Context, row *AccountRow) error {
	return errAccountsNotImplemented
}

// DeleteAccount 物理删行（纯面板名与墓碑 GC 用）。
func (s *Store) DeleteAccount(ctx context.Context, name string) error {
	return errAccountsNotImplemented
}

// GCTombstonedAccounts 收死墓碑：deleted=1 AND name NOT IN declared。
// 只在重推成功后由装配层调，读路径不做写。返回删除行数。
func (s *Store) GCTombstonedAccounts(ctx context.Context, declared []string) (int64, error) {
	return 0, errAccountsNotImplemented
}

// EffectiveAccounts 是读路径便捷封装：ListAccounts + MergeAccounts。
func (s *Store) EffectiveAccounts(ctx context.Context, declared []config.DevinAccountConfig) ([]ResolvedAccount, error) {
	rows, err := s.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	return MergeAccounts(declared, rows), nil
}
