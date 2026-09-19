package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	waitPayloadSeed(t, s)
	return s
}

// waitPayloadSeed 等异步播种协程落地：全新库/计数器行缺席时 Open 只探
// 不测，真值由后台协程结算落库；持久化行命中的快路径 seedDone 为 nil，
// 直接返回。
func waitPayloadSeed(t *testing.T, s *Store) {
	t.Helper()
	if s.seedDone == nil {
		return
	}
	select {
	case <-s.seedDone:
	case <-time.After(30 * time.Second):
		t.Fatal("payload seed did not finish")
	}
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
		if err := s.appendDebugChunk(ctx, "d1", "04-devin-response.jsonl", []byte(part)); err != nil {
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
	if err := s.appendDebugChunk(ctx, "d1", "06-http-response.jsonl", []byte("x\n")); err != nil {
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
		if err := s.appendDebugChunk(ctx, "d1", "05-response-events.jsonl", []byte(part)); err != nil {
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

func TestDebugFileCompression(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// 可压缩大内容：库存 gzip 帧，usize 记解压前尺寸。
	payload := bytes.Repeat([]byte(`{"k":"value"} `), 512)
	if err := s.PutDebugFile(ctx, "d1", "big.json", payload); err != nil {
		t.Fatal(err)
	}
	var usize, stored int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT usize, LENGTH(content) FROM debug_files WHERE dir='d1' AND name='big.json'`).Scan(&usize, &stored); err != nil {
		t.Fatal(err)
	}
	if usize != int64(len(payload)) || stored >= usize {
		t.Fatalf("usize=%d stored=%d, want usize=%d > stored", usize, stored, len(payload))
	}
	data, total, ok, err := s.DebugFile(ctx, "d1", "big.json", 0)
	if err != nil || !ok || !bytes.Equal(data, payload) || total != int64(len(payload)) {
		t.Fatalf("compressed read = %d,%d,%v,%v", len(data), total, ok, err)
	}
	// 截断读作用在解压后的内容上，total 仍是逻辑尺寸。
	data, total, _, err = s.DebugFile(ctx, "d1", "big.json", 10)
	if err != nil || !bytes.Equal(data, payload[:10]) || total != int64(len(payload)) {
		t.Fatalf("compressed trunc = %q,%d,%v", data, total, err)
	}
	// chunk 路径同一语义。
	if err := s.appendDebugChunk(ctx, "d1", "04-devin-response.jsonl", payload); err != nil {
		t.Fatal(err)
	}
	data, total, ok, err = s.DebugFile(ctx, "d1", "04-devin-response.jsonl", 0)
	if err != nil || !ok || !bytes.Equal(data, payload) || total != int64(len(payload)) {
		t.Fatalf("compressed chunk = %d,%d,%v,%v", len(data), total, ok, err)
	}
	// DebugFileList 报逻辑尺寸而非库存尺寸。
	files, err := s.DebugFileList(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name == "big.json" && f.Size != int64(len(payload)) {
			t.Fatalf("list size = %d, want %d", f.Size, len(payload))
		}
	}
	// 压不出 ≥10% 收益的内容原样入库（usize=0），读回不变。
	rnd := make([]byte, 4096)
	if _, err := rand.Read(rnd); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDebugFile(ctx, "d1", "rnd.bin", rnd); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT usize FROM debug_files WHERE dir='d1' AND name='rnd.bin'`).Scan(&usize); err != nil {
		t.Fatal(err)
	}
	if usize != 0 {
		t.Fatalf("incompressible usize = %d, want 0", usize)
	}
	data, _, ok, err = s.DebugFile(ctx, "d1", "rnd.bin", 0)
	if err != nil || !ok || !bytes.Equal(data, rnd) {
		t.Fatalf("raw read = %d,%v,%v", len(data), ok, err)
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
	must(s.appendDebugChunk(ctx, "d1", "04-devin-response.jsonl", []byte("ab")))
	must(s.PutDebugFile(ctx, "d2", "error.json", []byte("e")))
	must(s.appendDebugChunk(ctx, "d2", "04-devin-response.jsonl", []byte("x")))

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

func TestDeleteDebugPayloadsBefore(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"meta.json", "03-devin-request.json", "03-devin-request.attempt2.json", "attachments/image-001.png"} {
		must(s.PutDebugFile(ctx, "d1", name, []byte("x")))
	}
	must(s.appendDebugChunk(ctx, "d1", "04-devin-response.jsonl", []byte("a")))
	must(s.appendDebugChunk(ctx, "d1", "06-http-response.jsonl", []byte("b")))
	must(s.appendDebugChunk(ctx, "d1", "05-response-events.jsonl", []byte("c")))
	must(s.PutDebugFile(ctx, "d2", "03-devin-request.json", []byte("y")))

	// 剥离 bound 之下目录的 03*/04/06/attachments，证据文件留下；
	// bound 之外的 d2 不动。
	must(s.DeleteDebugPayloadsBefore(ctx, "d2",
		[]string{"04-devin-response.jsonl", "06-http-response.jsonl"},
		[]string{"03-devin-request.", "attachments/"}))
	names, err := s.DebugFileNames(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"05-response-events.jsonl", "meta.json"}) {
		t.Fatalf("after strip = %v", names)
	}
	names, err = s.DebugFileNames(ctx, "d2")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"03-devin-request.json"}) {
		t.Fatalf("d2 = %v", names)
	}
	// 空名单是空操作。
	must(s.DeleteDebugPayloadsBefore(ctx, "", nil, nil))
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
		must(s.appendDebugChunk(ctx, dir, "04-devin-response.jsonl", []byte("y")))
	}
	// exclude 豁免 keep_error_dirs 保护集内的过期目录。
	must(s.DeleteDebugDirsBefore(ctx, "20260910-120000", []string{"20260909-235959"}))
	dirs, err := s.DebugDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dirs, []string{"20260909-235959", "20260910-120000-01"}) {
		t.Fatalf("after excluded retention = %v", dirs)
	}
	must(s.DeleteDebugDirsBefore(ctx, "20260910-120000", nil))
	dirs, err = s.DebugDirs(ctx)
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
	must(s.appendDebugChunk(ctx, "d1", "04-devin-response.jsonl", []byte("y")))
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

