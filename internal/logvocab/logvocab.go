// Package logvocab 是请求日志/调试持久化域的跨包词汇表叶子包：
// 敏感键名、错误阶段名、阶段文件名、号池换号归因词、失败责任
// 归因规则与文件时代遗留形状。
//
// 它没有任何 internal 依赖——debuglog（写方）、store（读方）、
// obs（诊断脱敏）、ccpanel（面板投影）都从同一处取字面量。
// store 不能反向 import debuglog（debuglog 依赖 store，会成环），
// 词表下沉到这里让原先散在各包的「两侧同步改」注释对全部消失：
//   - 敏感键名：sanitize.go 的归一化名单与 obs 的自由文本正则
//     曾各持一份；
//   - 阶段名/文件名：logOwnerCase、deltaBaseFileName、矩阵谓词
//     里的字面量曾是 stages.go 的镜像；
//   - 责任归因链：SQL CASE 与 Go switch 曾各写一遍同一判定；
//   - 遗留形状：auth_tokens.json 的读写结构曾镜像在 store 内。
package logvocab

import (
	"strings"
)

// ── 敏感键名 ─────────────────────────────────────────────────
//
// SecretKeys 是会被脱敏的 JSON 键名的规范形态（snake_case 原文）。
// 两个消费形态都由它派生：debuglog 的预筛/脱敏按「剔除 '_'/'-'、
// 小写」归一化后比较（见 SecretKey/SecretKeysNormalized）；obs 的
// sensitiveAssignmentPattern 把 '_' 展开成 [\s_-]* 分隔匹配原文。
// metadata 作用域专属的键（如上游 Metadata.f）不属于本名单——
// 全局脱敏会把客户端负载里的同名短键一并遮盖，归写方私表。

var SecretKeys = []string{
	"authorization", "cookie", "set_cookie", "api_key", "access_key",
	"token", "session_token", "access_token", "refresh_token",
	"bearer_token", "password", "client_secret", "device_fingerprint",
	// model_assignment_jwt 是 AssignModel 按请求签发的 router jwt，
	// 03 请求体里的凭证级字段。
	"model_assignment_jwt",
}

// keyNormalizer 归一化 JSON 键名：剔除 '_' 与 '-'，配合小写折叠让
// api_key / api-key / APIKEY 等变体命中同一份名单。
var keyNormalizer = strings.NewReplacer("_", "", "-", "")

// NormalizeKey 返回 key 的归一化形态（剔除 '_'/'-' 后小写）。
func NormalizeKey(key string) string {
	return strings.ToLower(keyNormalizer.Replace(key))
}

// SecretKeysNormalized 是 SecretKeys 的归一化形态（初始化时派生，
// 与名单同序）。字节级比较器（debuglog 的 secretKeySpan）逐条对照
// 本清单，免每键名归一化分配。
var SecretKeysNormalized = func() []string {
	out := make([]string, len(SecretKeys))
	for i, name := range SecretKeys {
		out[i] = NormalizeKey(name)
	}
	return out
}()

// SecretKey 判定 key 是否命中脱敏名单（归一化比较）。
func SecretKey(key string) bool {
	normalized := NormalizeKey(key)
	for _, name := range SecretKeysNormalized {
		if normalized == name {
			return true
		}
	}
	return false
}

// ── 阶段文件名 ───────────────────────────────────────────────
//
// 请求调试目录内的阶段文件名与 logs 根目录下的共享文件名。
// 入库后这些「文件名」是 debug_files/debug_chunks 行的 name 键，
// 写侧在 app/adapter，读侧在 cleaner/dashboard/store 淘汰谓词。

const (
	// StageHTTPRequest 是客户端原始请求投影；也是 zstd delta 帧
	// 的字典基座（同目录 02/03-devin-request* 以它为 raw dict）。
	StageHTTPRequest = "01-http-request.json"
	// StageRequestMessages 是中间模型投影。
	StageRequestMessages = "02-request-messages.json"
	// StageDevinRequest 是首个上游 wire 请求；重试分片词干见
	// DevinRequestStem。
	StageDevinRequest = DevinRequestStem + ".json"
	// StageDevinResponse 是上游原始响应帧。
	StageDevinResponse = "04-devin-response.jsonl"
	// StageResponseEvents 是内部响应事件流。
	StageResponseEvents = "05-response-events.jsonl"
	// StageHTTPResponse 是下发客户端的 SSE 帧。
	StageHTTPResponse = "06-http-response.jsonl"

	// AttachmentsDir 是请求目录内的附件子目录名。
	AttachmentsDir = "attachments"
	// MetaFile 是请求元信息文件（创建时与完结时各写一次）。
	MetaFile = "meta.json"
	// ErrorFile 记录首个失败点；容量淘汰按它识别失败目录。
	ErrorFile = "error.json"
	// StderrFile 是进程 stderr 日志（slog 行），部署脚本负责重定向写入。
	StderrFile = "stderr.log"
	// BindFailureFile 记录最近一次 listen 绑定失败（reuseport
	// 交接争抢等），main 侧写、Stats 侧读。
	BindFailureFile = "bind-failure.json"
)

