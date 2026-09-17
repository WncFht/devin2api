// 本文件实现单次 HTTP 请求的分阶段调试 payload 写入：目录名即请求身份
// （X-Request-Id/debug_ref），内容落在 store 的 debug_files/debug_chunks
// 两表——整文件（meta/01/02/03/error/attachments）是 files 行，流式
// JSONL（04/05/06）按 flush 批追加为 chunks 行。
//
// Package debuglog 负责记录兼容 API 请求在 HTTP、中间模型和供应商协议之间的转换过程。
// 所有写库作业经每请求一个有界任务队列交给单 worker 串行执行——
// 事件顺序即入队顺序，热路径只承担一次 channel send；队列满时丢弃并计数，
// 观测系统自身降级不拖垮请求。生命周期管理（保留期/总量清理）见 cleaner.go。
package debuglog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// writeQueueSize 是单请求写任务的排队上限；流式帧在万级以下时绰绰有余。
const writeQueueSize = 4096

// chunkFlushInterval 是 JSONL 缓冲合批提交周期：窗口内各文件缓冲合并为
// 一个事务一次 commit，把高频流式期的逐行 fsync 压到每秒数次；窗口长度
// 同时是进行中请求的已提交前缀对面板可见的延迟上限。
const chunkFlushInterval = 200 * time.Millisecond

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
// 日志行的 store 句柄与后台清理器。
type Manager struct {
	// root 是所有请求日志目录的根路径；空值表示禁用调试日志。
	root string
	// now 返回当前时间；测试会固定它以验证同秒目录分配。
	now func() time.Time
	// mutex 串行化目录名分配与 activeDirs 维护——锁内只做内存操作，
	// mkdir 一律在锁外：一次磁盘停滞曾让所有排队请求的 ReadTimeout
	// 在持锁等待中过期，锁一释放即批量假死。
	mutex sync.Mutex
	// activeDirs 记录仍有进行中请求的目录名→recorder，清理器必须跳过；
	// 存指针是为了 ActiveRequests 能直出进行中请求的活快照。
	activeDirs map[string]*Recorder
	// takenNames 记录本进程已知被占、但不在 activeDirs 的目录名——
	// NewManager 把盘上待导入的遗留目录播种进来，claim 撞名（已入库
	// 的同名目录）也入账——锁内选名时跳过它们，避免同秒重启后反复
	// 撞名。体量极小（遗留目录数 + 撞名次数）。
	takenNames map[string]struct{}
	// enabled 是请求日志的运行时开关；关闭时 Start 返回 nil，已有目录不受影响。
	enabled atomic.Bool
	// policy 是日志生命周期策略；policyMu 保护它：配置 reload 会运行时换值，
	// cleaner 协程与 Stats 每轮经 Policy() 取快照。
	policyMu sync.RWMutex
	policy   RetentionPolicy
	// store 是 logs 表的持久层；nil 时 insertLog 静默跳过（测试/未接线）。
	store *store.Store
	// logRowRetentionDays 是 logs 行的时间保留天数（面板可热改）；
	// <=0 不按时间清理。与目录的 policy.Days 是两条独立的生命周期轴：
	// 行是检索面、目录是证据面。
	logRowRetentionDays atomic.Int64
	// cleanerStop/cleanerDone 控制后台清理协程生命周期。
	cleanerStop chan struct{}
	cleanerDone chan struct{}
	// droppedTotal 汇总各请求被丢弃的写任务数，供 Stats 暴露。
	droppedTotal atomic.Uint64
	// ioErrors 汇总日志行与阶段文件的写失败数——日志管道自身故障不静默。
	ioErrors atomic.Uint64
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

// Recorder 保存单次请求的目录名、开始时间和异步写队列。
type Recorder struct {
	// manager 回指所属 Manager，Complete 时写索引并释放目录保护。
	manager *Manager
	// dir 是本次请求的调试目录名（内嵌进入时刻，不再对应磁盘目录）。
	dir string
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
	// upstreamAccount 是最终服务本请求的上游账号名（号池 lane 名；
	// 历史行有 ''/'default' 残留）；号池 failover 时它只记成功那次的归属，
	// 之前的失败尝试落在 accountAttempts。
	upstreamAccount string
	// accountAttempts 是号池 failover 的有序失败尝试——每个被试过又
	// 放弃的 lane 各记一笔；请求 goroutine 经 NoteAccountAttempt 追加，
	// writeMeta/insertLog 读，与 retries 同一把锁。
	accountAttempts []accountAttempt
	// poolCandidates 是开流前的候选序快照（含每 lane 降级原因），
	// 由 Pool.Stream 排序后登记，writeMeta 落 meta.pool_candidates。
	poolCandidates []PoolCandidate
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
	// meta.json 与日志行出账，检索/聚合不必再解析错误文案。
	retryAfterSeconds atomic.Int64
	// rateLimited 标记本请求被限流语义终结（上游 429 或本地闸门快败）。
	// 流内错误事件下发的限流 HTTP 状态仍是 200，单靠 status_code 认不出——
	// 责任归因与 429 采样都靠这个显式标记而不是状态码。
	rateLimited atomic.Bool
	// retries 记录上游重发（attempt2+）的触发原因与相对时刻，与 04
	// 的 retry_attempt 分界行同源；请求 goroutine 经 NoteRetryAttempt
	// 追加，writeMeta/insertLog 读，走 mutex 同步。
	retries []retryAttempt
	// firstError 是首个失败点的同步记录：WriteError 调用时 CAS 抢占
	//（first-write-wins），writeLoggedError 的 WARN 行与 insertLog
	// 据此读到归原点阶段——等 worker 排空再读会把「捕获点」误当
	//「失败点」。error.json 落盘仍在 worker 内由 errorWritten 去重。
	firstError atomic.Pointer[errorRecord]
	// upstreamConn 是成功建流那次发送的连接来源（复用/新建与 idle
	// 时长）；connect 段延迟靠它拆成「握手成本」与「上游响应头延迟」。
	upstreamConn atomic.Pointer[connInfo]
	// repairs 是请求投影为上游 wire 格式时的静默修复计数，由适配器在
	// 构建请求后写入；Complete 时随 meta.json 与日志行出账。
	repairs atomic.Pointer[llm.RequestRepairs]

	// 以下字段仅由写 worker 访问，无需加锁：
	// sequences 保存每个 JSONL 文件各自的递增序号。
	sequences map[string]int
	// attachmentByHash 用于复用在多个转换阶段重复出现的同一附件。
	attachmentByHash map[string]attachmentReference
	// attachmentCount 是附件文件名的递增编号。
	attachmentCount int
	// chunkBufs 按 JSONL 文件名缓冲已序列化行；每个刷写周期全部非空
	// 缓冲合并为一个事务提交为 debug_chunks 行（追加行代替整文件重写，
	// 已提交前缀对面板实时可见，见 chunkFlushInterval）。
	chunkBufs map[string]*bytes.Buffer
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

// PoolCandidate 是号池一次选号的候选快照行：Name 是 lane 名，Healthy/
// Bound 是当时判定位，Reason 是它被降级/跳过的归因词表（bound、
// auth_cooldown、generic_cooldown、gate_latched、gate_window_full、
// quota_low；首位被选中者可空）。整张表回答「这次为什么去了这个号」。
type PoolCandidate struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Bound   bool   `json:"bound,omitempty"`
	Reason  string `json:"reason,omitempty"`
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
// st 是 logs 表的持久层句柄——历史行已由启动导入器搬入库，无需回放。
func NewManager(root string, policy RetentionPolicy, st *store.Store) *Manager {
	manager := &Manager{
		root:       root,
		now:        time.Now,
		activeDirs: make(map[string]*Recorder),
		takenNames: make(map[string]struct{}),
		policy:     policy,
		store:      st,
	}
	manager.enabled.Store(true)
	manager.logRowRetentionDays.Store(DefaultLogRowRetentionDays)
	if root == "" {
		return manager
	}
	// 提前建好根目录：stderr.log 等顶层文件不经过 Start() 的惰性建目录。
	if err := os.MkdirAll(root, 0o700); err != nil {
		slog.Warn("debuglog: create log root failed", "root", root, "error", err)
	}
	// 把盘上遗留的请求目录（待导入或导入失败）播种进撞名集：它们对
	// claim 不可见（行还没进库），不挡住会同秒重启把新请求撞进旧目录名。
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && requestDirPattern.MatchString(entry.Name()) {
				manager.takenNames[entry.Name()] = struct{}{}
			}
		}
	}
	// cleaner 恒启动：策略全零时 cleanOnce 空转（每 5min 一次 ReadDir），
	// 若按初始策略条件启动，全零起步的进程热开保留策略（SetPolicy）后
	// 无人消费——热路径会是死开关。
	manager.cleanerStop = make(chan struct{})
	manager.cleanerDone = make(chan struct{})
	go manager.runCleaner()
	return manager
}