// TestDeleteDebugRowsChunked 覆盖分片删除：目录数超过单片上限
// （deleteChunkDirs）时剥载与整删都必须跨片不重不漏，计数器随各片
// 提交逐步减量。
func TestDeleteDebugRowsChunked(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const n = deleteChunkDirs + 5
	for i := 0; i < n; i++ {
		dir := fmt.Sprintf("20260910-%06d", i)
		must(s.PutDebugFile(ctx, dir, "meta.json", []byte("m")))
		must(s.PutDebugFile(ctx, dir, "01-http-request.json", []byte("p")))
		must(s.appendDebugChunk(ctx, dir, "04-devin-response.jsonl", []byte("c")))
	}
	assertPayloadBytes(t, s)
	// 剥载跨片：全部目录剥到锚点，01/04 命中删除、meta.json 留下。
	must(s.StripDebugDirsBefore(ctx, "\xff", []string{"meta.json", "error.json"}, nil))
	for i := 0; i < n; i++ {
		names, err := s.DebugFileNames(ctx, fmt.Sprintf("20260910-%06d", i))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(names, []string{"meta.json"}) {
			t.Fatalf("dir %d after strip = %v", i, names)
		}
	}
	assertPayloadBytes(t, s)
	// 整删跨片：一个目录都不剩。
	must(s.DeleteDebugDirsBefore(ctx, "\xff", nil))
	dirs, err := s.DebugDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 0 {
		t.Fatalf("remaining = %v", dirs)
	}
	assertPayloadBytes(t, s)
}