// DevinRequestStem 是上游请求文件名的公共词干：首个请求是
// 03-devin-request.json，第 N 次重发是 03-devin-request.attemptN.json
// ——词干加 "." 前缀匹配可同时圈出主文件与全部重试分片；服务端
// 托管搜索调用用 ".search" 词干另起编号，不占 chat 发送序号。
const DevinRequestStem = "03-devin-request"

// AnchorFiles 是剥载/容量淘汰时保留的归因锚点名单：meta.json 记
// 结果与延迟分解、error.json 记首败点——payload 被淘汰后检索仍能
// 回答「结果是什么、为什么败」。写路径 StripDirs、读路径
// StripDebugDirsBefore/DebugDirSizesSplit 共用这一份。
var AnchorFiles = []string{MetaFile, ErrorFile}

// ── 错误阶段名 ───────────────────────────────────────────────
//
// WriteError/writeLoggedError 的 stage 实参、logs.error_stage 列、
// error.json 的 stage 字段共用这组取值——读侧在 usage 聚合、矩阵
// 谓词与面板 error_stage 筛选。

const (
	// ErrStageHTTPRead 是请求体读取失败（超限/连接中断）。
	ErrStageHTTPRead = "http_read"
	// ErrStageHTTPDecode 是请求体解码或中间层校验失败。
	ErrStageHTTPDecode = "http_decode"
	// ErrStageRequestBuild 是本地请求投影失败（tool_choice 指空等
	// 参数校验），请求未触达上游。
	ErrStageRequestBuild = "request_build"
	// ErrStageProviderStream 是上游流式响应中途失败（上游责任或
	// 语义拒绝）。
	ErrStageProviderStream = "provider_stream"
	// ErrStageHTTPStream 是下发客户端的 SSE 写出失败（非断连类）。
	ErrStageHTTPStream = "http_stream"
	// ErrStageResponseEvent 是内部事件投影为协议帧时的失败。
	ErrStageResponseEvent = "response_event"
	// ErrStageHTTPEncode 是响应体序列化失败。
	ErrStageHTTPEncode = "http_encode"
	// ErrStageClientDisconnected 是客户端断连/取消终止了请求。
	ErrStageClientDisconnected = "client_disconnected"
	// ErrStageDrainTimeout 是排空超时强掐：与客户端断连同为 ctx
	// 取消但责任在运维侧（部署掐断），不污染断连口径。
	ErrStageDrainTimeout = "drain_timeout"
	// ErrStageDevinConnect 是上游语义拒绝（参数/权限/限流的
	// Connect 层错误）。
	ErrStageDevinConnect = "devin_connect"
	// ErrStageDevinTransport 是上游传输断裂（EOF/帧截断，非语义响应）。
	ErrStageDevinTransport = "devin_transport"
	// ErrStageRateGate 是本地速率闸门快败，请求未触达上游。
	ErrStageRateGate = "rate_gate"
	// ErrStageTokenLimit 是下游令牌准入拒绝（并发/模型白名单/费用
	// 限额），发生在读体解码后，留有调试目录。
	ErrStageTokenLimit = "token_limit"
	// ErrStageModelDisabled 是模型注册表准入拒绝（模型被停用），
	// 同样发生在解码后，留有调试目录。
	ErrStageModelDisabled = "model_disabled"
	// ErrStagePrePipeline 是管线前拒绝（鉴权 401/并发 429/排空
	// 503/WS 准入/读体中断）：请求从未进入处理管线，无调试目录，
	// logs 行仅作留存检索（log_source=rejected 与服役流量分域）。
	ErrStagePrePipeline = "pre_pipeline"
)

// ── 号池换号归因词 ───────────────────────────────────────────
//
// lane_attempt_causes 的 cause 词表：写方把一次被放弃 lane 尝试
// 压成一个词——本地闸门快败是零上游发送的幻影换号（带可选
// ":reason" 后缀），connect code 是真实 failover 发送，无 code
// 的传输断裂类归 nocode。

const (
	// CauseLocalGate 是本地闸门快败的幻影换号；形如
	// "local_gate[:reason]"。
	CauseLocalGate = "local_gate"
	// CauseNoCode 是无 connect code 的传输断裂类放弃。
	CauseNoCode = "nocode"
)

// ── 失败责任归因 ─────────────────────────────────────────────
//
// 责任归因把一条日志行压成单维三值（对齐 sub2api 的 error_owner +
// is_business_limited 双标记）：client 是调用方责任（断连/中断/
// 请求体阶段失败），business_limited 是 429 配额动作（SLA 分母
// 剔除），upstream 是其余失败（上游 5xx/语义错误/transport 断裂/
// 代理自身编码失败——SLA 唯一失分类别），none 是非失败请求。
// 判定链只读 result/status_code/rate_limited/error_stage 四列，
// OwnerCaseSQL 与 ClassifyOwner 是同一链的 SQL 与 Go 形态——
// 规则顺序即优先级，改一处语义必须两处同改。

