// 本文件是 debug payload 的内容寻址存储（CAS）层：跨目录重复的
// 字节区间（系统提示/工具声明/会话历史前缀——01 叶子质量跨 dir
// 复用实测 ~92%）以 CDC 切块后按内容哈希共享入库。
//
// 存储模型：debug_blobs 装切块明文（EncodePayload 编码，hash 取
// 明文 sha256 截 16B），debug_chunk_refs 记「哪个文件行引用了哪些
// blob」（GC 视图，随行同生死），debug_files 的 01 行本体只存
// manifest（casMagic + usize + 有序 (hash,len) 表）。保活不变式
// 「blob 活 ⟺ ∃ ref 行」由三件事维持：blob/ref/manifest 同批事务
// 落库（无孤儿窗口）；ref 随文件行同一 WHERE 被删除漏斗带走；
// ReapOrphanBlobs 在写连接上做 mark-sweep 兜底（含回滚期旧二进制
// 删 dir 不知 refs 表留下的悬垂 ref 先扫一道）。
//
// 只有 01-http-request.json 走本形态：02/03* 对 01 的同 dir delta
// 残差已 1-5%，切块对齐不上；04/06 流式行不进首版。manifest 行
// usize 恒 >0（逻辑尺寸），DebugFile 的「usize>0 → 整读解码」分支
// 因此恒兜住它。
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/restic/chunker"
)

// casMagic 是 manifest 行的魔数：与 gzip(1f8b)/zstd(28b52ffd)/JSON('{')
// 无碰撞面，读侧按它把文件行分派到 refs→blobs 重组路径。
var casMagic = []byte{0x00, 'C', 'A', 'S', '1'}

// 切块参数：均值 ~64KB（splitmask 16 bit）对 01 的 ~100KB-2.4MB 体量
// 给出 2-40 块/文件；16KB 下限防碎块把 refs 表打成粉尘，256KB 上限
// 钳尾部方差。多项式取 restic 的测试/生产同款（不可约、deg≤53）。
const (
	casMinChunk  = 16 << 10
	casAvgBits   = 16
	casMaxChunk  = 256 << 10
	casHashBytes = 16
)

var casPol = chunker.Pol(0x3DA3358B4DC173)

// CASChunk 是一块待入库的共享内容：Hash 是明文 sha256 截 16B（blob 行
// 与 ref 行的共同键），Stored/Usize 是 EncodePayload 产物（与
// debug_files 行同编码口径——读侧对 blob 复用 decodePayload）。
type CASChunk struct {
	Hash   []byte
	Stored []byte
	Usize  int64
}

// DebugBlobRow 是批量事务里的一行共享 blob：OR IGNORE 语义，同 hash
// 已存在时本行不写（字节记账按 RowsAffected 实计）。
type DebugBlobRow struct {
	Hash   []byte
	Stored []byte
	Usize  int64
}

// DebugRefRow 是批量事务里的一行引用：PK(dir,name,hash)，随文件行
// 同一 WHERE 被删除漏斗带走——引用即行是 CAS 的 GC 事实源。
type DebugRefRow struct {
	Dir  string
	Name string
	Hash []byte
}

// refRowBytes 是一行 ref 的库存字节口径（与 deleteDebugRows/
// DebugDirSizes 的 RETURNING/SUM 表达式同源）。
func refRowBytes(dir, name string) int64 {
	return int64(len(dir) + len(name) + casHashBytes)
}

