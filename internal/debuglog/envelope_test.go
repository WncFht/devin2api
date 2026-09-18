// 本文件锁定 JSONL 信封手写拼装与 encoding/json 的逐字节等价：
// marshalJSONLRecord 的输出必须与 json.Marshal(JSONLRecord) 完全一致
// （含 omitempty、RawMessage compaction、HTML 转义与非法输入的错误口径），
// 任何一侧编码规则变化都应在这里当场爆掉。
package debuglog

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestMarshalJSONLRecordMatchesEncodingJSON 覆盖字段各形态：seq/elapsed
// 极值、event 省略与全类转义字符、data 的 nil/空白/缩进/嵌套引号/HTML
// 敏感字节——手写拼装的输出必须与反射 marshal 逐字节一致。
func TestMarshalJSONLRecordMatchesEncodingJSON(t *testing.T) {
	rfcTime := "2026-09-18T10:24:36.123456789+08:00"
	datas := []json.RawMessage{
		nil,
		{},
		[]byte("null"),
		[]byte(`{}`),
		[]byte(`{"a":1}`),
		[]byte(`[1,2,"x"]`),
		// 带可压缩空白与 <>& 的原文：marshal 侧 appendCompact 与
		// 手写侧 json.Compact 必须产出同一份结果。
		[]byte("{\n  \"k\": \"v\"\n}"),
		[]byte(`{"s":"<a>&'\"\\"}`),
		[]byte("invalid"),
		[]byte(`"quoted"`),
		[]byte(" \t\r\n"),
		[]byte(`{"nested":{"u":"事件","esc":"A\u2028B"}}`),
		// 字面 U+2028/2029 与字符串内非法 UTF-8：marshal 侧在 verbatim
		// 拷贝时转义 U+2028/9（EscapeForJS）、非法字节原样透传——
		// 手写侧 Compact 后补转必须同口径。
		[]byte("{\"s\":\"line\u2028sep\u2029end\"}"),
		{'{', '"', 's', '"', ':', '"', 'B', 'A', 'D', 0xff, 0xfe, '"', '}'},
	}
	events := []string{
		"",
		"message",
		`quote"back\slash`,
		"<tag>&amp",
		"line\nbreak\ttab\rcr",
		"ctrl\x01\x1f\x7f",
		"utf8-事件-🚀",
		"sep\u2028para\u2029end",
		"bad-utf8-\xff\xfe",
	}
	for _, data := range datas {
		for _, event := range events {
			record := JSONLRecord{
				Seq:       7,
				Time:      rfcTime,
				ElapsedMS: 1234,
				Event:     event,
				Data:      data,
			}
			want, wantErr := json.Marshal(record)
			got, gotErr := marshalJSONLRecord(record)
			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("error mismatch for event %q data %q: marshal err %v, hand err %v", event, data, wantErr, gotErr)
			}
			if wantErr != nil {
				continue
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("byte mismatch for event %q data %q:\n got: %q\nwant: %q", event, data, got, want)
			}
		}
	}
}

// TestMarshalJSONLRecordSeqAndTimeEdges 校验 seq/elapsed 数值边界与
// time 字段作为普通 JSON 字符串的转义路径（生产值恒为 RFC3339Nano，
// 字段本身是 string——手写信封对任意内容仍须与 marshal 一致）。
func TestMarshalJSONLRecordSeqAndTimeEdges(t *testing.T) {
	times := []string{
		"2026-09-18T10:24:36+08:00",
		"",
		`"quoted"time`,
		"<b>&",
		"2026-09-18T10:24:36.999999999Z",
	}
	for _, seq := range []int{0, 1, 9, 10, 12345, -3} {
		for _, elapsed := range []int64{0, -1, 42, 99999999999} {
			for _, tm := range times {
				record := JSONLRecord{Seq: seq, Time: tm, ElapsedMS: elapsed, Data: json.RawMessage(`{"x":1}`)}
				want, err := json.Marshal(record)
				if err != nil {
					t.Fatalf("json.Marshal failed: %v", err)
				}
				got, err := marshalJSONLRecord(record)
				if err != nil {
					t.Fatalf("marshalJSONLRecord failed: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("byte mismatch for seq %d elapsed %d time %q:\n got: %q\nwant: %q", seq, elapsed, tm, got, want)
				}
			}
		}
	}
}

// TestMarshalJSONLRecordNoTrailingNewline 行尾换行由 appendJSONL 的
// 缓冲层统一加，信封函数自身不得携带——防回归出双换行行。
func TestMarshalJSONLRecordNoTrailingNewline(t *testing.T) {
	got, err := marshalJSONLRecord(JSONLRecord{Seq: 1, Time: "t", Data: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[len(got)-1] != '}' {
		t.Fatalf("line should end with '}': %q", got)
	}
}
