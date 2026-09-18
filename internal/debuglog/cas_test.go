// 本文件验证 CAS 写路径的端到端语义：01 经编码协程切块后以 manifest
// 文件行 + 共享 blob + ref 行同批落库，两个目录的重复前缀只存一份；
// 读侧 manifest 重组与「delta 帧以 manifest 重组出的 01 明文为字典」
// 的混合路径都按端点口径读出原文。
package debuglog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WncFht/devin2api/internal/store"
)

// casCorpus 造 01 体量的脱敏 JSON：大体积共享「系统提示+工具声明」位
// （跨请求逐字节重复的 CAS 主场景）加每请求独立尾部，长度远超
// casMaxChunk 保证切出多块。内容按位置变化，模拟真实工具声明的
// 非周期结构——纯周期文本的 rolling digest 退化、切块过少。
func casCorpus(tail string) map[string]any {
	var sys strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&sys, `{"name":"tool-%04d","description":"%s","schema":{"type":"object"}} `,
			i, strings.Repeat(string(rune('a'+i%26)), 20+i%40))
	}
	return map[string]any{
		"model": "claude",
		"sys":   sys.String(),
		"tail":  tail,
	}
}

// TestCASEndToEnd 走完整管道：WriteJSON→编码协程→写 worker→事务→
// DebugFile 重组。断言库存形态（manifest 行 + blobs + refs）、两目录
// 共享块只存一份、delta 帧的 01 字典经 manifest 重组取回。库存断言
// 直查 SQLite——形态本身是交付物，不能只信读路径的自洽。
func TestCASEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, st)
	defer manager.Close()
	ctx := context.Background()

	write := func(tail string) *Recorder {
		recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/responses"})
		if recorder == nil {
			t.Fatal("Start() = nil")
		}
		recorder.WriteJSON(StageHTTPRequest, casCorpus(tail))
		// 03 与 01 大量同文——走 delta 帧形态，读侧字典须由 manifest
		// 重组：这条混合路径是二期最容易断的链。
		wire := casCorpus(tail)
		wire["wire"] = true
		recorder.WriteJSON(StageDevinRequest, wire)
		recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
		waitDrained(recorder)
		return recorder
	}
	r1 := write("dir-one-tail")
	r2 := write("dir-two-tail")

	// 直查库存形态（WAL 下并发只读连接不干扰写侧）。
	ro, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	// 01 行是 manifest（\x00CAS1），非 gzip/raw。
	var magic []byte
	if err := ro.QueryRowContext(ctx,
		`SELECT SUBSTR(content,1,5) FROM debug_files WHERE dir=? AND name=?`,
		r1.dir, StageHTTPRequest).Scan(&magic); err != nil {
		t.Fatal(err)
	}
	if string(magic) != "\x00CAS1" {
		t.Fatalf("01 stored magic = %x, want CAS manifest", magic)
	}
	// 03 走 zstd delta 帧（magic 28B52FFD）。
	if err := ro.QueryRowContext(ctx,
		`SELECT SUBSTR(content,1,4) FROM debug_files WHERE dir=? AND name=?`,
		r1.dir, StageDevinRequest).Scan(&magic); err != nil {
		t.Fatal(err)
	}
	if string(magic) != "\x28\xb5\x2f\xfd" {
		t.Fatalf("03 stored magic = %x, want zstd delta frame", magic)
	}

	// 跨目录共享：两目录的 ref 行各自挂全量切块，blob 只存去重并集——
	// 共享块数 = refs − blobs。
	var blobs, refs int64
	for _, q := range []struct {
		sql  string
		dest *int64
	}{
		{`SELECT COUNT(*) FROM debug_blobs`, &blobs},
		{`SELECT COUNT(*) FROM debug_chunk_refs`, &refs},
	} {
		if err := ro.QueryRowContext(ctx, q.sql).Scan(q.dest); err != nil {
			t.Fatal(err)
		}
	}
	if blobs == 0 || refs == 0 || blobs >= refs {
		t.Fatalf("dedup broken: blobs %d refs %d", blobs, refs)
	}
	t.Logf("e2e dedup: blobs %d shared across %d refs (saved %d)", blobs, refs, refs-blobs)

	// 读回逐字节语义校验：manifest 重组出的 JSON 与脱敏后原文等价。
	tails := map[string]string{r1.dir: "dir-one-tail", r2.dir: "dir-two-tail"}
	for _, recorder := range []*Recorder{r1, r2} {
		var got map[string]any
		if err := json.Unmarshal([]byte(readTestFile(t, manager, recorder.dir, StageHTTPRequest)), &got); err != nil {
			t.Fatalf("%s 01 reassembled is not JSON: %v", recorder.dir, err)
		}
		want := casCorpus(tails[recorder.dir])
		if got["sys"] != want["sys"] || got["tail"] != want["tail"] {
			t.Fatalf("%s 01 reassembled content drifted", recorder.dir)
		}
		var wire map[string]any
		if err := json.Unmarshal([]byte(readTestFile(t, manager, recorder.dir, StageDevinRequest)), &wire); err != nil {
			t.Fatalf("%s 03 delta read is not JSON: %v", recorder.dir, err)
		}
		if wire["tail"] != want["tail"] || wire["sys"] != want["sys"] || wire["wire"] != true {
			t.Fatalf("%s 03 delta-decoded content drifted", recorder.dir)
		}
	}
}
