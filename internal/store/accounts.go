package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/config"
)

// AccountRow 是 upstream_accounts 表一行的领域形状。token/
// credentials_file 可空（NULL→""）；deleted=1 是墓碑——压住 config
// 同名声明，等 config 撤名后由 GC 物理收掉。时间一律 unix 毫秒，
// 对齐 TokenRow 口径。priority/max_rpm 是 *int64——NULL 语义是「无行
// 覆盖」，merge 时回落 config 值；notes 照 nullAccountField 惯例
// ""↔NULL。
type AccountRow struct {
	Name            string
	Token           string
	CredentialsFile string // 写入时已锚定为绝对路径
	Disabled        bool
	Deleted         bool
	Priority        *int64 // 池级排序元数据，nil=无覆盖
	MaxRPM          *int64 // 该号自己的分钟窗口配额，nil=继承全局
	Notes           string // 面板侧自由注解，config 无对应字段
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
	Priority        int    // 行 ?? config ?? 0
	MaxRPM          int    // 行 ?? config ?? 0（0=继承全局 max_rpm）
	Notes           string // 仅行值（config 无该字段）
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
			Priority:        decl.Priority,
			MaxRPM:          decl.MaxRPM,
			Source:          AccountSourceConfig,
			ConfigDeclared:  true,
		}
		if row, ok := byName[decl.Name]; ok {
			acc.HasRow = true
			acc.Disabled = row.Disabled
			acc.CreatedAt = row.CreatedAt
			acc.UpdatedAt = row.UpdatedAt
			// priority/max_rpm/notes 按「行覆盖恒赢」解析，墓碑行同受：
			// restore 复活的就是这套行值，视图如实透出。notes 无
			// config 对应物，行值即注解本体。
			acc.Notes = row.Notes
			if row.Priority != nil {
				acc.Priority = int(*row.Priority)
			}
			if row.MaxRPM != nil {
				acc.MaxRPM = int(*row.MaxRPM)
			}
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
			Priority:        accountInt64Or0(row.Priority),
			MaxRPM:          accountInt64Or0(row.MaxRPM),
			Notes:           row.Notes,
			Source:          AccountSourcePanel,
			HasRow:          true,
			CreatedAt:       row.CreatedAt,
			UpdatedAt:       row.UpdatedAt,
		})
	}
	return out
}

// 账号操作的领域错误（%w 包装携带名字）：ops 层（main）只产语义错误，
// HTTP 状态码映射归面板 handlers——同一条件在不同端点状态码不同
// （PUT tombstoned=409 而 quota/refresh tombstoned=404）。
var (
	// ErrAccountExists 重名（含 config 声明名与墓碑名——墓碑走 restore）。
	ErrAccountExists = errors.New("account already exists")
	// ErrAccountNotFound 名不在生效集（含死墓碑与 quota/refresh 视角的 tombstoned）。
	ErrAccountNotFound = errors.New("account not found")
	// ErrAccountTombstoned 写路径撞上墓碑名：先 restore 再改。
	ErrAccountTombstoned = errors.New("account is tombstoned")
	// ErrAccountNotTombstoned restore 撞上活号：无墓可还。
	ErrAccountNotTombstoned = errors.New("account is not tombstoned")
)

// accountColumnList 是 upstream_accounts 的全部列，INSERT/SELECT 共用
// 同一份列清单；token/credentials_file/priority/max_rpm/notes 可空，
// 读写两侧做零值/NULL 互转。
var accountColumnList = []string{
	"name", "token", "credentials_file", "disabled", "deleted", "created_at", "updated_at",
	"priority", "max_rpm", "notes",
}

var (
	accountColumns = strings.Join(accountColumnList, ", ")
	// created_at 不进 DO UPDATE——首插值永久保留，updated_at 恒刷成 now。
	accountUpsert = `INSERT INTO upstream_accounts(` + accountColumns + `) VALUES(` +
		placeholders(len(accountColumnList)) + `) ON CONFLICT(name) DO UPDATE SET
		token=excluded.token, credentials_file=excluded.credentials_file,
		disabled=excluded.disabled, deleted=excluded.deleted, updated_at=excluded.updated_at,
		priority=excluded.priority, max_rpm=excluded.max_rpm, notes=excluded.notes`
)

