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
	"log/slog"
	"math"
	"strconv"
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
	// OR REPLACE 的计数增量 = 新行库存尺寸 − 被顶掉的旧行尺寸；旧行
	// 尺寸同事务先读，写连接串行化保证读到的是真实前驱。先读后写在
	// deferred 下有 BUSY_SNAPSHOT 裸露面（升级写锁撞上过期快照），
	// BEGIN IMMEDIATE 先取写锁再读——与 WriteDebugBatch 同形。
	var delta int64
	err := immediateTx(ctx, s.db.DB, "PutDebugFile", func(ctx context.Context, q dbtx) error {
		var old int64
		if err := q.QueryRowContext(ctx,
			`SELECT LENGTH(content) FROM debug_files WHERE dir=? AND name=?`,
			dir, name).Scan(&old); err != nil && err != sql.ErrNoRows {
			return err
		}
		if _, err := q.ExecContext(ctx,
			`INSERT OR REPLACE INTO debug_files(dir, name, content, usize, updated_at) VALUES(?,?,?,?,?)`,
			dir, name, stored, usize, time.Now().UnixMilli()); err != nil {
			return err
		}
		delta = int64(len(stored)) - old
		return addPayloadBytes(ctx, q, delta)
	})
	if err != nil {
		return err
	}
	s.debugBytes.Add(delta)
	return nil
}