const (
	OwnerNone            = "none"
	OwnerClient          = "client"
	OwnerBusinessLimited = "business_limited"
	OwnerUpstream        = "upstream"
)

// OwnerCaseSQL 是归因链的 SQL CASE 形态（logs 行作用域）：
// rejected 行是管线前拒绝的留存记录，责任归因恒为 none。
const OwnerCaseSQL = `CASE
		WHEN result = 'rejected' THEN 'none'
		WHEN status_code = 429 OR rate_limited != 0 THEN 'business_limited'
		WHEN result IN ('disconnected', 'aborted') THEN 'client'
		WHEN status_code < 400 AND result != 'failed' THEN 'none'
		WHEN error_stage IN ('` + ErrStageHTTPRead + `', '` + ErrStageHTTPDecode + `') THEN 'client'
		ELSE 'upstream' END`

// OwnerInput 是归因链的行级输入（logs 表同名列的 Go 投影）。
type OwnerInput struct {
	Result      string
	StatusCode  int
	RateLimited bool
	ErrorStage  string
}

// ClassifyOwner 是 OwnerCaseSQL 的 Go 镜像（同序短路）：返回
// client/business_limited/upstream/none。rollup 的 Go 侧贡献计算
// 与它逐行等价由 store 的 cellsConsistencyTest 钉死。
func ClassifyOwner(in OwnerInput) string {
	switch {
	case in.Result == "rejected":
		return OwnerNone
	case in.StatusCode == 429 || in.RateLimited:
		return OwnerBusinessLimited
	case in.Result == "disconnected" || in.Result == "aborted":
		return OwnerClient
	case in.StatusCode < 400 && in.Result != "failed":
		return OwnerNone
	case in.ErrorStage == ErrStageHTTPRead || in.ErrorStage == ErrStageHTTPDecode:
		return OwnerClient
	default:
		return OwnerUpstream
	}
}

// ── 文件时代遗留形状 ─────────────────────────────────────────
//
// auth_tokens.json 的读写形状（文件时代 authtoken.Token/tokenFile
// 的逐字段镜像，含 omitempty 分布）。authtoken 不能反向依赖 store，
// store 也不能依赖 authtoken——文件格式归本包，导入（Unmarshal）
// 与导出（Marshal）共用同一结构，随导入器退役一起删。

type LegacyTokenFile struct {
	NextID int64          `json:"next_id"`
	Tokens []*LegacyToken `json:"tokens"`
}

type LegacyToken struct {
	ID             int64   `json:"id"`
	Hash           string  `json:"token"`
	Description    string  `json:"description"`
	CreatedAt      string  `json:"created_at"`
	ExpiresAt      *int64  `json:"expires_at,omitempty"`
	LastUsedAt     *int64  `json:"last_used_at,omitempty"`
	IsActive       bool    `json:"is_active"`
	SuccessCount   int64   `json:"success_count"`
	FailureCount   int64   `json:"failure_count"`
	StreamAvgTTFB  float64 `json:"stream_avg_ttfb"`
	NonStreamAvgRT float64 `json:"non_stream_avg_rt"`
	StreamCount    int64   `json:"stream_count"`
	NonStreamCount int64   `json:"non_stream_count"`

	PromptTokensTotal        int64   `json:"prompt_tokens_total"`
	CompletionTokensTotal    int64   `json:"completion_tokens_total"`
	CacheReadTokensTotal     int64   `json:"cache_read_tokens_total"`
	CacheCreationTokensTotal int64   `json:"cache_creation_tokens_total"`
	TotalCostUSD             float64 `json:"total_cost_usd"`
	EffectiveCostUSD         float64 `json:"effective_cost_usd"`

	CostUsedMicroUSD     int64 `json:"cost_used_micro_usd"`
	CostLimitMicroUSD    int64 `json:"cost_limit_micro_usd"`
	DailyUsedMicroUSD    int64 `json:"cost_daily_used_micro_usd"`
	DailyLimitMicroUSD   int64 `json:"cost_daily_limit_micro_usd"`
	DailyPeriodStart     int64 `json:"cost_daily_period_start"`
	MonthlyUsedMicroUSD  int64 `json:"cost_monthly_used_micro_usd"`
	MonthlyLimitMicroUSD int64 `json:"cost_monthly_limit_micro_usd"`
	MonthlyPeriodStart   int64 `json:"cost_monthly_period_start"`
	Cost5hUsedMicroUSD   int64 `json:"cost_5h_used_micro_usd"`
	Cost5hLimitMicroUSD  int64 `json:"cost_5h_limit_micro_usd"`
	Cost5hAnchor         int64 `json:"cost_5h_anchor"`
	WeeklyUsedMicroUSD   int64 `json:"cost_weekly_used_micro_usd"`
	WeeklyLimitMicroUSD  int64 `json:"cost_weekly_limit_micro_usd"`
	WeeklyPeriodStart    int64 `json:"cost_weekly_period_start"`

	AllowedModels  []string `json:"allowed_models,omitempty"`
	MaxConcurrency int      `json:"max_concurrency"`
	MaxRPM         int      `json:"max_rpm"`
}
