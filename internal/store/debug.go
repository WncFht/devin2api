// 本文件是调试 payload（debug_files/debug_chunks 两表）的领域方法。
//
// 存储模型镜像文件时代的 logs/<dir>/ 目录：dir 是请求目录名（仍作
// X-Request-Id/debug_ref 身份），name 是目录内相对路径（顶层文件或
// attachments/xxx）。一次性整文件（meta.json、error.json、01-03、
// attachments/*）进 debug_files；流式 JSONL（04/05/06）按 flush 批
// 追加进 debug_chunks，读时 ORDER BY seq 拼接——追加行代替 BLOB
// 反复重写，飞行中请求的已提交前缀对面板实时可见，崩溃后已写帧不丢。
package store

import (
	"bytes"
	"context"
	"database/sql"
	"math"
	"time"
)

// DebugFileInfo 是目录内一个文件的清单项。
type DebugFileInfo struct {
	// Name 是相对请求目录的路径（顶层文件名或 attachments/xxx）。
	Name string
	// Size 是文件字节数；chunked 文件是全部 chunk 的合计。
	Size int64
}

// PutDebugFile 写 debug_files 一行（同名覆写）：meta.json 等
// 可重写文件走这里。
func (s *Store) PutDebugFile(ctx context.Context, dir, name string, content []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO debug_files(dir, name, content, updated_at) VALUES(?,?,?,?)`,
		dir, name, content, time.Now().UnixMilli())
	return err
}

// PutDebugFileIfAbsent 只在 (dir,name) 不存在时写入——error.json 的
// first-write-wins：首个失败点最有诊断价值，覆盖语义由调用方表达。
func (s *Store) PutDebugFileIfAbsent(ctx context.Context, dir, name string, content []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO debug_files(dir, name, content, updated_at) VALUES(?,?,?,?)`,
		dir, name, content, time.Now().UnixMilli())
	return err
}

