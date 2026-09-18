// 本文件实现单次 HTTP 请求的分阶段调试 payload 写入：目录名即请求身份
// （X-Request-Id/debug_ref），内容落在 store 的 debug_files/debug_chunks
// 两表——整文件（meta/01/02/03/error/attachments）是 files 行，流式
// JSONL（04/05/06）按 flush 批追加为 chunks 行。
//
// Package debuglog 负责记录兼容 API 请求在 HTTP、中间模型和供应商协议之间的转换过程。
// 写路径是两段流水线：请求 goroutine 只把任务排进按目录名哈希的分片队列
// （热路径一次 channel send），encoderShards 个编码协程并行消费——
// sanitize/marshal/压缩这些 CPU 密集段在多核上摊平，同一 recorder 恒落
// 同一分片使分片内 FIFO 即该请求的事件序；编码产物经 insertQ 汇聚给
// 唯一的写 worker，它独占 store 写连接做暂存与 200ms 合批提交——写库
// 竞争在结构上归零。两级队列任一满即丢弃并计数，观测系统自身降级不拖垮
// 请求。生命周期管理（保留期/总量清理）见 cleaner.go。
package debuglog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// encoderShards 是分片队列与编码协程的数量：编码（sanitize/marshal/
// gzip）是日志管道唯一的 CPU 密集段，单协程曾是高峰期批量丢弃的根因；
// 上限 8 防大核机器上编码协程反挤数据面。
var encoderShards = min(8, max(2, runtime.GOMAXPROCS(0)-2))

// globalQueueSize 是全部请求共享的编码队列总容量（各分片均摊）。入队方
// 只做 µs 级的序号分配+channel send，容量需吸收分片级突发——一个请求的
// 全部事件落同一分片（目录名哈希），单请求流式期可产数百条记录，
// 2048/分片在几条并发流同片突发时整段溢出丢弃；8192/分片覆盖 ~16 条
// 并发突发流。积压超界即丢弃计数——观测内存不随流量膨胀。
const globalQueueSize = 65536

// insertQueueSize 是编码产物待落库队列的容量：编码协程推满即阻塞形成
// 背压（分片队列随之积压、入队端开始丢弃），bound 住「已编码未入库」的
// 内存水位。容量须盖住一次批量事务的提交窗口——实测满载下大 flush 事务
// 可达数百毫秒，4096 在峰速 ~19k ops/s 时撑不到 250ms，提交期间在飞
// 请求整段流被丢弃（每请求数百条）；16384 覆盖 ~860ms 峰值摄入。
const insertQueueSize = 16384

// pendingPayloadCapBytes 是全部编码产物（stagedFiles/chunkBufs/收尾项）
// 的在飞字节预算：计数界（65536 任务 + 16384 op）对载荷方差失真——
// 事件间尺寸差两个数量级（04 行 ~400B 对 01 ~400KB），深度×单事件
// 最大载荷的上界既虚高又不可预测。取与 deltaBaseCapBytes 同量级：
// 约等于故障期 2-3 分钟原始摄入的缓冲量，两账合计最坏 ~512MB 观测
// 自有堆。两级 shed 若落地，protected 名单（01/02/03/error）改走
// 「cap + 小 reserve 顶」而非硬闸——chargePayload 的 headroom 形参
// 即挂接点；不落地则全员同闸，语义即现状。
const pendingPayloadCapBytes = 256 << 20

// completionChargeBytes 是一份 pendingCompletions 收尾项的估值记账：
// logRow 结构体 + completionItem 本体合计 KB 级——收尾必须落库故不
// 过闸，1024 dir × KB 级的超顶代价可忽略。
const completionChargeBytes = 1024

// chunkFlushInterval 是 JSONL 缓冲合批提交周期：窗口内各文件缓冲合并为
// 一个事务一次 commit，把高频流式期的逐行 fsync 压到每秒数次；窗口长度
// 同时是进行中请求的已提交前缀对面板可见的延迟上限。
const chunkFlushInterval = 200 * time.Millisecond

// completionFlushGap 是完成收尾的攒批间隙：Complete 的排空哨兵抵达写
// worker 后不单独提交，距上次冲刷不足该间隙时攒进下次批量事务——
// 高 rps 下收尾与常规缓冲共用一次 commit，落库延迟被压到
// 间隙+事务时长量级；写队列瞬时排空或超时则立即冲刷，不加等待。
const completionFlushGap = 50 * time.Millisecond

// storeOpTimeout 是日志写路径单次 store 调用的上限。观测管道不能拿无界
// ctx 进 SQLite：库卡死时无界调用会把写 worker 与 Complete 收尾 goroutine
// 永久挂起（activeDirs 不释放）；分钟级超时会丢观测副本但不拖死数据面。
const storeOpTimeout = 2 * time.Minute

