// 本文件实现单次 HTTP 请求的分阶段调试日志目录和 JSON/JSONL 写盘。
//
// Package debuglog 负责记录兼容 API 请求在 HTTP、中间模型和供应商协议之间的转换过程。
// 所有写盘作业经每请求一个有界任务队列交给单 worker 串行执行——
// 事件顺序即入队顺序，热路径只承担一次 channel send；队列满时丢弃并计数，
// 观测系统自身降级不拖垮请求。生命周期管理（保留期/总量清理）见 cleaner.go。
package debuglog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
)

// writeQueueSize 是单请求写任务的排队上限；流式帧在万级以下时绰绰有余。
const writeQueueSize = 4096

// RetentionPolicy 是请求日志的生命周期策略。
type RetentionPolicy struct {
	// Days 是请求目录整体保留天数；<=0 不按时间清理。
	Days int
	// MaxTotalMB 是 logs 根目录总量上限（MB），超限从最旧目录开始删；<=0 不按大小清理。
	MaxTotalMB int64
	// PayloadHours 是大体积阶段文件（03/04/06 与 attachments/）的保留小时数；
	// 超时只剥负载、保留 meta.json/error.json/01/02 等证据文件。<=0 不剥离。
	PayloadHours int
	// KeepErrorDirs 是容量淘汰时受保护的最新失败目录数（含 error.json 的目录）；
	// 超过该数量的旧失败目录仍可被淘汰，时间清理不受影响。<=0 不保护。
	KeepErrorDirs int
}

// Manager 在固定 logs 根目录下为每次请求创建独立 recorder，并持有
// 全局索引（index.jsonl）、用量聚合器与后台清理器。
type Manager struct {
	// root 是所有请求日志目录的根路径；空值表示禁用调试日志。
	root string
	// now 返回当前时间；测试会固定它以验证同秒目录分配。
	now func() time.Time
	// mutex 串行化目录名分配与 activeDirs 维护——锁内只做内存操作，
	// mkdir/索引写盘一律在锁外（见 indexMu）：一次磁盘停滞曾让所有
	// 排队请求的 ReadTimeout 在持锁等待中过期，锁一释放即批量假死。
	mutex sync.Mutex
	// activeDirs 记录仍有进行中请求的目录名→recorder，清理器必须跳过；
	// 存指针是为了 ActiveRequests 能直出进行中请求的活快照。
	activeDirs map[string]*Recorder
	// takenNames 记录本进程已知存在于磁盘、但不在 activeDirs 的目录名
	//（mkdir EEXIST 撞到的遗留目录）——锁内选名时跳过它们，避免同秒
	// 重启后反复撞名。只有撞名才入账，体量极小。
	takenNames map[string]struct{}
	// enabled 是请求日志的运行时开关；关闭时 Start 返回 nil，已有目录不受影响。
	enabled atomic.Bool
	// policy 是日志生命周期策略；policyMu 保护它：配置 reload 会运行时换值，
	// cleaner 协程与 Stats 每轮经 Policy() 取快照。
	policyMu sync.RWMutex
	policy   RetentionPolicy
	// indexMu 串行化 index.jsonl 的全部 IO（惰性打开/追加/Flush/截断重写）
	// 与启动回放的快照边界——索引写盘与目录分配分锁，索引侧的磁盘停滞
	// 不再堵死 Start。
	indexMu sync.Mutex
	// indexFile/indexWriter 是跨请求索引（index.jsonl）的持久句柄。
	indexFile   *os.File
	indexWriter *bufio.Writer
	// indexBytes 跟踪 index.jsonl 当前体积，超 indexFileCap 时保尾部一半重写。
	indexBytes int64
	// indexSnapshotted 标记启动回放已在 indexMu 内截取索引快照：此前完成的
	// 请求其索引行已在快照内、由回放统一入账，appendIndex 不再单独累加；
	// 此后写入的行在快照之外，必须由实时路径自计——任一行恰入账一次。
	indexSnapshotted atomic.Bool
	// cleanerStop/cleanerDone 控制后台清理协程生命周期。
	cleanerStop chan struct{}
	cleanerDone chan struct{}
	// droppedTotal 汇总各请求被丢弃的写任务数，供 Stats 暴露。
	droppedTotal atomic.Uint64
	// ioErrors 汇总索引与阶段文件的写失败数——日志管道自身故障不静默。
	ioErrors atomic.Uint64
	// usage 是 index.jsonl 的内存聚合器；启动时回放、请求完成时累加。
	usage *usageAggregator
	// replayDone 在启动回放结束时关闭；UsageStats 等它而不是返回半成数据。
	replayDone chan struct{}
	// listCache 是 ListRequests 的尾部窗口解析缓存，listCacheMu 保护；
	// 面板轮询（概览矩阵 1s、请求页 1-5s）反复扫同一 index.jsonl，
	// 文件 (size,mtime) 没变就免掉 4MB 尾读 + 全量 JSON 解析。
	listCacheMu sync.Mutex
	listCache   listIndexCache
}

// RequestMeta 是创建请求日志时已经确定的 HTTP 元信息。
// json tag 与 meta.json 的 client 块字段同名，ActiveRequest.Meta 经
// /requests/active 下发时与完成请求保持同一 wire 口径。
type RequestMeta struct {
	// Method 是 HTTP 请求方法。
	Method string `json:"method"`
	// Path 是 HTTP 请求路径。
	Path string `json:"path"`
	// API 是入口协议标识（openai-chat、openai-responses、responses-ws、anthropic）。
	API string `json:"api,omitempty"`
	// ClientIP 是下游客户端地址（不含端口）。
	ClientIP string `json:"client_ip,omitempty"`
	// UserAgent 是下游客户端声明的 UA。
	UserAgent string `json:"user_agent,omitempty"`
	// KeyHash 是客户端凭据的 SHA-256 前 8 字节十六进制——
	// 用于按 key 关联请求，不明文落盘。
	KeyHash string `json:"key_hash,omitempty"`
	// ClientRequestID 是客户端自带的关联 ID（X-Request-Id/X-Session-Id），
	// 让调用方能用自己的 ID 检索本次请求。
	ClientRequestID string `json:"client_request_id,omitempty"`
}

// Completion 是请求结束时写入 meta.json 的结果摘要。
type Completion struct {
	// StatusCode 是最终 HTTP 状态码。
	StatusCode int
	// Result 是 completed、failed 或 disconnected。
	Result string
	// Model 是实际发给上游的模型标识（别名解析后）。
	Model string
	// RequestedModel 是客户端原始请求的模型名（可能命中别名）。
	RequestedModel string
	// ResponseModel 是上游响应声明的模型；为空表示上游未声明。
	ResponseModel string
	// ModelMismatch 表示上游声明模型与实际请求模型不一致。
	ModelMismatch bool
	// Provider 是实际生成响应的供应商标识。
	Provider string
	// Stream 表示请求是否使用流式响应。
	Stream bool
	// UpstreamRequestID 是上游为本次调用分配的追踪标识，报障时可引用。
	UpstreamRequestID string
	// Usage 是上游报告的最终 token 用量；失败或未上报时为零值。
	Usage llm.Usage
	// PrematureEndTurn 标记可疑的正常收尾：请求以工具结果结尾、
	// 模型却返回无工具调用的 end_turn。实测存在模型声称要继续动作
	// 后直接 EOS 的故障形态；该标记仅用于观测统计，不改变响应。
	PrematureEndTurn bool
}

