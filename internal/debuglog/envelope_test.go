// 本文件锁定 JSONL 信封手写拼装与 encoding/json 的逐字节等价：
// marshalJSONLRecord 的输出必须与 json.Marshal(JSONLRecord) 完全一致
// （含 omitempty、RawMessage compaction、HTML 转义与非法输入的错误口径），
// 任何一侧编码规则变化都应在这里当场爆掉。
package debuglog

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// TestMarshalJSONLRecordMatchesEncodingJSON 覆盖字段各形态：seq/elapsed
// 极值、event 省略与全类转义字符、data 的 nil/空白/缩进/嵌套引号/HTML
// 敏感字节——手写拼装的输出必须与反射 marshal 逐字节一致。
func TestMarshalJSONLRecordMatchesEncodingJSON(t *testing.T) {
	rfcTime := time.Date(2026, 9, 18, 10, 24, 36, 123456789, time.FixedZone("CST", 8*3600))
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
		[]byte("{\"s\":\"line sep end\"}"),
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
		"sep para end",
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
// Time 各形态（零值/纳秒/时区偏移）的拼装——生产值恒为 RFC3339Nano
// 可表区间内的真实时刻，任意字符串的转义覆盖由 Event 字段承担。
func TestMarshalJSONLRecordSeqAndTimeEdges(t *testing.T) {
	times := []time.Time{
		time.Date(2026, 9, 18, 10, 24, 36, 0, time.FixedZone("CST", 8*3600)),
		{},
		time.Date(2026, 9, 18, 10, 24, 36, 999999999, time.UTC),
		time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("N", -7*3600-30*60)),
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

// TestMarshalJSONLRecordMarshalClean 校验 DataMarshalClean 快路径：
// json.Marshal 直产字节被逐字透传，与 marshal 参考输出一致——直拷
// 与 Compact+escape 对 marshal 洁净输入是恒等变换。
func TestMarshalJSONLRecordMarshalClean(t *testing.T) {
	tm := time.Date(2026, 9, 18, 10, 24, 36, 123456789, time.UTC)
	for _, value := range []any{
		map[string]any{"s": "<a>&'\"\\", "n": 1.5, "u": "事件"},
		[]any{1, "x", map[string]any{"k": "sep para"}},
		"plain string",
		nil,
		42,
	} {
		clean, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		record := JSONLRecord{Seq: 3, Time: tm, Data: clean, DataMarshalClean: true}
		want, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		got, err := marshalJSONLRecord(record)
		if err != nil {
			t.Fatalf("marshalJSONLRecord: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("clean fast-path mismatch for %q:\n got: %q\nwant: %q", clean, got, want)
		}
	}
}

// TestMarshalJSONLRecordNoTrailingNewline 行尾换行由 appendJSONL 的
// 缓冲层统一加，信封函数自身不得携带——防回归出双换行行。
func TestMarshalJSONLRecordNoTrailingNewline(t *testing.T) {
	got, err := marshalJSONLRecord(JSONLRecord{Seq: 1, Time: time.Unix(0, 0).UTC(), Data: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[len(got)-1] != '}' {
		t.Fatalf("line should end with '}': %q", got)
	}
}
