// 本文件是 debug payload 的透明压缩层：写入侧对超阈值的内容先 gzip
// 再入库，读出侧按 magic bytes 判帧解压——无魔数的存量行原样透传，
// 不需要回填。usize 列记解压前字节数（0=未压缩），清单与截断读的
// total 口径保持逻辑尺寸，容量淘汰仍按库存尺寸计磁盘占用。
package store

import (
	"bytes"
	"compress/gzip"
	"io"
	"sync"
)

// compressMinBytes 是压缩启用的尺寸阈值：小体的压缩收益换不回一
// 帧 gzip 的头尾固定开销与读侧解压成本。
const compressMinBytes = 1024

// gzipMagic 是 RFC1952 帧头魔数，读侧按它判帧。
var gzipMagic = []byte{0x1f, 0x8b}

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

// decodePayload 按 magic bytes 判帧解压；非 gzip 帧原样透传。
func decodePayload(data []byte) ([]byte, error) {
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