// Recorder 保存单次请求的目录、开始时间和异步写队列。
type Recorder struct {
	// manager 回指所属 Manager，Complete 时写索引并释放目录保护。
	manager *Manager
	// directory 是本次请求的日志目录。
	directory string
	// startedAt 是 HTTP 请求进入应用的时间。
	startedAt time.Time
	// requestMeta 保存创建时的 HTTP 元信息。
	requestMeta RequestMeta
	// mutex 保护 closed、abortCancel、requestedModel、resolvedModel、
	// keyHash、retries、upstreamAccount、accountAttempts；worker 自身状态无锁。
	mutex sync.Mutex
	// closed 表示 Complete 已关闭队列，之后入队请求直接计入丢弃。
	closed bool
	// abortCancel 是请求 ctx 的取消函数，面板 Abort 时调用；nil 表示不可中断。
	abortCancel context.CancelFunc
	// requestedModel 是解码后的客户端请求模型名（面板进行中列表展示用）。
	requestedModel string
	// resolvedModel 是别名解析与路由判定后实际发给上游的 uid；
	// 进行中行据此把模型列渲染成「请求名 → 实际 uid」，不必等完成。
	resolvedModel string
	// keyHash 是准入阶段回填的令牌 key_hash 覆盖值：匿名通道请求不携带
	// 凭据，requestMeta.KeyHash 为空——拿到令牌后回填，index/meta 才能把
	// 匿名流量归到该令牌行。非空时优先于 requestMeta.KeyHash。
	keyHash string
	// upstreamAccount 是最终服务本请求的上游账号名（号池 lane 身份，
	// 单号部署恒为 "default"）；号池 failover 时它只记成功那次的归属，
	// 之前的失败尝试落在 accountAttempts。
	upstreamAccount string
	// accountAttempts 是号池 failover 的有序失败尝试——每个被试过又
	// 放弃的 lane 各记一笔；请求 goroutine 经 NoteAccountAttempt 追加，
	// writeMeta/appendIndex 读，与 retries 同一把锁。
	accountAttempts []accountAttempt
	// tasks 是待执行写任务的有界队列；满时丢弃而非阻塞调用方。
	tasks chan writeTask
	// writerDone 在 worker 排空队列并关闭文件后关闭。
	writerDone chan struct{}
	// dropped 是本次请求被丢弃的写任务数。
	dropped atomic.Uint64
	// aborted 标记请求被面板主动中断（区别于客户端自行断连）。
	aborted atomic.Bool
	// clientBytes 是已下发给客户端的累计字节数。
	clientBytes atomic.Int64
	// requestReadyMS/upstreamSentMS/upstreamOpenMS/firstUpstreamMS/
	// firstClientMS 是首字延迟分解的五个阶段标记，-1 表示尚未发生：
	//   ready→sent  = 本地投影转换（validate/sanitize/routing/buildRequest/闸门排队）
	//   sent→open   = 上游建流往返（POST + 响应头）
	//   open→first_upstream = 上游思考 TTFT
	//   first_upstream→first_client = 代理编码+flush 下发
	// 区分「上游慢」与「网关编码慢」之外，sent 之前的部分即本进程自加延迟。
	requestReadyMS  atomic.Int64
	upstreamSentMS  atomic.Int64
	upstreamOpenMS  atomic.Int64
	firstUpstreamMS atomic.Int64
	firstClientMS   atomic.Int64
	// retryAfterSeconds 是上游限流文案里的 reset 秒数 hint；>0 时随
	// meta.json 与 index 落盘，grep/聚合不必再解析错误文案。
	retryAfterSeconds atomic.Int64
	// rateLimited 标记本请求被限流语义终结（上游 429 或本地闸门快败）。
	// 流内错误事件下发的限流 HTTP 状态仍是 200，单靠 status_code 认不出——
	// 责任归因与 429 采样都靠这个显式标记而不是状态码。
	rateLimited atomic.Bool
	// retries 记录上游重发（attempt2+）的触发原因与相对时刻，与 04
	// 的 retry_attempt 分界行同源；请求 goroutine 经 NoteRetryAttempt
	// 追加，writeMeta/appendIndex 读，走 mutex 同步。
	retries []retryAttempt
	// firstError 是首个失败点的同步记录：WriteError 调用时 CAS 抢占
	//（first-write-wins），writeLoggedError 的 WARN 行与 appendIndex
	// 据此读到归原点阶段——等 worker 排空再读会把「捕获点」误当
	//「失败点」。error.json 落盘仍在 worker 内由 errorWritten 去重。
	firstError atomic.Pointer[errorRecord]
	// upstreamConn 是成功建流那次发送的连接来源（复用/新建与 idle
	// 时长）；connect 段延迟靠它拆成「握手成本」与「上游响应头延迟」。
	upstreamConn atomic.Pointer[connInfo]
	// repairs 是请求投影为上游 wire 格式时的静默修复计数，由适配器在
	// 构建请求后写入；Complete 时随 meta.json 与 index 落盘。
	repairs atomic.Pointer[llm.RequestRepairs]

	// 以下字段仅由写 worker 访问，无需加锁：
	// sequences 保存每个 JSONL 文件各自的递增序号。
	sequences map[string]int
	// attachmentByHash 用于复用在多个转换阶段重复出现的同一附件。
	attachmentByHash map[string]attachmentReference
	// attachmentCount 是附件文件名的递增编号。
	attachmentCount int
	// jsonlFiles 保存已打开的 JSONL 文件，避免每帧重复 open/close。
	jsonlFiles map[string]*jsonlFile
	// errorWritten 保证 error.json 只保留首个错误（最先失败点最有诊断价值）。
	errorWritten bool
	// ioErrSeen 按类别去重本目录已上报的写失败，见 noteIOErr。
	ioErrSeen map[string]struct{}
}

