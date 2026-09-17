package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeDebugDirFixture 在 logRoot 摆两个请求目录：dir1 含整文件、
// attachments 子目录、行对齐多 chunk 的 04 与空 06；dir2 含一条超长
// 行（>256KB，硬切成多 chunk）的 06。另摆顶层文件与不匹配请求目录
// 名的子目录，验证导入器不碰它们。
func writeDebugDirFixture(t *testing.T, logRoot string) (bigLine []byte) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	dir1 := filepath.Join(logRoot, "20260910-120000")
	dir2 := filepath.Join(logRoot, "20260911-120000")
	must(os.MkdirAll(filepath.Join(dir1, "attachments"), 0o755))
	must(os.MkdirAll(dir2, 0o755))
	must(os.WriteFile(filepath.Join(dir1, "meta.json"), []byte(`{"status":200}`), 0o644))
	must(os.WriteFile(filepath.Join(dir1, "error.json"), []byte(`{"stage":"x"}`), 0o644))
	must(os.WriteFile(filepath.Join(dir1, "attachments", "image-001.png"), []byte("PNGDATA"), 0o644))
	var lines []byte
	for i := 0; i < 300; i++ {
		lines = append(lines, []byte(fmt.Sprintf(`{"seq":%d,"pad":"%s"}`, i, string(make([]byte, 1000))))...)
		lines = append(lines, '\n')
	}
	must(os.WriteFile(filepath.Join(dir1, "04-devin-response.jsonl"), lines, 0o644))
	must(os.WriteFile(filepath.Join(dir1, "06-http-response.jsonl"), nil, 0o644))

	bigLine = bytes.Repeat([]byte("z"), debugImportChunkSize+1000)
	must(os.WriteFile(filepath.Join(dir2, "06-http-response.jsonl"),
		append(bigLine, '\n'), 0o644))

	// 顶层文件与不匹配模式的目录：一律不动。
	must(os.WriteFile(filepath.Join(logRoot, "stderr.log"), []byte("log"), 0o644))
	must(os.WriteFile(filepath.Join(logRoot, "index.jsonl.migrated"), []byte("x"), 0o644))
	must(os.MkdirAll(filepath.Join(logRoot, "stray-dir"), 0o755))
	must(os.WriteFile(filepath.Join(logRoot, "stray-dir", "keep.txt"), []byte("keep"), 0o644))
	return bigLine
}

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestImportDebugDirs(t *testing.T) {
	base := t.TempDir()
	logRoot := filepath.Join(base, "logs")
	bigLine := writeDebugDirFixture(t, logRoot)
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.ImportDebugDirs(ctx, logRoot, "import_debug_progress"); err != nil {
		t.Fatalf("ImportDebugDirs: %v", err)
	}

	// 内容逐字节回读。
	data, _, ok, err := s.DebugFile(ctx, "20260910-120000", "meta.json", 0)
	if err != nil || !ok || string(data) != `{"status":200}` {
		t.Fatalf("meta = %q,%v,%v", data, ok, err)
	}
	data, _, ok, _ = s.DebugFile(ctx, "20260910-120000", "attachments/image-001.png", 0)
	if !ok || string(data) != "PNGDATA" {
		t.Fatalf("attachment = %q,%v", data, ok)
	}
	// 04 约 300KB → 多 chunk；行数与首行前缀校验拼接正确性。
	var chunks int
	var sum int64
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(LENGTH(data)),0) FROM debug_chunks WHERE dir='20260910-120000' AND name='04-devin-response.jsonl'`).Scan(&chunks, &sum); err != nil {
		t.Fatal(err)
	}
	if chunks < 2 {
		t.Fatalf("04 chunks = %d, want >1", chunks)
	}
	data, total, ok, _ := s.DebugFile(ctx, "20260910-120000", "04-devin-response.jsonl", 0)
	if !ok || total != sum || !bytes.HasPrefix(data, []byte(`{"seq":0,`)) {
		t.Fatalf("04 = %d bytes ok=%v", total, ok)
	}
	if lines := bytes.Count(data, []byte("\n")); lines != 300 {
		t.Fatalf("04 lines = %d, want 300", lines)
	}
	// 空 06 → 一行空 chunk，存在但为空。
	data, total, ok, _ = s.DebugFile(ctx, "20260910-120000", "06-http-response.jsonl", 0)
	if !ok || total != 0 || len(data) != 0 {
		t.Fatalf("empty 06 = %q,%d,%v", data, total, ok)
	}
	// dir2 的超长行被硬切成两块，拼接后逐字节还原。
	data, _, ok, _ = s.DebugFile(ctx, "20260911-120000", "06-http-response.jsonl", 0)
	want := append(bytes.Clone(bigLine), '\n')
	if !ok || !bytes.Equal(data, want) {
		t.Fatalf("big line roundtrip: got %d bytes ok=%v", len(data), ok)
	}
	var maxChunk int64
	if err := s.db.QueryRow(`SELECT MAX(LENGTH(data)) FROM debug_chunks WHERE dir='20260911-120000'`).Scan(&maxChunk); err != nil {
		t.Fatal(err)
	}
	if maxChunk > debugImportChunkSize {
		t.Fatalf("chunk over size: %d", maxChunk)
	}
	// 进度标记与目录删除。
	progress, ok, _ := s.GetState(ctx, "import_debug_progress")
	if !ok || progress != "20260911-120000" {
		t.Fatalf("progress = %q,%v", progress, ok)
	}
	for _, d := range []string{"20260910-120000", "20260911-120000"} {
		if _, err := os.Stat(filepath.Join(logRoot, d)); !os.IsNotExist(err) {
			t.Fatalf("dir %s should be deleted", d)
		}
	}
	// 非目录与陌生目录原样保留。
	for _, p := range []string{"stderr.log", "index.jsonl.migrated", "stray-dir/keep.txt"} {
		if _, err := os.Stat(filepath.Join(logRoot, p)); err != nil {
			t.Fatalf("%s should be kept: %v", p, err)
		}
	}
	dirs, _ := s.DebugDirs(ctx)
	if !slices.Equal(dirs, []string{"20260910-120000", "20260911-120000"}) {
		t.Fatalf("dirs = %v", dirs)
	}

	// 重跑：磁盘已空，行数不变，幂等。
	before := countRows(t, s, `SELECT (SELECT COUNT(*) FROM debug_files) + (SELECT COUNT(*) FROM debug_chunks)`)
	if err := s.ImportDebugDirs(ctx, logRoot, "import_debug_progress"); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	after := countRows(t, s, `SELECT (SELECT COUNT(*) FROM debug_files) + (SELECT COUNT(*) FROM debug_chunks)`)
	if before != after {
		t.Fatalf("rerun changed rows: %d -> %d", before, after)
	}
}

// TestImportDebugDirsResume 验证断点续传：进度标记之前的目录已入库
// （只剩删盘兜底），之后的才导入。
func TestImportDebugDirsResume(t *testing.T) {
	base := t.TempDir()
	logRoot := filepath.Join(base, "logs")
	writeDebugDirFixture(t, logRoot)
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SetState(ctx, "import_debug_progress", "20260910-120000"); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportDebugDirs(ctx, logRoot, "import_debug_progress"); err != nil {
		t.Fatalf("ImportDebugDirs: %v", err)
	}
	// dir1 未入库（进度水位之下），但磁盘目录已被兜底删除。
	if _, _, ok, _ := s.DebugFile(ctx, "20260910-120000", "meta.json", 0); ok {
		t.Fatal("dir1 should not be imported")
	}
	if _, err := os.Stat(filepath.Join(logRoot, "20260910-120000")); !os.IsNotExist(err) {
		t.Fatal("dir1 should be deleted")
	}
	if _, _, ok, _ := s.DebugFile(ctx, "20260911-120000", "06-http-response.jsonl", 0); !ok {
		t.Fatal("dir2 should be imported")
	}
}

// TestImportDebugDirsSkipsExisting 验证「目录已在库」的幂等跳过：
// 在线写入或上轮已导入的目录不重复合并，磁盘残留直接删。
func TestImportDebugDirsSkipsExisting(t *testing.T) {
	base := t.TempDir()
	logRoot := filepath.Join(base, "logs")
	writeDebugDirFixture(t, logRoot)
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.PutDebugFile(ctx, "20260910-120000", "meta.json", []byte("live")); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportDebugDirs(ctx, logRoot, "import_debug_progress"); err != nil {
		t.Fatalf("ImportDebugDirs: %v", err)
	}
	// dir1 只保留了已有行——磁盘版 meta.json 没有混入。
	data, _, _, _ := s.DebugFile(ctx, "20260910-120000", "meta.json", 0)
	if string(data) != "live" {
		t.Fatalf("meta = %q, want live", data)
	}
	if _, _, ok, _ := s.DebugFile(ctx, "20260910-120000", "error.json", 0); ok {
		t.Fatal("disk error.json should not be merged into existing dir")
	}
	if _, err := os.Stat(filepath.Join(logRoot, "20260910-120000")); !os.IsNotExist(err) {
		t.Fatal("dir1 should be deleted")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM debug_files WHERE dir='20260910-120000'`); n != 1 {
		t.Fatalf("dir1 files = %d, want 1", n)
	}
}

func TestImportDebugDirsEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// logRoot 不存在 → 空操作。
	if err := s.ImportDebugDirs(ctx, filepath.Join(t.TempDir(), "nope"), "k"); err != nil {
		t.Fatalf("missing logRoot: %v", err)
	}
}