// TestSplitDeleteChunks 覆盖切片的双界：目录数界与字节界任一先到即
// 切片——字节肥厚的目录序列必须切出比纯目录界更多的片；单目录自身
// 越字节界时独占一片（目录是不可拆分的原子单位）；切片不丢不重、
// 保持名序。
func TestSplitDeleteChunks(t *testing.T) {
	mk := func(n int, size int64) []dirSize {
		dirs := make([]dirSize, n)
		for i := range dirs {
			dirs[i] = dirSize{dir: fmt.Sprintf("20260910-%06d", i), size: size}
		}
		return dirs
	}
	flatten := func(chunks [][]string) []string {
		var out []string
		for _, c := range chunks {
			out = append(out, c...)
		}
		return out
	}

	// 字节界主导：201 个 1MiB 目录——目录界只切 2 片，字节界按
	// 32MiB/片切出 7 片（32×6+9），且不重不漏保序。
	dirs := mk(deleteChunkDirs+1, 1<<20)
	chunks := splitDeleteChunks(dirs)
	if len(chunks) != 7 {
		t.Fatalf("byte-fat sequence = %d chunks, want 7 (count bound alone gives 2)", len(chunks))
	}
	if len(chunks[0]) != 32 {
		t.Fatalf("first chunk = %d dirs, want 32 (32MiB / 1MiB)", len(chunks[0]))
	}
	var flat []string
	for _, d := range dirs {
		flat = append(flat, d.dir)
	}
	if !slices.Equal(flatten(chunks), flat) {
		t.Fatal("chunks lost, duplicated, or reordered dirs")
	}
	// 每片字节合计不超界（单目录越界豁免之外）。
	sz := map[string]int64{}
	for _, d := range dirs {
		sz[d.dir] = d.size
	}
	for i, c := range chunks {
		var sum int64
		for _, dir := range c {
			sum += sz[dir]
		}
		if sum > deleteChunkBytes {
			t.Fatalf("chunk %d bytes = %d, over cap %d", i, sum, deleteChunkBytes)
		}
	}

	// 目录数界主导：小尺寸目录超 200 仍按数切片。
	chunks = splitDeleteChunks(mk(deleteChunkDirs+5, 1))
	if len(chunks) != 2 || len(chunks[0]) != deleteChunkDirs || len(chunks[1]) != 5 {
		t.Fatalf("count-bound split = %d chunks", len(chunks))
	}

	// 单目录越字节界：不与邻居同片、也不被拆开，独占一片。
	chunks = splitDeleteChunks([]dirSize{
		{dir: "a", size: 1},
		{dir: "huge", size: deleteChunkBytes + 1},
		{dir: "b", size: 1},
	})
	if len(chunks) != 3 ||
		!slices.Equal(chunks[0], []string{"a"}) ||
		!slices.Equal(chunks[1], []string{"huge"}) ||
		!slices.Equal(chunks[2], []string{"b"}) {
		t.Fatalf("oversize dir split = %v", chunks)
	}

	// 空枚举不产生任何片。
	if chunks := splitDeleteChunks(nil); len(chunks) != 0 {
		t.Fatalf("empty = %v", chunks)
	}
}

// assertPayloadBytes 断言内存计数器、runtime_state 持久化行与权威聚合
// 三者一致——目录口径 DebugDirSizes（files+chunks+refs）加全局口径
// DebugBlobBytes。每个写/删操作后调一次，漏记账或重复记账立刻暴露。
func assertPayloadBytes(t *testing.T, s *Store) {
	t.Helper()
	sizes, err := s.DebugDirSizes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, n := range sizes {
		want += n
	}
	blobBytes, err := s.DebugBlobBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want += blobBytes
	if got := s.DebugPayloadBytes(); got != want {
		t.Fatalf("payload bytes = %d, want %d (sizes %v, blobs %d)", got, want, sizes, blobBytes)
	}
	// 持久化行与内存镜像同事务维护，必须逐笔对齐。
	var raw string
	if err := s.db.QueryRow(
		`SELECT value FROM runtime_state WHERE "key"=?`, debugPayloadBytesKey).Scan(&raw); err != nil {
		t.Fatalf("read persisted payload bytes: %v", err)
	}
	if got, err := strconv.ParseInt(raw, 10, 64); err != nil || got != want {
		t.Fatalf("persisted payload bytes = %q (%v), want %d", raw, err, want)
	}
}