// retryAttempt 是一次上游重发的记录：attempt 是请求体序号（2 起），
// cause 是触发原因（token 自愈/空响应续说/transport 断裂重开）。
type retryAttempt struct {
	Attempt   int    `json:"attempt"`
	Cause     string `json:"cause"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// accountAttempt 是号池内一次失败尝试的记录：account 是被试的 lane，
// code/message 是它放弃时的分类码与文案（截断至 errorMessageCap）。
// 注意 error.json 是 first-write-wins：failover 救回的请求目录里仍
// 留有首个失败 lane 的 error.json——它描述的是「第一次失败」而非
// 「最终下发给客户端的结果」，终局 lane 看 upstream_account。
type accountAttempt struct {
	Account   string `json:"account"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// errorRecord 是首个失败点的同步快照：stage 是归原点阶段名
// （index error_stage 同源），message 是错误文案（截断后随索引落盘，
// 请求目录被淘汰后仍可归因）。
type errorRecord struct {
	stage   string
	message string
}

// connInfo 是一次成功建流所用连接的画像：reused 表示命中 idle 池复用，
// idleMS 是该连接在池中的空闲时长。
type connInfo struct {
	reused bool
	idleMS int64
}

// writeTask 是交给写 worker 的一次作业，worker 内串行执行。
type writeTask func()

// jsonlFile 保存单个已打开的 JSONL 文件句柄及其缓冲写。
type jsonlFile struct {
	file   *os.File
	writer *bufio.Writer
}

// JSONLRecord 是一个 JSONL 文件中的统一行信封。
type JSONLRecord struct {
	// Seq 是当前文件内从 1 开始的顺序号。
	Seq int `json:"seq"`
	// Time 是事件发生时带时区的 RFC3339Nano 时间。
	Time string `json:"time"`
	// ElapsedMS 是相对请求进入时间的毫秒数。
	ElapsedMS int64 `json:"elapsed_ms"`
	// Event 是协议事件名；没有独立事件名时省略。
	Event string `json:"event,omitempty"`
	// Data 是本行记录的结构化内容。
	Data any `json:"data"`
}

// contextKey 是 request context 中 recorder 的私有键类型。
type contextKey struct{}

// attachmentReference 是 JSON 中替代图片 base64 正文的附件引用。
type attachmentReference struct {
	// File 是相对于请求日志目录的附件路径。
	File string `json:"file"`
	// MIMEType 是附件的媒体类型。
	MIMEType string `json:"mime_type"`
	// Size 是解码后二进制内容的字节数。
	Size int `json:"size"`
	// SHA256 是附件内容的 SHA-256 十六进制摘要。
	SHA256 string `json:"sha256"`
}

// NewManager 创建写入指定 logs 根目录的管理器；空路径返回禁用状态的管理器。
// policy 控制后台清理；清理协程恒启动（全零策略下空转），热改策略即时生效。
// 启动时异步回放 index.jsonl 尾部重建用量聚合——尾部上限 64MB，同步解析会
// 拖住 listen 之后的首次应答；UsageStats 在读侧等回放完成，不会返回半成数据。
func NewManager(root string, policy RetentionPolicy) *Manager {
	manager := &Manager{
		root:       root,
		now:        time.Now,
		activeDirs: make(map[string]*Recorder),
		takenNames: make(map[string]struct{}),
		policy:     policy,
		usage:      newUsageAggregator(),
		replayDone: make(chan struct{}),
	}
	manager.enabled.Store(true)
	if root == "" {
		close(manager.replayDone)
		return manager
	}
	// 提前建好根目录：quota.jsonl/stderr.log 等顶层文件不经过 Start() 的惰性建目录。
	if err := os.MkdirAll(root, 0o700); err != nil {
		slog.Warn("debuglog: create log root failed", "root", root, "error", err)
	}
	go func() {
		defer close(manager.replayDone)
		// 快照边界必须持锁划定：appendIndex 在同一 indexMu 内完成「写文件
		// +条件入账」——边界前写入的行全部落在快照内，其自身入账被
		// indexSnapshotted 闸门跳过、由回放统一补记；边界后写入的行在
		// 快照之外，由实时路径自计。任一行恰入账一次，无锁读则边界前后
		// 都可能与 appendIndex 交错，把同一行计两遍。indexMu 而非
		// manager.mutex：回放是磁盘 IO，不该占目录分配锁。
		manager.indexMu.Lock()
		indexPath := filepath.Join(root, IndexFile)
		// 尺寸必须在读之前取：读后 stat 会把回放窗口内的并发追加误判成
		// 截断（文件在两次调用之间增长）——高负载时这是必然假阳性。
		info, statErr := os.Stat(indexPath)
		data, err := TailRead(indexPath, usageReplayTailBytes)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			// 瞬态 IO 失败原地重试一次：快照没读成却照落闸门，边界前
			// 完成的行会被回放假设覆盖、又被实时路径跳过，永久漏记。
			manager.indexMu.Unlock()
			time.Sleep(200 * time.Millisecond)
			manager.indexMu.Lock()
			info, statErr = os.Stat(indexPath)
			data, err = TailRead(indexPath, usageReplayTailBytes)
		}
		manager.indexSnapshotted.Store(true)
		manager.indexMu.Unlock()
		if err != nil {
			// 索引不存在（首装）是常态；其他读失败意味着窗口统计丢历史，值得告警。
			if !errors.Is(err, os.ErrNotExist) {
				slog.Warn("debuglog: replay index tail failed", "error", err)
			}
			return
		}
		// 回放窗口与 indexFileCap 同值，正常时文件整体被覆盖；读前的文件
		// 比读到的内容大说明上限被撑破（外部追加/双写/常量漂移），最旧
		// 的行对聚合静默不可见——值得告警而不是无声丢历史。
		if statErr == nil && info.Size() > int64(len(data)) {
			slog.Warn("debuglog: index.jsonl exceeds replay window; oldest entries excluded from usage stats",
				"size", info.Size(), "replayed_bytes", len(data))
		}
		if parsed := manager.usage.replayLines(data); parsed > 0 {
			slog.Info("debuglog: replayed request index", "entries", parsed)
		}
	}()
	// cleaner 恒启动：策略全零时 cleanOnce 空转（每 5min 一次 ReadDir），
	// 若按初始策略条件启动，全零起步的进程热开保留策略（SetPolicy）后
	// 无人消费——热路径会是死开关。
	manager.cleanerStop = make(chan struct{})
	manager.cleanerDone = make(chan struct{})
	go manager.runCleaner()
	return manager
}

// Close 停止后台清理并关闭索引文件句柄；进程退出前调用一次。
func (manager *Manager) Close() {
	if manager == nil {
		return
	}
	// root 为空的禁用管理器提前返回、不起 cleaner（cleanerStop 为 nil）。
	if manager.cleanerStop != nil {
		close(manager.cleanerStop)
		<-manager.cleanerDone
	}
	manager.indexMu.Lock()
	defer manager.indexMu.Unlock()
	if manager.indexWriter != nil {
		if err := manager.indexWriter.Flush(); err != nil {
			manager.ioErrors.Add(1)
		}
		_ = manager.indexFile.Close()
		manager.indexWriter = nil
		manager.indexFile = nil
	}
}

