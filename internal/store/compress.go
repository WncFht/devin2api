// 本文件是 debug payload 的透明压缩层：写入侧对超阈值的内容先压缩
// 再入库，读出侧按 magic bytes 判帧解压——无魔数的存量行原样透传，
// 不需要回填。usize 列记解压前字节数（0=未压缩），清单与截断读的
// total 口径保持逻辑尺寸，容量淘汰仍按库存尺寸计磁盘占用。
//
// 四种库存形态：gzip（独立压缩）、zstd delta 帧（以同目录 01 明文为
// raw dict 的 patch-from 编码——02/03-devin-request* 与 01 是同一请求
// 的三重投影，字节级冗余让它们只存残差）、raw（小体/压不出收益的）、
// CAS manifest（01 的内容寻址指针行，见 cas.go）。读侧对 zstd 帧必须
// 取回字典才能解码，见 DebugFile 的基座取用。
package store

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// compressMinBytes 是压缩启用的尺寸阈值：小体的压缩收益换不回一
// 帧压缩格式的头尾固定开销与读侧解压成本。
const compressMinBytes = 1024

// gzipMagic 是 RFC1952 帧头魔数，读侧按它判帧。
var gzipMagic = []byte{0x1f, 0x8b}

// zstdMagic 是 RFC8478 帧头魔数，读侧按它判 delta 帧——zstd 形态
// 在本库只由 EncodePayloadDelta 产出，帧头嵌 deltaDictID 指向字典。
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// deltaDictID 是二期写出的 delta 帧内嵌字典 id：编码侧用它注册
// raw dict。二期起 01 行可能是 CAS manifest——回滚到一期二进制时
// 旧解码链会把 manifest 字节当字典；帧引用 id=2 而旧解码器只注册
// id=1，zstd 显式报 dictionary mismatch 而不是吐乱码。
const deltaDictID = 2

// deltaDictIDLegacy 是一期帧的字典 id：解码侧把同一份 01 明文按两个
// id 注册，新旧帧都能解。
const deltaDictIDLegacy = 1

// deltaBaseFileName 是 delta 基座在目录内的文件名：写侧把它的脱敏
// 后字节当 dict，读侧按它取回字典。与 debuglog.StageHTTPRequest 同一
// 字面量——store 不能反向 import debuglog（会成环），文件名契约的
// 归属方是 stages.go，本常量仅为存储格式的自包含描述。
const deltaBaseFileName = "01-http-request.json"

// payloadGzipPool 复用 gzip.Writer：flate 的窗口与哈希表是压缩路径的
// 分配大头（profiler 实测该路径占 debug 写入 alloc 的大半），池化后
// 每写只剩输出缓冲一份临时分配。归还前 Reset(io.Discard) 断开对
// 输出缓冲的引用。
var payloadGzipPool = sync.Pool{
	New: func() any {
		zw, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return zw
	},
}

// PayloadEncoder 是单持有者复用的 payload 编码器：内部 gzip.Writer
// 跨调用 Reset 续用 flate 窗口与哈希表——sync.Pool 会被 GC 清空，
// 长驻协程（编码分片/写 worker）各自持有一份后，表重建摊成一次性
// 成本。非并发安全：持有者必须保证串行调用。
type PayloadEncoder struct {
	zw *gzip.Writer
}

// NewPayloadEncoder 返回一个可长期持有的编码器，压缩档与
// EncodePayload 同为 BestSpeed。
func NewPayloadEncoder() *PayloadEncoder {
	zw, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return &PayloadEncoder{zw: zw}
}

// Encode 与 EncodePayload 同语义，只是复用持有者的 gzip.Writer。
func (e *PayloadEncoder) Encode(data []byte) (stored []byte, usize int64) {
	return encodePayload(e.zw, data)
}

// EncodePayload 返回入库字节与 usize：能压出 ≥10% 收益的内容存
// 压缩形态（usize=解压前尺寸），否则原样（usize=0）。BestSpeed
// 对 JSON 文本已有一个量级压缩率，更高档换 CPU 不值。
// 导出供 debuglog 在请求 goroutine 上预编码——压缩是写路径 CPU 大头，
// 摊到调用方后写 worker 只剩落库。
func EncodePayload(data []byte) (stored []byte, usize int64) {
	zw := payloadGzipPool.Get().(*gzip.Writer)
	stored, usize = encodePayload(zw, data)
	payloadGzipPool.Put(zw)
	return stored, usize
}