func TestDebugPayloadBytes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	assertPayloadBytes(t, s)

	// 可压缩内容走一条：计数按库存帧长而非逻辑尺寸。
	big := bytes.Repeat([]byte(`{"k":"value"} `), 512)
	must(s.PutDebugFile(ctx, "d1", "meta.json", big))
	assertPayloadBytes(t, s)
	// OR REPLACE 覆写：增量 = 新帧 − 旧帧。
	must(s.PutDebugFile(ctx, "d1", "meta.json", []byte("small")))
	assertPayloadBytes(t, s)
	// IfAbsent/Claim 的未命中路径不计数。
	if _, err := s.ClaimDebugFile(ctx, "d1", "error.json", []byte("e1")); err != nil {
		t.Fatal(err)
	}
	assertPayloadBytes(t, s)
	if _, err := s.ClaimDebugFile(ctx, "d1", "error.json", []byte("e2-ignored")); err != nil {
		t.Fatal(err)
	}
	assertPayloadBytes(t, s)
	if _, err := s.ClaimDebugFile(ctx, "d2", "meta.json", []byte("c")); err != nil {
		t.Fatal(err)
	}
	assertPayloadBytes(t, s)
	if claimed, err := s.ClaimDebugFile(ctx, "d2", "meta.json", []byte("c2-ignored")); err != nil || claimed {
		t.Fatalf("re-claim = %v,%v", claimed, err)
	}
	assertPayloadBytes(t, s)
	must(s.appendDebugChunk(ctx, "d1", "04-devin-response.jsonl", big))
	assertPayloadBytes(t, s)

	// 批量事务混合四类：OR REPLACE、OR IGNORE、chunk、strip、logrow。
	must(s.WriteDebugBatch(ctx, DebugBatch{
		Files: []DebugFileRow{
			{Dir: "d3", Name: "meta.json", Stored: []byte("m3")},
			{Dir: "d3", Name: "03-devin-request.json", Stored: []byte("payload")},
			{Dir: "d2", Name: "meta.json", Stored: []byte("c3")},
			{Dir: "d2", Name: "error.json", Stored: []byte("ign"), IfAbsent: true},
			{Dir: "d2", Name: "error.json", Stored: []byte("new"), IfAbsent: true},
		},
		Chunks:    []DebugChunkRow{{Dir: "d3", Name: "04-devin-response.jsonl", Data: []byte("chunk")}},
		StripDirs: []string{"d3"},
		LogRows:   []*LogRow{{Dir: "d3", StartedAt: time.Now(), Result: "completed"}},
	}))
	assertPayloadBytes(t, s)

	// 三条删除路径各自减量。
	must(s.DeleteDebugPayloadsBefore(ctx, "d2",
		[]string{"04-devin-response.jsonl"}, []string{"03-devin-request."}))
	assertPayloadBytes(t, s)
	must(s.DeleteDebugDir(ctx, "d2"))
	assertPayloadBytes(t, s)
	must(s.DeleteDebugDirsBefore(ctx, "\xff", nil))
	assertPayloadBytes(t, s)

	// 磁盘目录导入同样入账。
	logRoot := filepath.Join(t.TempDir(), "logs")
	writeDebugDirFixture(t, logRoot)
	must(s.ImportDebugDirs(ctx, logRoot, "import_debug_progress"))
	assertPayloadBytes(t, s)

	// 绕开记账路径直插一行制造漂移：对账返回差值并把计数器拉回权威值。
	must(func() error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO debug_files(dir, name, content, updated_at) VALUES('drift','x',?,0)`,
			[]byte("unaccounted"))
		return err
	}())
	drift, err := s.ReconcileDebugPayloadBytes(ctx, func() int64 {
		sizes, err := s.DebugDirSizes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, n := range sizes {
			total += n
		}
		return total
	}())
	if err != nil {
		t.Fatalf("ReconcileDebugPayloadBytes: %v", err)
	}
	if drift != -11 {
		t.Fatalf("drift = %d, want -11", drift)
	}
	assertPayloadBytes(t, s)
}

func TestDebugPayloadBytesReseed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.PutDebugFile(ctx, "d1", "meta.json", []byte("persist")); err != nil {
		t.Fatal(err)
	}
	if err := s.appendDebugChunk(ctx, "d1", "04-devin-response.jsonl", []byte("chunk")); err != nil {
		t.Fatal(err)
	}
	waitPayloadSeed(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 重启后计数器从持久化行 O(1) 播种，不吃旧内存态。
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	waitPayloadSeed(t, s)
	assertPayloadBytes(t, s)
}

// TestDebugPayloadBytesSeedFallback 模拟升级后首启：库里有 payload 但
// runtime_state 没有计数器行，Open 不阻塞、由异步协程走权威聚合重建
// 并重新落行；值损坏时同样重建。重建后再次启动即回 O(1) 读路径。
func TestDebugPayloadBytesSeedFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.PutDebugFile(ctx, "d1", "meta.json", []byte("persist")); err != nil {
		t.Fatal(err)
	}
	if err := s.appendDebugChunk(ctx, "d1", "04-devin-response.jsonl", []byte("chunk")); err != nil {
		t.Fatal(err)
	}
	waitPayloadSeed(t, s)
	want := s.DebugPayloadBytes()
	// 删掉计数器行——旧二进制写出的库没有它。
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM runtime_state WHERE "key"=?`, debugPayloadBytesKey); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen after key delete: %v", err)
	}
	waitPayloadSeed(t, s)
	if got := s.DebugPayloadBytes(); got != want {
		t.Fatalf("reseeded counter = %d, want %d", got, want)
	}
	assertPayloadBytes(t, s)
	// 值损坏也走重建：写一段非数字文本，重开后应回到权威值。
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runtime_state SET value='garbage' WHERE "key"=?`, debugPayloadBytesKey); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen after corrupt value: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	waitPayloadSeed(t, s)
	if got := s.DebugPayloadBytes(); got != want {
		t.Fatalf("counter after corrupt reseed = %d, want %d", got, want)
	}
	assertPayloadBytes(t, s)
}

// putDeltaFile 把一段内容按 delta 编码直接落成 debug_files 行——测试
// 绕过 debuglog 写路径，借 WriteDebugBatch 的预编码文件行入库。
func putDeltaFile(t *testing.T, s *Store, ctx context.Context, dir, name string, data, base []byte) {
	t.Helper()
	stored, usize := EncodePayloadDelta(data, base)
	if err := s.WriteDebugBatch(ctx, DebugBatch{Files: []DebugFileRow{{Dir: dir, Name: name, Stored: stored, Usize: usize}}}); err != nil {
		t.Fatal(err)
	}
}

// TestDeltaPayloadRoundTrip 验证 01 基座 + zstd delta 的库存语义：
// 02/03* 存帧后读出与原文逐字节一致，attemptN/searchN 分片同名不同序
// 号也走同一字典。
func TestDeltaPayloadRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// 模拟同 dir 三重投影：01 是客户端原文（含系统提示+历史），02/03
	// 与 01 共享大部分叶子串。
	base := bytes.Repeat([]byte(`{"role":"user","content":[{"type":"text","text":"shared-prefix-block"}]}`), 256)
	if err := s.PutDebugFile(ctx, "d1", deltaBaseFileName, base); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"02-request-messages.json", "03-devin-request.json", "03-devin-request.attempt2.json", "03-devin-request.search1.json"} {
		want := append(bytes.Repeat([]byte(`{"role":"user","content":[{"type":"text","text":"shared-prefix-block"}]}`), 200), []byte(name)...)
		putDeltaFile(t, s, ctx, "d1", name, want, base)
		data, total, ok, err := s.DebugFile(ctx, "d1", name, 0)
		if err != nil || !ok || !bytes.Equal(data, want) || total != int64(len(want)) {
			t.Fatalf("%s: read = %d,%d,%v,%v", name, len(data), total, ok, err)
		}
		// 库存形态必须是 zstd 帧且远小于原文（残差），usize 记逻辑尺寸。
		var magic []byte
		var usize, stored int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT SUBSTR(content,1,4), usize, LENGTH(content) FROM debug_files WHERE dir='d1' AND name=?`, name).Scan(&magic, &usize, &stored); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(magic, zstdMagic) {
			t.Fatalf("%s: magic = %x, want zstd frame", name, magic)
		}
		if usize != int64(len(want)) || stored >= usize/4 {
			t.Fatalf("%s: usize=%d stored=%d, want stored << usize", name, usize, stored)
		}
	}
}