// SetEnabled 运行时切换请求日志；关闭后新请求不再创建目录，历史仍可查询。
func (manager *Manager) SetEnabled(enabled bool) {
	if manager == nil {
		return
	}
	manager.enabled.Store(enabled)
}

// Enabled 返回请求日志当前是否开启。无 root 的 manager 永远写不了盘，
// 不报 enabled——healthz 之类读它判服务状态。
func (manager *Manager) Enabled() bool {
	return manager != nil && manager.enabled.Load() && manager.root != ""
}

// SetPolicy 运行时更换日志生命周期策略（配置 reload 热路径）；cleaner
// 协程下一轮 tick 即按新策略执行。
func (manager *Manager) SetPolicy(policy RetentionPolicy) {
	if manager == nil {
		return
	}
	manager.policyMu.Lock()
	manager.policy = policy
	manager.policyMu.Unlock()
}

// Policy 返回当前生效的生命周期策略快照。
func (manager *Manager) Policy() RetentionPolicy {
	if manager == nil {
		return RetentionPolicy{}
	}
	manager.policyMu.RLock()
	defer manager.policyMu.RUnlock()
	return manager.policy
}

// Root 返回日志根目录；禁用态返回空串。配额历史等顶层文件与其同目录。
func (manager *Manager) Root() string {
	if manager == nil {
		return ""
	}
	return manager.root
}

// Stats 返回日志管道自身的运行指标：丢弃数、活跃请求目录数、写队列积压、
// IO 失败数——观测系统自己的健康状况也应可观测（参考同类代理的 drop/backlog 计数）。
func (manager *Manager) Stats() map[string]any {
	if manager == nil {
		return nil
	}
	manager.mutex.Lock()
	active := len(manager.activeDirs)
	queued := 0
	for _, recorder := range manager.activeDirs {
		queued += len(recorder.tasks)
	}
	manager.mutex.Unlock()
	var indexBytes int64
	if info, err := os.Stat(filepath.Join(manager.root, IndexFile)); err == nil {
		indexBytes = info.Size()
	}
	// bind-failure.json 由 main 侧在 listen 绑定失败时写入；缺失/损坏
	// 都不透出——面板只需知道「最近一次为什么没绑上」，没有就是没发生过。
	var bindFailure json.RawMessage
	if data, err := os.ReadFile(filepath.Join(manager.root, BindFailureFile)); err == nil && json.Valid(data) {
		bindFailure = data
	}
	policy := manager.Policy()
	stats := map[string]any{
		"log_root":            manager.root,
		"enabled":             manager.enabled.Load(),
		"active_request_dirs": active,
		"queued_log_events":   queued,
		"queue_capacity":      active * writeQueueSize,
		"dropped_log_events":  manager.droppedTotal.Load(),
		"io_errors":           manager.ioErrors.Load(),
		"index_bytes":         indexBytes,
		"retention_days":      policy.Days,
		"max_total_mb":        policy.MaxTotalMB,
		"payload_hours":       policy.PayloadHours,
		"keep_error_dirs":     policy.KeepErrorDirs,
	}
	if bindFailure != nil {
		stats["last_bind_failure"] = bindFailure
	}
	return stats
}

// UsageStats 返回 index.jsonl 的聚合快照（今日/窗口累计、按模型、按 key、
// 错误阶段、小时趋势、延迟分位数）。启动回放完成前调用会阻塞到回放结束，
// 保证面板看到的口径是完整的而不是部分数据。
func (manager *Manager) UsageStats() UsageSnapshot {
	if manager == nil {
		return UsageSnapshot{}
	}
	<-manager.replayDone
	return manager.usage.snapshot()
}

// UsageLatency 返回全局延迟分位数摘要——/admin/runtime-metrics 的轮询
// 只消费这两行；全量聚合视图见 UsageStats。阻塞语义与 UsageStats 一致。
func (manager *Manager) UsageLatency() map[string]latencyStats {
	if manager == nil {
		return nil
	}
	<-manager.replayDone
	return manager.usage.latencySummary()
}

// Abort 中断指定进行中请求的 ctx；目录不存在或不可中断时返回 false。
func (manager *Manager) Abort(dir string) bool {
	if manager == nil || !requestDirPattern.MatchString(dir) {
		return false
	}
	manager.mutex.Lock()
	recorder := manager.activeDirs[dir]
	manager.mutex.Unlock()
	if recorder == nil {
		return false
	}
	return recorder.Abort()
}

// Start 为一个 HTTP 请求创建按进入秒命名的独立日志目录。
// 目录名在锁内预订（写入 activeDirs），mkdir 移到锁外：磁盘停滞只拖慢
// 本请求，不再堵死全部排队请求的目录分配。EEXIST 撞名说明磁盘上有
// 本进程不知道的遗留目录（同秒重启等），记入 takenNames 后换名重试。
func (manager *Manager) Start(meta RequestMeta) *Recorder {
	if manager == nil || manager.root == "" || !manager.enabled.Load() {
		return nil
	}
	lockWaitAt := time.Now()
	manager.mutex.Lock()
	if waited := time.Since(lockWaitAt); waited > 5*time.Second {
		// 正常锁内只有内存操作，等这么久意味着有路径又把 IO 带进了锁——告警。
		slog.Warn("debuglog: dir allocation lock wait exceeded", "waited", waited.String())
	}
	now := manager.now()
	base := now.Format("20060102-150405")
	for suffix := 1; ; suffix++ {
		name := base
		if suffix > 1 {
			name = fmt.Sprintf("%s-%02d", base, suffix)
		}
		if _, ok := manager.activeDirs[name]; ok {
			continue
		}
		if _, ok := manager.takenNames[name]; ok {
			continue
		}
		directory := filepath.Join(manager.root, name)
		recorder := &Recorder{
			manager:          manager,
			directory:        directory,
			startedAt:        now,
			requestMeta:      meta,
			tasks:            make(chan writeTask, writeQueueSize),
			writerDone:       make(chan struct{}),
			sequences:        make(map[string]int),
			attachmentByHash: make(map[string]attachmentReference),
			jsonlFiles:       make(map[string]*jsonlFile),
			ioErrSeen:        make(map[string]struct{}),
		}
		recorder.requestReadyMS.Store(-1)
		recorder.upstreamSentMS.Store(-1)
		recorder.upstreamOpenMS.Store(-1)
		recorder.firstUpstreamMS.Store(-1)
		recorder.firstClientMS.Store(-1)
		manager.activeDirs[name] = recorder
		manager.mutex.Unlock()
		err := mkdirRequestDir(directory)
		if err == nil {
			go recorder.runWriter()
			// meta.json 作为首个写任务入队：保持「目录一出现就有 meta」的语义，
			// 同时把同步写盘移出 manager.mutex——目录分配锁不该挡文件 IO。
			recorder.enqueue(func() { recorder.writeMeta(nil) })
			return recorder
		}
		manager.mutex.Lock()
		delete(manager.activeDirs, name)
		if os.IsExist(err) {
			manager.takenNames[name] = struct{}{}
			continue
		}
		manager.mutex.Unlock()
		// 建目录失败返回 nil = 本请求静默无日志；ioErrors 计数 +
		// Warn 让「日志为什么没了」可查（磁盘满/权限等）。
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: create request dir failed", "dir", name, "error", err)
		return nil
	}
}