// storeCtx 返回带 storeOpTimeout 上限的 ctx，供写路径的 store 调用。
func storeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), storeOpTimeout)
}

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
	// LogRowDays 是 logs 表摘要行的时间保留天数（面板可热改）；<=0 不按
	// 时间清理。与目录的 Days 是两条独立生命周期轴：行是检索面、目录是
	// 证据面。行删除由 store.Maintain 执行（main 侧周期任务），不在
	// debug 目录清理器里——debug 关时表级保洁不停。
	LogRowDays int64
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
	// errorsOnly 只保留失败请求的调试 payload：干净完成的请求在 Complete
	// 时整删目录行（含 claim 的空 meta 占位），logs 摘要行照常落库。
	errorsOnly atomic.Bool
	// policy 是日志生命周期策略；policyMu 保护它：配置 reload 会运行时换值，
	// cleaner 协程与 Stats 每轮经 Policy() 取快照。
	policyMu sync.RWMutex
	policy   RetentionPolicy
	// logRowRetentionDays 是 logs 摘要行的时间保留天数（独立于 payload
	// 保留），面板设置项热改走 SetLogRowRetentionDays。
	logRowRetentionDays atomic.Int64
	// store 是 logs 表的持久层；nil 时日志行静默跳过（测试/未接线）。
	store *store.Store
	// cleanerStop/cleanerDone 控制后台清理协程生命周期。
	cleanerStop chan struct{}
	cleanerDone chan struct{}
	// queues 是按目录名哈希分片的编码任务队列（长 encoderShards），
	// insertQ 是编码产物汇给写 worker 的队列；workerStop/workerGone 是
	// 关停协议——Close 关 workerStop，编码协程排空各自分片后退，写
	// worker 收干 insertQ、冲刷残余缓冲再关 workerGone。workerGone 同时
	// 是 Complete 哨兵与迟到入队在 worker 已退场景下的兜底逃生口。
	queues     []chan writeTask
	insertQ    chan insertOp
	workerStop chan struct{}
	workerGone chan struct{}
	// encWG 计在役编码协程，encodersDone 在它们全部退出后关闭——关停时
	// 写 worker 必须先等编码侧排空（否则任务转成 op 的途中 insertQ
	// 已无人续收会死锁）。
	encWG        sync.WaitGroup
	encodersDone chan struct{}
	// shardEncoders 按分片下标持有各编码协程的专属 payload 编码器：
	// 构造期填齐，任务闭包只在本协程上执行（同 dir 恒同分片），免锁
	// 复用 flate 内部表——sync.Pool 会被 GC 清空，专属实例把表重建
	// 摊成一次性成本。
	shardEncoders []*store.PayloadEncoder
	// writerEncoder 是写 worker 的专属 payload 编码器：flushAll 的
	// chunk 编码与 queueCompletion 的 meta 编码在它上面跑；fallbackMu
	// 兜底路径在 workerGone 后串行触碰，不构成并发。
	writerEncoder *store.PayloadEncoder
	// closing 置位（Close 开始）后 enqueue 直接丢弃——关停期入队方
	// 立即降级，不向正在排空的队列再压任务。
	closing atomic.Bool
	// dirtyBufs 是有未冲刷 JSONL 缓冲的 recorder 集合，flush tick 只扫
	// 它而不是全部活跃目录。仅写 worker goroutine 读写（任务体也在其
	// 内执行），无需加锁。
	dirtyBufs map[*Recorder]struct{}
	// pendingCompletions 是已抵达写 worker、待随下次批量事务提交的
	// 收尾集合（终态 meta 已暂存进各自 stagedFiles，日志行已按完成
	// 时刻构建）；事务提交（或无内容可提交）后逐项解除目录的清理
	// 保护并关闭 recorder.drained 放行等待方。仅写 worker 读写，
	// fallbackMu 兜底路径除外。
	pendingCompletions []completionItem
	// lastFlush 是上次批量事务的发起时刻：completionFlushGap 内到达的
	// 收尾攒成一批，超时或写队列排空即随当前 op 立即冲刷。
	lastFlush time.Time
	// lastPayloadReconcile 是上次用 DebugDirSizes 权威聚合对账 payload
	// 计数器的时刻；仅 cleaner 协程读写，零值表示从未对账（首个
	// tick 即对一次）。
	lastPayloadReconcile time.Time
	// fallbackMu 串行化写 worker 死后的兜底收尾：workerGone 关闭后
	// Complete 的调用方、关停看守与编码协程上的投递失败分支可同时
	// 直跑 queueCompletion/flushAll，此时写侧私有状态已无人持有，
	// 触发方之间需互斥。写 worker 存活期它从不被取。
	fallbackMu sync.Mutex
	// pendingCompletionCount 镜像 pendingCompletions 长度供 Stats 读
	//（写 worker 私有切片不能跨 goroutine 取 len）。
	pendingCompletionCount atomic.Int64
	// droppedTotal 汇总各请求被丢弃的写任务数，供 Stats 暴露。
	droppedTotal atomic.Uint64
	// ioErrors 汇总日志行与阶段文件的写失败数——日志管道自身故障不静默。
	ioErrors atomic.Uint64
	// deltaBaseBytes 是全进程已钉 delta 基座的字节量：每个在飞目录把
	// 脱敏后 01 明文钉给同目录的 02/03-devin-request* 作 zstd dict，
	// 超 deltaBaseCapBytes 时新目录放弃钉座（其 delta 候选回退独立
	// gzip）。该预算与 inflightBytes 是同一账目概念——都是「为观测
	// 而暂存的字节」，两者仍分管各自的钉留动机。
	deltaBaseBytes atomic.Int64
	// inflightBytes 是写侧在飞 payload 的全局字节账：编码产物在
	// pushInsert 前挂上预留（chargePayload/chargeStageFile），op 落地
	// 时转记进 recorder.stagedBytes、flushAll 提交后归还。正常稳态
	// 占用只有摄入速率×flush 窗（百 KB 级），预算的全部价值在 DB
	// 病态期——写事务持续失败时 stagedFiles/chunkBufs/
	// pendingCompletions 只增不减（prod WAL 17.7GB 事故形态），无界
	// 增长直奔 OOM；超 pendingPayloadCapBytes 即按到达序丢弃编码产物
	//（与队列满丢弃同语义），把「静默 OOM」换成「可计数、有上限的
	// 证据丢弃」。meta/error/logRow 收尾锚点不过闸（必须落库，KB 级
	// 超顶可忽略）但仍入账。
	inflightBytes atomic.Int64
	// droppedPayloadBytes 汇总被在飞预算丢弃的编码产物字节量，
	// 供 Stats 量化「丢了多少证据体积」。
	droppedPayloadBytes atomic.Uint64
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
	// Stream 表示请求是否要求流式响应；请求体解码后才确定，由
	// SetStream 回填（requestMeta 快照先于解码创建）。
	Stream bool `json:"stream,omitempty"`
	// Class 是令牌声明的请求类（fg/bg——闸门分级准入词表）；令牌
	// 解析后由 SetClass 回填，进行中行与 meta.json client 块同出。
	Class string `json:"class,omitempty"`
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
	// keyHash、retries、sequences、upstreamAccount、accountAttempts、
	// affinityHash、poolCandidates；worker 自身状态无锁。
	mutex sync.Mutex
	// closed 表示 Complete 已关闭队列，之后入队请求直接计入丢弃。
	closed bool
	// abortCancel 是请求 ctx 的带因取消函数，Abort 时以调用方给的归因
	// 取消；nil 表示不可中断。
	abortCancel context.CancelCauseFunc
	// requestedModel 是解码后的客户端请求模型名（面板进行中列表展示用）。
	requestedModel string
	// resolvedModel 是别名解析与路由判定后实际发给上游的 uid；
	// 进行中行据此把模型列渲染成「请求名 → 实际 uid」，不必等完成。
	resolvedModel string
	// keyHash 是准入阶段回填的令牌 key_hash 覆盖值：匿名通道请求不携带
	// 凭据，requestMeta.KeyHash 为空——拿到令牌后回填，index/meta 才能把
	// 匿名流量归到该令牌行。非空时优先于 requestMeta.KeyHash。
	keyHash string
	// stream 是解码出 options 后回填的流式标记：requestMeta 快照先于
	// 请求体解码创建，进行中行的 Meta.Stream 由它在 snapshot 时补投。
	stream bool
	// requestClass 是令牌准入解析后回填的请求类（fg/bg）：requestMeta
	// 快照先于令牌解析创建，snapshot/metaJSON 由它补投 Meta.Class。
	requestClass string
	// upstreamAccount 是最终服务本请求的上游账号名（号池 lane 名；
	// 历史行有 ''/'default' 残留）；号池 failover 时它只记成功那次的归属，
	// 之前的失败尝试落在 accountAttempts。
	upstreamAccount string
	// accountAttempts 是号池 failover 的有序失败尝试——每个被试过又
	// 放弃的 lane 各记一笔；请求 goroutine 经 NoteAccountAttempt 追加，
	// metaJSON/logRowFor 读，与 retries 同一把锁。
	accountAttempts []AccountAttempt
	// affinityHash 是号池选号用的会话亲和键（SessionAffinityKey 的
	// 截断 SHA-256，与 key_hash 同脱敏口径）；Pool.Stream 排序前经
	// SetAffinityHash 回填，metaJSON 落 meta.affinity_hash——把
	// 「同一会话」与 upstream_attempts/pool_candidates 的换号痕迹
	// 关联起来，会话级钉选/迁移分析不必回 03 重算种子。
	affinityHash string
	// poolCandidates 是开流前的候选序快照（含每 lane 降级原因），
	// 由 Pool.Stream 排序后登记，metaJSON 落 meta.pool_candidates。
	poolCandidates []PoolCandidate
	// drained 在覆盖本请求收尾的批量事务首次 resolve 后由写 worker
	// 关闭（提交成功，或失败放行——收尾留在 pendingCompletions 继续
	// 重试但不再让等待方挂着）。Complete 不再等它：哨兵在本请求自己
	// 的分片 FIFO 里排在全部已入队任务之后，它对应的 op 把收尾挂进
	// pendingCompletions，drained 关闭即说明前序任务都已随同一事务
	// 落库（或已失败放行）。需要落库可见性的调用方经 Drained 等它。
	drained chan struct{}
	// completionQueued 保证收尾只入列一次：哨兵 op 与 workerGone 兜底
	// 可能都走到 queueCompletion（哨兵已入列而 Complete 恰选了
	// workerGone 分支）；重复入列会向 logs 写重复行（dir 唯一约束
	// 还会把整批拖进失败重试）。
	completionQueued atomic.Bool
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
	// 追加，metaJSON/logRowFor 读，走 mutex 同步。
	retries []RetryAttempt
	// devinSends 是 03-devin-request 词干已分配的上游发送序号：计数
	// 挂在请求目录上跨 lane 共享——号池 failover 后新 lane 的首发续占
	// attemptN 分片而非以基座名覆写（debug_files 同名 REPLACE 会把
	// 上一 lane 的 wire 体顶掉）。
	devinSends atomic.Int64
	// sequences 保存每个 JSONL 文件各自的递增序号：序号在入队前的
	// 临界区分配（record 含 Seq 须在 marshal 前定版），等于入队次序。
	sequences map[string]int
	// firstError 是首个失败点的同步记录：WriteError 调用时 CAS 抢占
	//（first-write-wins），writeLoggedError 的 WARN 行与日志行
	// 据此读到归原点阶段——等 worker 排空再读会把「捕获点」误当
	//「失败点」。error.json 落盘仍在 worker 内由 errorWritten 去重。
	firstError atomic.Pointer[errorRecord]
	// upstreamConn 是首个成功建流那次发送的连接来源（复用/新建与 idle
	// 时长，first-write-wins）；connect 段延迟靠它拆成「握手成本」与
	//「上游响应头延迟」。
	upstreamConn atomic.Pointer[connInfo]
	// repairs 是请求投影为上游 wire 格式时的静默修复计数，由适配器在
	// 构建请求后写入；Complete 时随 meta.json 与日志行出账。
	repairs atomic.Pointer[llm.RequestRepairs]

	// attachmentByHash 用于复用在多个转换阶段重复出现的同一附件。
	// attachmentCount 是附件文件名的递增编号。
	// 两者只在编码协程上被 sanitize 访问——同一 recorder 的任务恒落同一
	// 分片队列、由同一协程串行执行，故无需加锁。
	attachmentByHash map[string]attachmentReference
	attachmentCount  int
	// shard 是本 recorder 的编码分片下标（按目录名哈希，Start 时定版）——
	// 分片内 FIFO 保证本请求的事件序即入队序。
	shard int
	// deltaBase 是本目录的 delta 编码基座：01 任务在编码协程上把脱敏后
	// 字节钉进来（含尾 \n，与库存 01 行解码结果逐字节一致），02/03*
	// 任务以它为 zstd dict。同 dir 任务恒由同一分片协程串行执行，字段
	// 无并发访问；releaseDir 时把字节量退回 deltaBaseBytes 预算。
	deltaBase []byte

	// 以下字段仅由写 worker 访问，无需加锁：
	// stagedFiles 按文件名暂存已编码的整文件行；刷写周期与 chunkBufs
	// 一起合并为一个跨目录事务提交（编码在调用方完成，worker 只暂存）。
	stagedFiles map[string]stagedFile
	// chunkBufs 按 JSONL 文件名缓冲已序列化行；每个刷写周期全部非空
	// 缓冲合并为一个事务提交为 debug_chunks 行（追加行代替整文件重写，
	// 已提交前缀对面板实时可见，见 chunkFlushInterval）。
	chunkBufs map[string]*bytes.Buffer
	// stagedBytes 是本目录暂存面（stagedFiles/chunkBufs/pendingCompletion
	// 收尾项）实际持有的字节量——写事务失败重试期间字节仍被持有，
	// flushAll 提交（或整体丢弃）后归零并从 inflightBytes 归还。
	stagedBytes int64
	// errorWritten 保证 error.json 只保留首个错误（最先失败点最有诊断价值）。
	errorWritten bool
	// ioErrSeen 按类别去重本目录已上报的写失败，见 noteIOErr。
	ioErrSeen map[string]struct{}
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