// DefaultLogRowRetentionDays 是 logs 行的默认时间保留天数；
// 面板设置项的 def 展示同源引用。
const DefaultLogRowRetentionDays = 90

// SetLogRowRetentionDays 热改 logs 行的时间保留天数；<=0 关闭按时间清理。
func (manager *Manager) SetLogRowRetentionDays(days int64) {
	if manager == nil {
		return
	}
	manager.logRowRetentionDays.Store(days)
}

// LogRowRetentionDays 返回当前 logs 行保留天数。
func (manager *Manager) LogRowRetentionDays() int64 {
	if manager == nil {
		return 0
	}
	return manager.logRowRetentionDays.Load()
}

// Close 停止后台清理协程；进程退出前调用一次。
func (manager *Manager) Close() {
	if manager == nil {
		return
	}
	// root 为空的禁用管理器提前返回、不起 cleaner（cleanerStop 为 nil）。
	if manager.cleanerStop != nil {
		close(manager.cleanerStop)
		<-manager.cleanerDone
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
	var logRows, dbBytes int64
	if manager.store != nil {
		if n, err := manager.store.LogCount(context.Background()); err == nil {
			logRows = n
		}
		dbBytes = manager.store.DBBytes()
	}
	// bind-failure.json 由 main 侧在 listen 绑定失败时写入；缺失/损坏
	// 都不透出——面板只需知道「最近一次为什么没绑上」，没有就是没发生过。
	var bindFailure json.RawMessage
	if data, err := os.ReadFile(filepath.Join(manager.root, BindFailureFile)); err == nil && json.Valid(data) {
		bindFailure = data
	}
	policy := manager.Policy()
	stats := map[string]any{
		"log_root":               manager.root,
		"enabled":                manager.enabled.Load(),
		"active_request_dirs":    active,
		"queued_log_events":      queued,
		"queue_capacity":         active * writeQueueSize,
		"dropped_log_events":     manager.droppedTotal.Load(),
		"io_errors":              manager.ioErrors.Load(),
		"log_rows":               logRows,
		"db_bytes":               dbBytes,
		"log_row_retention_days": manager.LogRowRetentionDays(),
		"retention_days":         policy.Days,
		"max_total_mb":           policy.MaxTotalMB,
		"payload_hours":          policy.PayloadHours,
		"keep_error_dirs":        policy.KeepErrorDirs,
	}
	if bindFailure != nil {
		stats["last_bind_failure"] = bindFailure
	}
	return stats
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

// Start 为一个 HTTP 请求分配按进入秒命名的调试目录名。
// 目录名在锁内预订（写入 activeDirs），claim 落库移到锁外：DB 停滞只
// 拖慢本请求，不再堵死全部排队请求的目录分配。claim 未抢到说明库里
// 已有同名目录（同秒重启等），记入 takenNames 后换名重试——等价文件
// 时代 mkdir 的 EEXIST。
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
		recorder := &Recorder{
			manager:          manager,
			dir:              name,
			startedAt:        now,
			requestMeta:      meta,
			tasks:            make(chan writeTask, writeQueueSize),
			writerDone:       make(chan struct{}),
			sequences:        make(map[string]int),
			attachmentByHash: make(map[string]attachmentReference),
			chunkBufs:        make(map[string]*bytes.Buffer),
			ioErrSeen:        make(map[string]struct{}),
		}
		recorder.requestReadyMS.Store(-1)
		recorder.upstreamSentMS.Store(-1)
		recorder.upstreamOpenMS.Store(-1)
		recorder.firstUpstreamMS.Store(-1)
		recorder.firstClientMS.Store(-1)
		manager.activeDirs[name] = recorder
		manager.mutex.Unlock()
		claimed, err := manager.claimDir(name)
		if err == nil && claimed {
			go recorder.runWriter()
			// meta.json 作为首个写任务入队：保持「目录一出现就有 meta」的语义，
			// 同时把同步写库移出 manager.mutex——目录分配锁不该挡 DB IO。
			recorder.enqueue(func() { recorder.writeMeta(nil) })
			return recorder
		}
		manager.mutex.Lock()
		delete(manager.activeDirs, name)
		if err == nil {
			manager.takenNames[name] = struct{}{}
			continue
		}
		manager.mutex.Unlock()
		// 占位失败返回 nil = 本请求静默无日志；ioErrors 计数 +
		// Warn 让「日志为什么没了」可查（DB 满/锁超时等）。
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: claim request dir failed", "dir", name, "error", err)
		return nil
	}
}