// EncodeCASManifest 把 data 按内容定义切块（CDC），逐块算 hash 与入库
// 编码，返回 manifest 入库字节、逻辑尺寸与去重后的块集。ok=false 表示
// 切不出 ≥2 块（小文件/无内部边界）——整文件存 manifest 只赔不赚，
// 调用方回退普通 EncodePayload。块集按 hash 去重（同文件内重复区间只
// 存一份），manifest 的位置表仍按序逐块登记——重组语义不变。
func EncodeCASManifest(enc *PayloadEncoder, data []byte) (manifest []byte, usize int64, chunks []CASChunk, ok bool) {
	if len(data) < casMinChunk*2 {
		return nil, 0, nil, false
	}
	bc := chunker.NewBase(casPol,
		chunker.WithBaseAverageBits(casAvgBits),
		chunker.WithBaseBoundaries(casMinChunk, casMaxChunk))
	// NextSplitPoint 是状态机：返 n>0 表示 rest[:n] 是一块；返 -1 表示
	// rest 全部并入当前块（流结尾的最后一块）。
	var spans [][2]int
	off := 0
	rest := data
	for len(rest) > 0 {
		n := bc.NextSplitPoint(rest)
		if n <= 0 {
			break
		}
		spans = append(spans, [2]int{off, n})
		off += n
		rest = rest[n:]
	}
	if off < len(data) {
		spans = append(spans, [2]int{off, len(data) - off})
	}
	if len(spans) < 2 {
		return nil, 0, nil, false
	}
	var mbuf bytes.Buffer
	mbuf.Grow(len(casMagic) + binary.MaxVarintLen64*2 + len(spans)*(casHashBytes+5))
	mbuf.Write(casMagic)
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(data)))
	mbuf.Write(tmp[:n])
	n = binary.PutUvarint(tmp[:], uint64(len(spans)))
	mbuf.Write(tmp[:n])
	seen := make(map[string]struct{}, len(spans))
	for _, sp := range spans {
		plain := data[sp[0] : sp[0]+sp[1]]
		sum := sha256.Sum256(plain)
		hash := append([]byte(nil), sum[:casHashBytes]...)
		mbuf.Write(hash)
		n = binary.PutUvarint(tmp[:], uint64(sp[1]))
		mbuf.Write(tmp[:n])
		if _, dup := seen[string(hash)]; dup {
			continue
		}
		seen[string(hash)] = struct{}{}
		stored, usize := enc.Encode(plain)
		chunks = append(chunks, CASChunk{Hash: hash, Stored: stored, Usize: usize})
	}
	return mbuf.Bytes(), int64(len(data)), chunks, true
}

// casEntry 是 manifest 位置表里的一项：hash 定位 blob，plainLen 是
// 该块的明文长度（校验重组完整性，也让截断读只取前缀所需的 blob）。
type casEntry struct {
	hash     []byte
	plainLen int64
}

// hasCASManifestMagic 判 manifest 魔数。
func hasCASManifestMagic(data []byte) bool {
	return len(data) >= len(casMagic) && bytes.Equal(data[:len(casMagic)], casMagic)
}

// parseCASManifest 解 manifest：usize + 有序 (hash,len) 表。格式损坏
// 返回显式错误——manifest 行破损等于文件不可读，静默透传只会把乱码
// 交给面板。
func parseCASManifest(manifest []byte) (usize int64, entries []casEntry, err error) {
	if !hasCASManifestMagic(manifest) {
		return 0, nil, fmt.Errorf("not a CAS manifest")
	}
	body := manifest[len(casMagic):]
	u, n := binary.Uvarint(body)
	if n <= 0 {
		return 0, nil, fmt.Errorf("CAS manifest: bad usize varint")
	}
	usize = int64(u)
	body = body[n:]
	count, n := binary.Uvarint(body)
	if n <= 0 {
		return 0, nil, fmt.Errorf("CAS manifest: bad chunk count varint")
	}
	body = body[n:]
	// count 先按剩余体长界住再分配：每条目至少 hash+1B varint，虚报
	// 的 count（行损坏）不该换来一次巨型预分配。
	if count > uint64(len(body))/(casHashBytes+1) {
		return 0, nil, fmt.Errorf("CAS manifest: chunk count %d exceeds body %d bytes", count, len(body))
	}
	entries = make([]casEntry, 0, count)
	for i := uint64(0); i < count; i++ {
		if len(body) < casHashBytes {
			return 0, nil, fmt.Errorf("CAS manifest: truncated hash at entry %d", i)
		}
		hash := body[:casHashBytes]
		body = body[casHashBytes:]
		l, n := binary.Uvarint(body)
		if n <= 0 {
			return 0, nil, fmt.Errorf("CAS manifest: bad length varint at entry %d", i)
		}
		body = body[n:]
		entries = append(entries, casEntry{hash: hash, plainLen: int64(l)})
	}
	if len(body) != 0 {
		return 0, nil, fmt.Errorf("CAS manifest: %d trailing bytes", len(body))
	}
	// 登记尺寸与块长合计必须相等——usize 与逐块 len 同源写出，
	// 对不上就是行内容损坏。
	var sum int64
	for _, e := range entries {
		sum += e.plainLen
	}
	if sum != usize {
		return 0, nil, fmt.Errorf("CAS manifest: chunk lengths sum %d, want usize %d", sum, usize)
	}
	return usize, entries, nil
}