// TestDeltaPayloadMissingBase 验证基座缺席两端的定版口径：写侧无
// 字典回退独立 gzip（读如常）；读侧撞到 delta 帧而目录无 01 行时
// 返回显式错误而不是乱码。
func TestDeltaPayloadMissingBase(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	payload := bytes.Repeat([]byte(`{"k":"v"} `), 512)
	// 写侧：字典为空 → 回退 EncodePayload（gzip），读不需要字典。
	putDeltaFile(t, s, ctx, "d1", "02-request-messages.json", payload, nil)
	data, _, ok, err := s.DebugFile(ctx, "d1", "02-request-messages.json", 0)
	if err != nil || !ok || !bytes.Equal(data, payload) {
		t.Fatalf("fallback read = %d,%v,%v", len(data), ok, err)
	}
	// 读侧：delta 帧在而 01 行缺席（手动删行/shed 丢任务）→ 显式错误。
	putDeltaFile(t, s, ctx, "d2", "02-request-messages.json", payload, payload)
	if _, _, _, err = s.DebugFile(ctx, "d2", "02-request-messages.json", 0); err == nil {
		t.Fatal("delta read without base returned no error")
	}
	// 基座本身是 delta 帧（手植的畸形行）→ 同样显式错误而非递归。
	stored, usize := EncodePayloadDelta(payload, payload)
	if err := s.WriteDebugBatch(ctx, DebugBatch{Files: []DebugFileRow{
		{Dir: "d3", Name: deltaBaseFileName, Stored: stored, Usize: usize},
		{Dir: "d3", Name: "02-request-messages.json", Stored: stored, Usize: usize},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = s.DebugFile(ctx, "d3", "02-request-messages.json", 0); err == nil {
		t.Fatal("delta read with delta-encoded base returned no error")
	}
}

// TestDecodePayloadFileMagic 覆盖导出文件的魔数分派：raw 透传、
// gzip 解压、zstd delta 按字典解（无字典显式错误）。
func TestDecodePayloadFileMagic(t *testing.T) {
	raw := []byte(`{"k":1}`)
	if got, err := DecodePayloadFile(raw, nil); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("raw = %v,%v", got, err)
	}
	big := bytes.Repeat([]byte(`{"k":"v"} `), 512)
	gz, usize := EncodePayload(big)
	if usize == 0 {
		t.Fatal("fixture did not compress")
	}
	if got, err := DecodePayloadFile(gz, nil); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("gzip = %d,%v", len(got), err)
	}
	delta, usize := EncodePayloadDelta(big, big)
	if usize == 0 || !bytes.Equal(delta[:4], zstdMagic) {
		t.Fatal("fixture did not delta-encode")
	}
	if _, err := DecodePayloadFile(delta, nil); err == nil {
		t.Fatal("delta without dict returned no error")
	}
	if got, err := DecodePayloadFile(delta, big); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("delta = %d,%v", len(got), err)
	}
}