// writeTask 是排进分片编码队列的一次作业：run 在编码协程上执行，承担
// evalDeferred/sanitize/marshal/压缩，产物经 pushInsert 汇给写 worker。
// 携带 recorder 是为了故障归属（panic 告警定位目录）与 op 的落库路由。
type writeTask struct {
	recorder *Recorder
	run      func()
}

// insertOp 是编码完成、待写 worker 落库的一次作业：apply 只在写 worker
// 上串行执行，承担 µs 级暂存（stageFile/appendJSONL）或 Complete 的
// 收尾入列。DB 写全部收敛到写 worker 后，store 写连接在结构上无竞争。
// charge 是编码期挂上 inflightBytes 的预留字节：apply 落地时暂存部分
// 转记 stagedBytes，runOp 统一结清 charge——apply 内跳过暂存（去重）
// 或 panic 都随结清归还，账目不留泄漏路径。
type insertOp struct {
	recorder *Recorder
	charge   int64
	apply    func()
}

// completionItem 是写 worker 上待随批量事务落库的一份收尾：终态 meta
// 在入列时已暂存进 stagedFiles，logRow 按完成时刻构建好（时点字段
// 不随冲刷等待漂移），strip 记录 errors_only 判定。事务提交（或无
// 内容可提交）后解除目录保护并关闭 recorder.drained 放行等待方；
// signaled 防止失败重试时重复关闭。
type completionItem struct {
	recorder *Recorder
	logRow   *store.LogRow
	strip    bool
	signaled bool
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
	// Data 是本行记录的结构化内容：写方产物已是脱敏后的 JSON 字节
	//（sanitizeJSON），RawMessage 让信封 marshal 只付一次 compaction
	// 扫描而不是重走反射编码。
	Data json.RawMessage `json:"data"`
}

