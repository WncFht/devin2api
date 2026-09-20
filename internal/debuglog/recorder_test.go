// 本文件验证调试日志的目录隔离、JSONL 顺序、脱敏和附件落库策略。
package debuglog

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

// TestRecorderWritesRedactedStagesAndAttachments 的测试动机是防止诊断日志泄露凭据或重复嵌入大图片。
func TestRecorderWritesRedactedStagesAndAttachments(t *testing.T) {
	st := openTestStore(t)
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, st)
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/responses"})
	if recorder == nil {
		t.Fatal("Start() = nil")
	}
	image := base64.StdEncoding.EncodeToString([]byte("png-data"))
	recorder.WriteJSON("01-http-request.json", map[string]any{
		"authorization": "secret",
		// "f" 是客户端负载里的普通短键名——只有上游 metadata.f 指纹该脱敏。
		"f": "client-field",
		"body": map[string]any{
			"image": map[string]any{"mime_type": "image/png", "data": image},
		},
	})
	recorder.WriteJSON("03-devin-request.json", map[string]any{"metadata": map[string]any{"api_key": "secret", "f": "fingerprint"}})
	recorder.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"delta_text": "a"})
	recorder.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"delta_text": "b"})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: "model", Provider: "devin", Stream: true})
	waitDrained(recorder)

	httpLog := readTestFile(t, manager, recorder.dir, "01-http-request.json")
	if strings.Contains(httpLog, "secret") || strings.Contains(httpLog, image) {
		t.Fatalf("request log contains a secret or inline image: %s", httpLog)
	}
	if !strings.Contains(httpLog, `"f":"client-field"`) {
		t.Fatalf("non-metadata \"f\" key was over-redacted: %s", httpLog)
	}
	if !strings.Contains(httpLog, `"file":"attachments/image-001.png"`) {
		t.Fatalf("request log has no attachment reference: %s", httpLog)
	}
	devinLog := readTestFile(t, manager, recorder.dir, "03-devin-request.json")
	if strings.Contains(devinLog, "secret") || strings.Contains(devinLog, "fingerprint") {
		t.Fatalf("Devin request log contains credentials: %s", devinLog)
	}
	lines := strings.Split(strings.TrimSpace(readTestFile(t, manager, recorder.dir, "04-devin-response.jsonl")), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"seq":1`) || !strings.Contains(lines[1], `"seq":2`) {
		t.Fatalf("JSONL sequence = %q", lines)
	}
	data, _, _, err := manager.ReadFile(context.Background(), recorder.dir, "attachments/image-001.png")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "png-data" {
		t.Fatalf("attachment = %q, want png-data", got)
	}
	meta := readTestFile(t, manager, recorder.dir, "meta.json")
	if !strings.Contains(meta, `"provider": "devin"`) || !strings.Contains(meta, `"result": "completed"`) {
		t.Fatalf("meta = %s", meta)
	}
}

// TestManagerAllocatesCollisionSuffix 的测试动机是保证同秒并发请求不会共写同一个目录。
func TestManagerAllocatesCollisionSuffix(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, nil)
	manager.now = func() time.Time { return time.Date(2027, time.January, 1, 23, 54, 54, 0, time.Local) }
	first := manager.Start(RequestMeta{Method: "POST", Path: "/v1/responses"})
	second := manager.Start(RequestMeta{Method: "POST", Path: "/v1/responses"})
	if first == nil || second == nil {
		t.Fatal("Start() returned nil")
	}
	if first.dir == second.dir {
		t.Fatalf("request directories are equal: %s", first.dir)
	}
	if !strings.HasSuffix(second.dir, "-02") {
		t.Fatalf("second directory = %q, want -02 suffix", second.dir)
	}
}

// TestClaimDirRetriesAfterTransientLock 验证写锁被外部连接瞬时独占时目录
// 占位退避重试仍抢到名——reuseport 交接期对端写者持锁正是 BEGIN IMMEDIATE
// 的形态。首个 reqStoreOpTimeout 窗口被 sqlite busy 等待耗尽后 claim 返回
// deadline，锁释放后的重试落库成功；没有重试时该请求只能走 dirless 兜底行。
func TestClaimDirRetriesAfterTransientLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, st)
	defer manager.Close()

	// 外部连接 BEGIN IMMEDIATE 持写锁比 reqStoreOpTimeout 略长：首试的
	// busy 等待被 5s ctx 掐断，退避重试在锁释放后落库。
	holder, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("sql.Open holder: %v", err)
	}
	defer func() { _ = holder.Close() }()
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	go func() {
		time.Sleep(reqStoreOpTimeout + 600*time.Millisecond)
		_, _ = conn.ExecContext(context.Background(), "COMMIT")
	}()

	started := time.Now()
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	elapsed := time.Since(started)
	if recorder == nil {
		t.Fatal("Start() = nil under transient write-lock hold; want retried claim to succeed")
	}
	if elapsed < reqStoreOpTimeout {
		t.Fatalf("Start() returned in %s — claim did not burn a full first-attempt window", elapsed)
	}
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(recorder)
	if _, _, _, err := manager.ReadFile(context.Background(), recorder.dir, MetaFile); err != nil {
		t.Fatalf("meta.json missing in retried dir %s: %v", recorder.dir, err)
	}
}

// TestClaimDirNoRetryOnPermanentError 验证非 deadline 错误不付退避重试的
// 代价：关库后 BeginTx 即刻返回「sql: database is closed」，不是停滞签名，
// Start 应立即放弃——若走重试路径至少会付出一个 claimRetryBackoff 的睡眠。
func TestClaimDirNoRetryOnPermanentError(t *testing.T) {
	st := openTestStore(t)
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, st)
	defer manager.Close()
	_ = st.Close()

	started := time.Now()
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	if recorder != nil {
		t.Fatal("Start() on closed store = non-nil, want nil")
	}
	if elapsed := time.Since(started); elapsed >= claimRetryBackoff {
		t.Fatalf("Start() took %s on permanent error — retry backoff should not run", elapsed)
	}
}

// TestWriteErrorKeepsFirstCause 的测试动机是让最接近故障源的阶段不被外层通用错误覆盖。
func TestWriteErrorKeepsFirstCause(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, openTestStore(t))
	recorder := manager.Start(RequestMeta{})
	recorder.WriteError("devin_connect", os.ErrPermission)
	recorder.WriteError("provider_stream", os.ErrNotExist)
	// 写任务经队列异步执行，等排空后才能读到行。
	recorder.Complete(Completion{StatusCode: 500, Result: "failed"})
	waitDrained(recorder)
	log := readTestFile(t, manager, recorder.dir, "error.json")
	if !strings.Contains(log, "devin_connect") || strings.Contains(log, "provider_stream") {
		t.Fatalf("error log = %s", log)
	}
}

// TestSameSecondSuffixBeyondPattern 验证同秒第 100+ 个请求的目录名仍被
// 读取面接受：%02d 后缀位数不设上限，三位数后缀不得被 requestDirPattern 拒绝。
func TestSameSecondSuffixBeyondPattern(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, openTestStore(t))
	manager.now = func() time.Time { return time.Date(2027, time.January, 1, 23, 54, 54, 0, time.Local) }
	var recorders []*Recorder
	for i := 0; i < 105; i++ {
		recorder := manager.Start(RequestMeta{Method: "POST", Path: "/x"})
		if recorder == nil {
			t.Fatalf("request %d: Start returned nil", i)
		}
		recorders = append(recorders, recorder)
	}
	dir := recorders[104].dir
	if !strings.HasSuffix(dir, "-105") {
		t.Fatalf("dir = %q, want -105 suffix", dir)
	}
	for _, recorder := range recorders {
		recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
		waitDrained(recorder)
	}
	if _, err := manager.Detail(context.Background(), dir); err != nil {
		t.Fatalf("Detail(%q): %v", dir, err)
	}
	if _, _, _, err := manager.ReadFile(context.Background(), dir, "meta.json"); err != nil {
		t.Fatalf("ReadFile(%q): %v", dir, err)
	}
}

// TestSanitizeEscapedAndHyphenatedKeys 验证预筛无法按字节判别的敏感键名
// ——JSON 转义拼写的键名与连字符变体——仍被完整脱敏，不以原文落库。
func TestSanitizeEscapedAndHyphenatedKeys(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, openTestStore(t))
	recorder := manager.Start(RequestMeta{})
	// 键名用 JSON \u 转义拼写：字节预筛看到的不是解码后的 "apikey"，
	// 必须靠「键名含转义→慢路径」兜底，否则原文落库。
	escapedKey := `{"api` + "\\u006b" + `ey":"secret","plain":1}`
	recorder.WriteJSON("01-http-request.json", json.RawMessage(escapedKey))
	recorder.WriteJSON("03-devin-request.json", json.RawMessage(`{"api-key":"secret","set-cookie":"secret","keep":"ok"}`))
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(recorder)
	for _, name := range []string{"01-http-request.json", "03-devin-request.json"} {
		log := readTestFile(t, manager, recorder.dir, name)
		if strings.Contains(log, "secret") {
			t.Fatalf("%s contains unredacted credentials: %s", name, log)
		}
	}
}

// TestIOErrorsCountedOncePerKind 验证写管道内写库失败计入 ioErrors，
// 且同目录同类别失败只记一笔。
func TestIOErrorsCountedOncePerKind(t *testing.T) {
	st := openTestStore(t)
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, st)
	defer manager.Close()
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/x"})
	// 关掉 store 后所有写库作业都失败：暂存文件、chunk 与 Complete 的
	// 日志行/收尾同一批事务提交，"batch" 类失败只记一笔。
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	recorder.WriteJSON("01-http-request.json", map[string]any{"x": 1})
	recorder.WriteJSON("03-devin-request.json", map[string]any{"x": 2})
	recorder.AppendJSONL("04-devin-response.jsonl", "e", map[string]any{"x": 1})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(recorder)
	if got := manager.Stats()["io_errors"]; got != uint64(1) {
		t.Fatalf("io_errors = %v, want 1（batch 一笔）", got)
	}
}

// readTestFile 经读路径取回一个调试文件内容（断言即端点口径）。
func readTestFile(t *testing.T, manager *Manager, dir, name string) string {
	t.Helper()
	data, _, _, err := manager.ReadFile(context.Background(), dir, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// waitDrained 等请求的完成收尾首次落库事务 resolve——Complete 已改为
// 异步排空，断言落库内容前须先等它（等价改动前 Complete 返回的保证）。
func waitDrained(recorder *Recorder) {
	<-recorder.drained
}

// TestIndexWrittenOnComplete 验证每完成一个请求向 logs 表落一行可定位摘要。
func TestIndexWrittenOnComplete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic", ClientIP: "127.0.0.1", KeyHash: "abcd1234"})
	recorder.NoteUpstreamLatency()
	recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: "swe-2-max", RequestedModel: "swe-2", ResponseModel: "swe-2-max", Stream: true, UpstreamRequestID: "req-1"})
	waitDrained(recorder)
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(rows))
	}
	row := rows[0]
	if row.Dir == "" || row.API != "anthropic" || row.RequestedModel != "swe-2" ||
		row.ResponseModel != "swe-2-max" || row.UpstreamRequestID != "req-1" ||
		row.KeyHash != "abcd1234" || row.FirstUpstreamMS == nil {
		t.Fatalf("log row missing fields: %+v", row)
	}
}

// TestCleanerRemovesExpiredDirs 验证清理器删除超龄目录、跳过活跃目录。
func TestCleanerRemovesExpiredDirs(t *testing.T) {
	st := openTestStore(t)
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{Days: 7}, st)
	defer manager.Close()

	// 目录名内嵌时间戳即年龄依据：2020 年的名字远超 7 天保留界。
	ctx := context.Background()
	if err := st.PutDebugFile(ctx, "20200101-000000", MetaFile, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	active := manager.Start(RequestMeta{Method: "POST", Path: "/x"})

	if removed, _ := manager.cleanOnce(); removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	dirs, err := st.DebugDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if dir == "20200101-000000" {
			t.Fatal("expired dir should be deleted")
		}
	}
	if _, err := manager.Detail(context.Background(), active.dir); err != nil {
		t.Fatal("active dir must be protected")
	}
	active.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(active)
}

// TestDroppedCounterOnClosedQueue 验证 Complete 之后的迟到写入计
// lateWrites，不进真丢弃口径（recorder.dropped 与 droppedTotal）。
func TestDroppedCounterOnClosedQueue(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, nil)
	recorder := manager.Start(RequestMeta{})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	recorder.WriteJSON("late.json", map[string]any{"x": 1})
	if got := recorder.dropped.Load(); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
	if got := manager.droppedTotal.Load(); got != 0 {
		t.Fatalf("droppedTotal = %d, want 0", got)
	}
	if got := manager.lateWrites.Load(); got != 1 {
		t.Fatalf("lateWrites = %d, want 1", got)
	}
	if got := recorder.lateWrites.Load(); got != 1 {
		t.Fatalf("recorder.lateWrites = %d, want 1", got)
	}
}

// TestLateWritesInMeta 验证收尾 meta 序列化前到达的门口拒收计入
// meta.json 的 late_writes：关停中（manager.closing）的入队在 closed
// 置位前被拒，拒收数随完结块出账。
func TestLateWritesInMeta(t *testing.T) {
	manager := &Manager{
		queues:      []chan writeTask{make(chan writeTask, 4)},
		encoderDone: []chan struct{}{make(chan struct{})},
		insertQ:     make(chan insertOp, 4),
		workerGone:  make(chan struct{}),
	}
	recorder := &Recorder{manager: manager}
	manager.closing.Store(true)

	recorder.enqueue(func() {})
	completion := Completion{StatusCode: 200, Result: "completed"}
	var meta MetaSummary
	if err := json.Unmarshal(recorder.metaJSON(&completion), &meta); err != nil {
		t.Fatalf("metaJSON unmarshal: %v", err)
	}
	if meta.LateWrites != 1 {
		t.Fatalf("meta.late_writes = %d, want 1", meta.LateWrites)
	}
	if got := manager.lateWrites.Load(); got != 1 {
		t.Fatalf("manager.lateWrites = %d, want 1", got)
	}
}

// TestDroppedCounterOnFullQueue 验证编码分片队列打满时 enqueue 走
// default 丢弃：手工搭一个不起编码/写协程的 manager，小容量分片队列
// 填满后下一个任务必掉。
func TestDroppedCounterOnFullQueue(t *testing.T) {
	manager := &Manager{
		queues:      []chan writeTask{make(chan writeTask, 4)},
		encoderDone: []chan struct{}{make(chan struct{})},
		insertQ:     make(chan insertOp, 4),
		workerGone:  make(chan struct{}),
	}
	recorder := &Recorder{manager: manager}
	for i := 0; i < 4; i++ {
		recorder.enqueue(func() {})
	}
	if got := recorder.dropped.Load(); got != 0 {
		t.Fatalf("dropped = %d after filling queue, want 0", got)
	}
	recorder.enqueue(func() {})
	if got := recorder.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d after overflow, want 1", got)
	}
}

// newBareManager 手搭一个不起编码/写协程的 manager：队列与 worker 通道
// 由测试直接驱动，用来确定性地重放 Complete 哨兵与兜底时序。
func newBareManager(st *store.Store, queueCap int) *Manager {
	return &Manager{
		store:         st,
		now:           time.Now,
		queues:        []chan writeTask{make(chan writeTask, queueCap)},
		encoderDone:   []chan struct{}{make(chan struct{})},
		shardEncoders: []*store.PayloadEncoder{store.NewPayloadEncoder()},
		writerEncoder: store.NewPayloadEncoder(),
		insertQ:       make(chan insertOp, 4),
		workerStop:    make(chan struct{}),
		workerGone:    make(chan struct{}),
		dirtyBufs:     map[*Recorder]struct{}{},
		activeDirs:    map[string]*Recorder{},
	}
}

// newBareRecorder 手搭挂在 manager 上的 recorder：writer 私有字段
// （stagedFiles/chunkBufs/ioErrSeen）初始化成写 worker 拿到的形状。
func newBareRecorder(manager *Manager, dir string) *Recorder {
	recorder := &Recorder{
		manager:     manager,
		dir:         dir,
		startedAt:   time.Now(),
		drained:     make(chan struct{}),
		stagedFiles: map[string]stagedFile{},
		chunkBufs:   map[string]*bytes.Buffer{},
		ioErrSeen:   map[string]struct{}{},
	}
	recorder.requestReadyMS.Store(-1)
	recorder.upstreamSentMS.Store(-1)
	recorder.upstreamOpenMS.Store(-1)
	recorder.firstUpstreamMS.Store(-1)
	recorder.firstClientMS.Store(-1)
	recorder.upstreamDoneMS.Store(-1)
	recorder.assignModelMS.Store(-1)
	recorder.modelsFetchMS.Store(-1)
	return recorder
}

// TestCompleteReturnsBeforeDrainCommit 钉死 C1 的两条时序保证：哨兵滞留
// 在分片队列时 Complete 已返回（不等落库），且目录的清理保护横跨「收尾
// 已入列 pendingCompletions、事务未提交」的整段窗口——releaseDir 只在
// 批量事务落库后发生，提前解除会让未落库目录被龄删/淘汰扫走。
func TestCompleteReturnsBeforeDrainCommit(t *testing.T) {
	st := openTestStore(t)
	manager := newBareManager(st, 4)
	manager.policy = RetentionPolicy{Days: 1}

	// 目录名内嵌 2020 年时间戳：Days:1 下它一旦失去活跃保护必被龄删——
	// 保护存续与否可以用 cleanOnce 直接裁决。先种一行 meta 让目录在
	// 清理器视野里存在。
	ctx := context.Background()
	dir := "20200101-000000"
	if err := st.PutDebugFile(ctx, dir, MetaFile, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	recorder := newBareRecorder(manager, dir)
	manager.activeDirs[dir] = recorder

	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})

	// Complete 返回时哨兵还躺在分片队列缓冲里：收尾未入列、无内容
	// 落库、保护未解除——队列积压时 Complete 不等 commit。
	if len(manager.pendingCompletions) != 0 {
		t.Fatal("completion queued before sentinel ran")
	}
	select {
	case <-recorder.drained:
		t.Fatal("drained closed before commit")
	default:
	}
	if removed, _ := manager.cleanOnce(); removed != 0 {
		t.Fatalf("active dir cleaned while completion pending: removed = %d", removed)
	}

	// 手动驱动编码→写段：哨兵 op 入 insertQ；垫一个 filler 并把
	// lastFlush 拨成新鲜值，按住 queueCompletion 的自动冲刷，让
	// 「收尾已入列未提交」的中间态可被断言。
	task := <-manager.queues[0]
	task.run()
	manager.lastFlush = time.Now()
	manager.insertQ <- insertOp{recorder: recorder, apply: func() {}}
	op := <-manager.insertQ
	op.apply()
	if len(manager.pendingCompletions) != 1 {
		t.Fatalf("pendingCompletions = %d, want 1", len(manager.pendingCompletions))
	}
	select {
	case <-recorder.drained:
		t.Fatal("drained closed before commit")
	default:
	}
	if rows, _, err := st.SearchLogs(ctx, store.LogQuery{}); err != nil || len(rows) != 0 {
		t.Fatalf("log row committed before flush: rows = %v, err = %v", rows, err)
	}
	// 收尾已入列、事务未提交：保护仍在——此刻失去它，未落库的暂存
	// 会随目录一起被龄删扫走。
	if removed, _ := manager.cleanOnce(); removed != 0 {
		t.Fatalf("uncommitted completion lost protection: removed = %d", removed)
	}

	manager.flushAll()
	select {
	case <-recorder.drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drained not closed after commit")
	}
	if _, ok := manager.activeDirs[dir]; ok {
		t.Fatal("dir still active after commit")
	}
	rows, _, err := st.SearchLogs(ctx, store.LogQuery{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, err = %v, want 1 committed row", rows, err)
	}
	// 落库后保护解除：同一轮清理判定现在删掉这个超龄目录。
	if removed, _ := manager.cleanOnce(); removed != 1 {
		t.Fatalf("removed = %d, want 1（提交后保护已解除）", removed)
	}
}

// TestCompleteFallbackAfterWriterGone 验证 worker 已退时 Complete 走
// 调用方兜底：分片队列送不进去（无缓冲、无消费者）即由 fallbackMu
// 直跑收尾与冲刷，Complete 返回时落库已完成。
func TestCompleteFallbackAfterWriterGone(t *testing.T) {
	st := openTestStore(t)
	manager := newBareManager(st, 0) // 无缓冲分片队列：发送必阻塞
	close(manager.workerGone)
	dir := "20990101-000000"
	recorder := newBareRecorder(manager, dir)
	manager.activeDirs[dir] = recorder

	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})

	select {
	case <-recorder.drained:
	default:
		t.Fatal("fallback did not commit completion synchronously")
	}
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, err = %v, want 1", rows, err)
	}
	if _, ok := manager.activeDirs[dir]; ok {
		t.Fatal("dir still active after fallback commit")
	}
}

// TestCompleteSentinelWatcherOnShutdown 复放关停竞态：workerStop 已闭、
// 编码协程已退（workerGone 未闭）时哨兵能送进分片缓冲但永无人消费——
// Complete 派的看守必须在 workerGone 关闭后用兜底把收尾落库。
func TestCompleteSentinelWatcherOnShutdown(t *testing.T) {
	st := openTestStore(t)
	manager := newBareManager(st, 1)
	close(manager.workerStop) // 关停进行中：编码协程排空后退出
	dir := "20990101-000001"
	recorder := newBareRecorder(manager, dir)
	manager.activeDirs[dir] = recorder

	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})

	// 哨兵滞留在分片缓冲（workerStop 已闭 → Complete 派了看守等
	// workerGone）；看守未醒前收尾不得入列。
	time.Sleep(20 * time.Millisecond)
	if len(manager.pendingCompletions) != 0 {
		t.Fatal("completion queued before workerGone")
	}
	select {
	case <-recorder.drained:
		t.Fatal("drained closed before workerGone")
	default:
	}

	close(manager.workerGone)
	select {
	case <-recorder.drained:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher fallback did not commit after workerGone")
	}
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, err = %v, want 1", rows, err)
	}
	if _, ok := manager.activeDirs[dir]; ok {
		t.Fatal("dir still active after watcher commit")
	}
}

// TestReaderListDetailAndFiles 验证日志行倒读、单请求详情与文件读取接口。
func TestReaderListDetailAndFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	defer manager.Close()
	for _, model := range []string{"m-a", "m-b"} {
		recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic"})
		recorder.WriteJSON("03-devin-request.json", map[string]any{"model": model})
		recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: model})
		waitDrained(recorder)
	}
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 2 || rows[0].Model != "m-b" || rows[1].Model != "m-a" {
		t.Fatalf("SearchLogs order = %+v", rows)
	}
	detail, err := manager.Detail(context.Background(), rows[0].Dir)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if len(detail.Meta) == 0 || !strings.Contains(string(detail.Meta), `"m-b"`) {
		t.Fatalf("detail meta = %s", detail.Meta)
	}
	var names []string
	for _, f := range detail.Files {
		names = append(names, f.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "meta.json") || !strings.Contains(strings.Join(names, ","), "03-devin-request.json") {
		t.Fatalf("files = %v", names)
	}
	data, total, truncated, err := manager.ReadFile(context.Background(), rows[0].Dir, "03-devin-request.json")
	if err != nil || truncated || total == 0 || !strings.Contains(string(data), "m-b") {
		t.Fatalf("ReadFile = %q total=%d truncated=%v err=%v", data, total, truncated, err)
	}
}

// TestReaderRejectsTraversal 验证目录名与文件名的路径穿越防护。
func TestReaderRejectsTraversal(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, openTestStore(t))
	defer manager.Close()
	recorder := manager.Start(RequestMeta{})
	dir := recorder.dir
	recorder.Complete(Completion{StatusCode: 200})
	waitDrained(recorder)
	if _, err := manager.Detail(context.Background(), "../etc"); err == nil {
		t.Fatal("Detail should reject traversal")
	}
	for _, bad := range []string{"../meta.json", "meta.json/../x", "/abs", "sub/dir/x.json", "attachments/a/b.bin"} {
		if _, _, _, err := manager.ReadFile(context.Background(), dir, bad); err == nil {
			t.Fatalf("ReadFile should reject %q", bad)
		}
	}
	if _, _, _, err := manager.ReadFile(context.Background(), dir, "meta.json"); err != nil {
		t.Fatalf("ReadFile meta.json: %v", err)
	}
}

// TestDetailMissingPayloadIsNotExist 验证日志行还在而 payload 已被整体淘汰
// 时 Detail 回 ErrNotExist——面板据此回「目录已删」404 而不是 200 空壳。
func TestDetailMissingPayloadIsNotExist(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, openTestStore(t))
	defer manager.Close()
	if _, err := manager.Detail(context.Background(), "20200101-000000"); !os.IsNotExist(err) {
		t.Fatalf("Detail on absent payload = %v, want ErrNotExist", err)
	}
}

// TestActiveRequestsSnapshot 验证进行中请求的活快照在 Complete 后消失。
func TestActiveRequestsSnapshot(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, nil)
	defer manager.Close()
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic"})
	active := manager.ActiveRequests()
	if len(active) != 1 || active[0].Meta.API != "anthropic" {
		t.Fatalf("ActiveRequests = %+v", active)
	}
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(recorder)
	if got := manager.ActiveRequests(); len(got) != 0 {
		t.Fatalf("ActiveRequests after Complete = %+v", got)
	}
}

// TestErrorsOnlyDropsCleanDirs 验证 errors-only 模式：干净完成的请求完结
// 即剥掉全部 payload（meta/error 锚点保留、logs 摘要行仍落），失败与
// premature_end_turn 可疑成功保留完整证据目录。
func TestErrorsOnlyDropsCleanDirs(t *testing.T) {
	st := openTestStore(t)
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, st)
	manager.SetErrorsOnly(true)
	defer manager.Close()

	clean := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	clean.WriteJSON("01-http-request.json", map[string]any{"x": 1})
	clean.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"d": 1})
	clean.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(clean)
	detail, err := manager.Detail(context.Background(), clean.dir)
	if err != nil {
		t.Fatalf("clean dir Detail: %v（meta 锚点应保留）", err)
	}
	for _, f := range detail.Files {
		if f.Name != MetaFile {
			t.Fatalf("clean dir retains payload file %q, want meta.json only", f.Name)
		}
	}
	// logs 行不受 errors-only 影响——payload 面收敛，检索面不丢。
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil || len(rows) != 1 || rows[0].Dir != clean.dir {
		t.Fatalf("clean request log row = %+v err=%v", rows, err)
	}

	failed := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	failed.WriteError(ErrStageDevinConnect, os.ErrPermission)
	failed.Complete(Completion{StatusCode: 500, Result: "failed"})
	waitDrained(failed)
	if _, err := manager.Detail(context.Background(), failed.dir); err != nil {
		t.Fatalf("failed dir must be kept: %v", err)
	}
	if _, _, _, err := manager.ReadFile(context.Background(), failed.dir, ErrorFile); err != nil {
		t.Fatalf("failed dir error.json: %v", err)
	}

	suspicious := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	suspicious.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"d": 1})
	suspicious.Complete(Completion{StatusCode: 200, Result: "completed", PrematureEndTurn: true})
	waitDrained(suspicious)
	if _, _, _, err := manager.ReadFile(context.Background(), suspicious.dir, "04-devin-response.jsonl"); err != nil {
		t.Fatalf("premature_end_turn dir must keep payload: %v", err)
	}
}

// TestErrorsOnlyKeepsInterestingSuccess 验证 errors-only 的「有趣成功」
// 豁免：救回例（retries/account_switches）、脱钩现场（04 标记行）与
// 慢尾（duration/first_upstream 越阈）命中的干净完成请求保留完整
// payload；不命中任何旗标的普通干净例仍被剥到 meta 锚点。
func TestErrorsOnlyKeepsInterestingSuccess(t *testing.T) {
	st := openTestStore(t)
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, st)
	manager.SetErrorsOnly(true)
	defer manager.Close()

	// keep 断言公共部：完结落库后 04 payload 必须仍可读。
	kept := func(recorder *Recorder, flag string) {
		t.Helper()
		waitDrained(recorder)
		if _, _, _, err := manager.ReadFile(context.Background(), recorder.dir, "04-devin-response.jsonl"); err != nil {
			t.Fatalf("%s: interesting dir must keep payload: %v", flag, err)
		}
	}

	retried := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	retried.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"d": 1})
	retried.NoteRetryAttempt(2, "transport")
	retried.Complete(Completion{StatusCode: 200, Result: "completed"})
	kept(retried, "retries>0")

	switched := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	switched.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"d": 1})
	switched.NoteAccountAttempt("lane-a", os.ErrPermission)
	switched.Complete(Completion{StatusCode: 200, Result: "completed"})
	kept(switched, "account_switches>0")

	attached := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	attached.AppendJSONL("04-devin-response.jsonl", "detached_attach", map[string]any{"key": "k", "origin_dir": "x"})
	attached.Complete(Completion{StatusCode: 200, Result: "completed"})
	kept(attached, "detached_attach marker")

	slow := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	slow.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"d": 1})
	slow.setStartedAt(time.Now().Add(-2 * interestingDurationMS * time.Millisecond))
	slow.Complete(Completion{StatusCode: 200, Result: "completed"})
	kept(slow, "duration outlier")

	slowUpstream := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	slowUpstream.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"d": 1})
	slowUpstream.firstUpstreamMS.Store(interestingFirstUpstreamMS + 1)
	slowUpstream.Complete(Completion{StatusCode: 200, Result: "completed"})
	kept(slowUpstream, "first_upstream outlier")

	plain := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	plain.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"d": 1})
	plain.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(plain)
	detail, err := manager.Detail(context.Background(), plain.dir)
	if err != nil {
		t.Fatalf("plain dir Detail: %v（meta 锚点应保留）", err)
	}
	for _, f := range detail.Files {
		if f.Name != MetaFile {
			t.Fatalf("plain clean dir retains payload file %q, want meta.json only", f.Name)
		}
	}
}

// TestPendingByteBudgetDropsAtCap 验证在飞预算满时编码产物按到达序丢弃：
// 预留回退不推进 insertQ、dropped 计数、丢弃字节量同步入账。
func TestPendingByteBudgetDropsAtCap(t *testing.T) {
	manager := newBareManager(nil, 4)
	recorder := newBareRecorder(manager, "20990101-000005")
	manager.activeDirs[recorder.dir] = recorder
	manager.inflightBytes.Store(pendingPayloadCapBytes)
	recorder.WriteJSON("03-devin-request.json", map[string]any{"x": "payload"})
	task := <-manager.queues[0]
	task.run()
	if got := manager.inflightBytes.Load(); got != pendingPayloadCapBytes {
		t.Fatalf("inflightBytes = %d, want cap（预留已回退）", got)
	}
	if got := recorder.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
	select {
	case <-manager.insertQ:
		t.Fatal("op pushed despite cap")
	default:
	}
	if got := manager.droppedPayloadBytes.Load(); got == 0 {
		t.Fatal("droppedPayloadBytes = 0, want >0")
	}
}

// TestPendingByteBudgetMandatoryBypass 验证锚点文件（error.json/终态
// meta/logRow）在预算满时照常入账暂存——归因锚点必须落库，超顶也收；
// 且随事务提交按同一归还路径归零。
func TestPendingByteBudgetMandatoryBypass(t *testing.T) {
	st := openTestStore(t)
	manager := newBareManager(st, 4)
	dir := "20990101-000006"
	recorder := newBareRecorder(manager, dir)
	manager.activeDirs[dir] = recorder
	manager.inflightBytes.Store(pendingPayloadCapBytes)
	recorder.WriteError("devin_connect", os.ErrPermission)
	task := <-manager.queues[0]
	task.run()
	op := <-manager.insertQ
	manager.runOp(op)
	if got := manager.inflightBytes.Load(); got <= pendingPayloadCapBytes {
		t.Fatalf("inflightBytes = %d, want > cap（锚点不过闸仍入账）", got)
	}
	recorder.Complete(Completion{StatusCode: 500, Result: "failed"})
	task = <-manager.queues[0]
	task.run()
	op = <-manager.insertQ
	manager.runOp(op) // queueCompletion → insertQ 空 → flushAll 提交
	// 提交成功后真实暂存账归零——种下的伪水位 cap 原样留下。
	if got := manager.inflightBytes.Load(); got != pendingPayloadCapBytes {
		t.Fatalf("inflightBytes = %d after commit, want seeded cap only", got)
	}
	if _, _, _, err := manager.ReadFile(context.Background(), dir, ErrorFile); err != nil {
		t.Fatalf("error.json not committed: %v", err)
	}
}

// TestPendingByteBudgetLifecycle 验证在飞字节账全生命周期归零：编码期
// 预留→op 落地转记 stagedBytes（同名覆盖只计净增量）→flush 提交归还；
// 收尾（meta/logRow）照常入账，提交完成后账面回到零。
func TestPendingByteBudgetLifecycle(t *testing.T) {
	st := openTestStore(t)
	manager := newBareManager(st, 8)
	dir := "20990101-000007"
	recorder := newBareRecorder(manager, dir)
	recorder.sequences = map[string]int{}
	manager.activeDirs[dir] = recorder
	drain := func() {
		for {
			select {
			case task := <-manager.queues[0]:
				task.run()
				continue
			default:
			}
			select {
			case op := <-manager.insertQ:
				manager.runOp(op)
			default:
				return
			}
		}
	}
	recorder.WriteJSON("03-devin-request.json", map[string]any{"x": "payload-a"})
	recorder.WriteJSON("03-devin-request.json", map[string]any{"x": "payload-b"})
	recorder.AppendJSONL("04-devin-response.jsonl", "e", map[string]any{"d": 1})
	drain()
	// op 落地后账面 = 暂存面实际持有：预留已结清，覆盖只留净增量。
	if got, staged := manager.inflightBytes.Load(), recorder.stagedBytes; got != staged || staged <= 0 {
		t.Fatalf("inflightBytes=%d stagedBytes=%d, want equal >0", got, staged)
	}
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	drain()
	select {
	case <-recorder.drained:
	default:
		t.Fatal("drained not closed after commit")
	}
	if got := manager.inflightBytes.Load(); got != 0 {
		t.Fatalf("inflightBytes = %d after commit, want 0", got)
	}
	if got := recorder.stagedBytes; got != 0 {
		t.Fatalf("stagedBytes = %d after commit, want 0", got)
	}
	if rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{}); err != nil || len(rows) != 1 {
		t.Fatalf("log rows = %v err=%v, want 1 committed row", rows, err)
	}
}

// TestPendingByteBudgetFailureRetains 验证写事务失败时暂存账不释放——
// 字节确实仍被持有（暂存保留下轮重试），水位如实反映积压；这正是预算
// 要 bound 住的病态增长面。
func TestPendingByteBudgetFailureRetains(t *testing.T) {
	st := openTestStore(t)
	manager := newBareManager(st, 4)
	dir := "20990101-000008"
	recorder := newBareRecorder(manager, dir)
	manager.activeDirs[dir] = recorder
	recorder.WriteJSON("03-devin-request.json", map[string]any{"x": "payload"})
	task := <-manager.queues[0]
	task.run()
	op := <-manager.insertQ
	manager.runOp(op)
	held := recorder.stagedBytes
	if held <= 0 {
		t.Fatalf("stagedBytes = %d, want >0", held)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	manager.flushAll()
	if got := manager.inflightBytes.Load(); got != held {
		t.Fatalf("inflightBytes = %d after failed flush, want %d（字节仍持有）", got, held)
	}
	if got := recorder.stagedBytes; got != held {
		t.Fatalf("stagedBytes = %d after failed flush, want %d", got, held)
	}
	if len(recorder.stagedFiles) == 0 {
		t.Fatal("stagedFiles dropped after failed flush")
	}
	if _, ok := manager.dirtyBufs[recorder]; !ok {
		t.Fatal("recorder removed from dirtyBufs after failed flush")
	}
}

// TestPendingByteBudgetRevertsOnWorkerGone 验证 op 在 pushInsert 遇
// workerGone 时当场退还 charge——字节未送达写侧即不再占账面。
// insertQ 填满让 select 只剩 workerGone 分支可走（双就绪时 select 随机）。
func TestPendingByteBudgetRevertsOnWorkerGone(t *testing.T) {
	manager := newBareManager(nil, 4)
	recorder := newBareRecorder(manager, "20990101-000010")
	manager.activeDirs[recorder.dir] = recorder
	recorder.WriteJSON("03-devin-request.json", map[string]any{"x": "payload"})
	for i := 0; i < cap(manager.insertQ); i++ {
		manager.insertQ <- insertOp{recorder: recorder, apply: func() {}}
	}
	close(manager.workerGone)
	task := <-manager.queues[0]
	task.run()
	if got := manager.inflightBytes.Load(); got != 0 {
		t.Fatalf("inflightBytes = %d after workerGone drop, want 0", got)
	}
	if got := manager.droppedTotal.Load(); got != 1 {
		t.Fatalf("droppedTotal = %d, want 1", got)
	}
}

// TestPendingBytesMaxTracksPeak 验证水位峰值的进程期单调口径：峰值只随
// 账面真实到达过的水位上移，归还后不回落、未超旧峰不动——pending_bytes
// 只报瞬时值，逼近 cap 的预警靠 max 口径。
func TestPendingBytesMaxTracksPeak(t *testing.T) {
	manager := newBareManager(nil, 4)
	manager.addInflight(100)
	manager.addInflight(300) // 账面水位 400 = 峰值
	manager.addInflight(-150)
	if got := manager.inflightBytesMax.Load(); got != 400 {
		t.Fatalf("inflightBytesMax = %d, want 400（峰值不随归还回落）", got)
	}
	manager.addInflight(100) // 账面 350，未超旧峰
	if got := manager.inflightBytesMax.Load(); got != 400 {
		t.Fatalf("inflightBytesMax = %d, want 400（未超旧峰不动）", got)
	}
	manager.addInflight(200) // 账面 550，刷新峰值
	if got := manager.inflightBytesMax.Load(); got != 550 {
		t.Fatalf("inflightBytesMax = %d, want 550", got)
	}
	if got := manager.Stats()["pending_bytes_max"]; got != int64(550) {
		t.Fatalf("Stats pending_bytes_max = %v, want 550", got)
	}
}

// TestDeltaStageRoundTrip 验证 01 基座钉入与 02/03* delta 落库的端到端
// 口径：写满一个 dir 后经读路径取回的字节与写入一致，且 02/03 行的库存
// 尺寸远小于逻辑尺寸（残差而非全量）。
func TestDeltaStageRoundTrip(t *testing.T) {
	st := openTestStore(t)
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, st)
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	// 载荷内嵌一段不可压缩的随机串：gzip 与无字典 zstd 都缩不动它，
	// 只有字典匹配的 delta 能把它摊没——库存尺寸因此能区分两种形态。
	seg := make([]byte, 48<<10)
	if _, err := rand.Read(seg); err != nil {
		t.Fatal(err)
	}
	shared := base64.StdEncoding.EncodeToString(seg)
	recorder.WriteJSON(StageHTTPRequest, map[string]any{"method": "POST", "body": shared})
	recorder.WriteJSON(StageRequestMessages, map[string]any{"model": "m", "messages": shared})
	recorder.WriteJSON(StageDevinRequest, map[string]any{"prompts": shared})
	recorder.WriteJSON(StageDevinRequestAttempt(2), map[string]any{"prompts": shared})
	recorder.WriteJSON(StageDevinSearchStem+"1.json", map[string]any{"query": shared})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(recorder)

	// 读回逐字节一致（含 01 自身的独立 gzip 形态与全部 03* 分片）。
	for _, name := range []string{StageHTTPRequest, StageRequestMessages, StageDevinRequest, StageDevinRequestAttempt(2), StageDevinSearchStem + "1.json"} {
		got := readTestFile(t, manager, recorder.dir, name)
		var decoded map[string]string
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, v := range decoded {
			if v == shared {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s read-back lost the shared segment", name)
		}
	}
	// 库存口径：随机段不可压缩 → 若 02/03* 走了独立 gzip，dir 库存总量
	// 应≈逻辑总量；delta 命中字典则只剩残差。
	files, err := st.DebugFileList(context.Background(), recorder.dir)
	if err != nil {
		t.Fatal(err)
	}
	var logical int64
	for _, f := range files {
		logical += f.Size
	}
	sizes, err := st.DebugDirSizes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stored := sizes[recorder.dir]
	if stored >= logical/3 {
		t.Fatalf("stored=%d logical=%d, want stored << logical（delta 残差）", stored, logical)
	}
	// 基座预算随 releaseDir 归零——不留账面泄漏。
	if got := manager.deltaBaseBytes.Load(); got != 0 {
		t.Fatalf("deltaBaseBytes = %d after release, want 0", got)
	}
}

// TestDeltaStageFallbacks 覆盖基座缺席/超限的降级口径：01 缺席时 02/03
// 落独立 gzip 照常读出；预算占满时钉座被拒，同样回退独立存储。
func TestDeltaStageFallbacks(t *testing.T) {
	st := openTestStore(t)
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, st)
	seg := make([]byte, 48<<10)
	if _, err := rand.Read(seg); err != nil {
		t.Fatal(err)
	}
	shared := base64.StdEncoding.EncodeToString(seg)
	dirSize := func(dir string) (stored, logical int64) {
		t.Helper()
		files, err := st.DebugFileList(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			logical += f.Size
		}
		sizes, err := st.DebugDirSizes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return sizes[dir], logical
	}

	// 无 01 的目录：02/03 照常落库读出；独立 gzip 对随机段缩不动，
	// 库存应≈逻辑量（与 delta 目录的近零残差形成对照）。
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/x"})
	recorder.WriteJSON(StageRequestMessages, map[string]any{"messages": shared})
	recorder.WriteJSON(StageDevinRequest, map[string]any{"prompts": shared})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(recorder)
	if got := readTestFile(t, manager, recorder.dir, StageRequestMessages); !strings.Contains(got, shared) {
		t.Fatal("02 without base lost content")
	}
	stored, logical := dirSize(recorder.dir)
	if stored <= logical/3 {
		t.Fatalf("stored=%d logical=%d, want stored ≈ logical（独立 gzip 缩不动随机段）", stored, logical)
	}

	// 预算占满的目录：钉座被拒，02/03 同样回退独立存储且读如常。
	manager.deltaBaseBytes.Store(deltaBaseCapBytes)
	defer manager.deltaBaseBytes.Store(0)
	full := manager.Start(RequestMeta{Method: "POST", Path: "/x"})
	full.WriteJSON(StageHTTPRequest, map[string]any{"body": shared})
	full.WriteJSON(StageRequestMessages, map[string]any{"messages": shared})
	full.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(full)
	if got := readTestFile(t, manager, full.dir, StageRequestMessages); !strings.Contains(got, shared) {
		t.Fatal("02 over cap lost content")
	}
	stored, logical = dirSize(full.dir)
	if stored <= logical/3 {
		t.Fatalf("stored=%d logical=%d over cap, want stored ≈ logical", stored, logical)
	}
}

// TestPregateTimingMetaJSON 钉住闸门前相位计数的 meta.json 口径：未发生
// 时缺席（-1 哨兵 → nil → omitempty），记录后按原值出账（含 0ms——
// 缓存命中与「相位未发生」靠 presence 区分）。
func TestPregateTimingMetaJSON(t *testing.T) {
	manager := NewManager("", RetentionPolicy{}, nil)
	recorder := newBareRecorder(manager, "20200101-000000")

	data := recorder.metaJSON(nil)
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if _, ok := meta["assign_model_ms"]; ok {
		t.Fatal("assign_model_ms present before recording")
	}
	if _, ok := meta["models_fetch_ms"]; ok {
		t.Fatal("models_fetch_ms present before recording")
	}

	recorder.NoteAssignModelMS(0)
	recorder.NoteModelsFetchMS(42150)
	data = recorder.metaJSON(nil)
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if v, ok := meta["assign_model_ms"]; !ok || v.(float64) != 0 {
		t.Fatalf("assign_model_ms = %v ok=%v, want 0", v, ok)
	}
	if v, ok := meta["models_fetch_ms"]; !ok || v.(float64) != 42150 {
		t.Fatalf("models_fetch_ms = %v ok=%v, want 42150", v, ok)
	}
}

// TestDetachedEventsMirrorToMeta 钉住脱钩标记的 meta 镜像口径：
// NoteDetachedEvent 累积的事件随 metaJSON 出账为 detached_events，
// 条目带 kind/time/elapsed_ms 三戳且 detail 键原样透传——队列满把
// 04 标记行丢弃时，meta 仍是脱钩生命周期的持久记录。
func TestDetachedEventsMirrorToMeta(t *testing.T) {
	manager := NewManager("", RetentionPolicy{}, nil)
	recorder := newBareRecorder(manager, "20200101-000007")

	data := recorder.metaJSON(nil)
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if _, ok := meta["detached_events"]; ok {
		t.Fatal("detached_events present before any NoteDetachedEvent")
	}

	detail := map[string]any{"key": "k1", "buffered_events": 7}
	recorder.NoteDetachedEvent("detached", detail)
	recorder.NoteDetachedEvent("detached_attach", map[string]any{"key": "k1", "origin_dir": "d0", "state": "running", "buffered_events": 7})
	data = recorder.metaJSON(nil)
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	events, ok := meta["detached_events"].([]any)
	if !ok || len(events) != 2 {
		t.Fatalf("detached_events = %v, want 2 entries", meta["detached_events"])
	}
	first, _ := events[0].(map[string]any)
	second, _ := events[1].(map[string]any)
	if first["kind"] != "detached" || first["key"] != "k1" || first["buffered_events"] != float64(7) {
		t.Fatalf("detached_events[0] = %v", first)
	}
	if second["kind"] != "detached_attach" || second["origin_dir"] != "d0" {
		t.Fatalf("detached_events[1] = %v", second)
	}
	for i, event := range []map[string]any{first, second} {
		if _, ok := event["time"]; !ok {
			t.Fatalf("detached_events[%d] missing time stamp", i)
		}
		if _, ok := event["elapsed_ms"]; !ok {
			t.Fatalf("detached_events[%d] missing elapsed_ms", i)
		}
	}
	// 入参 map 不被改写：调用方把同一张表同时喂给 04 行与本镜像，
	// 若原地注入 kind/time 会污染 AppendJSONL 的延迟求值产物。
	if _, ok := detail["kind"]; ok {
		t.Fatal("NoteDetachedEvent must not mutate the caller's detail map")
	}
}

// TestDetachedEventsSurviveQueueDrop 钉住镜像的免疫性：分片队列满把
// AppendJSONL 的 04 标记行丢弃时，同点的 NoteDetachedEvent 仍随完结
// meta 出账——这正是本字段的存在理由（生产丢标记事故的直接回归）。
func TestDetachedEventsSurviveQueueDrop(t *testing.T) {
	manager := &Manager{
		queues:      []chan writeTask{make(chan writeTask, 1)},
		encoderDone: []chan struct{}{make(chan struct{})},
		insertQ:     make(chan insertOp, 4),
		workerGone:  make(chan struct{}),
	}
	recorder := &Recorder{manager: manager, startedAt: time.Now(), sequences: make(map[string]int)}
	// 填满分片队列：下一条 AppendJSONL 必走 default 丢弃。
	recorder.enqueue(func() {})
	recorder.AppendJSONL(StageDevinResponse, "detached", map[string]any{"key": "k9"})
	if got := recorder.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1（标记行确已被丢）", got)
	}
	recorder.NoteDetachedEvent("detached", map[string]any{"key": "k9"})
	data := recorder.metaJSON(&Completion{StatusCode: 200, Result: "completed", EndedAt: time.Now()})
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	events, ok := meta["detached_events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("detached_events = %v, want the dropped marker mirrored", meta["detached_events"])
	}
	if kind := events[0].(map[string]any)["kind"]; kind != "detached" {
		t.Fatalf("kind = %v, want detached", kind)
	}
}

// TestDetachedMarkerSurvivesComplete 钉住 04 脱钩标记行的 closed 豁免：
// 消费方 Recv 内的脱钩登记与写出方 Complete 在 ctx.Done 上竞速，标记
// 晚于 Complete 入队时必须仍落 debug_chunks——哨兵之后入队的标记任务
// 由下一轮 flushAll 照收；同刻入队的非标记行仍被 closed 门口拒收。
// 手工驱动分片/写段重放「标记行排在排空哨兵之后」的竞速输家形态。
func TestDetachedMarkerSurvivesComplete(t *testing.T) {
	st := openTestStore(t)
	manager := newBareManager(st, 4)
	dir := "20200101-000099"
	recorder := newBareRecorder(manager, dir)
	recorder.sequences = map[string]int{}
	manager.activeDirs[dir] = recorder

	recorder.Complete(Completion{StatusCode: 499, Result: "disconnected"})

	// closed 置位后：脱钩标记经豁免闸进分片队列，普通行照常计入
	// lateWrites——豁免只覆盖标记词表不外溢。
	recorder.AppendJSONL(StageDevinResponse, "detached", map[string]any{"key": "k", "buffered_events": 3})
	recorder.AppendJSONL(StageDevinResponse, "frame", map[string]any{"x": 1})
	if got := manager.lateWrites.Load(); got != 1 {
		t.Fatalf("lateWrites = %d, want 1（非标记行仍被 closed 拒收）", got)
	}

	// 依序驱动编码段：先哨兵（收尾入列）后标记任务。
	for i := 0; i < 2; i++ {
		task := <-manager.queues[0]
		task.run()
	}
	// insertQ 序即落库序：哨兵 op 跑 queueCompletion（内部 flushAll 先把
	// meta/日志行落库），标记 op 的暂存留给下一轮 flushAll。
	for i := 0; i < 2; i++ {
		op := <-manager.insertQ
		op.apply()
	}
	manager.flushAll()

	data, _, _, err := manager.ReadFile(context.Background(), dir, StageDevinResponse)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", StageDevinResponse, err)
	}
	if !strings.Contains(string(data), `"event":"detached"`) {
		t.Fatalf("04 = %q, want the post-Complete detached marker row", data)
	}
	if strings.Contains(string(data), `"event":"frame"`) {
		t.Fatalf("04 = %q, non-marker row must not survive closed", data)
	}
}

// TestFlushSplitsBatchAtDirBoundary 验证追平冲刷按目录界分包：批内目录
// 体积合计越过 writeBatchBoundBytes 时拆成多个事务提交——每个目录的
// files/chunks/refs 完整落在同一事务（目录是读者一致性的最小单位，
// 跨事务拆开会被读成撕裂半成品）。缩小界值让三目录批确定性分包，
// debugBatchWrite 换壳观察各次提交的目录归属。
func TestFlushSplitsBatchAtDirBoundary(t *testing.T) {
	st := openTestStore(t)
	manager := newBareManager(st, 4)
	oldBound, oldWrite := writeBatchBoundBytes, debugBatchWrite
	writeBatchBoundBytes = 11 << 10
	defer func() { writeBatchBoundBytes, debugBatchWrite = oldBound, oldWrite }()

	dirs := []string{"20990101-000201", "20990101-000202", "20990101-000203"}
	recorders := make([]*Recorder, len(dirs))
	for i, dir := range dirs {
		recorder := newBareRecorder(manager, dir)
		manager.activeDirs[dir] = recorder
		recorder.stageFile("01-http-request.json", stagedFile{stored: make([]byte, 5000), usize: 5000})
		recorders[i] = recorder
	}

	var batchDirs []map[string]bool
	debugBatchWrite = func(s *store.Store, ctx context.Context, b store.DebugBatch) error {
		seen := map[string]bool{}
		for _, f := range b.Files {
			seen[f.Dir] = true
		}
		for _, c := range b.Chunks {
			seen[c.Dir] = true
		}
		for _, r := range b.Refs {
			seen[r.Dir] = true
		}
		for _, d := range b.StripDirs {
			seen[d] = true
		}
		for _, l := range b.LogRows {
			if l != nil {
				seen[l.Dir] = true
			}
		}
		batchDirs = append(batchDirs, seen)
		return s.WriteDebugBatch(ctx, b)
	}
	manager.flushAll()

	// 5000B×3、界 11264B：{d1,d2} 一事务、{d3} 一事务——拆分前的单事务
	// 口径只有一次提交。
	if len(batchDirs) != 2 {
		t.Fatalf("WriteDebugBatch calls = %d, want 2（按目录界分包）", len(batchDirs))
	}
	landed := map[string]int{}
	for _, seen := range batchDirs {
		for dir := range seen {
			landed[dir]++
		}
	}
	for i, dir := range dirs {
		if landed[dir] != 1 {
			t.Fatalf("dir %s 落在 %d 个事务里（目录不可撕裂）", dir, landed[dir])
		}
		if _, _, ok, err := st.DebugFile(context.Background(), dir, "01-http-request.json", 0); err != nil || !ok {
			t.Fatalf("dir %s file not committed: ok=%v err=%v", dir, ok, err)
		}
		if _, dirty := manager.dirtyBufs[recorders[i]]; dirty {
			t.Fatalf("dir %s still dirty after commit", dir)
		}
	}
}

// TestFlushPartialCommitRetainsRemainder 钉死分包失败语义：首个分组事务
// 落库、第二组失败时，已提交前缀的目录照常释放（暂存归还、行可读），
// 失败组的暂存与收尾原样留下随下轮重试——drained 照常放行、清理保护
// 不解、幂等重放不产生重复行。注入第二组失败确定性复放该形态。
func TestFlushPartialCommitRetainsRemainder(t *testing.T) {
	st := openTestStore(t)
	manager := newBareManager(st, 4)
	oldBound, oldWrite := writeBatchBoundBytes, debugBatchWrite
	writeBatchBoundBytes = 8 << 10
	defer func() { writeBatchBoundBytes, debugBatchWrite = oldBound, oldWrite }()

	r1 := newBareRecorder(manager, "20990101-000301")
	r2 := newBareRecorder(manager, "20990101-000302")
	manager.activeDirs[r1.dir] = r1
	manager.activeDirs[r2.dir] = r2
	r1.stageFile("01-http-request.json", stagedFile{stored: make([]byte, 5000), usize: 5000})
	r1.appendJSONL("04-devin-response.jsonl", []byte(`{"seq":1}`))
	r2.stageFile("01-http-request.json", stagedFile{stored: make([]byte, 5000), usize: 5000})
	// r2 挂一份完成收尾——insertQ 占位 + lastFlush 拨新按住
	// queueCompletion 的自动冲刷，先攒出「收尾在列未提交」的中间态。
	manager.insertQ <- insertOp{recorder: r2, apply: func() {}}
	manager.lastFlush = time.Now()
	manager.queueCompletion(r2, Completion{StatusCode: 200, Result: "completed"})

	calls := 0
	debugBatchWrite = func(s *store.Store, ctx context.Context, b store.DebugBatch) error {
		calls++
		if calls == 2 {
			return errors.New("injected batch failure")
		}
		return s.WriteDebugBatch(ctx, b)
	}
	manager.flushAll()
	// 按 dir 名序 d1 在前：d1(~5KB) 一事务、d2(~6KB 文件+meta+收尾)
	// 一事务，第二组被注入失败。
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if _, _, ok, err := st.DebugFile(context.Background(), r1.dir, "01-http-request.json", 0); err != nil || !ok {
		t.Fatalf("committed prefix dir lost: ok=%v err=%v", ok, err)
	}
	if _, dirty := manager.dirtyBufs[r1]; dirty || r1.stagedBytes != 0 {
		t.Fatalf("committed dir still held: dirty=%v stagedBytes=%d", dirty, r1.stagedBytes)
	}
	if _, _, ok, err := st.DebugFile(context.Background(), r2.dir, "01-http-request.json", 0); err != nil || ok {
		t.Fatalf("failed dir prematurely visible: ok=%v err=%v", ok, err)
	}
	if _, dirty := manager.dirtyBufs[r2]; !dirty {
		t.Fatal("failed dir dropped from dirtyBufs")
	}
	if r2.stagedBytes == 0 {
		t.Fatal("failed dir stagedBytes released despite failed tx")
	}
	if len(manager.pendingCompletions) != 1 {
		t.Fatalf("pendingCompletions = %d, want 1（失败组收尾留下重试）", len(manager.pendingCompletions))
	}
	select {
	case <-r2.drained:
	default:
		t.Fatal("failed completion drained not released")
	}
	if got := manager.ioErrors.Load(); got != 1 {
		t.Fatalf("ioErrors = %d, want 1", got)
	}

	// 恢复后重试：d2 暂存与收尾补齐落库；d1 已释放不再重放——04 chunk
	// 若被重复追加会是两行，这里断言仍是一行（幂等无前缀重复）。
	debugBatchWrite = oldWrite
	manager.flushAll()
	data, _, ok, err := st.DebugFile(context.Background(), r1.dir, "04-devin-response.jsonl", 0)
	if err != nil || !ok || strings.Count(strings.TrimSpace(string(data)), "\n") != 0 {
		t.Fatalf("d1 04 lines after retry = %q ok=%v err=%v, want 单行不重复", data, ok, err)
	}
	if _, _, ok, err := st.DebugFile(context.Background(), r2.dir, "01-http-request.json", 0); err != nil || !ok {
		t.Fatalf("retried dir not committed: ok=%v err=%v", ok, err)
	}
	if len(manager.pendingCompletions) != 0 {
		t.Fatalf("pendingCompletions = %d after retry, want 0", len(manager.pendingCompletions))
	}
	if _, ok := manager.activeDirs[r2.dir]; ok {
		t.Fatal("retried dir still active after committed completion")
	}
	if rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{}); err != nil || len(rows) != 1 {
		t.Fatalf("log rows = %v err=%v, want 1（d2 收尾行）", rows, err)
	}
}

// TestDetachedEventRefreshesMetaAfterComplete 钉住 post-Complete 的脱钩
// 镜像刷新：终态 meta 首次落库后，NoteDetachedEvent 的追加经 closed
// 豁免闸重序列化 meta 并 OR REPLACE 重写 meta.json 行——脱钩泵余生里
// 到达的 detached_truncated 等事件不再沉默在内存累积器。断言完结块
// 字段不回退、finished_at 沿用首值不被刷新稀释、多条事件逐次覆盖。
// 手工驱动分片/写段复放「事件在收尾提交之后才入队」的后台泵形态。
func TestDetachedEventRefreshesMetaAfterComplete(t *testing.T) {
	st := openTestStore(t)
	manager := newBareManager(st, 4)
	dir := "20200101-000100"
	recorder := newBareRecorder(manager, dir)
	recorder.sequences = map[string]int{}
	manager.activeDirs[dir] = recorder

	recorder.Complete(Completion{StatusCode: 499, Result: "disconnected"})
	// 驱动哨兵→收尾入列：insertQ 空时 queueCompletion 内联 flushAll
	// 提交，终态 meta 首版落库（此刻无 detached_events）。
	task := <-manager.queues[0]
	task.run()
	op := <-manager.insertQ
	manager.runOp(op)
	waitDrained(recorder)

	before := readTestFile(t, manager, dir, MetaFile)
	if strings.Contains(before, "detached_events") {
		t.Fatalf("meta has detached_events before any event: %s", before)
	}

	// 首个 post-Complete 事件：豁免闸排入刷新任务——与 04 标记行同路。
	recorder.NoteDetachedEvent("detached_truncated", map[string]any{"key": "k", "budget_bytes": 8388608})
	task = <-manager.queues[0]
	task.run()
	op = <-manager.insertQ
	manager.runOp(op)
	manager.flushAll()

	after := readTestFile(t, manager, dir, MetaFile)
	for _, want := range []string{`"kind": "detached_truncated"`, `"status_code": 499`, `"result": "disconnected"`, `"budget_bytes": 8388608`} {
		if !strings.Contains(after, want) {
			t.Fatalf("refreshed meta missing %s: %s", want, after)
		}
	}
	// finished_at 沿用首值：finished−ended 的收尾排队口径不被刷新稀释。
	var first, refreshed map[string]any
	if err := json.Unmarshal([]byte(before), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(after), &refreshed); err != nil {
		t.Fatal(err)
	}
	if first["finished_at"] != refreshed["finished_at"] {
		t.Fatalf("finished_at moved across refresh: %v → %v", first["finished_at"], refreshed["finished_at"])
	}

	// 第二条事件再触发一轮重写：镜像覆盖脱钩全程而非只刷一次——
	// 终版 meta 同时留住两条事件。
	recorder.NoteDetachedEvent("detached", map[string]any{"key": "k", "buffered_events": 3})
	task = <-manager.queues[0]
	task.run()
	op = <-manager.insertQ
	manager.runOp(op)
	manager.flushAll()

	final := readTestFile(t, manager, dir, MetaFile)
	if !strings.Contains(final, `"kind": "detached_truncated"`) || !strings.Contains(final, `"kind": "detached"`) {
		t.Fatalf("final meta lost earlier refresh events: %s", final)
	}
}

// TestClaimStallErrorClassifiesSignatures 验证 claim 停滞签名分类：真实
// SQLITE_BUSY（裸连接对制造——*sqlite.Error 字段私有无法手造）与
// DeadlineExceeded 命中重试；Canceled、永久错误与 nil 不命中，不付退避
// 代价。BUSY 经本分类器进与 deadline 同一条退避重试路径（重试机制本身
// 由 TestClaimDirRetriesAfterTransientLock 覆盖）。
func TestClaimStallErrorClassifiesSignatures(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "busy.db")
	holder, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("sql.Open holder: %v", err)
	}
	defer func() { _ = holder.Close() }()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE t(x)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `COMMIT`) }()
	// 竞争者裸连接不装 busy handler：撞上 holder 的文件锁立即返回
	// 真实 *sqlite.Error{SQLITE_BUSY}。
	contender, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("sql.Open contender: %v", err)
	}
	defer func() { _ = contender.Close() }()
	_, busyErr := contender.ExecContext(ctx, `INSERT INTO t VALUES(1)`)
	if busyErr == nil {
		t.Fatal("contended insert = nil error, want SQLITE_BUSY")
	}
	if !store.IsBusy(busyErr) {
		t.Fatalf("fixture error %v is not BUSY — test vacuous", busyErr)
	}
	if !claimStallError(busyErr) {
		t.Fatal("raw SQLITE_BUSY must be a stall signature")
	}
	if !claimStallError(context.DeadlineExceeded) {
		t.Fatal("DeadlineExceeded must be a stall signature")
	}
	for _, other := range []error{nil, context.Canceled, os.ErrPermission} {
		if claimStallError(other) {
			t.Fatalf("claimStallError(%v) = true, want false", other)
		}
	}
}

// TestCloseStopsBackgroundWorkers 钉住关停协议：Close 返回后 worker/
// encoder/cleaner 三路 done 全部闭合——任何一路协程不退出会让 Close
// 挂死，回归在这里是具名断言失败而非整包超时。
func TestCloseStopsBackgroundWorkers(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, nil)
	closed := make(chan struct{})
	go func() { manager.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("Close did not return — background worker wedged")
	}
	for name, ch := range map[string]chan struct{}{
		"workerGone":   manager.workerGone,
		"encodersDone": manager.encodersDone,
		"cleanerDone":  manager.cleanerDone,
	} {
		select {
		case <-ch:
		default:
			t.Fatalf("%s still open after Close", name)
		}
	}
}

// TestNilReceiverTolerance 把 *Recorder 与 *Manager 的全部导出方法各调
// 一遍 nil 接收者：app.go 的调用点（debugManager.Start、recorder.Complete
// 等）刻意不做 nil 守卫——Start 在 debug 关闭时返回 nil，整条链路靠
// 「每个方法自身容忍 nil 接收者」成立。反射枚举方法集并喂零值参数，
// 某个方法失去 nil 守卫时这里直接 panic 现形，而不是等生产路径踩中。
func TestNilReceiverTolerance(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf((*Recorder)(nil)),
		reflect.TypeOf((*Manager)(nil)),
	} {
		for i := 0; i < typ.NumMethod(); i++ {
			method := typ.Method(i)
			// method.Func 的签名把接收者放在 In(0)；零值参数按签名现取。
			// 变参函数走 CallSlice（末参零值即 nil slice），普通走 Call。
			args := []reflect.Value{reflect.Zero(typ)}
			for j := 1; j < method.Type.NumIn(); j++ {
				args = append(args, reflect.Zero(method.Type.In(j)))
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s.%s panicked on nil receiver: %v", typ, method.Name, r)
					}
				}()
				if method.Type.IsVariadic() {
					method.Func.CallSlice(args)
				} else {
					method.Func.Call(args)
				}
			}()
		}
	}
}
