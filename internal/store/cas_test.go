// 本文件验证 CAS（跨目录内容寻址共享）的库存语义：manifest 编码/解析/
// 重组的回环、跨目录共享块的去重、混合年代（一期 gzip 基座+一期
// 字典 id 的 delta 帧）读出、mark-sweep 收尸的生命周期与兜底。
package store

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// casCorpus 构造 01 体量的 JSON：tagA/tagB 段让两份语料共享大前缀
// （系统提示/工具声明的跨请求复用形态）、各带独立尾部。repeats 放大到
// casMaxChunk 之上，保证 CDC 必有 ≥2 个切块（上限强制切）。
func casCorpus(tagA, tagB string, prefixRepeats, tailRepeats int) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"model":"claude","messages":[{"role":"system","content":"`)
	for i := 0; i < prefixRepeats; i++ {
		fmt.Fprintf(&buf, "%s-shared-tool-schema-%05d ", tagA, i)
	}
	buf.WriteString(`"},{"role":"user","content":"`)
	for i := 0; i < tailRepeats; i++ {
		fmt.Fprintf(&buf, "%s-unique-tail-%05d ", tagB, i)
	}
	buf.WriteString(`"}]}`)
	return buf.Bytes()
}

// putCASFile 把 data 按 CAS 形态落成一整批（manifest 文件行 + blobs +
// refs——与 recorder 写路径同构）：返回切块集供调用方断言。
func putCASFile(t *testing.T, s *Store, ctx context.Context, dir, name string, data []byte) []CASChunk {
	t.Helper()
	manifest, usize, chunks, ok := EncodeCASManifest(NewPayloadEncoder(), data)
	if !ok {
		t.Fatalf("EncodeCASManifest ok=false for %d bytes", len(data))
	}
	batch := DebugBatch{Files: []DebugFileRow{{Dir: dir, Name: name, Stored: manifest, Usize: usize}}}
	for _, c := range chunks {
		batch.Blobs = append(batch.Blobs, DebugBlobRow(c))
		batch.Refs = append(batch.Refs, DebugRefRow{Dir: dir, Name: name, Hash: c.Hash})
	}
	if err := s.WriteDebugBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	return chunks
}

// countTable 返回一张表当前的行数（测试断言用）。
func countTable(t *testing.T, s *Store, ctx context.Context, table string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCASManifestRoundTrip 验证 manifest 形态的完整回环：入库读出与
// 原文逐字节一致、total 报逻辑尺寸、文件行本体是 manifest 而非原文。
func TestCASManifestRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	data := casCorpus("d1", "d1", 12000, 800)
	chunks := putCASFile(t, s, ctx, "d1", deltaBaseFileName, data)
	assertPayloadBytes(t, s)

	got, total, ok, err := s.DebugFile(ctx, "d1", deltaBaseFileName, 0)
	if err != nil || !ok || !bytes.Equal(got, data) || total != int64(len(data)) {
		t.Fatalf("read = %d,%d,%v,%v", len(got), total, ok, err)
	}
	// 库存形态断言：文件行是 manifest（魔数开头、远小于原文），blob
	// 数=去重切块数，refs 按 dir+name 挂接。
	var storedLen, usize int64
	var magic []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUBSTR(content,1,5), LENGTH(content), usize FROM debug_files WHERE dir='d1' AND name=?`,
		deltaBaseFileName).Scan(&magic, &storedLen, &usize); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(magic, casMagic) || usize != int64(len(data)) || storedLen >= int64(len(data))/10 {
		t.Fatalf("manifest row: magic=%x usize=%d stored=%d data=%d", magic, usize, storedLen, len(data))
	}
	if n := countTable(t, s, ctx, "debug_blobs"); n != int64(len(chunks)) {
		t.Fatalf("blobs = %d, want %d", n, len(chunks))
	}
	if n := countTable(t, s, ctx, "debug_chunk_refs"); n != int64(len(chunks)) {
		t.Fatalf("refs = %d, want %d", n, len(chunks))
	}

	// 截断读：limit 内的前缀与原文前缀逐字节一致，total 仍报逻辑尺寸。
	limit := int64(len(data) / 3)
	got, total, ok, err = s.DebugFile(ctx, "d1", deltaBaseFileName, limit)
	if err != nil || !ok || total != int64(len(data)) {
		t.Fatalf("truncated = %d,%v,%v", total, ok, err)
	}
	if int64(len(got)) > limit || !bytes.Equal(got, data[:len(got)]) {
		t.Fatalf("truncated prefix = %d bytes, mismatch", len(got))
	}
}

// TestCASSmallFileFallback 验证小块/无边界语料回退独立编码：CAS 化只
// 在能切出 ≥2 块时才成立。
func TestCASSmallFileFallback(t *testing.T) {
	enc := NewPayloadEncoder()
	if _, _, _, ok := EncodeCASManifest(enc, []byte("tiny")); ok {
		t.Fatal("tiny payload should not CAS-encode")
	}
	if _, _, _, ok := EncodeCASManifest(enc, bytes.Repeat([]byte("x"), casMinChunk*2-1)); ok {
		t.Fatal("sub-2*minChunk payload should not CAS-encode")
	}
}

// TestCASChunkerDeterminism 验证切块与 manifest 的跨实例确定性——同一
// 输入在两份编码器上产出逐字节相同的 manifest（hash 表一致）。
func TestCASChunkerDeterminism(t *testing.T) {
	data := casCorpus("det", "det", 9000, 500)
	m1, _, c1, ok1 := EncodeCASManifest(NewPayloadEncoder(), data)
	m2, _, c2, ok2 := EncodeCASManifest(NewPayloadEncoder(), data)
	if !ok1 || !ok2 || !bytes.Equal(m1, m2) || len(c1) != len(c2) {
		t.Fatalf("non-deterministic: ok %v/%v manifest-eq %v chunks %d/%d", ok1, ok2, bytes.Equal(m1, m2), len(c1), len(c2))
	}
}

// TestCASCrossDirDedup 验证跨目录共享：两个 dir 的 01 共享大前缀时，
// 共享切块在 debug_blobs 只存一份（ref 各自挂），两端读出都逐字节
// 正确——这是 CAS 相对于同目录 dedup 的增量收益面。
func TestCASCrossDirDedup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dataA := casCorpus("shared", "dirA", 12000, 700)
	dataB := casCorpus("shared", "dirB", 12000, 700)
	chunksA := putCASFile(t, s, ctx, "dirA", deltaBaseFileName, dataA)
	chunksB := putCASFile(t, s, ctx, "dirB", deltaBaseFileName, dataB)
	assertPayloadBytes(t, s)

	setA := map[string]struct{}{}
	for _, c := range chunksA {
		setA[string(c.Hash)] = struct{}{}
	}
	shared := 0
	for _, c := range chunksB {
		if _, ok := setA[string(c.Hash)]; ok {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("no shared chunks between overlapping corpora")
	}
	blobs := countTable(t, s, ctx, "debug_blobs")
	refs := countTable(t, s, ctx, "debug_chunk_refs")
	if want := int64(len(chunksA) + len(chunksB) - shared); blobs != want {
		t.Fatalf("blobs = %d, want deduped %d (A %d + B %d - shared %d)", blobs, want, len(chunksA), len(chunksB), shared)
	}
	if refs != int64(len(chunksA)+len(chunksB)) {
		t.Fatalf("refs = %d, want %d", refs, len(chunksA)+len(chunksB))
	}
	t.Logf("cross-dir dedup: %d/%d chunks of B shared with A; blobs %d vs naive %d",
		shared, len(chunksB), blobs, len(chunksA)+len(chunksB))

	got, _, _, err := s.DebugFile(ctx, "dirA", deltaBaseFileName, 0)
	if err != nil || !bytes.Equal(got, dataA) {
		t.Fatalf("dirA read: %v len %d", err, len(got))
	}
	got, _, _, err = s.DebugFile(ctx, "dirB", deltaBaseFileName, 0)
	if err != nil || !bytes.Equal(got, dataB) {
		t.Fatalf("dirB read: %v len %d", err, len(got))
	}
}

// TestCASMissingBlob 验证引用在而 blob 缺席时的显式错误——mark-sweep
// 竞态残迹/库损坏不能吐乱码给面板。
func TestCASMissingBlob(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	data := casCorpus("m", "m", 12000, 400)
	chunks := putCASFile(t, s, ctx, "d1", deltaBaseFileName, data)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM debug_blobs WHERE hash=?`, chunks[0].Hash); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.DebugFile(ctx, "d1", deltaBaseFileName, 0); err == nil || !strings.Contains(err.Error(), "missing blob") {
		t.Fatalf("want missing-blob error, got %v", err)
	}
}