// ClaimDebugFile 是带占位语义的 IfAbsent 变体：无行时插入并返回
// true，已有行则原样保留并返回 false——目录分配把它当原子占位用
// （等价文件时代 mkdir 的 EEXIST），也是 error.json 的
// first-write-wins：首个失败点最有诊断价值，覆盖语义由调用方表达。
func (s *Store) ClaimDebugFile(ctx context.Context, dir, name string, content []byte) (claimed bool, err error) {
	stored, usize := EncodePayload(content)
	tx, done, err := s.writeTx(ctx, "ClaimDebugFile")
	if err != nil {
		return false, err
	}
	defer done()
	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO debug_files(dir, name, content, usize, updated_at) VALUES(?,?,?,?,?)`,
		dir, name, stored, usize, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	var delta int64
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n > 0 {
		delta = int64(len(stored))
		claimed = true
	}
	if err := addPayloadBytes(ctx, tx, delta); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.debugBytes.Add(delta)
	return claimed, nil
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

// appendDebugChunk 追加一行到 debug_chunks（AppendDebugChunks 的单条形态，
// 仅同包测试用——单行便捷壳不进公共 API 面）。
func (s *Store) appendDebugChunk(ctx context.Context, dir, name string, data []byte) error {
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
	// delta 累计本事务对 payload 库存字节的净增量：OR REPLACE 取新旧
	// 行尺寸差（旧尺寸同事务先读），OR IGNORE 按 RowsAffected 实计，
	// 删除路径由 RETURNING 直接汇总被删行——计数器只随提交成功的
	// 真实变更走，回滚不记账。
	var delta int64
	// BEGIN IMMEDIATE 而非 deferred：非 IfAbsent 文件行的 LENGTH 预读
	// 会让 deferred 事务持 WAL 读快照进写升级，跨进程并发提交（交接期
	// 在役实例、外部 sqlite3）推进 WAL 末尾即 SQLITE_BUSY_SNAPSHOT
	// 快败——本批是冲刷 tick 上最频的多语句写事务，正是该物种的头号
	// 暴露面。IMMEDIATE 在 BEGIN 即取写锁，读之前没有快照可过期。
	err := immediateTx(ctx, s.db.DB, "WriteDebugBatch", func(ctx context.Context, q dbtx) error {
		for _, f := range batch.Files {
			if f.IfAbsent {
				res, err := q.ExecContext(ctx,
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
			if err := q.QueryRowContext(ctx,
				`SELECT LENGTH(content) FROM debug_files WHERE dir=? AND name=?`,
				f.Dir, f.Name).Scan(&old); err != nil && err != sql.ErrNoRows {
				return err
			}
			if _, err := q.ExecContext(ctx,
				`INSERT OR REPLACE INTO debug_files(dir, name, content, usize, updated_at) VALUES(?,?,?,?,?)`,
				f.Dir, f.Name, f.Stored, f.Usize, time.Now().UnixMilli()); err != nil {
				return err
			}
			delta += int64(len(f.Stored)) - old
		}
		for i, r := range batch.Chunks {
			if _, err := q.ExecContext(ctx, appendChunkSQL, r.Dir, r.Name, storedChunks[i], chunkUsizes[i], r.Dir, r.Name); err != nil {
				return err
			}
			delta += int64(len(storedChunks[i]))
		}
		// CAS 共享对象与引用它们的文件行同事务：OR IGNORE 让跨目录/跨批
		// 重复的块自然去重，记账只随真实插入走——manifest+refs+blob 要么
		// 整体落库要么整体回滚，结构上无孤儿窗口。
		for _, b := range batch.Blobs {
			res, err := q.ExecContext(ctx,
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
			res, err := q.ExecContext(ctx,
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
			freed, err := deleteReturningBytes(ctx, q,
				`DELETE FROM debug_files WHERE `+where+` RETURNING LENGTH(content)`, args...)
			if err != nil {
				return err
			}
			delta -= freed
			freed, err = deleteReturningBytes(ctx, q,
				`DELETE FROM debug_chunks WHERE `+where+` RETURNING LENGTH(data)`, args...)
			if err != nil {
				return err
			}
			delta -= freed
			// 引用随行死：剥离删除文件行的同一 WHERE 原样套到 refs 表
			// （meta/error 没有 CAS 引用，谓词天然不伤锚点）。
			freed, err = deleteReturningBytes(ctx, q,
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
				res, err := q.ExecContext(ctx, logsBatchInsertSQL, logInsertArgs(row)...)
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
				if err := upsertCells(ctx, q, cells, errCells); err != nil {
					return err
				}
				if err := upsertCauseCells(ctx, q, causes); err != nil {
					return err
				}
				if err := setCellsWatermark(ctx, q, maxID); err != nil {
					return err
				}
			}
		}
		return addPayloadBytes(ctx, q, delta)
	})
	if err != nil {
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
	return s.deleteDebugRows(ctx, "DeleteDebugDir", `dir=?`, dir)
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
	return s.deleteDebugRows(ctx, "DeleteDebugDirsBefore", where, args...)
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
	return s.deleteDebugRows(ctx, "StripDebugDirsBefore", where, args...)
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
	return s.deleteDebugRows(ctx, "DeleteDebugPayloadsBefore", `dir<? AND (`+strings.Join(names, ` OR `)+`)`, args...)
}

// deleteChunkDirs 是单个删除事务覆盖的目录数上界：片内命中行一次删完
// 提交、让出唯一写连接，claimDir/批量写等请求路径在片间插队。目录界
// 单调推进、片级提交即进度——中断（错误/重启）后下一轮按原谓词重枚举
// 自然续删，无需额外簿记。
const deleteChunkDirs = 200

// deleteChunkBytes 是单个删除事务覆盖的命中字节上界：目录界对 payload
// 密度无感——实测典型片 ~19MB 占写连接 1-3s，但 200 个附件肥厚目录
// 同片可达 ~500MB、占连接 10-30s。32MiB 约是典型片的 1.7×，按实测
// 删除速率 ~6-19MB/s 折合 2-5s 占用，最劣档仍是个位数秒——与请求
// 路径能容忍的写连接排队窗口同量级；64MiB 在最劣速率下会摸到 ~10s。
const deleteChunkBytes = 32 << 20

// dirSize 是删除枚举的一行：目录名与 WHERE 命中行的库存字节合计
// （与删除 RETURNING 的尺寸表达式同口径）。它度量的是本谓词将删出的
// 体量——剥载谓词下天然只计可剥部分，比目录总账更准。
type dirSize struct {
	dir  string
	size int64
}

// splitDeleteChunks 把按名序枚举的目录序列切成删除片：目录数与命中
// 字节任一上界先到即切片，写连接单次占用被双界钳住。目录是原子单位
// ——单片至少装一个目录；单目录自身越字节界时独占一片，行不拆片
// （同目录的删除必须在一个事务内原子完成）。
func splitDeleteChunks(dirs []dirSize) [][]string {
	var chunks [][]string
	var cur []string
	var curBytes int64
	for _, d := range dirs {
		if len(cur) > 0 && (len(cur) >= deleteChunkDirs || curBytes+d.size > deleteChunkBytes) {
			chunks = append(chunks, cur)
			cur, curBytes = nil, 0
		}
		cur = append(cur, d.dir)
		curBytes += d.size
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// deleteDebugRows 对三张行表执行同一 WHERE 的分片删除：先在读池把命中
// 目录连同各自的命中字节枚举成有序快照（UNION ALL + GROUP BY，同一趟
// 三表扫描既给名单也给字节账），再按 splitDeleteChunks 的双界切片逐片
// 在写事务内删除提交，代替原先三表无界 DELETE 挤占唯一写连接一整轮。
// 删除范围用 dir IN 名单精确圈定：期间新建目录名按时间序排在快照末尾
// 之后、且不在名单内，不会被中途误删；字节账同样取自该快照，枚举后
// 追加的写只会让当片略肥、不失正确性。RETURNING 顺带汇总被删行的库存
// 字节：payload 计数器按各片真实提交减量，中途失败时已完成片不回滚、
// 计数已落账。refs 表与文件行共用 WHERE 是 CAS 引用随行的落点——ref
// 行无独立生命周期。
func (s *Store) deleteDebugRows(ctx context.Context, op, where string, args ...any) error {
	enumArgs := make([]any, 0, len(args)*3)
	for i := 0; i < 3; i++ {
		enumArgs = append(enumArgs, args...)
	}
	rows, err := s.ro.QueryContext(ctx,
		`SELECT dir, SUM(sz) FROM (
			SELECT dir, LENGTH(content) AS sz FROM debug_files WHERE `+where+`
			UNION ALL
			SELECT dir, LENGTH(data) FROM debug_chunks WHERE `+where+`
			UNION ALL
			SELECT dir, LENGTH(dir)+LENGTH(name)+LENGTH(hash) FROM debug_chunk_refs WHERE `+where+`
		) GROUP BY dir ORDER BY dir`, enumArgs...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var dirs []dirSize
	for rows.Next() {
		var d dirSize
		if err := rows.Scan(&d.dir, &d.size); err != nil {
			return err
		}
		dirs = append(dirs, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, chunk := range splitDeleteChunks(dirs) {
		delWhere := where + ` AND dir IN (` + placeholders(len(chunk)) + `)`
		delArgs := make([]any, 0, len(args)+len(chunk))
		delArgs = append(delArgs, args...)
		for _, dir := range chunk {
			delArgs = append(delArgs, dir)
		}
		tx, done, err := s.writeTx(ctx, op)
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
				done()
				return err
			}
			freed += n
		}
		if err := addPayloadBytes(ctx, tx, -freed); err != nil {
			done()
			return err
		}
		err = tx.Commit()
		done()
		if err != nil {
			return err
		}
		s.debugBytes.Add(-freed)
	}
	return nil
}

// deleteReturningBytes 执行一条带 RETURNING 长度列的 DELETE 并返回
// 被删行的库存字节合计。
func deleteReturningBytes(ctx context.Context, q dbtx, query string, args ...any) (int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
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

// debugPayloadBytesKey 是 runtime_state 里持久化 payload 计数器的键名：
// 与 debugBytes 内存镜像同源（四表库存字节合计），全部写/删路径在各自
// 事务内对它做净增量，Open 以它 O(1) 播种；行缺席时异步协程权威聚合
// 重建（debugPayloadBytesPendingKey 收编窗内增量），cleaner 的周期对账
// 兜底残余漂移。
const debugPayloadBytesKey = "debug_payload_bytes"

// debugPayloadBytesPendingKey 是异步播种期间的增量流水账键：计数器行
// 缺席的窗口里，写/删路径的净增量改记到它名下（与真实行同一条 UPDATE，
// 命中哪行记哪行），播种结算时按「现值−快照读数」折算回窗内增量后删除。
// 它只由播种协程建立与清除——行存在不代表已播种，crash 残留的行对下次
// 播种无害（两点差值抵消历史结余），周期对账顺带清尸。
const debugPayloadBytesPendingKey = "debug_payload_bytes_pending"

// addPayloadBytes 在 tx 内把净库存字节增量写进持久化计数器——与 payload
// 行变更同事务提交，崩溃不留半账。两行都缺席时为 no-op：下次权威聚合
// 重建会收进期间的全部净量（升级首启窗内的漏计由周期对账兜底）。
// 异步播种窗内（pending 行已建、真实行未落）增量记到 pending——同一条
// UPDATE 命中哪行记哪行，调用方不需要知道自己处在哪个播种相位。
func addPayloadBytes(ctx context.Context, q dbtx, delta int64) error {
	if delta == 0 {
		return nil
	}
	// TEXT 亲和列把整数结果存成十进制文本，读侧 ParseInt 还原。
	_, err := q.ExecContext(ctx,
		`UPDATE runtime_state SET value = CAST(value AS INTEGER) + ?, updated_at = ? WHERE "key" IN (?, ?)`,
		delta, time.Now().UnixMilli(), debugPayloadBytesKey, debugPayloadBytesPendingKey)
	return err
}

// seedDebugPayloadBytes 探测 payload 计数器的持久化行：存在且可解析时
// O(1) 读回（persisted=true）；行缺席或值损坏时返回未播种——权威聚合
// 由 Open 返回后的异步协程完成（seedPayloadBytesAsync），不在启动路径
// 上付全表扫描（5.6GB 库实测 ~26s，曾挡住就绪）。
func seedDebugPayloadBytes(db *sql.DB) (total int64, persisted bool, err error) {
	var raw string
	err = db.QueryRow(`SELECT value FROM runtime_state WHERE "key"=?`, debugPayloadBytesKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read payload bytes counter: %w", err)
	}
	if total, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
		return total, true, nil
	}
	return 0, false, nil
}

// payloadSeedBudget 给异步播种协程的墙钟上限：聚合是 GB 级库上的一次性
// 全扫，量级秒到几十秒；超时取消留 pending 行，由下次启动重播或对账收敛。
const payloadSeedBudget = 10 * time.Minute

// seedPayloadBytesAsync 在计数器行缺席时后台重建它：先立 pending 行收编
// 窗内增量，再在 ro 读池的单快照里跑四表权威聚合（读快照不占写连接，
// 聚合时长不阻塞写者），最后在一条写事务里结算落库。结算公式
// final = 快照真值 + pending现值 − pending快照读数：pending 从建行起
// 累计全部净增量，两点差值恰是「快照时刻→结算时刻」窗内、快照收不进
// 的那部分增量——写者无需感知播种相位，增量恒精确入账。real 行落地与
// pending 删除同事务提交，崩溃只留「无 real」态，半成品不会被当真值读。
func (s *Store) seedPayloadBytesAsync(ctx context.Context) {
	defer close(s.seedDone)
	ctx, cancel := context.WithTimeout(ctx, payloadSeedBudget)
	defer cancel()
	start := time.Now()
	// INSERT OR IGNORE 而非重置：上任崩溃残留的 pending 值不丢，续种
	// 结算取两点差值时历史结余自动抵消；并发同版本二进程播种同理。
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO runtime_state("key", value, updated_at) VALUES(?,?,?)`,
		debugPayloadBytesPendingKey, "0", time.Now().UnixMilli()); err != nil {
		slog.Warn("payload seed: create pending marker failed", "error", err)
		return
	}
	conn, err := s.ro.Conn(ctx)
	if err != nil {
		slog.Warn("payload seed: acquire read conn failed", "error", err)
		return
	}
	rtx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = conn.Close()
		slog.Warn("payload seed: begin snapshot failed", "error", err)
		return
	}
	var snapshot, pendingAtSnap int64
	err = rtx.QueryRowContext(ctx,
		`SELECT
			(SELECT COALESCE(SUM(LENGTH(content)),0) FROM debug_files) +
			(SELECT COALESCE(SUM(LENGTH(data)),0) FROM debug_chunks) +
			(SELECT COALESCE(SUM(LENGTH(content)),0) FROM debug_blobs) +
			(SELECT COALESCE(SUM(LENGTH(dir)+LENGTH(name)+LENGTH(hash)),0) FROM debug_chunk_refs)`).Scan(&snapshot)
	if err == nil {
		// pending 行先于快照建立，同事务读回必存在。
		err = rtx.QueryRowContext(ctx,
			`SELECT CAST(value AS INTEGER) FROM runtime_state WHERE "key"=?`,
			debugPayloadBytesPendingKey).Scan(&pendingAtSnap)
	}
	_ = rtx.Commit()
	_ = conn.Close()
	if err != nil {
		slog.Warn("payload seed: snapshot aggregate failed", "error", err)
		return
	}
	tx, done, err := s.writeTx(ctx, "SeedDebugPayloadBytes")
	if err != nil {
		slog.Warn("payload seed: begin finalize failed", "error", err)
		return
	}
	var pendingNow int64
	err = tx.QueryRowContext(ctx,
		`SELECT CAST(value AS INTEGER) FROM runtime_state WHERE "key"=?`,
		debugPayloadBytesPendingKey).Scan(&pendingNow)
	if err == sql.ErrNoRows {
		// pending 被对账先行收编（real 行已存在）——采用既有值即可，
		// 不覆写：对账写的是另一时刻的权威值，语义等价。
		var existing int64
		err = tx.QueryRowContext(ctx,
			`SELECT CAST(value AS INTEGER) FROM runtime_state WHERE "key"=?`,
			debugPayloadBytesKey).Scan(&existing)
		done()
		if err != nil {
			slog.Warn("payload seed: read reconciled counter failed", "error", err)
			return
		}
		s.debugBytes.Store(existing)
		slog.Info("debug payload bytes seed adopted reconciled value", "debug_payload_bytes", existing)
		return
	}
	if err != nil {
		done()
		slog.Warn("payload seed: read pending failed", "error", err)
		return
	}
	final := snapshot + pendingNow - pendingAtSnap
	if _, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO runtime_state("key", value, updated_at) VALUES(?,?,?)`,
		debugPayloadBytesKey, strconv.FormatInt(final, 10), time.Now().UnixMilli()); err == nil {
		_, err = tx.ExecContext(ctx,
			`DELETE FROM runtime_state WHERE "key"=?`, debugPayloadBytesPendingKey)
	}
	if err == nil {
		err = tx.Commit()
	}
	done()
	if err != nil {
		slog.Warn("payload seed: finalize failed", "error", err)
		return
	}
	s.debugBytes.Store(final)
	slog.Info("debug payload bytes seeded async",
		"debug_payload_bytes", final, "duration_ms", time.Since(start).Milliseconds())
}

// DebugPayloadBytes 返回 debug payload 库存字节合计的内存镜像——与
// DebugDirSizes 各目录值之和同口径（库存字节，压缩行计 gzip 帧长）。
// 派生态：权威值是表内聚合，cleaner 以周期对账修漂。
func (s *Store) DebugPayloadBytes() int64 {
	return s.debugBytes.Load()
}

// ReconcileDebugPayloadBytes 用权威总量重置计数器（持久化行与内存镜像
// 一起校正），返回重置前内存读数与权威值之差（正=计数高估，负=低估）——
// 供 cleaner 对账记录漂移。upsert 顺带补回被手删的计数器行，并清掉
// 异步播种半途残留的 pending 行（crash 僵尸继续吞增量会让内存镜像的
// 增量被收进死账）；持久化失败时内存镜像仍被校正，错误如实返回由调用方
// 告警。
func (s *Store) ReconcileDebugPayloadBytes(ctx context.Context, actual int64) (drift int64, err error) {
	tx, done, err := s.writeTx(ctx, "ReconcileDebugPayloadBytes")
	if err != nil {
		return s.debugBytes.Swap(actual) - actual, err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO runtime_state("key", value, updated_at) VALUES(?,?,?)`,
		debugPayloadBytesKey, strconv.FormatInt(actual, 10), time.Now().UnixMilli()); err == nil {
		_, err = tx.ExecContext(ctx,
			`DELETE FROM runtime_state WHERE "key"=?`, debugPayloadBytesPendingKey)
	}
	if err == nil {
		err = tx.Commit()
	}
	done()
	return s.debugBytes.Swap(actual) - actual, err
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