// DirectoryPath 返回本请求的日志目录绝对路径；禁用态 recorder 为空串。
func (recorder *Recorder) DirectoryPath() string {
	if recorder == nil {
		return ""
	}
	return recorder.directory
}

// ClientRequestID 返回客户端自带的关联 ID（X-Request-Id/X-Client-Request-Id
// 等，建目录时快照进 requestMeta 后不再变，读侧免锁）。adapter 层用它识别
// 面板探活等内部流量——它们走真实 /v1 管线但不属于客户端会话簿记。
func (recorder *Recorder) ClientRequestID() string {
	if recorder == nil {
		return ""
	}
	return recorder.requestMeta.ClientRequestID
}

// ProbeClientRequestID 是面板探活请求打在 client_request_id 上的留痕值。
// 发送方（ccpanel 模型探针）与多个消费方（日志页 manual_test 归组、
// adapter 保温簿记豁免）共认同一常量，故定义在本包而不是任一消费侧。
const ProbeClientRequestID = "panel-probe"

// WithRecorder 将本次请求 recorder 放入 context 供供应商 adapter 使用。
func WithRecorder(ctx context.Context, recorder *Recorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, recorder)
}

// FromContext 返回当前请求 recorder；未启用日志时返回 nil。
func FromContext(ctx context.Context) *Recorder {
	recorder, _ := ctx.Value(contextKey{}).(*Recorder)
	return recorder
}

// mkdirRequestDir 创建请求日志目录；根目录在运行期被删时重建父目录后重试一次。
// 不预先 MkdirAll——根目录由 NewManager 建好，每请求一次 stat 是无谓开销。
func mkdirRequestDir(path string) error {
	err := os.Mkdir(path, 0o700)
	if errors.Is(err, os.ErrNotExist) && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		err = os.Mkdir(path, 0o700)
	}
	return err
}

// enqueue 把一个写任务交给 worker；队列满或已关闭时丢弃并计数。
// 丢弃计数的归属恰在 closed 置位那刻切分：此前进 recorder.dropped，
// 由 Complete 收尾时一并折进 droppedTotal；此后直接折进 droppedTotal——
// 迟到入队（如未 join 的泵 goroutine）的丢弃不能落进无人再读的字段。
func (recorder *Recorder) enqueue(task writeTask) {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if recorder.closed {
		recorder.dropped.Add(1)
		recorder.manager.droppedTotal.Add(1)
		return
	}
	select {
	case recorder.tasks <- task:
	default:
		recorder.dropped.Add(1)
	}
}

// runWriter 是单请求写协程：串行执行任务，保证 JSONL 事件序与入队序一致；
// 队列排空时把缓冲刷盘（进行中的请求目录对面板也应实时可读，不能只等
// Complete）；tasks 关闭后排空残余任务，统一刷盘并关闭所有 JSONL 文件。
func (recorder *Recorder) runWriter() {
	for task := range recorder.tasks {
		task()
		// len(channel) 的竞态无碍：多看一个任务只是少刷一次，
		// 关闭前的统一 flush 仍兜底。
		if len(recorder.tasks) == 0 {
			recorder.flushJSONL()
		}
	}
	for _, f := range recorder.jsonlFiles {
		if err := f.writer.Flush(); err != nil {
			recorder.noteIOErr("jsonl", err)
		}
		_ = f.file.Close()
	}
	recorder.jsonlFiles = nil
	close(recorder.writerDone)
}

// flushJSONL 把已打开 JSONL 文件的缓冲写落盘；仅写协程调用。
func (recorder *Recorder) flushJSONL() {
	for _, f := range recorder.jsonlFiles {
		if err := f.writer.Flush(); err != nil {
			recorder.noteIOErr("jsonl", err)
		}
	}
}

// noteIOErr 把本目录一次写失败计入 manager.ioErrors 并告警；同一类别
// （kind）只记一笔——磁盘满等持续故障若逐帧计数，总量会失真到无法反映
// 影响面。仅在写 worker 与 Complete 收尾（writerDone 关闭后，与其构成
// happens-after）调用，去重集合无需加锁。
func (recorder *Recorder) noteIOErr(kind string, err error) {
	if _, ok := recorder.ioErrSeen[kind]; ok {
		return
	}
	recorder.ioErrSeen[kind] = struct{}{}
	recorder.manager.ioErrors.Add(1)
	slog.Warn("debuglog: write failed", "dir", filepath.Base(recorder.directory), "kind", kind, "error", err)
}

// NoteRequestReady 记录请求体解码+投影完成、泵协程即将调 adapter.Stream
// 的时刻——此前全部耗时是入口段（读体+JSON 解码+消息投影）。
func (recorder *Recorder) NoteRequestReady() {
	if recorder == nil {
		return
	}
	recorder.requestReadyMS.CompareAndSwap(-1, time.Since(recorder.startedAt).Milliseconds())
}

// NoteUpstreamSend 记录首个上游 RPC 真实发往连线的时刻（幂等，只记第一次）。
// 与 requestReady 之差即适配器转换耗时（含本地速率闸门排队）。
func (recorder *Recorder) NoteUpstreamSend() {
	if recorder == nil {
		return
	}
	recorder.upstreamSentMS.CompareAndSwap(-1, time.Since(recorder.startedAt).Milliseconds())
}

// NoteUpstreamOpen 记录上游流建立成功（响应头到达）的时刻（幂等，只记第一次）。
// 与 upstreamSent 之差是建流往返；与 firstUpstream 之差才是上游思考 TTFT。
func (recorder *Recorder) NoteUpstreamOpen() {
	if recorder == nil {
		return
	}
	recorder.upstreamOpenMS.CompareAndSwap(-1, time.Since(recorder.startedAt).Milliseconds())
}

// NoteUpstreamLatency 记录首个上游事件到达的相对毫秒数（幂等，只记第一次）。
func (recorder *Recorder) NoteUpstreamLatency() {
	if recorder == nil {
		return
	}
	recorder.firstUpstreamMS.CompareAndSwap(-1, time.Since(recorder.startedAt).Milliseconds())
}

// FirstUpstreamMS 返回首个上游事件距请求开始的毫秒数；未发生返回负值。
// 供令牌统计回写 TTFB（authtoken.AddResult 的 FirstByteSec）。
func (recorder *Recorder) FirstUpstreamMS() int64 {
	if recorder == nil {
		return -1
	}
	return recorder.firstUpstreamMS.Load()
}