// TestCASMixedEraRead 验证混合年代同库可读：一期形态的 gzip 基座 +
// 字典 id=1 的 delta 帧，与二期 manifest 基座 + 字典 id=2 的帧并存，
// 两种基座形态都能给 delta 供字典。
func TestCASMixedEraRead(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	payload := bytes.Repeat([]byte(`{"k":"v"} `), 2048)

	// 一期目录：01 是 gzip 行，02 是字典 id=1 的手植帧。
	base1 := casCorpus("era1", "era1", 400, 60)
	stored1, usize1 := EncodePayload(base1)
	if err := s.WriteDebugBatch(ctx, DebugBatch{Files: []DebugFileRow{
		{Dir: "era1", Name: deltaBaseFileName, Stored: stored1, Usize: usize1},
	}}); err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderDictRaw(deltaDictIDLegacy, base1))
	if err != nil {
		t.Fatal(err)
	}
	legacyFrame := zw.EncodeAll(payload, nil)
	_ = zw.Close()
	if err := s.WriteDebugBatch(ctx, DebugBatch{Files: []DebugFileRow{
		{Dir: "era1", Name: "02-request-messages.json", Stored: legacyFrame, Usize: int64(len(payload))},
	}}); err != nil {
		t.Fatal(err)
	}
	got, _, _, err := s.DebugFile(ctx, "era1", "02-request-messages.json", 0)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("legacy-id delta read: %v len %d", err, len(got))
	}

	// 二期目录：01 是 manifest，02 是字典 id=2 的新帧——字典由 manifest
	// 重组而来。
	base2 := casCorpus("era2", "era2", 12000, 60)
	putCASFile(t, s, ctx, "era2", deltaBaseFileName, base2)
	frame2, usize2 := EncodePayloadDelta(payload, base2)
	if !bytes.Equal(frame2[:4], zstdMagic) {
		t.Fatal("phase-2 delta fixture did not encode as zstd frame")
	}
	if err := s.WriteDebugBatch(ctx, DebugBatch{Files: []DebugFileRow{
		{Dir: "era2", Name: "02-request-messages.json", Stored: frame2, Usize: usize2},
	}}); err != nil {
		t.Fatal(err)
	}
	got, _, _, err = s.DebugFile(ctx, "era2", "02-request-messages.json", 0)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("manifest-based delta read: %v len %d", err, len(got))
	}
	// 二期基座自身读出 = manifest 重组明文。
	got, _, _, err = s.DebugFile(ctx, "era2", deltaBaseFileName, 0)
	if err != nil || !bytes.Equal(got, base2) {
		t.Fatalf("manifest base read: %v len %d", err, len(got))
	}
}