// encodePayload 是 EncodePayload/PayloadEncoder.Encode 的共用实现：
// zw 由调用方供给并复用；返回前 Reset(io.Discard) 断开对输出缓冲的
// 引用，调用方无需再 detach。
func encodePayload(zw *gzip.Writer, data []byte) (stored []byte, usize int64) {
	if len(data) < compressMinBytes {
		return data, 0
	}
	var buf bytes.Buffer
	buf.Grow(len(data) / 4)
	zw.Reset(&buf)
	_, werr := zw.Write(data)
	cerr := zw.Close()
	zw.Reset(io.Discard)
	if werr != nil || cerr != nil {
		return data, 0
	}
	if stored = buf.Bytes(); int64(len(stored)) >= int64(len(data))*9/10 {
		return data, 0
	}
	return stored, int64(len(data))
}

// EncodePayloadDelta 以 base 为字典把 data 编成 zstd delta 帧（raw
// content dict，patch-from 语义）：同目录 02/03-devin-request* 相对
// 01 的差异只剩投影改写的结构部分。档位取 SpeedBetterCompression——
// SpeedFastest 的 dict 编码器是 32K 槽单探针快表（enc_fast.go
// fastEncoderDict，tableBits=15），dict 超 ~300KB 碰撞饱和、残差随
// 字典尺寸单调劣化（生产实测 0.6%@150K → 41.6%@750K dict）；level 3
// 全尺寸段覆盖且部分场景编码更快。「SpeedFastest ≈ zstd CLI -3」是
// 错误等价——CLI -3 对应的是 klauspost SpeedBetterCompression。
// base 为空或残差收益不达阈值时回退 EncodePayload 独立存储——读侧
// 按魔数自判形态，回退不需要任何标记位。
func EncodePayloadDelta(data, base []byte) (stored []byte, usize int64) {
	if len(data) < compressMinBytes || len(base) == 0 {
		return EncodePayload(data)
	}
	zw, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderDictRaw(deltaDictID, base),
	)
	if err != nil {
		return EncodePayload(data)
	}
	defer func() { _ = zw.Close() }()
	if stored = zw.EncodeAll(data, nil); int64(len(stored)) >= int64(len(data))*9/10 {
		return EncodePayload(data)
	}
	return stored, int64(len(data))
}

// errDeltaNeedsBase 是 delta 帧走到无字典解码口的显式失败：读侧必须
// 先取同目录基座，静默透传 zstd 帧等于交出乱码。
var errDeltaNeedsBase = fmt.Errorf("payload is a zstd delta frame: decode with the dir's %s as dict", deltaBaseFileName)

// decodePayload 按 magic bytes 判帧解压；非压缩帧原样透传。
// zstd delta 帧不属于本函数口径（字典在调用方手里）——撞见即显式
// 报错，防 chunks 路径把 delta 帧当原文透传成乱码。
func decodePayload(data []byte) ([]byte, error) {
	if hasZstdMagic(data) {
		return nil, errDeltaNeedsBase
	}
	if len(data) < 2 || !bytes.Equal(data[:2], gzipMagic) {
		return data, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}

// decodePayloadDelta 用字典解 zstd delta 帧；dict 是同目录 01 的脱敏
// 后明文（与写侧 EncodePayloadDelta 的 base 逐字节同源）。同一份明文
// 按新旧两个字典 id 注册：一期帧引用 id=1、二期帧引用 id=2，都能解。
func decodePayloadDelta(data, dict []byte) ([]byte, error) {
	zr, err := zstd.NewReader(nil,
		zstd.WithDecoderDictRaw(deltaDictID, dict),
		zstd.WithDecoderDictRaw(deltaDictIDLegacy, dict))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return zr.DecodeAll(data, nil)
}

// hasZstdMagic 判 zstd 帧头：delta 帧与普通 zstd 帧同魔数——本库只产
// delta 形态，判到即按 delta 处理。
func hasZstdMagic(data []byte) bool {
	return len(data) >= 4 && bytes.Equal(data[:4], zstdMagic)
}

// errManifestNeedsStore 是 manifest 走到进程外解码口的显式失败：
// 重组要查 debug_blobs 表，离线解码器拿不到——端点导出才是正路。
var errManifestNeedsStore = fmt.Errorf("payload is a CAS manifest: chunks live in debug_blobs, export via /file/{name} endpoint instead")

// DecodePayloadFile 是导出文件（writefile() 直出/端点下载）的魔数
// 分派解码：gzip 帧解压、raw 透传、zstd delta 帧用 dict 解（dict 取
// 同目录 01 的解后明文）。供进程外消费方（cmd/probe 等）复用与读
// 路径同一套判帧口径；delta 帧缺 dict 时返回显式错误。
func DecodePayloadFile(data, dict []byte) ([]byte, error) {
	if hasCASManifestMagic(data) {
		return nil, errManifestNeedsStore
	}
	if hasZstdMagic(data) {
		if len(dict) == 0 {
			return nil, errDeltaNeedsBase
		}
		return decodePayloadDelta(data, dict)
	}
	return decodePayload(data)
}