// NoteClientLatency 记录首个下发给客户端的内容字节的相对毫秒数。
// SSE 保活注释不计——它是链路保活不是内容。
func (recorder *Recorder) NoteClientLatency() {
	if recorder == nil {
		return
	}
	recorder.firstClientMS.CompareAndSwap(-1, time.Since(recorder.startedAt).Milliseconds())
}

// SetAbort 挂接请求 ctx 的取消函数，使面板 Abort 能真正中断请求。
// Complete 后自动失效；ctx 为 nil 时忽略。
func (recorder *Recorder) SetAbort(cancel context.CancelFunc) {
	if recorder == nil || cancel == nil {
		return
	}
	recorder.mutex.Lock()
	if !recorder.closed {
		recorder.abortCancel = cancel
	}
	recorder.mutex.Unlock()
}

// SetModel 记录解码后的客户端请求模型名，用于进行中列表与诊断。
func (recorder *Recorder) SetModel(model string) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	recorder.requestedModel = model
	recorder.mutex.Unlock()
}

// SetResolvedModel 记录别名/路由判定后实际发给上游的模型 uid；
// 进行中行用它即时呈现映射终点，完成行的 requested→resolved 口径同源。
func (recorder *Recorder) SetResolvedModel(model string) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	recorder.resolvedModel = model
	recorder.mutex.Unlock()
}

// SetKeyHash 在准入解析出令牌后回填 key_hash：匿名通道请求不带凭据，
// requestMeta.KeyHash 为空，靠它把 index/meta/进行中行归到该令牌。
func (recorder *Recorder) SetKeyHash(keyHash string) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	recorder.keyHash = keyHash
	recorder.mutex.Unlock()
}

// effectiveKeyHash 返回落入 index/meta 的凭据哈希：准入覆盖值优先，
// 未覆盖时回到请求创建时采样的 requestMeta.KeyHash。
func (recorder *Recorder) effectiveKeyHash() string {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if recorder.keyHash != "" {
		return recorder.keyHash
	}
	return recorder.requestMeta.KeyHash
}

// AddClientBytes 累加已下发给客户端的字节数，用于进行中列表观察流出速率。
func (recorder *Recorder) AddClientBytes(n int64) {
	if recorder == nil || n <= 0 {
		return
	}
	recorder.clientBytes.Add(n)
}

// SetRetryAfter 记录上游限流文案里的 reset 秒数 hint（写进 meta/index，
// 与错误原文分离，grep/聚合不必再解析文案）；<=0 或非限流错误忽略。
func (recorder *Recorder) SetRetryAfter(seconds int) {
	if recorder == nil || seconds <= 0 {
		return
	}
	recorder.retryAfterSeconds.Store(int64(seconds))
}

// SetRateLimited 标记本请求被限流语义终结：状态码映射为 429 的错误
// （上游 resource_exhausted / 本地闸门）都该置位——流内错误事件下发的
// 限流 HTTP 状态仍是 200，没这个标记聚合层认不出它是限流。
func (recorder *Recorder) SetRateLimited() {
	if recorder == nil {
		return
	}
	recorder.rateLimited.Store(true)
}

// SetRepairs 记录请求投影到上游协议时发生的修复计数；全零不存，
// meta.json 就不出现 repairs 字段——「代理没动过」本身就是排障答案。
func (recorder *Recorder) SetRepairs(repairs llm.RequestRepairs) {
	if recorder == nil || repairs.Total() == 0 {
		return
	}
	recorder.repairs.Store(&repairs)
}

// NoteRetryAttempt 记录一次上游重发及其触发原因；调用方在同处写
// 04 的 retry_attempt 分界行，两处记录保持同源——每次重发各记一笔。
func (recorder *Recorder) NoteRetryAttempt(attempt int, cause string) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	recorder.retries = append(recorder.retries, retryAttempt{
		Attempt:   attempt,
		Cause:     cause,
		ElapsedMS: time.Since(recorder.startedAt).Milliseconds(),
	})
	recorder.mutex.Unlock()
}

// retryAttempts 返回重发记录的拷贝；无重发返回 nil。
func (recorder *Recorder) retryAttempts() []retryAttempt {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return append([]retryAttempt(nil), recorder.retries...)
}

// SetUpstreamAccount 记录最终服务本请求的上游账号（号池 lane 名）。
// 号池在 lane.Stream 成功开流后调用；failover 只留成功归属，
// 被放弃 lane 的明细走 NoteAccountAttempt。
func (recorder *Recorder) SetUpstreamAccount(account string) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	recorder.upstreamAccount = account
	recorder.mutex.Unlock()
}

// NoteAccountAttempt 记录号池内一次失败尝试：lane 开流报可换号错误、
// 或流内 pre-content 终局 error 事件被 poolStream 拦截转投下一候选时
// 由 pool 调用。错误经 Classify 压成 code+截断文案——这份有序尝试表
// 是「为什么换号」的归因痕迹（救回的请求仍可能有首失败 lane 的
// error.json，见 accountAttempt 说明）。
func (recorder *Recorder) NoteAccountAttempt(account string, err error) {
	if recorder == nil {
		return
	}
	attempt := accountAttempt{
		Account:   account,
		ElapsedMS: time.Since(recorder.startedAt).Milliseconds(),
	}
	if failure := llm.Classify(err); failure != nil {
		attempt.Code = failure.Code
		attempt.Message = truncateRunes(failure.Message, errorMessageCap)
	}
	recorder.mutex.Lock()
	recorder.accountAttempts = append(recorder.accountAttempts, attempt)
	recorder.mutex.Unlock()
}

// upstreamAttribution 返回号池归因快照：最终服务账号与有序失败尝试，
// 一把锁取齐两者——writeMeta 与 appendIndex 都要这对值。
func (recorder *Recorder) upstreamAttribution() (string, []accountAttempt) {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return recorder.upstreamAccount, append([]accountAttempt(nil), recorder.accountAttempts...)
}

// Abort 中断请求：标记 aborted 并调用挂接的取消函数。
// 无可中断的请求（未挂接或已完结）返回 false。
func (recorder *Recorder) Abort() bool {
	if recorder == nil {
		return false
	}
	recorder.mutex.Lock()
	cancel := recorder.abortCancel
	recorder.mutex.Unlock()
	if cancel == nil {
		return false
	}
	recorder.aborted.Store(true)
	cancel()
	return true
}