// TestWriteDebugBatchImmediateUnderLock 验证 WriteDebugBatch 改 BEGIN
// IMMEDIATE 后的等锁形状：竞争写者持写锁期间批次经 busy_timeout 排队，
// 而非 deferred 时代「预读快照→写升级」的 SQLITE_BUSY_SNAPSHOT 快败；
// 持锁者提交后批次照常落库——覆盖含 LENGTH 预读的非 IfAbsent 文件行
// （prod 517 事故实录的语句形态）与 chunk 追加行。
func TestWriteDebugBatchImmediateUnderLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contended.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	// 竞争写者持文件写锁（第二连接池，模拟交接期在役实例）。
	comp, err := sql.Open("sqlite", walDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = comp.Close() }()
	compConn, err := comp.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compConn.Close() }()
	if _, err := compConn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}

	stored, usize := EncodePayload([]byte(`{"m":1}`))
	batch := DebugBatch{
		Files:  []DebugFileRow{{Dir: "d1", Name: "meta.json", Stored: stored, Usize: usize}},
		Chunks: []DebugChunkRow{{Dir: "d1", Name: "04-devin-response.jsonl", Data: []byte("{}\n")}},
	}
	done := make(chan error, 1)
	go func() { done <- s.WriteDebugBatch(ctx, batch) }()
	select {
	case err := <-done:
		t.Fatalf("WriteDebugBatch returned while write lock held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := compConn.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WriteDebugBatch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WriteDebugBatch did not finish after lock release")
	}
	data, _, ok, err := s.DebugFile(ctx, "d1", "meta.json", 0)
	if err != nil || !ok || string(data) != `{"m":1}` {
		t.Fatalf("meta.json = %q,%v,%v", data, ok, err)
	}
	chunk, _, ok, err := s.DebugFile(ctx, "d1", "04-devin-response.jsonl", 0)
	if err != nil || !ok || string(chunk) != "{}\n" {
		t.Fatalf("chunk = %q,%v,%v", chunk, ok, err)
	}
}