// reapRefChunkRows / reapBlobChunkRows 是 mark-sweep 单片事务的行数
// 上界：ref 行 ~40B，成本在逐行 NOT EXISTS 探测；blob 行含 ~64KB 切块
// 内容，页改动量主导——两者各取上界让单片占用远低于 claim 预算。
const (
	reapRefChunkRows  = 10000
	reapBlobChunkRows = 256
)

// ReapOrphanBlobs 做 CAS 的 mark-sweep 收尸，两个 sweep 各自分片
// （原先同一事务全量删除随 CAS 体量长到秒级，独占唯一写连接饿死
// claim/写路径）。先扫悬垂 ref——引用即行的保活前提是「(dir,name)
// 文件行还是 manifest」，文件行已死（回滚期旧二进制删 dir 不知
// refs 表的泄漏路径）或被覆写/剥离成非 manifest（OR IGNORE 重放
// 等）时 ref 已死，留着只会让 blob 假活。再删过宽限期仍无引用的
// blob。每片内谓词复检与删除同事务原子完成，等价原单事务的
// 「判定到提交无并发写者撞入」；片间中断下轮重枚举续删。挂在
// Maintain 的周期养护里——对象是表不是目录，节奏与淘汰 tick
// 同量级即可，共享字节滞后释放。
func (s *Store) ReapOrphanBlobs(ctx context.Context) error {
	danglingRef := `NOT EXISTS (
		SELECT 1 FROM debug_files
		WHERE debug_files.dir = debug_chunk_refs.dir AND debug_files.name = debug_chunk_refs.name
		AND substr(debug_files.content, 1, ?) = ?)`
	if err := s.reapWhere(ctx, "ReapOrphanBlobs.refs", "debug_chunk_refs", danglingRef,
		`LENGTH(dir)+LENGTH(name)+LENGTH(hash)`, reapRefChunkRows, len(casMagic), casMagic); err != nil {
		return err
	}
	grace := time.Now().Add(-blobReapGrace).UnixMilli()
	return s.reapWhere(ctx, "ReapOrphanBlobs.blobs", "debug_blobs",
		`created_at < ? AND NOT EXISTS (
			SELECT 1 FROM debug_chunk_refs WHERE debug_chunk_refs.hash = debug_blobs.hash
		)`, `LENGTH(content)`, reapBlobChunkRows, grace)
}