// nullAccountField 把 "" 落成 NULL：可空列的 NULL 语义是「无行覆盖」，
// 与空串区分（读侧 NULL→""，见 scanAccount）。
func nullAccountField(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullAccountInt64 是 priority/max_rpm 的写侧转换：nil 落 NULL。
func nullAccountInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// accountInt64Or0 是读侧反向转换：NULL 回落 0。
func accountInt64Or0(v *int64) int {
	if v == nil {
		return 0
	}
	return int(*v)
}

func scanAccount(row sqlScanner) (*AccountRow, error) {
	var a AccountRow
	var token, credentialsFile, notes sql.NullString
	var priority, maxRPM sql.NullInt64
	err := row.Scan(&a.Name, &token, &credentialsFile, &a.Disabled, &a.Deleted,
		&a.CreatedAt, &a.UpdatedAt, &priority, &maxRPM, &notes)
	if err != nil {
		return nil, err
	}
	a.Token = token.String
	a.CredentialsFile = credentialsFile.String
	a.Notes = notes.String
	if priority.Valid {
		a.Priority = &priority.Int64
	}
	if maxRPM.Valid {
		a.MaxRPM = &maxRPM.Int64
	}
	return &a, nil
}

// ListAccounts 返回全部行含墓碑，ORDER BY created_at, name。
func (s *Store) ListAccounts(ctx context.Context) ([]*AccountRow, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+accountColumns+` FROM upstream_accounts ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AccountRow
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount 按名取行；不存在时 ok=false。
func (s *Store) GetAccount(ctx context.Context, name string) (row *AccountRow, ok bool, err error) {
	a, err := scanAccount(s.ro.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM upstream_accounts WHERE name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return a, true, nil
}

// UpsertAccount INSERT ... ON CONFLICT(name) DO UPDATE；created_at 只在
// 首插写（row.CreatedAt 为 0 时取 now），updated_at 恒刷新成 now。整行
// 覆盖语义——调用方做部分字段补丁时须先读后写；token/credentials_file/
// notes 传 "" 落 NULL、priority/max_rpm 传 nil 落 NULL，即清掉行覆盖。
func (s *Store) UpsertAccount(ctx context.Context, row *AccountRow) error {
	now := time.Now().UnixMilli()
	created := row.CreatedAt
	if created == 0 {
		created = now
	}
	_, err := s.db.ExecContext(ctx, accountUpsert, row.Name,
		nullAccountField(row.Token), nullAccountField(row.CredentialsFile),
		row.Disabled, row.Deleted, created, now,
		nullAccountInt64(row.Priority), nullAccountInt64(row.MaxRPM),
		nullAccountField(row.Notes))
	return err
}

// DeleteAccount 物理删行（纯面板名与墓碑 GC 用）；名不存在是空操作。
func (s *Store) DeleteAccount(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM upstream_accounts WHERE name=?`, name)
	return err
}

// GCTombstonedAccounts 收死墓碑：deleted=1 AND name NOT IN declared
// （declared 为空时收掉全部墓碑）。只在重推成功后由装配层调，读路径
// 不做写。返回删除行数。
func (s *Store) GCTombstonedAccounts(ctx context.Context, declared []string) (int64, error) {
	query := `DELETE FROM upstream_accounts WHERE deleted=1`
	var args []any
	if len(declared) > 0 {
		query += ` AND name NOT IN (` + placeholders(len(declared)) + `)`
		for _, name := range declared {
			args = append(args, name)
		}
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// EffectiveAccounts 是读路径便捷封装：ListAccounts + MergeAccounts。
func (s *Store) EffectiveAccounts(ctx context.Context, declared []config.DevinAccountConfig) ([]ResolvedAccount, error) {
	rows, err := s.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	return MergeAccounts(declared, rows), nil
}