// snapshot 返回进行中请求的活快照：队列积压、首字节计时、阶段状态、模型。
// State 分三档：waiting_upstream（上游未回首事件）→ receiving_upstream
// （上游在回但未下发客户端内容）→ streaming_client（正在向客户端流出）。
func (recorder *Recorder) snapshot() ActiveRequest {
	recorder.mutex.Lock()
	model := recorder.requestedModel
	resolved := recorder.resolvedModel
	meta := recorder.requestMeta
	if recorder.keyHash != "" {
		meta.KeyHash = recorder.keyHash
	}
	retries := len(recorder.retries)
	var lastRetryCause string
	if retries > 0 {
		lastRetryCause = recorder.retries[retries-1].Cause
	}
	abortable := recorder.abortCancel != nil
	account := recorder.upstreamAccount
	accountSwitches := len(recorder.accountAttempts)
	recorder.mutex.Unlock()
	firstUpstream := optionalLatency(recorder.firstUpstreamMS.Load())
	state := "waiting_upstream"
	switch {
	case recorder.firstClientMS.Load() >= 0:
		state = "streaming_client"
	case firstUpstream != nil:
		state = "receiving_upstream"
	}
	return ActiveRequest{
		Dir:             filepath.Base(recorder.directory),
		Meta:            meta,
		Model:           model,
		ResolvedModel:   resolved,
		Retries:         retries,
		LastRetryCause:  lastRetryCause,
		Account:         account,
		AccountSwitches: accountSwitches,
		StartedAt:       recorder.startedAt,
		ElapsedMS:       time.Since(recorder.startedAt).Milliseconds(),
		State:           state,
		FirstUpstreamMS: firstUpstream,
		ClientBytes:     recorder.clientBytes.Load(),
		QueuedEvents:    len(recorder.tasks),
		DroppedEvents:   recorder.dropped.Load(),
		Abortable:       abortable,
	}
}

// evalDeferred 展开调用方传入的延迟求值 thunk：传 func() any 时投影/建树
// 在写 worker 内执行，请求/泵 goroutine 只承担一次 channel send——热路径
// 不为日志付同步的 marshal/投影成本。注意调用方须保证 thunk 捕获的数据
// 在 worker 执行期间不被并发改写（不可变值或已冻结的快照）。
func evalDeferred(value any) any {
	if thunk, ok := value.(func() any); ok {
		return thunk()
	}
	return value
}

// WriteJSON 将一个阶段快照排入队列，由 worker 序列化并写为格式化 JSON 文件。
// value 可为 func() any 延迟求值（语义见 evalDeferred）。
func (recorder *Recorder) WriteJSON(name string, value any) {
	if recorder == nil || !validLogName(name, ".json") {
		return
	}
	recorder.enqueue(func() {
		value = recorder.sanitize(evalDeferred(value))
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return
		}
		data = append(data, '\n')
		if err := os.WriteFile(filepath.Join(recorder.directory, name), data, 0o600); err != nil {
			recorder.noteIOErr("file", err)
		}
	})
}

// AppendJSONL 将一个有序事件追加到指定 JSONL 文件。
// value 可为 func() any 延迟求值（语义见 evalDeferred）。
func (recorder *Recorder) AppendJSONL(name, event string, value any) {
	if recorder == nil || !validLogName(name, ".jsonl") {
		return
	}
	recorder.enqueue(func() {
		recorder.sequences[name]++
		record := JSONLRecord{
			Seq:       recorder.sequences[name],
			Time:      time.Now().Format(time.RFC3339Nano),
			ElapsedMS: time.Since(recorder.startedAt).Milliseconds(),
			Event:     event,
			Data:      recorder.sanitize(evalDeferred(value)),
		}
		data, err := json.Marshal(record)
		if err != nil {
			return
		}
		recorder.appendJSONL(name, data)
	})
}

// AppendValueJSONL 将一个结构化值直接追加为 JSONL 行，不添加事件信封。
// value 可为 func() any 延迟求值（语义见 evalDeferred）。
func (recorder *Recorder) AppendValueJSONL(name string, value any) {
	if recorder == nil || !validLogName(name, ".jsonl") {
		return
	}
	recorder.enqueue(func() {
		data, err := json.Marshal(recorder.sanitize(evalDeferred(value)))
		if err != nil {
			return
		}
		recorder.appendJSONL(name, data)
	})
}

// WriteError 写入请求失败的阶段和错误摘要；只保留首个错误。
// stage/message 在调用时同步抢占（first-write-wins）——调用方紧接着
// 就能经 FirstError 读到归原点；error.json 落盘仍在写 worker 内去重。
func (recorder *Recorder) WriteError(stage string, err error) {
	if recorder == nil || err == nil {
		return
	}
	recorder.firstError.CompareAndSwap(nil, &errorRecord{stage: stage, message: err.Error()})
	recorder.enqueue(func() {
		if recorder.errorWritten {
			return
		}
		recorder.errorWritten = true
		// 写盘内容取同步抢占的胜出版本：与 index error_stage/
		// error_message 逐字节一致，不随任务入队顺序漂移。
		recorded := recorder.firstError.Load()
		value := recorder.sanitize(map[string]any{
			"stage":      recorded.stage,
			"message":    recorded.message,
			"elapsed_ms": time.Since(recorder.startedAt).Milliseconds(),
		})
		data, marshalErr := json.MarshalIndent(value, "", "  ")
		if marshalErr != nil {
			return
		}
		if err := os.WriteFile(filepath.Join(recorder.directory, ErrorFile), append(data, '\n'), 0o600); err != nil {
			recorder.noteIOErr("file", err)
		}
	})
}

// FirstError 返回首个失败点的阶段与错误文案；未记录时返回空串。
// 等价于 error.json 的 stage/message 两字段，供写日志行与索引时取
// 归原点——WriteError 的 stage 实参是捕获点，两者可能不同。
func (recorder *Recorder) FirstError() (stage, message string) {
	if recorder == nil {
		return "", ""
	}
	if recorded := recorder.firstError.Load(); recorded != nil {
		return recorded.stage, recorded.message
	}
	return "", ""
}

// NoteUpstreamConn 记录成功建流所用连接的画像；last-write-wins，
// 调用点紧跟首个成功的 NoteUpstreamOpen。
func (recorder *Recorder) NoteUpstreamConn(reused bool, idle time.Duration) {
	if recorder == nil {
		return
	}
	recorder.upstreamConn.Store(&connInfo{reused: reused, idleMS: idle.Milliseconds()})
}

// Complete 关闭写队列、等待残余任务排空，然后写终态 meta.json、
// 追加全局索引行并释放目录的清理保护。幂等：二次调用直接返回——
// 否则 writeMeta 与 index 行会重复落一份。
func (recorder *Recorder) Complete(completion Completion) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	if recorder.closed {
		recorder.mutex.Unlock()
		return
	}
	recorder.closed = true
	recorder.abortCancel = nil
	close(recorder.tasks)
	// 折算必须在锁内完成：迟到的入队在 closed 置位后走 enqueue 的
	// closed 分支自折 droppedTotal；拖出锁外会把窗口内的迟到丢弃
	// 既算进 dropped.Load() 又算进对方的自折——双计。
	recorder.manager.droppedTotal.Add(recorder.dropped.Load())
	recorder.mutex.Unlock()
	if recorder.aborted.Load() && completion.Result == "disconnected" {
		completion.Result = "aborted"
	}
	<-recorder.writerDone
	recorder.writeMeta(&completion)
	recorder.manager.appendIndex(recorder, &completion)
	recorder.manager.releaseDir(recorder.directory)
}