// decodeCASManifest 按 manifest 重组文件明文：位置表累计长度圈出
// limit 覆盖的前缀块（面板 4MB 截断读不为尾部块白取 blob），一次
// IN 查询取回全部所需 blob（非逐块 N+1），逐块 decodePayload 后按
// 序拼接。引用在而 blob 缺席（mark-sweep 竞态残迹/库损坏）返回显式
// 错误；块解出长度与登记不符同样报错——内容寻址的信任锚是长度。
func (s *Store) decodeCASManifest(ctx context.Context, dir string, manifest []byte, limit int64) ([]byte, error) {
	_, entries, err := parseCASManifest(manifest)
	if err != nil {
		return nil, fmt.Errorf("decode CAS manifest in %s: %w", dir, err)
	}
	if limit <= 0 {
		limit = math.MaxInt64
	}
	var acc int64
	need := 0
	for i, e := range entries {
		acc += e.plainLen
		need = i + 1
		if acc >= limit {
			break
		}
	}
	entries = entries[:need]
	if len(entries) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(entries))
	uniq := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		key := string(e.hash)
		if _, dup := uniq[key]; dup {
			continue
		}
		uniq[key] = struct{}{}
		args = append(args, e.hash)
	}
	rows, err := s.ro.QueryContext(ctx,
		`SELECT hash, content FROM debug_blobs WHERE hash IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("decode CAS manifest in %s: fetch blobs: %w", dir, err)
	}
	blobs := make(map[string][]byte, len(uniq))
	for rows.Next() {
		var hash, content []byte
		if err := rows.Scan(&hash, &content); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("decode CAS manifest in %s: scan blob: %w", dir, err)
		}
		blobs[string(hash)] = content
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("decode CAS manifest in %s: fetch blobs: %w", dir, err)
	}
	var buf bytes.Buffer
	// acc 是前缀块的登记长度和（随后逐块与解出长度核对）；Grow 取它做
	// 一次性预分配，但先钳在合理界内——损坏行的 len 字段可能虚报。
	if hint := min(acc, limit); hint >= 0 && hint <= 512<<20 {
		buf.Grow(int(hint))
	}
	for _, e := range entries {
		content, ok := blobs[string(e.hash)]
		if !ok {
			return nil, fmt.Errorf("decode CAS manifest in %s: missing blob %x", dir, e.hash)
		}
		plain, err := decodePayload(content)
		if err != nil {
			return nil, fmt.Errorf("decode CAS manifest in %s: blob %x: %w", dir, e.hash, err)
		}
		if int64(len(plain)) != e.plainLen {
			return nil, fmt.Errorf("decode CAS manifest in %s: blob %x length %d, want %d", dir, e.hash, len(plain), e.plainLen)
		}
		buf.Write(plain)
	}
	data := buf.Bytes()
	if int64(len(data)) > limit {
		data = data[:limit]
	}
	return data, nil
}
