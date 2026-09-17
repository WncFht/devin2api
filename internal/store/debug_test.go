package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestDebugFileRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.PutDebugFile(ctx, "d1", "meta.json", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	data, total, ok, err := s.DebugFile(ctx, "d1", "meta.json", 0)
	if err != nil || !ok || string(data) != `{"v":1}` || total != 7 {
		t.Fatalf("DebugFile = %q,%d,%v,%v", data, total, ok, err)
	}
	// 同名覆写：meta.json 完结时会二次写入。
	if err := s.PutDebugFile(ctx, "d1", "meta.json", []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	data, _, _, _ = s.DebugFile(ctx, "d1", "meta.json", 0)
	if string(data) != `{"v":2}` {
		t.Fatalf("overwrite = %q", data)
	}
	// miss 两表 → ok=false。
	if _, _, ok, err = s.DebugFile(ctx, "d1", "nope", 0); err != nil || ok {
		t.Fatalf("miss = %v,%v", ok, err)
	}
}

func TestClaimDebugFile(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// error.json first-write-wins：第二次写入必须被忽略。
	claimed, err := s.ClaimDebugFile(ctx, "d1", "error.json", []byte("first"))
	if err != nil || !claimed {
		t.Fatalf("first claim = %v,%v", claimed, err)
	}
	claimed, err = s.ClaimDebugFile(ctx, "d1", "error.json", []byte("second"))
	if err != nil || claimed {
		t.Fatalf("second claim = %v,%v", claimed, err)
	}
	data, _, _, err := s.DebugFile(ctx, "d1", "error.json", 0)
	if err != nil || string(data) != "first" {
		t.Fatalf("error.json = %q, want first", data)
	}
}

func TestAppendDebugChunkOrdering(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, part := range []string{"line1\n", "line2\n", "line3\n"} {
		if err := s.AppendDebugChunk(ctx, "d1", "04-devin-response.jsonl", []byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	// seq 应连续 0,1,2。
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, data FROM debug_chunks WHERE dir='d1' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int
	for rows.Next() {
		var seq int
		var data string
		if err := rows.Scan(&seq, &data); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	_ = rows.Close()
	if !slices.Equal(seqs, []int{0, 1, 2}) {
		t.Fatalf("seqs = %v", seqs)
	}
	data, total, ok, err := s.DebugFile(ctx, "d1", "04-devin-response.jsonl", 0)
	if err != nil || !ok || string(data) != "line1\nline2\nline3\n" || total != 18 {
		t.Fatalf("concat = %q,%d,%v,%v", data, total, ok, err)
	}
	// 不同 (dir,name) 的 seq 空间独立。
	if err := s.AppendDebugChunk(ctx, "d1", "06-http-response.jsonl", []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	var seq int
	if err := s.db.QueryRowContext(ctx,
		`SELECT seq FROM debug_chunks WHERE name='06-http-response.jsonl'`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Fatalf("new file seq = %d, want 0", seq)
	}
}

func TestDebugFileTruncation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.PutDebugFile(ctx, "d1", "big.json", []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	data, total, ok, err := s.DebugFile(ctx, "d1", "big.json", 4)
	if err != nil || !ok || string(data) != "0123" || total != 10 {
		t.Fatalf("files trunc = %q,%d,%v,%v", data, total, ok, err)
	}
	// chunk 路径：上限落在第二块中间。
	for _, part := range []string{"aaaa", "bbbb", "cccc"} {
		if err := s.AppendDebugChunk(ctx, "d1", "05-response-events.jsonl", []byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	data, total, ok, err = s.DebugFile(ctx, "d1", "05-response-events.jsonl", 7)
	if err != nil || !ok || string(data) != "aaaabbb" || total != 12 {
		t.Fatalf("chunks trunc = %q,%d,%v,%v", data, total, ok, err)
	}
	// 上限超过总长 → 全文。
	data, total, _, _ = s.DebugFile(ctx, "d1", "05-response-events.jsonl", 100)
	if string(data) != "aaaabbbbcccc" || total != 12 {
		t.Fatalf("over-cap = %q,%d", data, total)
	}
}

func TestDebugFileNamesAndDirs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutDebugFile(ctx, "d1", "meta.json", []byte("m")))
	must(s.PutDebugFile(ctx, "d1", "attachments/image-001.png", []byte("png")))
	must(s.AppendDebugChunk(ctx, "d1", "04-devin-response.jsonl", []byte("ab")))
	must(s.PutDebugFile(ctx, "d2", "error.json", []byte("e")))
	must(s.AppendDebugChunk(ctx, "d2", "04-devin-response.jsonl", []byte("x")))

	names, err := s.DebugFileNames(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"04-devin-response.jsonl", "attachments/image-001.png", "meta.json"}
	if !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	files, err := s.DebugFileList(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{}
	for _, f := range files {
		sizes[f.Name] = f.Size
	}
	if len(files) != 3 || sizes["meta.json"] != 1 || sizes["attachments/image-001.png"] != 3 || sizes["04-devin-response.jsonl"] != 2 {
		t.Fatalf("files = %+v", files)
	}
	dirs, err := s.DebugDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dirs, []string{"d1", "d2"}) {
		t.Fatalf("dirs = %v", dirs)
	}
	dirSizes, err := s.DebugDirSizes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dirSizes["d1"] != 6 || dirSizes["d2"] != 2 {
		t.Fatalf("dirSizes = %v", dirSizes)
	}
	errDirs, err := s.DebugDirsContaining(ctx, "error.json")
	if err != nil {
		t.Fatal(err)
	}
	if !errDirs["d2"] || errDirs["d1"] {
		t.Fatalf("error dirs = %v", errDirs)
	}
}

func TestDeleteDebugPayloadFiles(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"meta.json", "03-devin-request.json", "attachments/image-001.png"} {
		must(s.PutDebugFile(ctx, "d1", name, []byte("x")))
	}
	must(s.AppendDebugChunk(ctx, "d1", "04-devin-response.jsonl", []byte("a")))
	must(s.AppendDebugChunk(ctx, "d1", "06-http-response.jsonl", []byte("b")))
	must(s.AppendDebugChunk(ctx, "d1", "05-response-events.jsonl", []byte("c")))

	// 剥离 03/04/06/attachments，证据文件留下。
	must(s.DeleteDebugPayloadFiles(ctx, "d1",
		[]string{"03-devin-request.json", "04-devin-response.jsonl", "06-http-response.jsonl", "attachments/image-001.png"}))
	names, err := s.DebugFileNames(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"05-response-events.jsonl", "meta.json"}) {
		t.Fatalf("after strip = %v", names)
	}
	// 空清单是空操作。
	must(s.DeleteDebugPayloadFiles(ctx, "d1", nil))
}

func TestDeleteDebugDirsBefore(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// 同秒后缀 -01 排在裸名之后：界限 "20260910-120000" 删旧留新。
	for _, dir := range []string{"20260909-235959", "20260910-115959-02", "20260910-120000-01"} {
		must(s.PutDebugFile(ctx, dir, "meta.json", []byte("x")))
		must(s.AppendDebugChunk(ctx, dir, "04-devin-response.jsonl", []byte("y")))
	}
	must(s.DeleteDebugDirsBefore(ctx, "20260910-120000"))
	dirs, err := s.DebugDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dirs, []string{"20260910-120000-01"}) {
		t.Fatalf("after retention = %v", dirs)
	}
}

func TestDeleteDebugDir(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutDebugFile(ctx, "d1", "meta.json", []byte("x")))
	must(s.AppendDebugChunk(ctx, "d1", "04-devin-response.jsonl", []byte("y")))
	must(s.PutDebugFile(ctx, "d2", "meta.json", []byte("z")))
	must(s.DeleteDebugDir(ctx, "d1"))
	dirs, err := s.DebugDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dirs, []string{"d2"}) {
		t.Fatalf("dirs = %v", dirs)
	}
}
