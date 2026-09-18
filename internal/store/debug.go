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
	"fmt"
	"math"
	"strings"
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
// 可重写文件走这里。超阈值内容透明压缩（usize=解压前尺寸）。
func (s *Store) PutDebugFile(ctx context.Context, dir, name string, content []byte) error {
	stored, usize := EncodePayload(content)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// OR REPLACE 的计数增量 = 新行库存尺寸 − 被顶掉的旧行尺寸；旧行
	// 尺寸同事务先读，写连接串行化保证读到的是真实前驱。
	var old int64
	if err := tx.QueryRowContext(ctx,
		`SELECT LENGTH(content) FROM debug_files WHERE dir=? AND name=?`,
		dir, name).Scan(&old); err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO debug_files(dir, name, content, usize, updated_at) VALUES(?,?,?,?,?)`,
		dir, name, stored, usize, time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.debugBytes.Add(int64(len(stored)) - old)
	return nil
}

// PutDebugFileIfAbsent 只在 (dir,name) 不存在时写入——error.json 的
// first-write-wins：首个失败点最有诊断价值，覆盖语义由调用方表达。
func (s *Store) PutDebugFileIfAbsent(ctx context.Context, dir, name string, content []byte) error {
	stored, usize := EncodePayload(content)
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO debug_files(dir, name, content, usize, updated_at) VALUES(?,?,?,?,?)`,
		dir, name, stored, usize, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		s.debugBytes.Add(int64(len(stored)))
	}
	return nil
}