// appendJSONL 把一行已序列化记录写进指定 JSONL 文件的缓冲；仅写 worker 调用。
func (recorder *Recorder) appendJSONL(name string, data []byte) {
	jf, err := recorder.getJSONLFile(name)
	if err != nil {
		recorder.noteIOErr("jsonl", err)
		return
	}
	if _, err := jf.writer.Write(data); err != nil {
		recorder.noteIOErr("jsonl", err)
	}
	if err := jf.writer.WriteByte('\n'); err != nil {
		recorder.noteIOErr("jsonl", err)
	}
}

// getJSONLFile 返回指定 JSONL 文件的缓冲写句柄，按需惰性打开；仅写 worker 调用。
func (recorder *Recorder) getJSONLFile(name string) (*jsonlFile, error) {
	if f, ok := recorder.jsonlFiles[name]; ok {
		return f, nil
	}
	file, err := os.OpenFile(filepath.Join(recorder.directory, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f := &jsonlFile{file: file, writer: bufio.NewWriter(file)}
	recorder.jsonlFiles[name] = f
	return f, nil
}

// writeMeta 写 meta.json：创建时（completion 为 nil）落进入时刻与客户端
// 元信息，Complete 时（非 nil）补完结时刻、耗时、状态码、结果与用量。
// 仅写 worker 与 Complete 收尾（writerDone 关闭后）调用。
func (recorder *Recorder) writeMeta(completion *Completion) {
	meta := map[string]any{
		"started_at": recorder.startedAt.Format(time.RFC3339Nano),
		"method":     recorder.requestMeta.Method,
		"path":       recorder.requestMeta.Path,
	}
	if recorder.requestMeta.API != "" {
		meta["api"] = recorder.requestMeta.API
	}
	client := map[string]any{}
	if recorder.requestMeta.ClientIP != "" {
		client["ip"] = recorder.requestMeta.ClientIP
	}
	if recorder.requestMeta.UserAgent != "" {
		client["user_agent"] = recorder.requestMeta.UserAgent
	}
	if keyHash := recorder.effectiveKeyHash(); keyHash != "" {
		client["key_hash"] = keyHash
	}
	if recorder.requestMeta.ClientRequestID != "" {
		client["request_id"] = recorder.requestMeta.ClientRequestID
	}
	if len(client) > 0 {
		meta["client"] = client
	}
	if ready := recorder.requestReadyMS.Load(); ready >= 0 {
		meta["request_ready_ms"] = ready
	}
	if sent := recorder.upstreamSentMS.Load(); sent >= 0 {
		meta["upstream_sent_ms"] = sent
	}
	if open := recorder.upstreamOpenMS.Load(); open >= 0 {
		meta["upstream_open_ms"] = open
	}
	if first := recorder.firstUpstreamMS.Load(); first >= 0 {
		meta["first_upstream_ms"] = first
	}
	if first := recorder.firstClientMS.Load(); first >= 0 {
		meta["first_client_ms"] = first
	}
	if dropped := recorder.dropped.Load(); dropped > 0 {
		meta["dropped_events"] = dropped
	}
	if retry := recorder.retryAfterSeconds.Load(); retry > 0 {
		meta["retry_after_seconds"] = retry
	}
	if recorder.rateLimited.Load() {
		meta["rate_limited"] = true
	}
	if conn := recorder.upstreamConn.Load(); conn != nil {
		// 连接画像拆开 connect 段：reused=false 时 sent→open 含完整
		// TCP+TLS 握手，reused=true 时该段基本是上游响应头延迟。
		meta["upstream_conn_reused"] = conn.reused
		meta["upstream_conn_idle_ms"] = conn.idleMS
	}
	if repairs := recorder.repairs.Load(); repairs != nil {
		meta["repairs"] = repairs
	}
	if retries := recorder.retryAttempts(); len(retries) > 0 {
		meta["retry_attempts"] = retries
	}
	// 号池归因沿用 upstream_* 扁平词表：account 是最终服务 lane，
	// attempts 是 failover 前的有序失败尝试（含 code 与截断文案）。
	// 全 lane 失败时 account 为空，attempts 仍要落——它是唯一痕迹。
	account, attempts := recorder.upstreamAttribution()
	if account != "" {
		meta["upstream_account"] = account
	}
	if len(attempts) > 0 {
		meta["upstream_attempts"] = attempts
	}
	if completion != nil {
		finishedAt := time.Now()
		meta["finished_at"] = finishedAt.Format(time.RFC3339Nano)
		meta["duration_ms"] = finishedAt.Sub(recorder.startedAt).Milliseconds()
		meta["status_code"] = completion.StatusCode
		meta["result"] = completion.Result
		meta["model"] = completion.Model
		meta["provider"] = completion.Provider
		meta["stream"] = completion.Stream
		if completion.RequestedModel != "" {
			meta["requested_model"] = completion.RequestedModel
		}
		if completion.ResponseModel != "" {
			meta["response_model"] = completion.ResponseModel
		}
		if completion.ModelMismatch {
			meta["model_mismatch"] = true
		}
		if completion.PrematureEndTurn {
			meta["premature_end_turn"] = true
		}
		if completion.UpstreamRequestID != "" {
			meta["upstream_request_id"] = completion.UpstreamRequestID
		}
		if completion.Usage != (llm.Usage{}) {
			usageMeta := map[string]any{
				"input":       completion.Usage.Input,
				"output":      completion.Usage.Output,
				"cache_read":  completion.Usage.CacheRead,
				"cache_write": completion.Usage.CacheWrite,
				"reasoning":   reasoningTokens(completion.Usage),
				"total":       completion.Usage.TotalTokens,
			}
			// 上游计费读数原样保留：credit_cost 是单请求可加总口径，
			// committed_* 系是该时刻的账户侧快照，进索引求和没有语义。
			if costs := completion.Usage.Costs; costs != nil {
				usageMeta["costs"] = map[string]any{
					"credit_cost":                       costs.CreditCost,
					"committed_credit_cost":             costs.CommittedCreditCost,
					"committed_acu_cost":                costs.CommittedAcuCost,
					"committed_quota_cost_basis_points": costs.CommittedQuotaCostBasisPoints,
					"committed_overage_cost_cents":      costs.CommittedOverageCostCents,
				}
			}
			meta["usage"] = usageMeta
		}
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err == nil {
		if err := os.WriteFile(filepath.Join(recorder.directory, MetaFile), append(data, '\n'), 0o600); err != nil {
			recorder.noteIOErr("file", err)
		}
	}
}

// validLogName 校验阶段文件名：禁止目录穿越，且必须带期望扩展名。
func validLogName(name, extension string) bool {
	return filepath.Base(name) == name && strings.HasSuffix(name, extension)
}