// ClaimDebugFile 是带占位语义的 IfAbsent 变体：无行时插入并返回
// true，已有行则原样保留并返回 false——目录分配把它当原子占位用
// （等价文件时代 mkdir 的 EEXIST），与 PutDebugFileIfAbsent 的差别
// 只在是否报告本次真正写入。
func (s *Store) ClaimDebugFile(ctx context.Context, dir, name string, content []byte) (claimed bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO debug_files(dir, name, content, updated_at) VALUES(?,?,?,?)`,
		dir, name, content, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// appendChunkSQL 的 seq 由同一条 INSERT 内的子查询取 MAX(seq)+1——聚合
// 查询在无命中行时也返回一行，首个 chunk 得 seq=0。写连接池单连接
// 串行化下整条语句原子；同事务内连续执行时子查询读到本批已写行，
// seq 随批次单调递增。
const appendChunkSQL = `INSERT INTO debug_chunks(dir, name, seq, data)
	SELECT ?, ?, COALESCE(MAX(seq), -1) + 1, ?
	FROM debug_chunks WHERE dir=? AND name=?`

// DebugChunk 是一次刷写里单个 JSONL 文件的待追加批：data 内可含多行
// 已序列化记录，入库为一行 chunk。
type DebugChunk struct {
	Name string
	Data []byte
}

// AppendDebugChunk 追加一行到 debug_chunks（AppendDebugChunks 的单条形态）。
func (s *Store) AppendDebugChunk(ctx context.Context, dir, name string, data []byte) error {
	return s.AppendDebugChunks(ctx, dir, []DebugChunk{{Name: name, Data: data}})
}

// AppendDebugChunks 把一个刷写周期的全部 chunk 合并进一个事务：每行
// INSERT 一次 commit/fsync 改为整批一次；任一失败整体回滚，调用方
// 保留全部缓冲下次重发，不会出现半截批次。
func (s *Store) AppendDebugChunks(ctx context.Context, dir string, chunks []DebugChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, c := range chunks {
		if _, err := tx.ExecContext(ctx, appendChunkSQL, dir, c.Name, c.Data, dir, c.Name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DebugFile 读一个文件的内容：先查 debug_files（整文件），miss 则按
// seq 顺序拼接 debug_chunks。maxBytes>0 时只取前 maxBytes 字节（面板
// 4MB 截断读取的落点），total 恒为文件真实大小——调用方以
// total>len(data) 判截断。maxBytes<=0 表示不限。文件不存在返回
// ok=false。
func (s *Store) DebugFile(ctx context.Context, dir, name string, maxBytes int64) (data []byte, total int64, ok bool, err error) {
	limit := maxBytes
	if limit <= 0 {
		limit = math.MaxInt64
	}
	err = s.ro.QueryRowContext(ctx,
		`SELECT SUBSTR(content, 1, ?), LENGTH(content) FROM debug_files WHERE dir=? AND name=?`,
		limit, dir, name).Scan(&data, &total)
	if err == nil {
		return data, total, true, nil
	}
	if err != sql.ErrNoRows {
		return nil, 0, false, err
	}

	var chunks int
	err = s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(LENGTH(data)), 0) FROM debug_chunks WHERE dir=? AND name=?`,
		dir, name).Scan(&chunks, &total)
	if err != nil {
		return nil, 0, false, err
	}
	if chunks == 0 {
		return nil, 0, false, nil
	}
	rows, err := s.ro.QueryContext(ctx,
		`SELECT data FROM debug_chunks WHERE dir=? AND name=? ORDER BY seq`, dir, name)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = rows.Close() }()
	var buf bytes.Buffer
	for rows.Next() && int64(buf.Len()) < limit {
		var chunk []byte
		if err := rows.Scan(&chunk); err != nil {
			return nil, 0, false, err
		}
		if remain := limit - int64(buf.Len()); int64(len(chunk)) > remain {
			chunk = chunk[:remain]
		}
		buf.Write(chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	return buf.Bytes(), total, true, nil
}

// DebugFileNames 返回目录内全部文件名（两表 UNION DISTINCT，按名排序）——
// 剥离负载时按 isPayloadName 类谓词筛选的枚举源。
func (s *Store) DebugFileNames(ctx context.Context, dir string) ([]string, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT name FROM debug_files WHERE dir=? UNION SELECT name FROM debug_chunks WHERE dir=? ORDER BY name`,
		dir, dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// DebugFileList 返回目录内全部文件的名字与字节数，按名排序——Detail
// 端点的 Files 清单等价物。同名文件在两表并存时尺寸合并（正常写入
// 路径按扩展名分表，不会撞名）。
func (s *Store) DebugFileList(ctx context.Context, dir string) ([]DebugFileInfo, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT name, SUM(sz) FROM (
			SELECT name, LENGTH(content) AS sz FROM debug_files WHERE dir=?
			UNION ALL
			SELECT name, LENGTH(data) AS sz FROM debug_chunks WHERE dir=?
		) GROUP BY name ORDER BY name`,
		dir, dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var files []DebugFileInfo
	for rows.Next() {
		var f DebugFileInfo
		if err := rows.Scan(&f.Name, &f.Size); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// DebugDirs 返回库中全部调试目录名，按名排序——目录名内嵌
// "20060102-150405" 时间戳，字典序即时间序，清理器直接复用该序。
func (s *Store) DebugDirs(ctx context.Context) ([]string, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT dir FROM debug_files UNION SELECT dir FROM debug_chunks ORDER BY dir`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var dirs []string
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, err
		}
		dirs = append(dirs, dir)
	}
	return dirs, rows.Err()
}

// DebugDirsByPrefix 返回名以 prefix 开头的目录名（字典序）——dir 名
// 前 15 字符内嵌 "20060102-150405" 时间戳，按 started_at 毫秒反查
// 目录时先圈同秒候选，再逐目录比对 meta.json 消歧。GLOB 前缀可走
// 索引；调用方保证 prefix 不含通配符（时间戳格式只含数字与 '-'）。
func (s *Store) DebugDirsByPrefix(ctx context.Context, prefix string) ([]string, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT dir FROM debug_files WHERE dir GLOB ? UNION SELECT dir FROM debug_chunks WHERE dir GLOB ? ORDER BY dir`,
		prefix+"*", prefix+"*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var dirs []string
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, err
		}
		dirs = append(dirs, dir)
	}
	return dirs, rows.Err()
}

// DebugDirSizes 返回各目录内容字节数合计，供 max_total_mb 容量淘汰。
func (s *Store) DebugDirSizes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT dir, SUM(sz) FROM (
			SELECT dir, LENGTH(content) AS sz FROM debug_files
			UNION ALL
			SELECT dir, LENGTH(data) AS sz FROM debug_chunks
		) GROUP BY dir`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	sizes := map[string]int64{}
	for rows.Next() {
		var dir string
		var size int64
		if err := rows.Scan(&dir, &size); err != nil {
			return nil, err
		}
		sizes[dir] = size
	}
	return sizes, rows.Err()
}

// DebugDirsContaining 返回含指定文件名（如 error.json）的目录集合——
// keep_error_dirs 容量淘汰的保护集来源。文件与 chunk 两表都查。
func (s *Store) DebugDirsContaining(ctx context.Context, name string) (map[string]bool, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT dir FROM debug_files WHERE name=? UNION SELECT dir FROM debug_chunks WHERE name=?`,
		name, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	dirs := map[string]bool{}
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, err
		}
		dirs[dir] = true
	}
	return dirs, rows.Err()
}

// DeleteDebugDir 删除一个目录在两表中的全部行；两表删除同事务提交。
func (s *Store) DeleteDebugDir(ctx context.Context, dir string) error {
	return s.deleteDebugRows(ctx, `dir=?`, dir)
}

// DeleteDebugDirsBefore 删除目录名字典序小于 prefix 的全部目录——
// retention_days 的落点：目录名内嵌 "20060102-150405" 时间戳，
// dir<cutoff 即「进入时刻早于界限」（同秒 -NN 后缀排在裸名之后，
// 字典序与时间序一致）。
func (s *Store) DeleteDebugDirsBefore(ctx context.Context, dirPrefix string) error {
	return s.deleteDebugRows(ctx, `dir<?`, dirPrefix)
}

// DeleteDebugPayloadFiles 删除目录内指定名字的全部行（两表）——
// payload_hours 剥离的落点：调用方按负载名谓词（03 词干前缀、
// 04/06 精确名、attachments/ 前缀）从 DebugFileNames 筛出清单。
func (s *Store) DeleteDebugPayloadFiles(ctx context.Context, dir string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	inClause := `dir=? AND name IN (` + placeholders(len(names)) + `)`
	args := make([]any, 0, len(names)+1)
	args = append(args, dir)
	for _, name := range names {
		args = append(args, name)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM debug_files WHERE `+inClause, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM debug_chunks WHERE `+inClause, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteDebugRows 对两表执行同一 WHERE 的删除，单事务提交。
func (s *Store) deleteDebugRows(ctx context.Context, where string, args ...any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM debug_files WHERE `+where, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM debug_chunks WHERE `+where, args...); err != nil {
		return err
	}
	return tx.Commit()
}
