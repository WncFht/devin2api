package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// debugImportChunkSize 是旧目录 .jsonl 文件切块入库的目标行体积。
// 与运行时单次 flush 批量同量级；每块尽量落在换行边界上，超长行
// 才硬切——sqlite3 里按行肉眼可查。
const debugImportChunkSize = 256 << 10

// requestDirPattern 镜像 debuglog.requestDirPattern（store 是被导入的
// 下层包，不能反向引用）：只有形如请求目录名的子目录才是可入库数据，
// logs/ 下的陌生目录原样留盘——它不是本服务写出的东西。
var requestDirPattern = regexp.MustCompile(`^\d{8}-\d{6}(-\d{2,})?$`)

// ImportDebugDirs 把 logRoot 下遗留的请求目录搬进 debug 两表后删除。
// 供 D3 换底后的启动后台任务调用，可在服务运行中执行——与在线写入
// 互不干扰（在线请求拿的是新时间戳目录名，不会撞上历史目录）。
//
// 断点续传：目录按名序（=时间序）逐个处理，每个目录的「文件行 +
// runtime_state[progressKey]=水线值」在同一事务提交；水线只前进不
// 后退。重跑不盲信水线：先查库，已入库的目录只剩删盘兜底；未入库
// 目录重新导入——包括水线之下被放回的旧名目录（备份恢复/手工拷贝），
// 否则它们会被静默删而从未入库。顶层文件（stderr.log、index.jsonl、
// *.migrated 等）与名字不匹配请求目录模式的子目录一律不碰。
func (s *Store) ImportDebugDirs(ctx context.Context, logRoot, progressKey string) error {
	entries, err := os.ReadDir(logRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	progress, _, err := s.GetState(ctx, progressKey)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		name := entry.Name()
		if !entry.IsDir() || !requestDirPattern.MatchString(name) {
			continue
		}
		dirPath := filepath.Join(logRoot, name)
		exists, err := s.debugDirExists(ctx, name)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		if !exists {
			// 单目录失败中断整轮：进度标记是水位线，越过失败目录
			// 继续前进会让它永远排在水线之下、再无重试机会。
			if err := s.importDebugDir(ctx, dirPath, name, progressKey, max(name, progress)); err != nil {
				return errors.Join(append(errs, fmt.Errorf("import %s: %w", name, err))...)
			}
		} else if name > progress {
			// 库里已有行的目录（进度标记丢失、或与在线写入撞名）
			// 跳过导入直接删盘，水线顺带推进。
			if err := s.SetState(ctx, progressKey, name); err != nil {
				return errors.Join(append(errs, err)...)
			}
		}
		if name > progress {
			progress = name
		}
		// 已入库目录走到这里只剩删盘；删除失败不阻断后续目录，
		// 下次启动会重删。
		if err := os.RemoveAll(dirPath); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// debugDirExists 报告目录在两表中是否已有任何行。
func (s *Store) debugDirExists(ctx context.Context, dir string) (bool, error) {
	var n int
	err := s.ro.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM debug_files WHERE dir=?) OR EXISTS(SELECT 1 FROM debug_chunks WHERE dir=?)`,
		dir, dir).Scan(&n)
	return n != 0, err
}

// importDebugDir 把一个磁盘目录的全部文件在单事务内入库，并在同一
// 事务写进度标记（progressKey=progress，调用方保证水线值不后退——
// 水线之下的目录导入时传原水线）——崩溃只可能留下「整目录没进库」，
// 不存在半目录态。常规文件（含 attachments/ 等子目录相对路径）整存
// debug_files，updated_at 取文件 mtime 保留取证时间线；*.jsonl 切
// 行进 debug_chunks。用裸 INSERT 而非 REPLACE：dir 已确认不在库，
// 撞键说明与在线写入同名碰撞，宁可报错重跑也不静默覆盖活数据。
func (s *Store) importDebugDir(ctx context.Context, dirPath, dir, progressKey, progress string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	err = filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dirPath, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if strings.HasSuffix(name, ".jsonl") {
			return importDebugJSONL(ctx, tx, dir, name, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO debug_files(dir, name, content, updated_at) VALUES(?,?,?,?)`,
			dir, name, data, info.ModTime().UnixMilli())
		return err
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO runtime_state("key", value, updated_at) VALUES(?,?,?)`,
		progressKey, progress, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// importDebugJSONL 把一个 .jsonl 文件切成 <=256KB 的行对齐块写入
// debug_chunks（seq 从 0 递增）；空文件也写一行空 chunk——「文件
// 存在但为空」在 UNION 名单语义下只能靠行存在性表达。
func importDebugJSONL(ctx context.Context, tx *sql.Tx, dir, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	reader := bufio.NewReaderSize(f, debugImportChunkSize)
	var buf bytes.Buffer
	seq := 0
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO debug_chunks(dir, name, seq, data) VALUES(?,?,?,?)`,
			dir, name, seq, buf.Bytes())
		seq++
		buf.Reset()
		return err
	}
	for {
		line, err := reader.ReadBytes('\n')
		for len(line) > 0 {
			// 写入会让 buf 溢出就先 flush——块尾落在行边界上；单行
			// 自身超 chunkSize 时才经 n=min(...) 硬切。
			if buf.Len()+len(line) > debugImportChunkSize {
				if ferr := flush(); ferr != nil {
					return ferr
				}
			}
			n := min(debugImportChunkSize-buf.Len(), len(line))
			buf.Write(line[:n])
			line = line[n:]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if seq == 0 && buf.Len() == 0 {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO debug_chunks(dir, name, seq, data) VALUES(?,?,0,?)`,
			dir, name, []byte{})
		return err
	}
	return flush()
}