// ClaimDebugFile 是带占位语义的 IfAbsent 变体：无行时插入并返回
// true，已有行则原样保留并返回 false——目录分配把它当原子占位用
// （等价文件时代 mkdir 的 EEXIST），与 PutDebugFileIfAbsent 的差别
// 只在是否报告本次真正写入。
func (s *Store) ClaimDebugFile(ctx context.Context, dir, name string, content []byte) (claimed bool, err error) {
	stored, usize := EncodePayload(content)
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO debug_files(dir, name, content, usize, updated_at) VALUES(?,?,?,?,?)`,
		dir, name, stored, usize, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err == nil && n > 0 {
		s.debugBytes.Add(int64(len(stored)))
	}
	return n > 0, err
}

// appendChunkSQL 的 seq 由同一条 INSERT 内的子查询取 MAX(seq)+1——聚合
// 查询在无命中行时也返回一行，首个 chunk 得 seq=0。写连接池单连接
// 串行化下整条语句原子；同事务内连续执行时子查询读到本批已写行，
// seq 随批次单调递增。
const appendChunkSQL = `INSERT INTO debug_chunks(dir, name, seq, data, usize)
	SELECT ?, ?, COALESCE(MAX(seq), -1) + 1, ?, ?
	FROM debug_chunks WHERE dir=? AND name=?`

// DebugChunkRow 是一次跨目录批量追加里的单行：全局写 worker 把多个
// 在飞请求的刷写缓冲合并进一个事务，行内自带 dir 归属。data 内可含
// 多行已序列化记录，入库为一行 chunk。
type DebugChunkRow struct {
	Dir  string
	Name string
	Data []byte
}

// AppendDebugChunk 追加一行到 debug_chunks（AppendDebugChunks 的单条形态）。
func (s *Store) AppendDebugChunk(ctx context.Context, dir, name string, data []byte) error {
	return s.AppendDebugChunks(ctx, []DebugChunkRow{{Dir: dir, Name: name, Data: data}})
}

// AppendDebugChunks 把一批 chunk（可跨目录）合并进一个事务：每行
// INSERT 一次 commit/fsync 改为整批一次；任一失败整体回滚，调用方
// 保留全部缓冲下次重发，不会出现半截批次。
func (s *Store) AppendDebugChunks(ctx context.Context, rows []DebugChunkRow) error {
	return s.WriteDebugBatch(ctx, DebugBatch{Chunks: rows})
}

// DebugFileRow 是一次跨目录批量写入里的单行整文件：Stored 是调用方
// 经 EncodePayload 预编码的入库字节（Usize 与之配套），IfAbsent 对应
// INSERT OR IGNORE（error.json 的 first-write-wins），否则 OR REPLACE。
type DebugFileRow struct {
	Dir      string
	Name     string
	Stored   []byte
	Usize    int64
	IfAbsent bool
}

// DebugBatch 是一次跨目录合并事务的全部内容：整文件行、chunk 行、
// CAS 共享对象（Blobs/Refs——manifest 文件行引用的切块与其 ref 行，
// 与文件行同事务落库无孤儿窗口）、StripDirs 列出「剥 payload」的
// 目录（errors_only 收尾——除 meta.json/error.json 锚点外行全删：
// meta 留下作目录锚点，X-Request-Id 仍可解析、占位防同秒目录名复用
// 撞 logs.dir UNIQUE）、LogRows 是完成请求的 logs 摘要行。几类写入
// 同一事务提交：完成请求不再为 payload 冲刷、剥离与日志行各付一次
// commit。
type DebugBatch struct {
	Files     []DebugFileRow
	Chunks    []DebugChunkRow
	Blobs     []DebugBlobRow
	Refs      []DebugRefRow
	StripDirs []string
	LogRows   []*LogRow
	// Encoder 非空时 chunk 的 gzip 编码用它（调用方持有的复用编码器，
	// 如写 worker 的专属实例——调用串行化由调用方保证）；空走共享池。
	Encoder *PayloadEncoder
}

// WriteDebugBatch 把一个 DebugBatch 合并进一个事务——写 worker 的周期
// 冲刷把各在飞请求的暂存与完成收尾合批，一次 commit 覆盖全部本周期
// 写入。文件行内容须预编码（编码成本由调用方 goroutine 承担）；chunk
// 行仍带原文，在本函数内编码。删除排在插入之后：同事务内为被剥目录
// 暂存的 payload 行一并清除，meta/error 锚点不在删除谓词内。任一失败
// 整体回滚。
func (s *Store) WriteDebugBatch(ctx context.Context, batch DebugBatch) error {
	if len(batch.Files) == 0 && len(batch.Chunks) == 0 && len(batch.Blobs) == 0 && len(batch.Refs) == 0 && len(batch.StripDirs) == 0 && len(batch.LogRows) == 0 {
		return nil
	}
	// chunk 的 gzip 编码在事务外完成：EncodePayload 是纯 CPU 工作，留在
	// tx 内只会拉长写连接的占有窗口，对批内其它语句无任何意义。
	storedChunks := make([][]byte, len(batch.Chunks))
	chunkUsizes := make([]int64, len(batch.Chunks))
	for i, r := range batch.Chunks {
		if batch.Encoder != nil {
			storedChunks[i], chunkUsizes[i] = batch.Encoder.Encode(r.Data)
		} else {
			storedChunks[i], chunkUsizes[i] = EncodePayload(r.Data)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// delta 累计本事务对 payload 库存字节的净增量：OR REPLACE 取新旧
	// 行尺寸差（旧尺寸同事务先读），OR IGNORE 按 RowsAffected 实计，
	// 删除路径由 RETURNING 直接汇总被删行——计数器只随提交成功的
	// 真实变更走，回滚不记账。
	var delta int64
	for _, f := range batch.Files {
		if f.IfAbsent {
			res, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO debug_files(dir, name, content, usize, updated_at) VALUES(?,?,?,?,?)`,
				f.Dir, f.Name, f.Stored, f.Usize, time.Now().UnixMilli())
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n > 0 {
				delta += int64(len(f.Stored))
			}
			continue
		}
		var old int64
		if err := tx.QueryRowContext(ctx,
			`SELECT LENGTH(content) FROM debug_files WHERE dir=? AND name=?`,
			f.Dir, f.Name).Scan(&old); err != nil && err != sql.ErrNoRows {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO debug_files(dir, name, content, usize, updated_at) VALUES(?,?,?,?,?)`,
			f.Dir, f.Name, f.Stored, f.Usize, time.Now().UnixMilli()); err != nil {
			return err
		}
		delta += int64(len(f.Stored)) - old
	}
	for i, r := range batch.Chunks {
		if _, err := tx.ExecContext(ctx, appendChunkSQL, r.Dir, r.Name, storedChunks[i], chunkUsizes[i], r.Dir, r.Name); err != nil {
			return err
		}
		delta += int64(len(storedChunks[i]))
	}
	// CAS 共享对象与引用它们的文件行同事务：OR IGNORE 让跨目录/跨批
	// 重复的块自然去重，记账只随真实插入走——manifest+refs+blob 要么
	// 整体落库要么整体回滚，结构上无孤儿窗口。
	for _, b := range batch.Blobs {
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO debug_blobs(hash, content, usize, created_at) VALUES(?,?,?,?)`,
			b.Hash, b.Stored, b.Usize, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n > 0 {
			delta += int64(len(b.Stored))
		}
	}
	for _, r := range batch.Refs {
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO debug_chunk_refs(dir, name, hash) VALUES(?,?,?)`,
			r.Dir, r.Name, r.Hash)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n > 0 {
			delta += refRowBytes(r.Dir, r.Name)
		}
	}
	if len(batch.StripDirs) > 0 {
		args := make([]any, 0, len(batch.StripDirs))
		for _, dir := range batch.StripDirs {
			args = append(args, dir)
		}
		where := `dir IN (` + placeholders(len(batch.StripDirs)) + `) AND name NOT IN ('meta.json', 'error.json')`
		freed, err := deleteReturningBytes(ctx, tx,
			`DELETE FROM debug_files WHERE `+where+` RETURNING LENGTH(content)`, args...)
		if err != nil {
			return err
		}
		delta -= freed
		freed, err = deleteReturningBytes(ctx, tx,
			`DELETE FROM debug_chunks WHERE `+where+` RETURNING LENGTH(data)`, args...)
		if err != nil {
			return err
		}
		delta -= freed
		// 引用随行死：剥离删除文件行的同一 WHERE 原样套到 refs 表
		// （meta/error 没有 CAS 引用，谓词天然不伤锚点）。
		freed, err = deleteReturningBytes(ctx, tx,
			`DELETE FROM debug_chunk_refs WHERE `+where+` RETURNING LENGTH(dir)+LENGTH(name)+LENGTH(hash)`, args...)
		if err != nil {
			return err
		}
		delta -= freed
	}
	if len(batch.LogRows) > 0 {
		cells := map[cellDim]*cellVals{}
		errCells := map[errCellDim]int64{}
		causes := map[laneCauseDim]int64{}
		var maxID int64
		for _, row := range batch.LogRows {
			res, err := tx.ExecContext(ctx, logsBatchInsertSQL, logInsertArgs(row)...)
			if err != nil {
				return err
			}
			// OR IGNORE 跳过的重复行（dir 撞部分唯一索引）不记账——
			// 首个落库者已在它自己的事务里把贡献记进 rollup。
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				continue
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			addCellContrib(cells, errCells, row, id)
			addCauseContrib(causes, row)
			if id > maxID {
				maxID = id
			}
		}
		if maxID > 0 {
			if err := upsertCells(ctx, tx, cells, errCells); err != nil {
				return err
			}
			if err := upsertCauseCells(ctx, tx, causes); err != nil {
				return err
			}
			if err := setCellsWatermark(tx, maxID); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.debugBytes.Add(delta)
	return nil
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
	var usize int64
	err = s.ro.QueryRowContext(ctx,
		`SELECT usize, LENGTH(content) FROM debug_files WHERE dir=? AND name=?`,
		dir, name).Scan(&usize, &total)
	if err == nil {
		if usize == 0 {
			// 未压缩行维持截断读：SUBSTR 只取前缀，total 即库存尺寸。
			if err = s.ro.QueryRowContext(ctx,
				`SELECT SUBSTR(content, 1, ?) FROM debug_files WHERE dir=? AND name=?`,
				limit, dir, name).Scan(&data); err != nil {
				return nil, 0, false, err
			}
			return data, total, true, nil
		}
		// 压缩行不能 SUBSTR 截断（魔数前缀是帧头不是内容）：
		// 整读解压后 Go 侧截断，total 报解压前尺寸。
		var stored []byte
		if err = s.ro.QueryRowContext(ctx,
			`SELECT content FROM debug_files WHERE dir=? AND name=?`,
			dir, name).Scan(&stored); err != nil {
			return nil, 0, false, err
		}
		if data, err = s.decodeFilePayload(ctx, dir, stored, limit); err != nil {
			return nil, 0, false, err
		}
		if total = usize; int64(len(data)) > limit {
			data = data[:limit]
		}
		return data, total, true, nil
	}
	if err != sql.ErrNoRows {
		return nil, 0, false, err
	}

	var chunks int
	err = s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN usize > 0 THEN usize ELSE LENGTH(data) END), 0) FROM debug_chunks WHERE dir=? AND name=?`,
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
		if chunk, err = decodePayload(chunk); err != nil {
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

// decodeFilePayload 解压一个 debug_files 行的库存字节：gzip/raw 直通
// decodePayload；CAS manifest 走 refs→blobs 重组（limit 只取前缀所需
// 的块）；zstd 帧是以本目录 01 文件为字典的 delta 编码——先取 01 行
// 解出明文再作 dict 还原。基座行缺失（手动删行/写侧任务被 shed 而未
// 钉座）时返回显式错误：delta 帧没有字典解出来只能是乱码。
func (s *Store) decodeFilePayload(ctx context.Context, dir string, stored []byte, limit int64) ([]byte, error) {
	if hasCASManifestMagic(stored) {
		return s.decodeCASManifest(ctx, dir, stored, limit)
	}
	if !hasZstdMagic(stored) {
		return decodePayload(stored)
	}
	dict, err := s.decodeFileDict(ctx, dir)
	if err != nil {
		return nil, err
	}
	return decodePayloadDelta(stored, dict)
}

// decodeFileDict 取本目录 01 行的解后明文作 delta 字典：01 自身可能是
// CAS manifest（二期起的新形态），递归一层经 manifest 重组；gzip/raw
// 直通。基座行缺失或基座本身是 delta 帧（一期畸形行——delta 当字典是
// 嵌套错误）都返回显式错误。
func (s *Store) decodeFileDict(ctx context.Context, dir string) ([]byte, error) {
	var base []byte
	if err := s.ro.QueryRowContext(ctx,
		`SELECT content FROM debug_files WHERE dir=? AND name=?`, dir, deltaBaseFileName).Scan(&base); err != nil {
		return nil, fmt.Errorf("decode delta payload in %s: fetch base %q: %w", dir, deltaBaseFileName, err)
	}
	var dict []byte
	var err error
	switch {
	case hasCASManifestMagic(base):
		dict, err = s.decodeCASManifest(ctx, dir, base, math.MaxInt64)
	case hasZstdMagic(base):
		err = errDeltaNeedsBase
	default:
		dict, err = decodePayload(base)
	}
	if err != nil {
		return nil, fmt.Errorf("decode delta payload in %s: base %q: %w", dir, deltaBaseFileName, err)
	}
	return dict, nil
}

// DebugFileNames 返回目录内全部文件名（两表 UNION DISTINCT，按名排序）。
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
			SELECT name, CASE WHEN usize > 0 THEN usize ELSE LENGTH(content) END AS sz FROM debug_files WHERE dir=?
			UNION ALL
			SELECT name, CASE WHEN usize > 0 THEN usize ELSE LENGTH(data) END AS sz FROM debug_chunks WHERE dir=?
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
// 按库存尺寸计（压缩行的 content/data 是 gzip 帧长）——淘汰的目标是
// 磁盘占用，usize 的逻辑尺寸在这里不适用。refs 行按 dir 归属计入
// （ref 随文件行同生死，尺寸口径与删除 RETURNING 表达式同源）；
// 共享 blob 字节不属于任何目录，全局口径走 DebugBlobBytes。
func (s *Store) DebugDirSizes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT dir, SUM(sz) FROM (
			SELECT dir, LENGTH(content) AS sz FROM debug_files
			UNION ALL
			SELECT dir, LENGTH(data) AS sz FROM debug_chunks
			UNION ALL
			SELECT dir, LENGTH(dir)+LENGTH(name)+LENGTH(hash) AS sz FROM debug_chunk_refs
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

// DebugErrorSignatures 返回「含 error.json 的 dir → (error_stage,
// error_message)」——keep_error_dirs 按签名限帽的归并源。join 只取
// logs 表里归因已落的行；无对应 logs 行（413/飞行中/行已先删）的
// 目录缺席，调用方按各自独立签名处理。
func (s *Store) DebugErrorSignatures(ctx context.Context) (map[string][2]string, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT l.dir, l.error_stage, l.error_message FROM logs l
		JOIN (
			SELECT dir FROM debug_files WHERE name='error.json'
			UNION SELECT dir FROM debug_chunks WHERE name='error.json'
		) e ON e.dir = l.dir
		WHERE l.error_stage != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	sigs := map[string][2]string{}
	for rows.Next() {
		var dir string
		var sig [2]string
		if err := rows.Scan(&dir, &sig[0], &sig[1]); err != nil {
			return nil, err
		}
		sigs[dir] = sig
	}
	return sigs, rows.Err()
}

// DeleteDebugDir 删除一个目录在两表中的全部行；两表删除同事务提交。
func (s *Store) DeleteDebugDir(ctx context.Context, dir string) error {
	return s.deleteDebugRows(ctx, `dir=?`, dir)
}

// DeleteDebugDirsBefore 删除目录名字典序小于 prefix 的全部目录——
// retention_days 的落点：目录名内嵌 "20060102-150405" 时间戳，
// dir<cutoff 即「进入时刻早于界限」（同秒 -NN 后缀排在裸名之后，
// 字典序与时间序一致）。exclude 里的目录名豁免（keep_error_dirs
// 保护集对龄删同样生效）。
func (s *Store) DeleteDebugDirsBefore(ctx context.Context, dirPrefix string, exclude []string) error {
	where := `dir<?`
	args := []any{dirPrefix}
	if len(exclude) > 0 {
		where += ` AND dir NOT IN (` + placeholders(len(exclude)) + `)`
		for _, dir := range exclude {
			args = append(args, dir)
		}
	}
	return s.deleteDebugRows(ctx, where, args...)
}

// DebugDirSizesSplit 是 DebugDirSizes 的双口径版：一趟聚合同时返回各
// 目录的总库存字节与「剥到锚点」可释放的字节（名字不在 anchor 名单
// 中的行合计）——容量淘汰两相各用一份：第一相按 strippable 预估剥载
// 释放，第二相按剥后残值圈整删界。refs 行按 name 同源判定（ref 的
// name 是引用它的 manifest 文件名——manifest 非锚点，ref 随之可剥）。
func (s *Store) DebugDirSizesSplit(ctx context.Context, anchor []string) (total, strippable map[string]int64, err error) {
	ph := placeholders(len(anchor))
	args := make([]any, 0, len(anchor)*3)
	for i := 0; i < 3; i++ {
		for _, name := range anchor {
			args = append(args, name)
		}
	}
	rows, err := s.ro.QueryContext(ctx,
		`SELECT dir, SUM(sz), SUM(CASE WHEN is_anchor=1 THEN 0 ELSE sz END) FROM (
			SELECT dir, LENGTH(content) AS sz, (name IN (`+ph+`)) AS is_anchor FROM debug_files
			UNION ALL
			SELECT dir, LENGTH(data), (name IN (`+ph+`)) FROM debug_chunks
			UNION ALL
			SELECT dir, LENGTH(dir)+LENGTH(name)+LENGTH(hash), (name IN (`+ph+`)) FROM debug_chunk_refs
		) GROUP BY dir`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	total, strippable = map[string]int64{}, map[string]int64{}
	for rows.Next() {
		var dir string
		var sz, st int64
		if err := rows.Scan(&dir, &sz, &st); err != nil {
			return nil, nil, err
		}
		total[dir], strippable[dir] = sz, st
	}
	return total, strippable, rows.Err()
}

// StripDebugDirsBefore 把目录名小于 bound、且不在 exclude 名单中的目录
// 剥到归因锚点：删除名字不在 keep 中的全部行——容量淘汰第一相的集合
// 形态，files/chunks/refs 三表共用同一 WHERE（ref 的 name 是 manifest
// 文件名，manifest 非锚点故引用随行死，锚点文件本身没有 CAS 引用）。
// 语义与 WriteDebugBatch 的 StripDirs 一致（errors_only 剥载同款）。
func (s *Store) StripDebugDirsBefore(ctx context.Context, bound string, keep, exclude []string) error {
	where := `dir<? AND name NOT IN (` + placeholders(len(keep)) + `)`
	args := []any{bound}
	for _, name := range keep {
		args = append(args, name)
	}
	if len(exclude) > 0 {
		where += ` AND dir NOT IN (` + placeholders(len(exclude)) + `)`
		for _, dir := range exclude {
			args = append(args, dir)
		}
	}
	return s.deleteDebugRows(ctx, where, args...)
}

// DeleteDebugPayloadsBefore 删除目录名小于 bound 的全部目录中、名字
// 命中 exact 精确名或 prefixes 前缀（GLOB 词干）的行——payload_hours
// 剥离的集合化形态：一条 DELETE 替代逐目录枚举+删除的 N+1。
// bound 由调用方先钳位到活跃集最小名之下（请求在写的目录不剥）。
func (s *Store) DeleteDebugPayloadsBefore(ctx context.Context, bound string, exact, prefixes []string) error {
	if len(exact) == 0 && len(prefixes) == 0 {
		return nil
	}
	args := []any{bound}
	names := make([]string, 0, len(prefixes)+1)
	if len(exact) > 0 {
		names = append(names, `name IN (`+placeholders(len(exact))+`)`)
		for _, name := range exact {
			args = append(args, name)
		}
	}
	for _, prefix := range prefixes {
		names = append(names, `name GLOB ?`)
		args = append(args, prefix+"*")
	}
	return s.deleteDebugRows(ctx, `dir<? AND (`+strings.Join(names, ` OR `)+`)`, args...)
}

// deleteChunkDirs 是单个删除事务覆盖的目录数上界：片内命中行一次删完
// 提交、让出唯一写连接，claimDir/批量写等请求路径在片间插队。目录界
// 单调推进、片级提交即进度——中断（错误/重启）后下一轮按原谓词重枚举
// 自然续删，无需额外簿记。
const deleteChunkDirs = 200

// deleteDebugRows 对三张行表执行同一 WHERE 的分片删除：先在读池把命中
// 目录枚举成有序快照，再按 deleteChunkDirs 目录一片逐片在写事务内删除
// 提交，代替原先三表无界 DELETE 挤占唯一写连接一整轮。删除范围用
// dir IN 名单精确圈定：期间新建目录名按时间序排在快照末尾之后、且
// 不在名单内，不会被中途误删。RETURNING 顺带汇总被删行的库存字节：
// payload 计数器按各片真实提交减量，中途失败时已完成片不回滚、计数
// 已落账。refs 表与文件行共用 WHERE 是 CAS 引用随行的落点——ref 行
// 无独立生命周期。
func (s *Store) deleteDebugRows(ctx context.Context, where string, args ...any) error {
	enumArgs := make([]any, 0, len(args)*3)
	for i := 0; i < 3; i++ {
		enumArgs = append(enumArgs, args...)
	}
	rows, err := s.ro.QueryContext(ctx,
		`SELECT dir FROM debug_files WHERE `+where+`
		UNION SELECT dir FROM debug_chunks WHERE `+where+`
		UNION SELECT dir FROM debug_chunk_refs WHERE `+where+`
		ORDER BY dir`, enumArgs...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var dirs []string
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return err
		}
		dirs = append(dirs, dir)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := 0; i < len(dirs); i += deleteChunkDirs {
		chunk := dirs[i:min(i+deleteChunkDirs, len(dirs))]
		delWhere := where + ` AND dir IN (` + placeholders(len(chunk)) + `)`
		delArgs := make([]any, 0, len(args)+len(chunk))
		delArgs = append(delArgs, args...)
		for _, dir := range chunk {
			delArgs = append(delArgs, dir)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var freed int64
		for _, del := range []struct{ table, sizeExpr string }{
			{"debug_files", "LENGTH(content)"},
			{"debug_chunks", "LENGTH(data)"},
			{"debug_chunk_refs", "LENGTH(dir)+LENGTH(name)+LENGTH(hash)"},
		} {
			n, err := deleteReturningBytes(ctx, tx,
				`DELETE FROM `+del.table+` WHERE `+delWhere+` RETURNING `+del.sizeExpr, delArgs...)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			freed += n
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.debugBytes.Add(-freed)
	}
	return nil
}

// deleteReturningBytes 执行一条带 RETURNING 长度列的 DELETE 并返回
// 被删行的库存字节合计。
func deleteReturningBytes(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var total int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, rows.Err()
}

// DebugPayloadBytes 返回 debug payload 库存字节合计的内存镜像——与
// DebugDirSizes 各目录值之和同口径（库存字节，压缩行计 gzip 帧长）。
// 派生态：权威值是表内聚合，cleaner 以周期对账修漂。
func (s *Store) DebugPayloadBytes() int64 {
	return s.debugBytes.Load()
}

// ReconcileDebugPayloadBytes 用权威总量重置内存计数器，返回重置前
// 读数与权威值之差（正=计数高估，负=低估）——供 cleaner 对账记录漂移。
func (s *Store) ReconcileDebugPayloadBytes(actual int64) (drift int64) {
	return s.debugBytes.Swap(actual) - actual
}

// DebugBlobBytes 返回 CAS 共享 blob 的库存字节合计——全局口径的
// 另一部分（目录口径见 DebugDirSizes）：blob 被 refs 跨目录共享，
// 不属于任何单一目录，淘汰闸门与计数器对账按 Σdir+blob 读总账。
func (s *Store) DebugBlobBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.ro.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(LENGTH(content)),0) FROM debug_blobs`).Scan(&n)
	return n, err
}

// blobReapGrace 是 blob 插入到可被 mark-sweep 收尸的宽限期：blob 与
// 引用它的 ref 行当前同批事务落库本没有孤儿窗口，宽限是为「先写
// blob、ref 后继批次再补」的演进留的保险——被误杀代价（manifest
// 指向失踪 blob）远高于滞后回收代价。
const blobReapGrace = 10 * time.Minute

// ReapOrphanBlobs 做 CAS 的 mark-sweep 收尸，两个 sweep 同一事务：
// 先扫悬垂 ref——引用即行的保活前提是「(dir,name) 文件行还是
// manifest」，文件行已死（回滚期旧二进制删 dir 不知 refs 表的泄漏
// 路径）或被覆写/剥离成非 manifest（OR IGNORE 重放等）时 ref 已死，
// 留着只会让 blob 假活。再删过宽限期仍无引用的 blob。写连接单线程
// 串行化：NOT EXISTS 判定到删除提交之间没有并发写者能插入新引用
// 撞破它。计数器按真实删除减量。挂在 Maintain 的周期养护里——对象
// 是表不是目录，节奏与淘汰 tick 同量级即可，共享字节滞后释放。
func (s *Store) ReapOrphanBlobs(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var freed int64
	n, err := deleteReturningBytes(ctx, tx,
		`DELETE FROM debug_chunk_refs WHERE NOT EXISTS (
			SELECT 1 FROM debug_files
			WHERE debug_files.dir = debug_chunk_refs.dir AND debug_files.name = debug_chunk_refs.name
			AND substr(debug_files.content, 1, ?) = ?
		) RETURNING LENGTH(dir)+LENGTH(name)+LENGTH(hash)`, len(casMagic), casMagic)
	if err != nil {
		return err
	}
	freed += n
	grace := time.Now().Add(-blobReapGrace).UnixMilli()
	n, err = deleteReturningBytes(ctx, tx,
		`DELETE FROM debug_blobs WHERE created_at < ? AND NOT EXISTS (
			SELECT 1 FROM debug_chunk_refs WHERE debug_chunk_refs.hash = debug_blobs.hash
		) RETURNING LENGTH(content)`, grace)
	if err != nil {
		return err
	}
	freed += n
	if err := tx.Commit(); err != nil {
		return err
	}
	s.debugBytes.Add(-freed)
	return nil
}