// claimDir 把目录名在持久层原子占位：插入空 meta.json 行成功=抢到名。
// store 为 nil（测试/未接线）时无共享状态可撞，直接视为占位成功——
// 名分配只剩本进程内存集合一重判定。
func (manager *Manager) claimDir(name string) (claimed bool, err error) {
	if manager.store == nil {
		return true, nil
	}
	return manager.store.ClaimDebugFile(context.Background(), name, MetaFile, []byte{})
}

// Dir 返回本请求的调试目录名（即 X-Request-Id/debug_ref）；
// 禁用态 recorder 为空串。
func (recorder *Recorder) Dir() string {
	if recorder == nil {
		return ""
	}
	return recorder.dir
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
// chunk 缓冲按 chunkFlushInterval 周期合并提交（进行中的请求对面板仍有
// 亚秒级可见性，而高频流式期不再每批帧各付一次 commit）；tasks 关闭后
// 排空残余任务，统一 flush 收尾。
func (recorder *Recorder) runWriter() {
	flushTick := time.NewTicker(chunkFlushInterval)
	defer flushTick.Stop()
	for {
		select {
		case task, ok := <-recorder.tasks:
			if !ok {
				recorder.flushJSONL()
				recorder.chunkBufs = nil
				close(recorder.writerDone)
				return
			}
			task()
		case <-flushTick.C:
			recorder.flushJSONL()
		}
	}
}

// flushJSONL 把全部非空 JSONL 缓冲合成一个事务提交为 chunk 行；仅写
// 协程调用。事务原子：失败时缓冲整体保留，下个周期整体重发，不会
// 出现半截批次（区别于 bufio 的「已写部分留不住」）。
func (recorder *Recorder) flushJSONL() {
	st := recorder.manager.store
	if st == nil {
		for name := range recorder.chunkBufs {
			delete(recorder.chunkBufs, name)
		}
		return
	}
	var chunks []store.DebugChunk
	for name, buf := range recorder.chunkBufs {
		if buf.Len() > 0 {
			chunks = append(chunks, store.DebugChunk{Name: name, Data: buf.Bytes()})
		}
	}
	if len(chunks) == 0 {
		return
	}
	if err := st.AppendDebugChunks(context.Background(), recorder.dir, chunks); err != nil {
		recorder.noteIOErr("jsonl", err)
		return
	}
	for _, c := range chunks {
		recorder.chunkBufs[c.Name].Reset()
	}
}

// noteIOErr 把本目录一次写失败计入 manager.ioErrors 并告警；同一类别
// （kind）只记一笔——DB 持续故障若逐帧计数，总量会失真到无法反映
// 影响面。仅在写 worker 与 Complete 收尾（writerDone 关闭后，与其构成
// happens-after）调用，去重集合无需加锁。
func (recorder *Recorder) noteIOErr(kind string, err error) {
	if _, ok := recorder.ioErrSeen[kind]; ok {
		return
	}
	recorder.ioErrSeen[kind] = struct{}{}
	recorder.manager.ioErrors.Add(1)
	slog.Warn("debuglog: write failed", "dir", recorder.dir, "kind", kind, "error", err)
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

// NotePoolCandidates 登记号池开流前的候选序快照：Pool.Stream 排完序
// 调一次，回答「这次为什么去了这个号」——被降级 lane 的 Reason 是
// 归因词表（见 PoolCandidate）。多次调用后者覆盖前者（换号重选时
// 保留最新一轮决策现场）。
func (recorder *Recorder) NotePoolCandidates(candidates []PoolCandidate) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	recorder.poolCandidates = candidates
	recorder.mutex.Unlock()
}

// upstreamAttribution 返回号池归因快照：最终服务账号与有序失败尝试，
// 一把锁取齐两者——writeMeta 与 insertLog 都要这对值。
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
		Dir:             recorder.dir,
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

// WriteJSON 将一个阶段快照排入队列，由 worker 序列化并写为格式化 JSON
// 文件（debug_files 行）。value 可为 func() any 延迟求值（语义见 evalDeferred）。
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
		recorder.putFile(name, data)
	})
}