// TestCASDictIDMismatchOnRollback 验证回滚保护：旧二进制只按字典 id=1
// 注册，读二期帧（id=2）时必须显式报 dictionary mismatch 而不是
// 误用 manifest 字节当字典解出乱码。
func TestCASDictIDMismatchOnRollback(t *testing.T) {
	base := casCorpus("rb", "rb", 6000, 400)
	payload := bytes.Repeat([]byte(`{"k":"v"} `), 2048)
	frame, _ := EncodePayloadDelta(payload, base)
	zr, err := zstd.NewReader(nil, zstd.WithDecoderDictRaw(deltaDictIDLegacy, base))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if _, err := zr.DecodeAll(frame, nil); err == nil {
		t.Fatal("id=1-only decoder accepted an id=2 frame: rollback would emit garbage")
	}
}

// TestCASReaperLifecycle 验证 mark-sweep 的全生命周期：删一个 dir 的
// 文件行时 refs 随删除漏斗死，被另一 dir 共享的 blob 存活；全部引用
// 消失且过宽限期后 blob 被收；宽限期内无引用 blob 不误收；悬垂 ref
// （文件行先死）先被扫掉。
func TestCASReaperLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dataA := casCorpus("shared", "dirA", 12000, 700)
	dataB := casCorpus("shared", "dirB", 12000, 700)
	putCASFile(t, s, ctx, "dirA", deltaBaseFileName, dataA)
	putCASFile(t, s, ctx, "dirB", deltaBaseFileName, dataB)
	blobsBefore := countTable(t, s, ctx, "debug_blobs")
	if blobsBefore == 0 {
		t.Fatal("fixture produced no blobs")
	}

	// 删 dirA：refs 随文件行的同一 WHERE 死；共享块仍被 dirB 引用——
	// 宽限期回拨后收尸也必须留下它们。
	if err := s.DeleteDebugDir(ctx, "dirA"); err != nil {
		t.Fatal(err)
	}
	assertPayloadBytes(t, s)
	if _, err := s.db.ExecContext(ctx, `UPDATE debug_blobs SET created_at=0`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReapOrphanBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	blobsAfterA := countTable(t, s, ctx, "debug_blobs")
	if blobsAfterA == 0 || blobsAfterA >= blobsBefore {
		t.Fatalf("shared blobs not preserved: before %d after %d", blobsBefore, blobsAfterA)
	}
	if n := countTable(t, s, ctx, "debug_chunk_refs"); n == 0 {
		t.Fatal("dirB refs were swept with dirA")
	}
	assertPayloadBytes(t, s)

	// 宽限期：dirB 的 ref 手工摘掉但 blob 时间戳是「现在」——reap 不收。
	if _, err := s.db.ExecContext(ctx, `DELETE FROM debug_chunk_refs`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE debug_blobs SET created_at=?`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.ReapOrphanBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countTable(t, s, ctx, "debug_blobs"); n != blobsAfterA {
		t.Fatalf("grace violated: blobs %d, want %d", n, blobsAfterA)
	}
	// 回拨过宽限期：全部收尸。
	if _, err := s.db.ExecContext(ctx, `UPDATE debug_blobs SET created_at=0`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReapOrphanBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countTable(t, s, ctx, "debug_blobs"); n != 0 {
		t.Fatalf("orphan blobs left: %d", n)
	}
}

// TestCASDanglingRefSweep 验证回滚期泄漏路径的兜底：旧二进制删目录
// 只清 debug_files/debug_chunks 不知 refs 表——悬垂 ref 会让 blob
// 假活，reaper 的第一道 sweep 按「文件行不再是 manifest」判定扫掉。
func TestCASDanglingRefSweep(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	data := casCorpus("dg", "dg", 12000, 500)
	putCASFile(t, s, ctx, "d1", deltaBaseFileName, data)
	// 模拟旧二进制：只删文件行，留下 refs 与 blobs。
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM debug_files WHERE dir='d1' AND name=?`, deltaBaseFileName); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE debug_blobs SET created_at=0`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReapOrphanBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countTable(t, s, ctx, "debug_chunk_refs"); n != 0 {
		t.Fatalf("dangling refs left: %d", n)
	}
	if n := countTable(t, s, ctx, "debug_blobs"); n != 0 {
		t.Fatalf("blobs pinned by dangling refs: %d", n)
	}
}

// TestCASManifestParseErrors 验证 manifest 解析的严格性：截断/尾随
// 字节/尺寸不一致都显式报错——manifest 行破损即文件不可读。
func TestCASManifestParseErrors(t *testing.T) {
	data := casCorpus("p", "p", 9000, 400)
	manifest, _, _, ok := EncodeCASManifest(NewPayloadEncoder(), data)
	if !ok {
		t.Fatal("fixture did not encode")
	}
	if _, _, err := parseCASManifest(manifest[:len(manifest)-3]); err == nil {
		t.Fatal("truncated manifest parsed")
	}
	if _, _, err := parseCASManifest(append(append([]byte(nil), manifest...), 0xff)); err == nil {
		t.Fatal("trailing-garbage manifest parsed")
	}
	if _, _, err := parseCASManifest([]byte("not a manifest")); err == nil {
		t.Fatal("non-manifest parsed")
	}
	// usize 与块长合计不符：改一个 length varint 的末位。
	broken := append([]byte(nil), manifest...)
	broken[len(broken)-1] ^= 0x7f
	if _, _, err := parseCASManifest(broken); err == nil {
		t.Fatal("size-mismatched manifest parsed")
	}
}