// reapWhere 分片执行一个 mark-sweep 删除：先在读池把命中谓词的候选
// rowid 枚举成快照（全表探扫不占写连接），再按 chunkRows 逐片在写
// 事务内删除——DELETE 带完整谓词复检：枚举快照过期（ref/blob 在枚举
// 后被重建、rowid 复用指向活行）时当片放生活行，只有仍满足死亡
// 条件的当前行才被删。片级提交即进度，计数器按各片真实删除减量。
func (s *Store) reapWhere(ctx context.Context, op, table, pred, sizeExpr string, chunkRows int, args ...any) error {
	rows, err := s.ro.QueryContext(ctx, `SELECT rowid FROM `+table+` WHERE `+pred, args...)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for i := 0; i < len(ids); i += chunkRows {
		chunk := ids[i:min(i+chunkRows, len(ids))]
		delArgs := make([]any, 0, len(args)+len(chunk))
		delArgs = append(delArgs, args...)
		for _, id := range chunk {
			delArgs = append(delArgs, id)
		}
		tx, done, err := s.writeTx(ctx, op)
		if err != nil {
			return err
		}
		freed, err := deleteReturningBytes(ctx, tx,
			`DELETE FROM `+table+` WHERE `+pred+` AND rowid IN (`+placeholders(len(chunk))+`) RETURNING `+sizeExpr,
			delArgs...)
		if err != nil {
			done()
			return err
		}
		if err := addPayloadBytes(ctx, tx, -freed); err != nil {
			done()
			return err
		}
		err = tx.Commit()
		done()
		if err != nil {
			return err
		}
		s.debugBytes.Add(-freed)
	}
	return nil
}