// putFile 覆写一个整文件行（meta/01/02/03 与 error.json 之外的写都走这里）；
// store 未接线时静默跳过——payload 是观测副本，不反向决定请求成败。
func (recorder *Recorder) putFile(name string, data []byte) {
	st := recorder.manager.store
	if st == nil {
		return
	}
	if err := st.PutDebugFile(context.Background(), recorder.dir, name, data); err != nil {
		recorder.noteIOErr("file", err)
	}
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
// 就能经 FirstError 读到归原点；error.json 落库仍在写 worker 内去重，
// 并经 INSERT OR IGNORE 在 DB 层再兜一次 first-write-wins。
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
		// 落库内容取同步抢占的胜出版本：与 index error_stage/
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
		st := recorder.manager.store
		if st == nil {
			return
		}
		if err := st.PutDebugFileIfAbsent(context.Background(), recorder.dir, ErrorFile, append(data, '\n')); err != nil {
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
// 向 logs 表插入请求行并释放目录的清理保护。幂等：二次调用直接
// 返回——否则 writeMeta 与日志行会重复落一份。
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
	recorder.manager.insertLog(recorder, &completion)
	recorder.manager.releaseDir(recorder.dir)
}

// appendJSONL 把一行已序列化记录追加进指定 JSONL 文件的缓冲；
// 缓冲按 chunkFlushInterval 周期随同事务合批入库。仅写 worker 调用。
func (recorder *Recorder) appendJSONL(name string, data []byte) {
	buf := recorder.chunkBufs[name]
	if buf == nil {
		buf = &bytes.Buffer{}
		recorder.chunkBufs[name] = buf
	}
	buf.Write(data)
	buf.WriteByte('\n')
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
	recorder.mutex.Lock()
	poolCandidates := append([]PoolCandidate(nil), recorder.poolCandidates...)
	recorder.mutex.Unlock()
	if len(poolCandidates) > 0 {
		meta["pool_candidates"] = poolCandidates
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
		recorder.putFile(MetaFile, append(data, '\n'))
	}
}

// validLogName 校验阶段文件名：禁止目录穿越，且必须带期望扩展名。
func validLogName(name, extension string) bool {
	return filepath.Base(name) == name && strings.HasSuffix(name, extension)
}
