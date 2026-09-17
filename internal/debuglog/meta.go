// 本文件定义 meta.json 的 schema：写方（Recorder.writeMeta）组
// MetaSummary marshal 落盘，读方（Detail.Summary、FindDirByStartedAt
// 与面板投影）解码同一类型——键名集中在一处定义，写读两侧不再靠
// map 字面量与匿名 struct 各自维持。
//
// 字段一律按 JSON 键名字典序声明：旧写法的 map[string]any 经
// encoding/json 排序输出，struct 按声明序输出，两者在此处逐字节一致——
// 目录里历史 meta.json 与新写出的形状无差别。
package debuglog

import (
	"github.com/WncFht/devin2api/internal/llm"
)

// MetaSummary 是 meta.json 的完整形状：创建期（Start 后首个写任务）
// 只有进入时刻与 HTTP 元信息；Complete 收尾补齐完结块（finished_at
// 起的字段）。完结块中 StatusCode/Result/Model/Provider/Stream/
// DurationMS 六个字段在完结时无条件出账（含零值）——指针区分
// 「完结写了零值」与「创建期不写」，omitempty 会把两者混同。
type MetaSummary struct {
	API    string      `json:"api,omitempty"`
	Client *MetaClient `json:"client,omitempty"`

	DroppedEvents uint64 `json:"dropped_events,omitempty"`

	DurationMS *int64 `json:"duration_ms,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	// 五段延迟分解：nil 表示该阶段未发生（区别于 0ms 即时发生）。
	FirstClientMS    *int64  `json:"first_client_ms,omitempty"`
	FirstUpstreamMS  *int64  `json:"first_upstream_ms,omitempty"`
	Method           string  `json:"method"`
	Model            *string `json:"model,omitempty"`
	ModelMismatch    bool    `json:"model_mismatch,omitempty"`
	Path             string  `json:"path"`
	PrematureEndTurn bool    `json:"premature_end_turn,omitempty"`
	Provider         *string `json:"provider,omitempty"`
	RateLimited      bool    `json:"rate_limited,omitempty"`
	// Repairs 是请求投影为上游 wire 格式时的静默修复计数；nil=未发生。
	Repairs        *llm.RequestRepairs `json:"repairs,omitempty"`
	RequestReadyMS *int64              `json:"request_ready_ms,omitempty"`
	RequestedModel string              `json:"requested_model,omitempty"`
	ResponseModel  string              `json:"response_model,omitempty"`
	Result         *string             `json:"result,omitempty"`
	// RetryAfterSeconds 是上游限流给出的 reset 秒数 hint；非限流请求为 0。
	RetryAfterSeconds int64          `json:"retry_after_seconds,omitempty"`
	RetryAttempts     []RetryAttempt `json:"retry_attempts,omitempty"`
	StartedAt         string         `json:"started_at"`
	StatusCode        *int           `json:"status_code,omitempty"`
	Stream            *bool          `json:"stream,omitempty"`
	// UpstreamAccount 是最终服务本请求的上游账号（号池 lane 名）；
	// UpstreamAttempts 是 failover 前的有序失败尝试——全 lane 失败时
	// account 为空，attempts 仍要落（唯一痕迹）。
	UpstreamAccount  string           `json:"upstream_account,omitempty"`
	UpstreamAttempts []AccountAttempt `json:"upstream_attempts,omitempty"`
	// PoolCandidates 是号池开流前的候选序快照（含每 lane 降级原因），
	// 由 Pool.Stream 排序后经 NotePoolCandidates 登记——回答「这次
	// 为什么去了这个号」。
	PoolCandidates []PoolCandidate `json:"pool_candidates,omitempty"`
	// 连接画像拆开 connect 段：Reused=false 时 sent→open 含完整
	// TCP+TLS 握手，Reused=true 时该段基本是上游响应头延迟。
	UpstreamConnIdleMS *int64     `json:"upstream_conn_idle_ms,omitempty"`
	UpstreamConnReused *bool      `json:"upstream_conn_reused,omitempty"`
	UpstreamOpenMS     *int64     `json:"upstream_open_ms,omitempty"`
	UpstreamRequestID  string     `json:"upstream_request_id,omitempty"`
	UpstreamSentMS     *int64     `json:"upstream_sent_ms,omitempty"`
	Usage              *MetaUsage `json:"usage,omitempty"`
}

// MetaClient 是 meta.json 的 client 块（进入期记录的 HTTP 客户端元信息）；
// 键名与 RequestMeta 的对外 wire 口径不同，是文件格式的历史词表。
type MetaClient struct {
	IP        string `json:"ip,omitempty"`
	KeyHash   string `json:"key_hash,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

// MetaUsage 是 meta.json 的 usage 块：完结时上游报告的最终 token 用量，
// 六列恒出账（含零值），costs 只在上游上报时出现。
type MetaUsage struct {
	CacheRead  int64      `json:"cache_read"`
	CacheWrite int64      `json:"cache_write"`
	Costs      *MetaCosts `json:"costs,omitempty"`
	Input      int64      `json:"input"`
	Output     int64      `json:"output"`
	Reasoning  int64      `json:"reasoning"`
	Total      int64      `json:"total"`
}

// MetaCosts 是上游计费读数：credit_cost 是单请求可加总口径，
// committed_* 系是该时刻的账户侧快照，进索引求和没有语义。
type MetaCosts struct {
	CommittedAcuCost              float64 `json:"committed_acu_cost"`
	CommittedCreditCost           int64   `json:"committed_credit_cost"`
	CommittedOverageCostCents     int64   `json:"committed_overage_cost_cents"`
	CommittedQuotaCostBasisPoints int64   `json:"committed_quota_cost_basis_points"`
	CreditCost                    int64   `json:"credit_cost"`
}

// RetryAttempt 是一次上游重发的记录：Attempt 是请求体序号（2 起），
// Cause 是触发原因（token 自愈/空响应续说/transport 断裂重开）。
type RetryAttempt struct {
	Attempt   int    `json:"attempt"`
	Cause     string `json:"cause"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// AccountAttempt 是号池内一次失败尝试的记录：Account 是被试的 lane，
// Code/Message 是它放弃时的分类码与文案（截断至 errorMessageCap）。
// 注意 error.json 是 first-write-wins：failover 救回的请求目录里仍
// 留有首个失败 lane 的 error.json——它描述的是「第一次失败」而非
// 「最终下发给客户端的结果」，终局 lane 看 upstream_account。
type AccountAttempt struct {
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

// 进行中请求的阶段名（ActiveRequest.State 的取值集）：waiting_upstream
// 上游未回首事件 → receiving_upstream 上游在回但未下发客户端内容 →
// streaming_client 正在向客户端流出。
const (
	StateWaitingUpstream   = "waiting_upstream"
	StateReceivingUpstream = "receiving_upstream"
	StateStreamingClient   = "streaming_client"
)