// marshalJSONLRecord 把 JSONLRecord 手写拼装成一行 JSON——与
// json.Marshal(record) 逐字节一致（字段序 seq,time,elapsed_ms,event,data；
// event 空按 omitempty 省略；Data 按 RawMessage 口径 compaction+HTML
// 转义，空为 null、非法即报错），但不为每行信封付一次反射编码与
// 多次中间分配。流式期每请求数百行的热路径走这里。
func marshalJSONLRecord(record JSONLRecord) ([]byte, error) {
	var buf bytes.Buffer
	// 信封本体约 80 字节+时间串与事件名原文——Grow 让全部拼装一次
	// 分配内完成（escapes 超界时按 buffer 常规倍增兜底）。
	buf.Grow(len(record.Data) + len(record.Event) + len(record.Time) + 96)
	var num [20]byte
	buf.WriteString(`{"seq":`)
	buf.Write(strconv.AppendInt(num[:0], int64(record.Seq), 10))
	buf.WriteString(`,"time":`)
	writeJSONString(&buf, record.Time)
	buf.WriteString(`,"elapsed_ms":`)
	buf.Write(strconv.AppendInt(num[:0], record.ElapsedMS, 10))
	if record.Event != "" {
		buf.WriteString(`,"event":`)
		writeJSONString(&buf, record.Event)
	}
	buf.WriteString(`,"data":`)
	if record.Data == nil {
		// 同 json.Marshal 对 nil RawMessage 的输出：null（空非 nil
		// 切片在 marshal 路径是 error，交给 Compact 同口径报错）。
		buf.WriteString("null")
	} else {
		start := buf.Len()
		if err := json.Compact(&buf, record.Data); err != nil {
			// json.Compact 与 marshal 对 RawMessage 的校验/compaction
			// 同口径，非法 JSON 在 marshal 路径同样整行失败。
			return nil, err
		}
		escapeCompactedJSON(&buf, start)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// jsonHex 是 JSON \u00xx 转义的十六进制数字表（writeJSONString 与
// escapeCompactedJSON 共用）。
const jsonHex = "0123456789abcdef"

// escapeCompactedJSON 给 buf[start:] 里刚 Compact 完的 JSON 补 marshal
// 同款转义：marshal 对 RawMessage 在 verbatim 拷贝时按 EscapeForHTML|
// EscapeForJS 转义 < > & 与 U+2028/2029 字面字节，而有效 JSON 里这些字节
// 只可能出现在字符串内（已转义形态不含字面字节、控制字符不可能原样
// 出现），净段整段补转即逐字节一致；无效 UTF-8 两路同样原样透传。
// 净段不含这些字节时零分配直接返回。
func escapeCompactedJSON(buf *bytes.Buffer, start int) {
	data := buf.Bytes()[start:]
	needs := false
	for i := 0; i < len(data); i++ {
		if data[i] == '<' || data[i] == '>' || data[i] == '&' ||
			(data[i] == 0xe2 && i+2 < len(data) && data[i+1] == 0x80 &&
				(data[i+2] == 0xa8 || data[i+2] == 0xa9)) {
			needs = true
			break
		}
	}
	if !needs {
		return
	}
	src := append([]byte(nil), data...)
	buf.Truncate(start)
	for i := 0; i < len(src); i++ {
		b := src[i]
		if b == '<' || b == '>' || b == '&' {
			buf.WriteString(`\u00`)
			buf.WriteByte(jsonHex[b>>4])
			buf.WriteByte(jsonHex[b&0x0f])
			continue
		}
		if b == 0xe2 && i+2 < len(src) && src[i+1] == 0x80 &&
			(src[i+2] == 0xa8 || src[i+2] == 0xa9) {
			buf.WriteString(`\u202`)
			buf.WriteByte('8' + src[i+2] - 0xa8)
			i += 2
			continue
		}
		buf.WriteByte(b)
	}
}

// writeJSONString 把 s 按 encoding/json 的字符串编码规则写入 buf——
// 控制字符短转义（\b\f\n\r\t）、<>& 的 \u00xx HTML 转义、U+2028/2029
// 特例与非法 UTF-8 写 U+FFFD 本体，与 Marshal 输出逐字节一致。
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			// escapeASCII 口径：0x00-0x1f 与 " \ & < > 需转义，其余
			// ASCII（含 0x7f）原样。
			if b >= 0x20 && b != '"' && b != '\\' && b != '<' && b != '>' && b != '&' {
				i++
				continue
			}
			if start < i {
				buf.WriteString(s[start:i])
			}
			switch b {
			case '"', '\\':
				buf.WriteByte('\\')
				buf.WriteByte(b)
			case '\b':
				buf.WriteString(`\b`)
			case '\f':
				buf.WriteString(`\f`)
			case '\n':
				buf.WriteString(`\n`)
			case '\r':
				buf.WriteString(`\r`)
			case '\t':
				buf.WriteString(`\t`)
			default:
				// <>& 与其余控制字符同走 \u00xx。
				buf.WriteString(`\u00`)
				buf.WriteByte(jsonHex[b>>4])
				buf.WriteByte(jsonHex[b&0x0f])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if start < i {
				buf.WriteString(s[start:i])
			}
			// 非法 UTF-8 写替换字符本体（jsonwire AppendQuote 口径）。
			buf.WriteString("\ufffd")
			i++
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			if start < i {
				buf.WriteString(s[start:i])
			}
			buf.WriteString(`\u202`)
			buf.WriteByte(byte('8' + r - '\u2028'))
			i += size
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		buf.WriteString(s[start:])
	}
	buf.WriteByte('"')
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
	// 构造期未显式给值时回填默认：LogRowDays 是面板侧热改的运维旋钮，
	// 没有 config 对应键——0 在构造期表示「未设置」而非「禁用」
	//（运行期经 SetPolicy 写 0 才是显式禁用）。
	if policy.LogRowDays == 0 {
		policy.LogRowDays = DefaultLogRowRetentionDays
	}
	manager := &Manager{
		root:          root,
		now:           time.Now,
		activeDirs:    make(map[string]*Recorder),
		takenNames:    make(map[string]struct{}),
		policy:        policy,
		store:         st,
		queues:        make([]chan writeTask, encoderShards),
		shardEncoders: make([]*store.PayloadEncoder, encoderShards),
		writerEncoder: store.NewPayloadEncoder(),
		insertQ:       make(chan insertOp, insertQueueSize),
		workerStop:    make(chan struct{}),
		workerGone:    make(chan struct{}),
		encodersDone:  make(chan struct{}),
		dirtyBufs:     make(map[*Recorder]struct{}),
	}
	shardCap := max(2048, globalQueueSize/encoderShards)
	for i := range manager.queues {
		manager.queues[i] = make(chan writeTask, shardCap)
		manager.shardEncoders[i] = store.NewPayloadEncoder()
	}
	manager.enabled.Store(true)
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
	// 无人消费——热路径会是死开关。编码协程与写 worker 同理恒启动：
	// enabled 热开关只截断 Start，已入队任务仍要有人消费。
	manager.cleanerStop = make(chan struct{})
	manager.cleanerDone = make(chan struct{})
	for shard := range manager.queues {
		manager.encWG.Add(1)
		go manager.runEncoder(shard)
	}
	go func() {
		manager.encWG.Wait()
		close(manager.encodersDone)
	}()
	go manager.runWriter()
	go manager.runCleaner()
	return manager
}

// DefaultLogRowRetentionDays 是 logs 行的默认时间保留天数；
// 面板设置项的 def 展示与 NewManager 构造期默认值同源引用。
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

// Close 排空写队列并停止后台协程；进程退出前调用一次。
// 顺序：closing 截断新任务 → workerStop 令写 worker 排空退出（残余
// 缓冲随最后一轮 flush 落库）→ 再停 cleaner——清理协程的收尾 vacuum
// 跑在全部写面静止之后。
func (manager *Manager) Close() {
	if manager == nil {
		return
	}
	// root 为空的禁用管理器提前返回、不起协程（cleanerStop 为 nil）。
	if manager.cleanerStop != nil {
		manager.closing.Store(true)
		close(manager.workerStop)
		<-manager.workerGone
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

// SetErrorsOnly 运行时切换「只留失败请求 payload」；对已完成请求不追溯，
// 只影响此后完结的请求。
func (manager *Manager) SetErrorsOnly(errorsOnly bool) {
	if manager == nil {
		return
	}
	manager.errorsOnly.Store(errorsOnly)
}

// ErrorsOnly 返回当前是否只保留失败请求的 payload。
func (manager *Manager) ErrorsOnly() bool {
	return manager != nil && manager.errorsOnly.Load()
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
	manager.mutex.Unlock()
	var logRows, dbBytes, walBytes, payloadBytes int64
	if manager.store != nil {
		if n, err := manager.store.LogCount(context.Background()); err == nil {
			logRows = n
		}
		dbBytes = manager.store.DBBytes()
		walBytes = manager.store.WALBytes()
		payloadBytes = manager.store.DebugPayloadBytes()
	}
	// bind-failure.json 由 main 侧在 listen 绑定失败时写入；缺失/损坏
	// 都不透出——面板只需知道「最近一次为什么没绑上」，没有就是没发生过。
	var bindFailure json.RawMessage
	if data, err := os.ReadFile(filepath.Join(manager.root, BindFailureFile)); err == nil && json.Valid(data) {
		bindFailure = data
	}
	// 积压口径 = 编码分片未消费任务 + 已编码未落库 op 之和。
	queued := len(manager.insertQ)
	for _, queue := range manager.queues {
		queued += len(queue)
	}
	policy := manager.Policy()
	stats := map[string]any{
		"log_root":            manager.root,
		"enabled":             manager.enabled.Load(),
		"errors_only":         manager.errorsOnly.Load(),
		"active_request_dirs": active,
		"queued_log_events":   queued,
		"pending_completions": manager.pendingCompletionCount.Load(),
		"queue_capacity":      globalQueueSize + insertQueueSize,
		"dropped_log_events":  manager.droppedTotal.Load(),
		// pending_bytes 是写侧在飞 payload 的实时水位（预留+暂存合计）；
		// 稳态只有摄入速率×flush 窗（百 KB 级），持续高位=写事务病态期
		// 暂存积压的直接读数。pending_bytes_cap 是它的硬顶。dropped_
		// payload_bytes 累计被预算丢弃的产物体积。
		"pending_bytes":         manager.inflightBytes.Load(),
		"pending_bytes_cap":     int64(pendingPayloadCapBytes),
		"dropped_payload_bytes": manager.droppedPayloadBytes.Load(),
		"io_errors":             manager.ioErrors.Load(),
		"log_rows":              logRows,
		"db_bytes":              dbBytes,
		// wal_bytes 单列：db_bytes 与 payload 口径差的主要解释项——
		// checkpoint 饥饿时 WAL 可远超主库文件，「WAL 顶爆 DBBytes」
		// 应面板可见而非事后挖掘。payload_bytes 是 debug 两表库存
		// 字节的内存计数器（容量淘汰的闸门口径）。
		"wal_bytes":              walBytes,
		"payload_bytes":          payloadBytes,
		"log_row_retention_days": policy.LogRowDays,
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

// errPanelAbort 是面板主动中断的归因文案：与原 app 侧内嵌文案逐字一致，
// 客户端/日志据此区分主动中断与裸断连。
var errPanelAbort = fmt.Errorf("aborted via panel request abort: %w", context.Canceled)

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
	return recorder.Abort(errPanelAbort)
}

// AbortAll 以同一归因中断全部进行中请求：排空超时强掐路径用——取消
// 原因沿各请求 ctx 链传到收尾簿记，返回实际中断数。已完结/未挂接的
// 目录被 Abort 自身跳过。
func (manager *Manager) AbortAll(cause error) int {
	if manager == nil {
		return 0
	}
	manager.mutex.Lock()
	recorders := make([]*Recorder, 0, len(manager.activeDirs))
	for _, recorder := range manager.activeDirs {
		recorders = append(recorders, recorder)
	}
	manager.mutex.Unlock()
	killed := 0
	for _, recorder := range recorders {
		if recorder.Abort(cause) {
			killed++
		}
	}
	return killed
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
			shard:            shardOf(name),
			drained:          make(chan struct{}),
			sequences:        make(map[string]int),
			attachmentByHash: make(map[string]attachmentReference),
			stagedFiles:      make(map[string]stagedFile),
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
			// meta.json 作为首个任务入队：编码在分片协程完成、落库由写
			// worker 合批提交，claim 的空行占位已先保证目录名不被撞走。
			recorder.enqueue(func() {
				if data := recorder.metaJSON(nil); data != nil {
					stored, usize := recorder.encodePayload(data)
					n := int64(len(stored))
					if !manager.chargeStageFile(MetaFile, n) {
						recorder.noteEncodeDrop(n)
						return
					}
					recorder.pushInsert(n, func() {
						recorder.stageFile(MetaFile, stored, usize, false)
					})
				}
			})
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
	ctx, cancel := storeCtx()
	defer cancel()
	return manager.store.ClaimDebugFile(ctx, name, MetaFile, []byte{})
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

// shardOf 把目录名哈希到编码分片下标：同 dir 恒同分片，编码协程间互不
// 干扰地保持每请求的事件序。
func shardOf(dir string) int {
	h := uint32(2166136261)
	for i := 0; i < len(dir); i++ {
		h = (h ^ uint32(dir[i])) * 16777619
	}
	return int(h % uint32(encoderShards))
}

// enqueue 把一个编码任务排进本请求的分片队列；队列满、已关闭或关停中
// 则丢弃并计数。丢弃计数的归属恰在 closed 置位那刻切分：此前进
// recorder.dropped，由 Complete 收尾时一并折进 droppedTotal；此后直接
// 折进 droppedTotal——迟到入队（如未 join 的泵 goroutine）的丢弃不能
// 落进无人再读的字段。
// 持锁发送：closed 判定与入队在同一把锁内完成，Complete 置位后不可能
// 再有任务渗进队列（否则它会排在排空哨兵之后，冲掉的缓冲永不再刷）。
func (recorder *Recorder) enqueue(task func()) {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	recorder.enqueueLocked(task)
}

// enqueueLocked 是 enqueue 的临界区形态：调用方已持有 recorder.mutex
// （AppendJSONL 需要序号分配与入队同临界区——分持两把锁会让序号序与
// 队列序错位）。发送是非阻塞 select，持锁期间不会挂起。
func (recorder *Recorder) enqueueLocked(task func()) {
	if recorder.closed || recorder.manager.closing.Load() {
		recorder.dropped.Add(1)
		recorder.manager.droppedTotal.Add(1)
		return
	}
	select {
	case recorder.manager.queues[recorder.shard] <- writeTask{recorder: recorder, run: task}:
	case <-recorder.manager.workerGone:
		recorder.dropped.Add(1)
		recorder.manager.droppedTotal.Add(1)
	default:
		recorder.dropped.Add(1)
	}
}

// chargePayload 把 n 字节挂进在飞账（add-or-revert，deltaBaseBytes 同
// 款语义）：水位超 cap+headroom 即回退返回 false，调用方按丢弃语义
// 降级。headroom 是两级 shed 的挂接点——protected 阶段传小顶额获得
// 「cap+顶」豁免而非硬闸；当前全员同闸传 0。
func (manager *Manager) chargePayload(n, headroom int64) bool {
	if manager.inflightBytes.Add(n) <= pendingPayloadCapBytes+headroom {
		return true
	}
	manager.inflightBytes.Add(-n)
	return false
}

// chargeStageFile 按阶段文件名的证据级分类记账：meta/error 是目录定位
// 与失败归因锚点，必须落库故不过闸恒收（KB 级，超顶代价可忽略）；
// 其余阶段走在飞预算闸。名单变化只改本函数，调用点无感。
func (manager *Manager) chargeStageFile(name string, n int64) bool {
	if name == MetaFile || name == ErrorFile {
		manager.inflightBytes.Add(n)
		return true
	}
	return manager.chargePayload(n, 0)
}

// noteEncodeDrop 在编码协程上计一次编码产物丢弃（计数归属与
// enqueueLocked 同口径切换）：closed 前挂 recorder.dropped 随 Complete
// 折算，closed 后（Complete 已折完）直挂 droppedTotal；n 字节同时
// 计入 droppedPayloadBytes 让丢弃体积可量化。
func (recorder *Recorder) noteEncodeDrop(n int64) {
	recorder.manager.droppedPayloadBytes.Add(uint64(n))
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	recorder.dropped.Add(1)
	if recorder.closed {
		recorder.manager.droppedTotal.Add(1)
	}
}

// pushInsert 把编码完成的作业交给写 worker；在编码协程上调用。charge
// 是该 op 在 inflightBytes 上的预留（0 表示写侧自产自收尾，无预留）。
// 写 worker 存活期间阻塞送达——insertQ 满即把背压传回分片队列，由入队
// 端按丢弃语义降级；worker 已退（关停收尾）则退还 charge 并丢弃计数，
// 编码协程不得陪葬。
func (recorder *Recorder) pushInsert(charge int64, apply func()) {
	select {
	case recorder.manager.insertQ <- insertOp{recorder: recorder, charge: charge, apply: apply}:
	case <-recorder.manager.workerGone:
		recorder.manager.inflightBytes.Add(-charge)
		recorder.manager.droppedTotal.Add(1)
	}
}

// encodePayload 用本请求分片协程的专属编码器压缩 payload。仅可在编码
// 任务闭包内调用——任务恒在本分片协程串行执行，天然免锁。写 worker
// 侧（flushAll/queueCompletion）走 manager.writerEncoder，零散调用方
// 用 store.EncodePayload 共享池。
func (recorder *Recorder) encodePayload(data []byte) (stored []byte, usize int64) {
	return recorder.manager.shardEncoders[recorder.shard].Encode(data)
}

// runEncoder 是一个分片的编码协程：串行消费分片队列——同 recorder 的
// 任务在本协程上保序执行，sanitize/marshal/压缩等 CPU 密集段随分片数
// 摊到多核。收到关停信号后排空本分片残余任务再退出。
func (manager *Manager) runEncoder(shard int) {
	defer manager.encWG.Done()
	queue := manager.queues[shard]
	for {
		select {
		case task := <-queue:
			manager.runTask(task)
		case <-manager.workerStop:
			for {
				select {
				case task := <-queue:
					manager.runTask(task)
				default:
					return
				}
			}
		}
	}
}

// runWriter 是全局写协程：独占 store 写连接，把各编码协程汇来的 op
// （暂存/冲刷/收尾）串行落地；各请求的 chunk 缓冲按 chunkFlushInterval
// 合并成一个跨目录事务提交（进行中的请求对面板仍有亚秒级可见性，高频
// 流式期不再每请求每批帧各付一次 commit）。收到关停信号后先等编码协程
// 排空（它们退出前仍会向 insertQ 推 op，只等不收会互相憋死），收干
// insertQ、冲刷全部脏缓冲再退出。
func (manager *Manager) runWriter() {
	// workerGone 走 defer：worker 以任何路径退出都必须关闭它，
	// Complete 的哨兵等待与迟到入队都在拿它兜底。
	defer close(manager.workerGone)
	flushTick := time.NewTicker(chunkFlushInterval)
	defer flushTick.Stop()
	for {
		select {
		case op := <-manager.insertQ:
			manager.runOp(op)
		case <-flushTick.C:
			manager.flushAll()
		case <-manager.workerStop:
			for {
				select {
				case op := <-manager.insertQ:
					manager.runOp(op)
				case <-manager.encodersDone:
					for {
						select {
						case op := <-manager.insertQ:
							manager.runOp(op)
						default:
							manager.flushAll()
							return
						}
					}
				}
			}
		}
	}
}

// runTask 执行一次编码作业并兜底 panic：编码任务 panic 若不 recover
// 就是未恢复的 goroutine panic，直接崩掉整个进程——观测管道故障绝不能
// 带走数据面。panic 计入 ioErrors，「日志为什么缺了一段」保持可查。
func (manager *Manager) runTask(task writeTask) {
	defer func() {
		if recovered := recover(); recovered != nil {
			manager.ioErrors.Add(1)
			slog.Warn("debuglog: encode task panicked", "dir", task.recorder.dir, "panic", recovered)
		}
	}()
	task.run()
}

// runOp 在写 worker 上执行一个落库 op，panic 兜底口径同 runTask。
// op.charge 的在飞预留随 defer 结清：已暂存字节由 stage*/appendJSONL
// 转记在 recorder.stagedBytes 随提交归还，未暂存（去重跳过/panic）
// 的部分随结清退回——两种结局都不留账面泄漏。
func (manager *Manager) runOp(op insertOp) {
	defer func() {
		manager.inflightBytes.Add(-op.charge)
		if recovered := recover(); recovered != nil {
			manager.ioErrors.Add(1)
			slog.Warn("debuglog: insert op panicked", "dir", op.recorder.dir, "panic", recovered)
		}
	}()
	op.apply()
}

// queueCompletion 把一份完成收尾挂上写侧状态：终态 meta 暂存进
// stagedFiles、本目录标脏、日志行与 errors_only 剥离标记打包进
// pendingCompletions——不单独提交事务，随下次批量冲刷与在飞缓冲
// 同批落库（completionFlushGap 内攒批）。仅写 worker 与 fallbackMu
// 兜底路径调用。
func (manager *Manager) queueCompletion(recorder *Recorder, completion Completion) {
	if !recorder.completionQueued.CompareAndSwap(false, true) {
		return
	}
	if data := recorder.metaJSON(&completion); data != nil {
		stored, usize := manager.writerEncoder.Encode(data)
		recorder.stageFile(MetaFile, stored, usize, false)
	}
	manager.dirtyBufs[recorder] = struct{}{}
	// errors-only 的收敛点：写面随哨兵到齐而静止，干净完成的请求剥掉
	// 全部 payload 只留 meta/error 锚点——logs 行照常落，目录名仍被
	// meta 占位（防同秒复用撞 logs.dir UNIQUE）。带 premature_end_turn
	// 的「可疑成功」保留——它恰是行为异常的取证面。
	manager.pendingCompletions = append(manager.pendingCompletions, completionItem{
		recorder: recorder,
		logRow:   manager.logRowFor(recorder, &completion),
		strip:    manager.errorsOnly.Load() && completion.Result == "completed" && !completion.PrematureEndTurn,
	})
	manager.pendingCompletionCount.Store(int64(len(manager.pendingCompletions)))
	// 收尾项（logRow 结构 + 条目本体）随批次挂写侧持有——必须落库
	// 不过闸，按 completionChargeBytes 估值入账随事务提交归还。
	recorder.stagedBytes += completionChargeBytes
	manager.inflightBytes.Add(completionChargeBytes)
	// 写队列已空说明没有积压：立即冲刷，落库不附加攒批等待；
	// 积压中则攒到 completionFlushGap 或下个 flush tick。
	if len(manager.insertQ) == 0 || time.Since(manager.lastFlush) >= completionFlushGap {
		manager.flushAll()
	}
}

// pendingBatch 汇出本目录当前暂存的整文件行与 chunk 行；仅写 worker
// 调用（stagedFiles/chunkBufs 是它的私有状态）。
func (recorder *Recorder) pendingBatch() ([]store.DebugFileRow, []store.DebugChunkRow) {
	var files []store.DebugFileRow
	for name, staged := range recorder.stagedFiles {
		files = append(files, store.DebugFileRow{
			Dir:      recorder.dir,
			Name:     name,
			Stored:   staged.stored,
			Usize:    staged.usize,
			IfAbsent: staged.ifAbsent,
		})
	}
	var rows []store.DebugChunkRow
	for name, buf := range recorder.chunkBufs {
		if buf.Len() > 0 {
			rows = append(rows, store.DebugChunkRow{Dir: recorder.dir, Name: name, Data: buf.Bytes()})
		}
	}
	return files, rows
}

// flushAll 把全部脏目录的暂存文件与 JSONL 缓冲、待收尾的剥离与日志行
// 合成一个跨目录事务提交；仅写 worker（及 fallbackMu 兜底路径）调用。
// 事务原子：失败时缓冲整体保留、目录留在 dirtyBufs、收尾留在
// pendingCompletions 下轮重试——各项的 drained 照常放行，等待方不为
// 病态 DB 陪葬；因此 logRow/strip 的失败语义从「当场丢弃」变成
// 「随批次重试」，覆盖力只增不减。目录的清理保护（releaseDir）只在
// 收尾随事务落库（或无内容可提交）后解除：未落库的暂存目录失去活跃
// 保护会被容量淘汰删掉，造成丢数据窗口。
func (manager *Manager) flushAll() {
	if len(manager.dirtyBufs) == 0 && len(manager.pendingCompletions) == 0 {
		return
	}
	st := manager.store
	if st == nil {
		for recorder := range manager.dirtyBufs {
			manager.releaseStaged(recorder)
			for name := range recorder.chunkBufs {
				delete(recorder.chunkBufs, name)
			}
			clear(recorder.stagedFiles)
			delete(manager.dirtyBufs, recorder)
		}
		for _, item := range manager.pendingCompletions {
			manager.releaseDir(item.recorder)
			if !item.signaled {
				close(item.recorder.drained)
			}
		}
		manager.pendingCompletions = nil
		manager.pendingCompletionCount.Store(0)
		return
	}
	var batch store.DebugBatch
	batch.Encoder = manager.writerEncoder
	for recorder := range manager.dirtyBufs {
		files, rows := recorder.pendingBatch()
		batch.Files = append(batch.Files, files...)
		batch.Chunks = append(batch.Chunks, rows...)
	}
	for _, item := range manager.pendingCompletions {
		if item.strip {
			batch.StripDirs = append(batch.StripDirs, item.recorder.dir)
		}
		if item.logRow != nil {
			batch.LogRows = append(batch.LogRows, item.logRow)
		}
	}
	manager.lastFlush = time.Now()
	if len(batch.Files) == 0 && len(batch.Chunks) == 0 && len(batch.StripDirs) == 0 && len(batch.LogRows) == 0 {
		for recorder := range manager.dirtyBufs {
			manager.releaseStaged(recorder)
			delete(manager.dirtyBufs, recorder)
		}
		for _, item := range manager.pendingCompletions {
			manager.releaseDir(item.recorder)
			if !item.signaled {
				close(item.recorder.drained)
			}
		}
		manager.pendingCompletions = nil
		manager.pendingCompletionCount.Store(0)
		return
	}
	ctx, cancel := storeCtx()
	err := st.WriteDebugBatch(ctx, batch)
	cancel()
	if err != nil {
		for recorder := range manager.dirtyBufs {
			recorder.noteIOErr("batch", err)
		}
		for index := range manager.pendingCompletions {
			item := &manager.pendingCompletions[index]
			if !item.signaled {
				close(item.recorder.drained)
				item.signaled = true
			}
		}
		return
	}
	for recorder := range manager.dirtyBufs {
		// 成功后放行同类告警：ioErrSeen 的一次性去重不该把恢复后的
		// 再次故障永久静默。
		delete(recorder.ioErrSeen, "batch")
		manager.releaseStaged(recorder)
		clear(recorder.stagedFiles)
		for _, buf := range recorder.chunkBufs {
			buf.Reset()
		}
		delete(manager.dirtyBufs, recorder)
	}
	for _, item := range manager.pendingCompletions {
		manager.releaseDir(item.recorder)
		if !item.signaled {
			close(item.recorder.drained)
		}
	}
	manager.pendingCompletions = nil
	manager.pendingCompletionCount.Store(0)
}

// noteIOErr 把本目录一次写失败计入 manager.ioErrors 并告警；同一类别
// （kind）只记一笔——DB 持续故障若逐帧计数，总量会失真到无法反映
// 影响面。仅在写 worker 与 Complete 收尾（drained 关闭后，与其构成
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

// SetAbort 挂接请求 ctx 的带因取消函数，使 Abort 能以发起方的归因
// 中断请求。Complete 后自动失效；ctx 为 nil 时忽略。
func (recorder *Recorder) SetAbort(cancel context.CancelCauseFunc) {
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

// SetStream 在请求体解码出 options 后回填流式标记：进行中列表的
// is_streaming 依赖它（requestMeta 在解码前已快照，等不到 options）。
func (recorder *Recorder) SetStream(stream bool) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	recorder.stream = stream
	recorder.mutex.Unlock()
}

// SetClass 在令牌准入解析出请求类后回填（fg/bg）：进行中行的
// Meta.Class 与 meta.json client 块由它在 snapshot/metaJSON 时补投。
func (recorder *Recorder) SetClass(class string) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	recorder.requestClass = class
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

// NextDevinSendOrdinal 分配 03-devin-request 词干下一次上游发送的
// 序号（1 起）：序号 1 落基座文件名，2+ 落 attemptN 分片名。计数按
// 请求目录共享——号池 failover 后新 lane 的首发延续上一 lane 的
// 序号，各 lane 的 wire 体各占独立分片不再互覆。
func (recorder *Recorder) NextDevinSendOrdinal() int {
	if recorder == nil {
		return 1
	}
	return int(recorder.devinSends.Add(1))
}

// NoteRetryAttempt 记录一次上游重发及其触发原因；调用方在同处写
// 04 的 retry_attempt 分界行，两处记录保持同源——每次重发各记一笔。
func (recorder *Recorder) NoteRetryAttempt(attempt int, cause string) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	recorder.retries = append(recorder.retries, RetryAttempt{
		Attempt:   attempt,
		Cause:     cause,
		ElapsedMS: time.Since(recorder.startedAt).Milliseconds(),
	})
	recorder.mutex.Unlock()
}

// retryAttempts 返回重发记录的拷贝；无重发返回 nil。
func (recorder *Recorder) retryAttempts() []RetryAttempt {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return append([]RetryAttempt(nil), recorder.retries...)
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
	attempt := AccountAttempt{
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

// SetAffinityHash 回填号池选号用的会话亲和键：Pool.Stream 算出
// SessionAffinityKey 后登记一次，metaJSON 落 meta.affinity_hash。
// 值本身已是截断哈希（32 位 hex），落盘无新增暴露面。
func (recorder *Recorder) SetAffinityHash(affinity string) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	recorder.affinityHash = affinity
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
// 一把锁取齐两者——metaJSON 与 logRowFor 都要这对值。
func (recorder *Recorder) upstreamAttribution() (string, []AccountAttempt) {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return recorder.upstreamAccount, append([]AccountAttempt(nil), recorder.accountAttempts...)
}

// Abort 中断请求：标记 aborted 并以 cause 取消挂接的请求 ctx——取消
// 原因沿 ctx 链传到收尾归因，不同发起方（面板中断/排空强掐）在
// error.json 与 logs 行留下各自的归因文案。
// 无可中断的请求（未挂接或已完结）返回 false。
func (recorder *Recorder) Abort(cause error) bool {
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
	cancel(cause)
	return true
}

// snapshot 返回进行中请求的活快照：首字节计时、阶段状态、模型与丢弃计数。
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
	meta.Stream = recorder.stream
	meta.Class = recorder.requestClass
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
	state := StateWaitingUpstream
	switch {
	case recorder.firstClientMS.Load() >= 0:
		state = StateStreamingClient
	case firstUpstream != nil:
		state = StateReceivingUpstream
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
		DroppedEvents:   recorder.dropped.Load(),
		Abortable:       abortable,
	}
}

// evalDeferred 展开调用方传入的延迟求值 thunk：传 func() any 时投影/建树
// 推迟到编码协程上执行——投影本身也是编码成本的一部分，不该由请求
// goroutine 付。约定不变：thunk 捕获的数据在入队后不得再被改写。
func evalDeferred(value any) any {
	if thunk, ok := value.(func() any); ok {
		return thunk()
	}
	return value
}

// stagedFile 是待刷写的一行整文件：stored 为调用方已编码的入库字节，
// ifAbsent 对应 INSERT OR IGNORE（error.json 的 first-write-wins）。
type stagedFile struct {
	stored   []byte
	usize    int64
	ifAbsent bool
}

// releaseStaged 归还 recorder 暂存账面的全部在飞字节：暂存内容随事务
// 落库（或整体丢弃）后不再由本进程持有。仅写 worker（及 fallbackMu
// 兜底路径）调用；stagedBytes 是写侧私有账本，无锁。
func (manager *Manager) releaseStaged(recorder *Recorder) {
	manager.inflightBytes.Add(-recorder.stagedBytes)
	recorder.stagedBytes = 0
}

// stageFile 把已编码的整文件行按名暂存并标记本目录脏；仅写 worker
// 调用。同窗口同名后写覆盖（等价 OR REPLACE 语义）；两边都是
// ifAbsent 时保留先到者（first-write-wins）——被拒收的 op 的预留账
// 由 runOp 统一结清，这里不动。
func (recorder *Recorder) stageFile(name string, stored []byte, usize int64, ifAbsent bool) {
	if old, ok := recorder.stagedFiles[name]; ok && old.ifAbsent && ifAbsent {
		return
	}
	// 净增量入账：同名覆盖时被替换的旧字节不再持有。op 自身的 charge
	// 由 runOp 结清，这里记的是暂存面实际持有的字节——两笔账在 op
	// 生命周期内是「预留→持有」的交接而非重复计数。
	delta := int64(len(stored) - len(recorder.stagedFiles[name].stored))
	recorder.stagedBytes += delta
	recorder.manager.inflightBytes.Add(delta)
	recorder.stagedFiles[name] = stagedFile{stored: stored, usize: usize, ifAbsent: ifAbsent}
	recorder.manager.dirtyBufs[recorder] = struct{}{}
}

// WriteJSON 将一个阶段快照排入编码队列：sanitize/marshal/压缩都在编码
// 协程上完成，产物暂存后随周期合批写为 debug_files 行。value 可为
// func() any 延迟求值（语义见 evalDeferred）。大体积阶段文件用紧凑
// JSON——体积与 marshal 成本都省约三成，meta.json/error.json 两个
// 人工常读的小文件例外保留缩进。
func (recorder *Recorder) WriteJSON(name string, value any) {
	if recorder == nil || !validLogName(name, ".json") {
		return
	}
	recorder.enqueue(func() {
		var buf bytes.Buffer
		recorder.writeSanitizedJSONLine(&buf, evalDeferred(value))
		stored, usize := recorder.encodeStageFile(name, buf.Bytes())
		n := int64(len(stored))
		if !recorder.manager.chargeStageFile(name, n) {
			recorder.noteEncodeDrop(n)
			return
		}
		recorder.pushInsert(n, func() {
			recorder.stageFile(name, stored, usize, false)
		})
	})
}

// deltaBaseCapBytes 是全进程 delta 基座的钉量上限（~256MB）：每个在飞
// 目录的 01 明文（~400KB 量级）钉给同目录 delta 候选作字典，超限后新
// 目录不再钉座——其 02/03* 照常落库，只是退回独立 gzip 形态。
const deltaBaseCapBytes = 256 << 20

// encodeStageFile 按阶段名选入库编码：01 独立 gzip 并把脱敏后字节钉为
// 本目录 delta 基座；02 与 03-devin-request*（含 attemptN/searchN
// 分片）以基座为 dict 存 zstd 帧——同一请求的三重投影只留残差。
// 基座缺席（01 任务被 shed/未写）或残差收益不足时回退独立 gzip，
// 读侧按魔数自判两种形态。仅编码协程调用（deltaBase 的无锁前提）。
func (recorder *Recorder) encodeStageFile(name string, data []byte) (stored []byte, usize int64) {
	switch {
	case name == StageHTTPRequest:
		if recorder.manager.deltaBaseBytes.Add(int64(len(data))) <= deltaBaseCapBytes {
			recorder.deltaBase = data
		} else {
			recorder.manager.deltaBaseBytes.Add(-int64(len(data)))
		}
	case name == StageRequestMessages || strings.HasPrefix(name, devinRequestStageStem):
		if recorder.deltaBase != nil {
			return store.EncodePayloadDelta(data, recorder.deltaBase)
		}
	}
	return recorder.encodePayload(data)
}

// AppendJSONL 将一个有序事件追加到指定 JSONL 文件。
// value 可为 func() any 延迟求值（语义见 evalDeferred）。
func (recorder *Recorder) AppendJSONL(name, event string, value any) {
	if recorder == nil || !validLogName(name, ".jsonl") {
		return
	}
	// 打戳在入队时刻：Time/ElapsedMS 的语义是「事件发生时」，在编码
	// 协程执行时刻打戳会让队列积压期的行系统性偏大——与 retryAttempt/
	// accountAttempt 的调用时刻打戳同口径。
	at := time.Now()
	// 序号分配与入队在同一临界区：record 含 Seq 须在 marshal 前定版，
	// 临界区序即队列序；分持两把锁会让序号序与队列序错位。任务被丢弃
	// 会留下序号空洞——读侧只按 seq 排序不假设连续，无害。
	recorder.mutex.Lock()
	recorder.sequences[name]++
	seq := recorder.sequences[name]
	recorder.enqueueLocked(func() {
		data, err := marshalJSONLRecord(JSONLRecord{
			Seq:       seq,
			Time:      at.Format(time.RFC3339Nano),
			ElapsedMS: at.Sub(recorder.startedAt).Milliseconds(),
			Event:     event,
			Data:      recorder.sanitizeJSON(evalDeferred(value)),
		})
		if err != nil {
			return
		}
		n := int64(len(data) + 1)
		if !recorder.manager.chargePayload(n, 0) {
			recorder.noteEncodeDrop(n)
			return
		}
		recorder.pushInsert(n, func() {
			recorder.appendJSONL(name, data)
		})
	})
	recorder.mutex.Unlock()
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
	// elapsed_ms 记录错误发生时刻，在入队时打戳（同 AppendJSONL 口径）。
	elapsedMS := time.Since(recorder.startedAt).Milliseconds()
	recorder.enqueue(func() {
		// 落库内容取同步抢占的胜出版本：与 index error_stage/
		// error_message 逐字节一致，不随任务入队顺序漂移。
		recorded := recorder.firstError.Load()
		var data bytes.Buffer
		if err := json.Indent(&data, recorder.sanitizeJSON(map[string]any{
			"stage":      recorded.stage,
			"message":    recorded.message,
			"elapsed_ms": elapsedMS,
		}), "", "  "); err != nil {
			return
		}
		data.WriteByte('\n')
		stored, usize := recorder.encodePayload(data.Bytes())
		n := int64(len(stored))
		if !recorder.manager.chargeStageFile(ErrorFile, n) {
			recorder.noteEncodeDrop(n)
			return
		}
		recorder.pushInsert(n, func() {
			// 去重判定留在写 worker：同目录多个 WriteError 任务产出的 op
			// 按序串行执行，首个到达者胜出——编码途中的丢失由下次调用兜底。
			if recorder.errorWritten {
				return
			}
			recorder.errorWritten = true
			recorder.stageFile(ErrorFile, stored, usize, true)
		})
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

// NoteUpstreamConn 记录首个成功建流所用连接的画像（first-write-wins，
// 与 NoteUpstreamSend/NoteUpstreamOpen 同口径）：续轮重开、搜索扇出的
// 后续建流不覆盖——sent→open 段延迟归因的是首个建流，连接画像必须
// 描述同一次发送，否则复用/新握手被错配到后段的流上。
func (recorder *Recorder) NoteUpstreamConn(reused bool, idle time.Duration) {
	if recorder == nil {
		return
	}
	recorder.upstreamConn.CompareAndSwap(nil, &connInfo{reused: reused, idleMS: idle.Milliseconds()})
}

// Complete 停止受理新写任务、投入排空哨兵后即刻返回：哨兵沿本请求的
// 分片 FIFO 推进，写 worker 执行到它对应的 op 即「本请求写面已齐」，
// 收尾挂进 pendingCompletions 随批量事务提交——payload/日志行/
// errors-only 剥离落库由写侧异步完成，响应路径不再为日志管道的
// 队列深度与批量 commit 付等待。目录的清理保护（releaseDir）由
// flushAll 在收尾落库后解除；需要「payload 已可对外读」语义的
// 调用方（取证导出、测试断言）经 Drained 自行等待。
// 幂等：二次调用直接返回——否则 meta.json 与日志行会重复落一份。
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
	// 折算必须在锁内完成：迟到的入队在 closed 置位后走 enqueue 的
	// closed 分支自折 droppedTotal；拖出锁外会把窗口内的迟到丢弃
	// 既算进 dropped.Load() 又算进对方的自折——双计。
	recorder.manager.droppedTotal.Add(recorder.dropped.Load())
	recorder.mutex.Unlock()
	if recorder.aborted.Load() && completion.Result == "disconnected" {
		completion.Result = "aborted"
	}
	manager := recorder.manager
	// worker 已退（关停收尾）时哨兵/op 都可能送不到：兜底在触发方
	// 直跑同一份收尾。queueCompletion 的 CAS 让并发触发的重复入列
	// 变空操作；fallbackMu 接管 worker 死后写侧私有状态的独占。
	fallback := func() {
		manager.fallbackMu.Lock()
		manager.queueCompletion(recorder, completion)
		manager.flushAll()
		manager.fallbackMu.Unlock()
	}
	// 排空哨兵走本请求自己的分片：分片 FIFO 保证编码协程跑到它时，本
	// 请求已入队的任务都已编码并推进 insertQ；哨兵 op 再经 insertQ FIFO
	// 落在全部前序 op 之后——写 worker 执行到它即收尾入列。op 投递
	// 失败（worker 已退）也走兜底：收尾的落库保证与普通 payload 的
	// 尽力而为不同——丢了就没有第二次。
	sentinel := func() {
		select {
		case manager.insertQ <- insertOp{recorder: recorder, apply: func() {
			manager.queueCompletion(recorder, completion)
		}}:
		case <-manager.workerGone:
			fallback()
		}
	}
	select {
	case manager.queues[recorder.shard] <- writeTask{recorder: recorder, run: sentinel}:
		// 关停竞态：workerStop 已闭说明编码协程在排空退出——它末次
		// 「队列空」判定若先于本次发送，哨兵将搁浅在缓冲里无人执行。
		// 此时派一个看守等 workerGone 兜底；workerStop 未闭则编码协程
		// 必然存活、哨兵必被消费（入队即入缓冲，排空循环必扫到），
		// 正常路径零成本。
		select {
		case <-manager.workerStop:
			go func() {
				<-manager.workerGone
				fallback()
			}()
		default:
		}
	case <-manager.workerGone:
		fallback()
	}
}

// appendJSONL 把一行已序列化记录追加进指定 JSONL 文件的缓冲，并把本
// recorder 登进 dirtyBufs——flush tick 只扫脏集。缓冲按
// chunkFlushInterval 周期随同事务合批入库。仅写 worker 调用
// （dirtyBufs 是它的私有状态，不加锁）。
func (recorder *Recorder) appendJSONL(name string, data []byte) {
	buf := recorder.chunkBufs[name]
	if buf == nil {
		buf = &bytes.Buffer{}
		recorder.chunkBufs[name] = buf
	}
	buf.Write(data)
	buf.WriteByte('\n')
	// 与 op 的 charge（len(data)+1）同口径入账——预留账由 runOp 结清，
	// 这里记暂存面实际持有（含行尾 \n）。commit 后 buf.Reset 保留的
	// 底层数组是二阶残余，按 len 近似忽略（随 recorder 释放归还）。
	n := int64(len(data) + 1)
	recorder.stagedBytes += n
	recorder.manager.inflightBytes.Add(n)
	recorder.manager.dirtyBufs[recorder] = struct{}{}
}

// metaJSON 序列化 meta.json：创建时（completion 为 nil）落进入时刻与
// 客户端元信息，Complete 时（非 nil）补完结时刻、耗时、状态码、结果与
// 用量。schema 是 MetaSummary（meta.go）——键名即字段名，omitempty
// 复刻旧 map 写法的出现条件。纯函数只读 recorder 快照状态，编码协程/
// 写 worker/收尾兜底三路都可调用；marshal 失败返回 nil。
func (recorder *Recorder) metaJSON(completion *Completion) []byte {
	meta := MetaSummary{
		StartedAt:         recorder.startedAt.Format(time.RFC3339Nano),
		Method:            recorder.requestMeta.Method,
		Path:              recorder.requestMeta.Path,
		API:               recorder.requestMeta.API,
		DroppedEvents:     recorder.dropped.Load(),
		RequestReadyMS:    optionalLatency(recorder.requestReadyMS.Load()),
		UpstreamSentMS:    optionalLatency(recorder.upstreamSentMS.Load()),
		UpstreamOpenMS:    optionalLatency(recorder.upstreamOpenMS.Load()),
		FirstUpstreamMS:   optionalLatency(recorder.firstUpstreamMS.Load()),
		FirstClientMS:     optionalLatency(recorder.firstClientMS.Load()),
		RetryAfterSeconds: recorder.retryAfterSeconds.Load(),
		RateLimited:       recorder.rateLimited.Load(),
		Repairs:           recorder.repairs.Load(),
		RetryAttempts:     recorder.retryAttempts(),
	}
	client := MetaClient{
		IP:        recorder.requestMeta.ClientIP,
		UserAgent: recorder.requestMeta.UserAgent,
		KeyHash:   recorder.effectiveKeyHash(),
		RequestID: recorder.requestMeta.ClientRequestID,
	}
	recorder.mutex.Lock()
	client.Class = recorder.requestClass
	recorder.mutex.Unlock()
	if client != (MetaClient{}) {
		meta.Client = &client
	}
	if conn := recorder.upstreamConn.Load(); conn != nil {
		meta.UpstreamConnReused = &conn.reused
		meta.UpstreamConnIdleMS = &conn.idleMS
	}
	meta.UpstreamAccount, meta.UpstreamAttempts = recorder.upstreamAttribution()
	recorder.mutex.Lock()
	meta.AffinityHash = recorder.affinityHash
	meta.PoolCandidates = append([]PoolCandidate(nil), recorder.poolCandidates...)
	recorder.mutex.Unlock()
	if completion != nil {
		finishedAt := time.Now()
		durationMS := finishedAt.Sub(recorder.startedAt).Milliseconds()
		meta.DurationMS = &durationMS
		meta.FinishedAt = finishedAt.Format(time.RFC3339Nano)
		meta.StatusCode = &completion.StatusCode
		meta.Result = &completion.Result
		meta.Model = &completion.Model
		meta.Provider = &completion.Provider
		meta.Stream = &completion.Stream
		meta.RequestedModel = completion.RequestedModel
		meta.ResponseModel = completion.ResponseModel
		meta.ModelMismatch = completion.ModelMismatch
		meta.PrematureEndTurn = completion.PrematureEndTurn
		meta.UpstreamRequestID = completion.UpstreamRequestID
		if completion.Usage != (llm.Usage{}) {
			meta.Usage = &MetaUsage{
				Input:      completion.Usage.Input,
				Output:     completion.Usage.Output,
				CacheRead:  completion.Usage.CacheRead,
				CacheWrite: completion.Usage.CacheWrite,
				Reasoning:  reasoningTokens(completion.Usage),
				Total:      completion.Usage.TotalTokens,
			}
			if costs := completion.Usage.Costs; costs != nil {
				meta.Usage.Costs = &MetaCosts{
					CreditCost:                    costs.CreditCost,
					CommittedCreditCost:           costs.CommittedCreditCost,
					CommittedAcuCost:              costs.CommittedAcuCost,
					CommittedQuotaCostBasisPoints: costs.CommittedQuotaCostBasisPoints,
					CommittedOverageCostCents:     costs.CommittedOverageCostCents,
				}
			}
		}
	}
	// Encoder.SetIndent + Encode 的输出与 MarshalIndent 逐字节一致且
	// 自带结尾 '\n'——定长 marshal 副本与 append 换行的二次拷贝全省。
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(meta); err != nil {
		return nil
	}
	return buf.Bytes()
}

// validLogName 校验阶段文件名：禁止目录穿越，且必须带期望扩展名。
func validLogName(name, extension string) bool {
	return filepath.Base(name) == name && strings.HasSuffix(name, extension)
}
