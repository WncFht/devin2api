// 性能基准：delta 编码器「每目录新建」对「分片常驻 + ResetWithOptions 换
// dict」。验证 zstd dict 编码器跨目录复用的分配摊销——dict 表重建走
// clear+重填零分配路径，hist/长表永续复用。
package store

import (
	"bytes"
	"fmt"
	"testing"
)

// benchDeltaBase 构造 ~400KB 的类 01 基座：JSON 骨架重复 + 变体内容，
// 与真实 http-request 投影的冗余结构同型。
func benchDeltaBase() []byte {
	var buf bytes.Buffer
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&buf, `{"role":"user","content":[{"type":"text","text":"please refactor module %d and keep the public API stable across all call sites including tests"}],"index":%d}`, i, i)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// benchDeltaPayload 从基座派生候选文件：前缀共享 + 尾部改写——02/03
// 对 01 的投影冗余模型。
func benchDeltaPayload(base []byte, seed int) []byte {
	var buf bytes.Buffer
	buf.Write(base[:len(base)*3/4])
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&buf, `{"projection":%d,"field":"variant value %d","nested":{"a":%d}}`, seed, i, i*seed)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// BenchmarkDeltaEncoderPerDir 重构前形态：每目录新建编码器，dict 表、
// hist、长短表全额重建——每目录 ~16MB 一次性分配。
func BenchmarkDeltaEncoderPerDir(b *testing.B) {
	base := benchDeltaBase()
	payloads := [][]byte{benchDeltaPayload(base, 1), benchDeltaPayload(base, 2), benchDeltaPayload(base, 3)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc := NewPayloadDeltaEncoder()
		for _, p := range payloads {
			stored, _ := enc.Encode(p, base)
			if len(stored) == 0 {
				b.Fatal("empty")
			}
		}
		enc.Close()
	}
}

// BenchmarkDeltaEncoderPooled 现行实现：分片常驻编码器跨目录复用。
// bytes.Clone 造同内容异指针基座——强制每轮走 ResetWithOptions 换 dict
// 路径（最坏情形，生产同目录多文件共享基座指针连换都不必），残差
// 行为与单基座一致。
func BenchmarkDeltaEncoderPooled(b *testing.B) {
	base := benchDeltaBase()
	alt := bytes.Clone(base)
	payloads := [][]byte{benchDeltaPayload(base, 1), benchDeltaPayload(base, 2), benchDeltaPayload(base, 3)}
	enc := NewPayloadDeltaEncoder()
	defer enc.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bb := base
		if i%2 == 1 {
			bb = alt
		}
		for _, p := range payloads {
			stored, _ := enc.Encode(p, bb)
			if len(stored) == 0 {
				b.Fatal("empty")
			}
		}
	}
}

// TestDeltaPooledRoundTrip 校验复用编码器跨基座换 dict 后、回到原基座
// 产出的帧仍可用原基座解码。
func TestDeltaPooledRoundTrip(t *testing.T) {
	base := benchDeltaBase()
	payload := benchDeltaPayload(base, 7)
	enc := NewPayloadDeltaEncoder()
	defer enc.Close()
	other := append([]byte("different base content"), base[len(base)/2:]...)
	storedA, _ := enc.Encode(payload, base)
	enc.Encode(payload, other)
	storedB, _ := enc.Encode(payload, base)
	for i, stored := range [][]byte{storedA, storedB} {
		got, err := decodePayloadDelta(stored, base)
		if err != nil {
			t.Fatalf("frame %d decode: %v", i, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("frame %d round-trip mismatch", i)
		}
	}
}
