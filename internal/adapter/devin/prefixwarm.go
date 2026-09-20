// 本文件实现前缀保温：上游 prompt cache 条目按「未接触即衰减」的 TTL
// 过期（实测有效 TTL ~600-780s 漂移），subagent 派发/用户离开造成的静默
// 让整段前缀 re-prefill（冷读 ~48ms/1K tok，生产口径每次命中省 ~4.9s
// TTFB）。cacheWarmer 按 lineage 键登记 sanitize 后的客户端请求
// （retained），对静默条目按节拍发 max_tokens=1 的逐字重放 ping 续命。
// ping 只能续命不能复活——死透条目重放救不回，所以退役只看「客户端
// 可归因上行静默超时」与「凭证自愈后仍语义错误」；单发 ping 的
// cache_read=0 永不作退役证据（相位 miss≠冷 miss，miss 请求本身
// 已完成重写兜底），但 K 连 miss 说明锚反复丢失——降级停 ping
// （demote≠retire：条目留表照 maxIdle 退役，retain 真流量重武装）。
// suspect（超任/换 lane 孤儿）同样停 ping——宽限（2×Interval）纯作
// 退役计时，首标定格不续期。设计定稿与实测依据见
// notes/archive/2026-09-15-claude-subagent-cache-cold-ttl.md。
package devin

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"io"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	devinproto "local/devinproto"

	"github.com/WncFht/devin2api/internal/llm"
)

const (
	// warmSweepInterval 是调度器清扫周期：到期判定与 ping 发射共用这一拍；
	// 30s 远小于默认节拍 180s，到期最多晚半拍发出。
	warmSweepInterval = 30 * time.Second
	// warmPingTimeout 是单发 ping 的整体超时；mt=1 命中帧实测 ~0.7-1.6s，
	// 60s 覆盖 miss 重算整段前缀的情形。
	warmPingTimeout = 60 * time.Second
	// warmAppendFloor 是「真追加」判定的体量净增下限：retained 增长达到
	// 它才计 sends++——同字节级别的原地改写不算追加，探活式重发
	// （逐字相同）靠指纹挡在门外。
	warmAppendFloor = 64
	// warmBytesPerToken 是 retained 字节到前缀 token 的换算估计
	//（JSON 体实测 ~4B/tok）——仅在还没有已完成响应 usage 观测时兜底。
	warmBytesPerToken = 4
	// warmMissDemoteK 是触发降级的连续 ping miss 数：K=4 按默认 180s
	// 节拍 ≈12min 连续不沾。单发相位 miss 的自愈窗是 1–2 拍（下一发
	// hit 即复锚），4 连 miss 需连错 ≥3 个独立窗口——抽签型 miss
	//（p≈0.5）自然命中 1/16，偶发误伤被 retain 复活兜底；连坐型死
	// lineage 百发百中，正是要停的负载。
	warmMissDemoteK = 4
	// warmEventCap 是 ping 结局事件环容量：180s 默认节拍下 256 条对单
	// 条目覆盖 ~12.8h，满载多条目齐射也能回看约十个饱和窗口——与
	// gateWaitCap 同规格的样本环定位。
	warmEventCap = 256
)

// WarmConfig 是前缀保温参数组；时长<=0、上限<=0、名表空时回落到内置
// 默认值（取值依据见设计文档「默认值推导依据」节）。Enabled=false 时
// 全部簿记入口是廉价 no-op、调度器不打 ping。
type WarmConfig struct {
	// Enabled 是总开关（灰度默认关）。
	Enabled bool
	// Interval 是静默条目的 ping 节拍，默认 180s：有效 TTL 的 ~1/4，
	// 每个窗口内打 ~4 次，早夭/lottery 后下一发最多晚一拍重暖。
	Interval time.Duration
	// JitterRatio 是节拍抖动比例（±），默认 0.15：同批派发的兄弟流
	// 同 lastTouch 静默会齐射，抖动把 ping 摊开。
	JitterRatio float64
	// MaxStreams 是流级条目上限，默认 256（生产 15min 活流峰值 ~23
	// 的 11 倍余量）；触顶按 suspect 优先、lastTouch LRU 挤。
	MaxStreams int
	// MaxRetainedMB 是 retained 请求体内存封顶（MB），默认 96；
	// 生产前缀 p90 ~0.7MB/条，重负载下此帽先于条数帽触发。
	MaxRetainedMB int64
	// MinPrefixTokens 是晋升保温的前缀下限，默认 8192：以下 miss 代价
	// <0.4s 不值得 RPM；usage 未观测到时按 retained 字节/4 估。
	MinPrefixTokens int
	// BlockedMaxIdle 是阻塞档（pending 同步派发/普通工具）的最大静默，
	// 默认 4h——等待时长由 agent/工具决定，长尾最重、miss 最贵。
	BlockedMaxIdle time.Duration
	// UserPacedMaxIdle 是用户节奏档（无 pending 或 pending 全为提问类
	// 工具）的最大静默，默认 45min。
	UserPacedMaxIdle time.Duration
	// SubDoneMaxIdle 是已完成 subagent 档的最大静默，默认 10min：
	// SendMessage/agentId 复活是真实但低概率的长尾。
	SubDoneMaxIdle time.Duration
	// UnknownMaxIdle 是无 SessionKey 流的兜底静默上限，默认 30min。
	UnknownMaxIdle time.Duration
	// BlockedNames 是阻塞型派发工具名表；分档的 catch-all 条款已把
	// 一切非 userpaced pending 归 blocked，名表只为可观测性存在。
	BlockedNames []string
	// UserPacedNames 是用户节奏工具名表：pending 全部落在表内才归
	// userpaced 档，否则落 blocked。
	UserPacedNames []string
}

// NormalizeWarmConfig 把无效值回落到默认值：时长/上限 <=0、抖动出
// (0,1) 区间、名表为空。运行时与面板展示共用此函数，两处口径一致。
func NormalizeWarmConfig(params WarmConfig) WarmConfig {
	if params.Interval <= 0 {
		params.Interval = 180 * time.Second
	}
	// 归一用 !(0<r<1) 而非 <=0||>=1：NaN（yaml .nan / ParseFloat("nan")）
	// 在两个比较下都是 false 会漏过，Duration(NaN) 是垃圾值。
	if !(params.JitterRatio > 0 && params.JitterRatio < 1) {
		params.JitterRatio = 0.15
	}
	if params.MaxStreams <= 0 {
		params.MaxStreams = 256
	}
	if params.MaxRetainedMB <= 0 {
		params.MaxRetainedMB = 96
	}
	if params.MinPrefixTokens <= 0 {
		params.MinPrefixTokens = 8192
	}
	if params.BlockedMaxIdle <= 0 {
		params.BlockedMaxIdle = 4 * time.Hour
	}
	if params.UserPacedMaxIdle <= 0 {
		params.UserPacedMaxIdle = 45 * time.Minute
	}
	if params.SubDoneMaxIdle <= 0 {
		params.SubDoneMaxIdle = 10 * time.Minute
	}
	if params.UnknownMaxIdle <= 0 {
		params.UnknownMaxIdle = 30 * time.Minute
	}
	if len(params.BlockedNames) == 0 {
		params.BlockedNames = []string{"Agent", "Task", "Workflow", "wait_agent"}
	}
	if len(params.UserPacedNames) == 0 {
		params.UserPacedNames = []string{"AskUserQuestion", "ExitPlanMode", "request_user_input"}
	}
	return params
}

// warmLineageKey 是一条可保温 lineage 的身份：上游缓存按 SessionKey
// 派生的 trajectory 命名空间隔离，键内四维是「前缀逐字相等」的最小
// 判据——SysHash 盖 system 头 4K（口径同 deriveSessionIDs 种子），
// ToolsHash 盖工具声明（withToolDescriptions 把说明注入 system 尾，
// 4K 之外的工具漂移不改键会留下 ping 自报 hit 的死条目），MsgHash
// 盖首条消息头 1K，Model 是解析后的 wire uid（防 alias/router 改写
// 后同内容跨模型撞键）。零值表示「不做簿记」：所有入口对零键 no-op。
type warmLineageKey struct {
	SessionKey string
	SysHash    string
	ToolsHash  string
	MsgHash    string
	Model      string
}

// warmTier 是条目的静默分级：档位只决定 maxIdle——保温有效的前提是
// 「会话还会 resume」，resume 越不可能，烧 ping 越不值。
type warmTier int

const (
	// warmTierUnknown 是创建初态与无 SessionKey 流的兜底档。
	warmTierUnknown warmTier = iota
	// warmTierBlocked 是 pending 含阻塞派发/普通工具的等待：静默时长
	// 由 agent/工具/权限提示决定（权限提示与普通工具 wire 不可分，
	// 归同一档不亏），长尾最重。
	warmTierBlocked
	// warmTierUserPaced 是无 pending（轮结束等用户）或 pending 全为
	// 提问类工具：静默=用户思考/离开时间。
	warmTierUserPaced
	// warmTierSubDone 是带 sub 标记的流已跑完：SendMessage/agentId
	// 复活长尾存在但概率低。
	warmTierSubDone
)

// warmEntry 是一条 lineage 的簿记：retained 是 sanitize 后的客户端
// 请求原文——ping 拿它重走 buildRequest 重建 wire 体，token/MessageId/
// step_index/execution_id/ModelAssignmentJWT 全部新鲜（生产每发本就
// 如此，实测这些字段不进缓存 token 流），前缀逐字不变才刷同一条目。
type warmEntry struct {
	key           warmLineageKey
	retained      llm.RequestMessages
	retainedBytes int64     // 请求体体量估计（容量帽与前缀粗估共用）
	digest        [32]byte  // retained 全内容指纹：分「逐字重发/真追加/改写」
	model         string    // 最近一发的解析后 wire uid——ping 定向用同一 uid
	router        string    // router uid；非空时 ping 需带 (router,cascade) 的 jwt
	isSub         bool      // system 头 4K 含 cc_is_subagent=true
	sends         int       // 客户端可归因且形态为追加的成功开流数（晋升计数）
	lastTouch     time.Time // 最近一次客户端可归因上行（ping 不刷它）
	lastPingAt    time.Time // 最近一次成功 ping
	nextDue       time.Time // 下一次 ping 到期时刻
	tier          warmTier
	prefixTokens  int       // 最近已完成响应的 input+cache_read 实测；0=未观测
	suspectAt     time.Time // 非零=疑似孤儿：停 ping，首标定格作退役宽限计时（不续期），真流量撤销
	missStreak    int       // 连续 ping miss（cache_read=0）数：hit 或 retain 清零
	demoted       bool      // K 连 miss 降级态：sweep 停发 ping，条目留表，retain 重武装
}

// WarmStats 是保温簿记快照，/admin/runtime-metrics 的 warm 组透出。
// PingsSent 只计打完的 ping；PingHits/Misses 按上游回报 cache_read>0
// 分桶；PingSkips 是闸门 tryAdmit 拒掉的轮次（未触达上游）；
// PingErrors 是发送出错的轮次（不当 miss 证据）；Retired 是条目被
// 移除的累计（静默过期/孤儿宽限期满/语义错误/容量挤），
// RetiredByCause 把同一总量按死因拆开——churn 构成是调参前的
// 必读账；PingMissPrefillTokens 按 miss 时的前缀体量估 prefill
// 成本（miss=全前缀重灌，实测优先 retained/4 兜底）——保温的
// 座位成本此前只在估算里存在；PingHitCacheReadTokens 累计 hit 轮
// 上游实报的 cache_read_tokens——与 miss prefill 估计配对成 ping
// 燃烧的完整账（两侧口径不同：hit 是实测，miss 是估计）；Demoted
// 是连 miss 降级停 ping 的现值（与 Promoted 正交：降级条目仍计
// 保温资格，只是暂停发射）；Events 是最近 ping 结局环（新在前，
// 容量 warmEventCap）——计数器只给累计量，环回答「什么时候发生、
// 为什么」，与 gate.events 同构。
type WarmStats struct {
	Enabled       bool  `json:"enabled"`
	Entries       int   `json:"entries"`
	Promoted      int   `json:"promoted"`
	Demoted       int   `json:"demoted"`
	Suspects      int   `json:"suspects"`
	RetainedBytes int64 `json:"retained_bytes"`
	PingsSent     int64 `json:"pings_sent"`
	PingHits      int64 `json:"ping_hits"`
	PingMisses    int64 `json:"ping_misses"`
	PingSkips     int64 `json:"ping_skips"`
	PingErrors    int64 `json:"ping_errors"`
	Retired       int64 `json:"retired"`

	RetiredByCause         WarmRetiredStats `json:"retired_by_cause"`
	PingMissPrefillTokens  int64            `json:"ping_miss_prefill_tokens"`
	PingHitCacheReadTokens int64            `json:"ping_hit_cache_read_tokens"`
	// FailoverSuspects 累计因会话换 lane 被标 suspect 的条目数（现值
	// 在 Suspects 里）。
	FailoverSuspects int64 `json:"failover_suspects"`
	// Events 是最近 ping 结局环，新在前；lane 身份由透出路径携带
	//（accounts.<name>.warm），与 GateEvent 不带 lane 同规。
	Events []WarmEvent `json:"events,omitempty"`
}

// WarmRetiredStats 是退役条目的死因分账：Idle=静默超档限、
// Suspect=孤儿宽限期满、Semantic=前缀形态被上游语义拒绝、
// Capacity=容量帽挤占——四桶合计 = Retired 总量。MissDemote 是
// 连 miss 降级次数（demote≠retire：条目留表停 ping、可经 retain
// 重武装），不计入 Retired。
type WarmRetiredStats struct {
	Idle       int64 `json:"idle"`
	Suspect    int64 `json:"suspect"`
	Semantic   int64 `json:"semantic"`
	Capacity   int64 `json:"capacity"`
	MissDemote int64 `json:"miss_demote"`
}

// ping 结局事件种类词表，与 PingHits/PingMisses/PingSkips/PingErrors
// 计数器一一对应：hit/miss 是打完的成功 ping 按 cache_read 分桶，
// skip 是闸门 tryAdmit 拒掉的轮次（未触达上游），error 是发送出错。
const (
	warmEventHit   = "hit"
	warmEventMiss  = "miss"
	warmEventSkip  = "skip"
	warmEventError = "error"
)

// WarmEvent 是一轮保温 ping 的结局采样：计数器只给累计量，本环回答
// 「什么时候发生、为什么」——miss 连发的时间分布与 skip 成因此前没有
// 观测面。Reason 只在 skip/error 上有值：skip 记 tryAdmit 的拒绝成因
// （latch/deadzone/quota/pace），error 记 llm.Classify 的 code；
// PrefillTokens 只在 miss 上有值（整段前缀重灌的体量估计，与
// PingMissPrefillTokens 同口径），CacheReadTokens 只在 hit 上有值
// （上游实报命中量，与 PingHitCacheReadTokens 同口径）。条目身份不带
// ——谱系级归因走 missStreak/demoted 簿记，本环只看轮次序列。
type WarmEvent struct {
	At              time.Time `json:"at"`
	Kind            string    `json:"kind"`
	Reason          string    `json:"reason,omitempty"`
	PrefillTokens   int       `json:"prefill_tokens,omitempty"`
	CacheReadTokens int64     `json:"cache_read_tokens,omitempty"`
}

// cacheWarmer 是前缀保温簿记与调度器：条目表 + 清扫协程。挂 Adapter
// 生命周期（New 创建、Close 停），setParams 随配置热更。全部簿记在
// 内存：重启即清空——留下来的流第一发真实请求自然重暖，持久化只会
// 引入陈旧 JWT/proto 烘焙风险。
type cacheWarmer struct {
	adapter *Adapter
	// sendPing 是 ping 发送出口：生产实现 adapter.sendWarmPing（直连
	// streamClient、绕过 app/recorder——ping 是内部流量，不进
	// logs 表/调试记录）；测试注入假实现。
	sendPing func(ctx context.Context, req *devinproto.GetChatMessageRequest) (cacheRead int64, err error)
	// now 是时钟源，测试替换为假钟后调度/退休判定全部可手动推进。
	now func() time.Time
	// jitter 返回 [-1,1] 抖动系数，测试钉 0 消除随机性。
	jitter    func() float64
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	mu            sync.Mutex
	params        WarmConfig
	drained       bool // 排空中：停发 ping，表留作观测，条目自然到期退役
	entries       map[warmLineageKey]*warmEntry
	retainedBytes int64 // 全部条目 retainedBytes 合计（容量帽账本）
	pingsSent     int64
	pingHits      int64
	pingMisses    int64
	pingSkips     int64
	pingErrors    int64
	retired       int64
	// failoverSuspects 累计因会话换 lane 被标 suspect 的条目数
	//（suspectSession 的实绩账——与 RetiredByCause.Suspect 的退役账
	// 对照能看出跨 lane 孤儿占 suspect 死因的比重）。
	failoverSuspects int64
	// retiredByCause 与 retired 同口径累加，按 removeLocked 调用方给
	// 的死因分桶；pingMissPrefillTokens 累计 miss 轮次的前缀体量
	// 估计（发送定影的 snap 口径）；pingHitCacheReadTokens 累计 hit
	// 轮次上游实报的 cache_read_tokens。
	retiredByCause         WarmRetiredStats
	pingMissPrefillTokens  int64
	pingHitCacheReadTokens int64
	// events 是 ping 结局事件环（与 gate.events 同构）：每轮 ping 一条，
	// hit/miss/skip/error 全录——计数器只有累计量，miss 连发的时间
	// 分布与 skip 成因靠本环观测。
	events eventRing[WarmEvent]
}

// newCacheWarmer 创建并启动保温调度协程：Enabled 与否都起——开关
// 热更经 setParams 生效，协程本身常驻。停止用 Close（幂等）。
func newCacheWarmer(adapter *Adapter, params WarmConfig) *cacheWarmer {
	w := &cacheWarmer{
		adapter:  adapter,
		sendPing: adapter.sendWarmPing,
		now:      time.Now,
		events:   newEventRing[WarmEvent](warmEventCap),
		jitter:   func() float64 { return rand.Float64()*2 - 1 },
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		entries:  make(map[warmLineageKey]*warmEntry),
	}
	w.setParams(params)
	go w.run()
	return w
}

// Close 停掉调度协程，幂等。排空语义是不再制造新上游工作；已在途的
// ping 随自己 60s 的 ctx 超时自然收尾，不另等。
func (w *cacheWarmer) Close() {
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
}

// BeginDrain 是 app 排空钩子：排空语义是不再制造新上游工作，从此
// 停发 ping；条目表保留供 stats 观测，退役/淘汰判定照常——排空期
// 可能长达数分钟，静默超期的条目留着只会白占 retained 内存。
// 幂等；进程若取消排空无回路（排空后唯一去向是退出）。
func (w *cacheWarmer) BeginDrain() {
	w.mu.Lock()
	w.drained = true
	w.mu.Unlock()
}

// setParams 热更新保温参数：下一轮清扫即按新节拍/上限/档位执行，
// 缩容即刻按同一淘汰序压回帽内。Enabled=false 停调度并清空条目表
// 释放 retained 内存——重开时各流的第一发真实请求自然重建簿记。
func (w *cacheWarmer) setParams(next WarmConfig) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.params = NormalizeWarmConfig(next)
	if !w.params.Enabled {
		w.entries = make(map[warmLineageKey]*warmEntry)
		w.retainedBytes = 0
		return
	}
	w.evictLocked(warmLineageKey{})
}

// keyOf 计算请求的 lineage 键；功能关闭时返回零键让下游全 no-op，
// 省掉每请求一次的哈希开销。
func (w *cacheWarmer) keyOf(request llm.RequestMessages, resolvedUID string) warmLineageKey {
	w.mu.Lock()
	enabled := w.params.Enabled
	w.mu.Unlock()
	if !enabled {
		return warmLineageKey{}
	}
	return warmLineageKey{
		SessionKey: request.SessionKey,
		SysHash:    hashHead(request.SystemPrompt, 4096),
		ToolsHash:  hashTools(request.Tools),
		MsgHash:    hashHead(firstText(request), 1024),
		Model:      resolvedUID,
	}
}

// noteSend 记一次客户端可归因的上行：lastTouch 推进、撤 suspect
// （真实流量复活）、重排 nextDue。仅更新已存在条目——条目由 retain
// 在首个开流成功后创建，此前的发送没有可记对象；首发/续试重发/
// continueTurn 各跳都在此记账，ping 自己只写 lastPingAt。
func (w *cacheWarmer) noteSend(key warmLineageKey) {
	if key == (warmLineageKey{}) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	entry := w.entries[key]
	if entry == nil {
		return
	}
	now := w.now()
	entry.lastTouch = now
	entry.suspectAt = time.Time{}
	entry.nextDue = w.dueAfterLocked(now)
}

// suspectSession 把本会话滞留本 lane 的全部条目标 suspect：号池把会话
// 换到别的 lane 后，这些谱系已成跨 lane 孤儿——会话流量不再经过本
// lane，烧 ping 续的锚谁也用不上，标记即停 ping、走 2×Interval 宽限
// 退役而非骑满 maxIdle。已 suspect 的不重置计时（不续宽限）；会话若
// 换回来，下一发真实上行（noteSend/retain）照常撤标记。无 SessionKey
// 时跳过：
// sessionless 条目无法按会话归属区分，全组误标会把没搬走的旁人提前
// 杀掉。
func (w *cacheWarmer) suspectSession(sessionKey string) {
	if sessionKey == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	for _, entry := range w.entries {
		if entry.key.SessionKey == sessionKey && entry.suspectAt.IsZero() {
			entry.suspectAt = now
			w.failoverSuspects++
		}
	}
}

// retain 在客户端请求的上游流成功打开后登记/刷新 retained。新建
// sends=1；同键再来按 retained 指纹分三态：逐字重发（探活/重试）
// 不计 sends——「同 SessionKey+同内容重发」型探针不能靠它晋升；
// 真追加（消息数增长或体量净增 >=64B）sends++；其余原地改写
// （microcompact 类前缀失配）sends 归 1 重新计。真流量到达顺带清
// 降级标记与 miss 连击——再武装免费。随后做超任扫描与容量淘汰。
// 续试变体不入表——调用方只在客户端原形态上调用。
func (w *cacheWarmer) retain(key warmLineageKey, request llm.RequestMessages, wireUID, routerUID string) {
	if key == (warmLineageKey{}) {
		return
	}
	// 指纹是全量内容 sha256（长历史 MB 级、数 ms）——纯函数无共享态，
	// 锁外先算好，别让整个 warmer 表为一次哈希停摆。
	size, digest := fingerprintRequest(request)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.params.Enabled {
		return
	}
	now := w.now()
	entry := w.entries[key]
	if entry == nil {
		entry = &warmEntry{
			key:   key,
			sends: 1,
			isSub: strings.Contains(headOf(request.SystemPrompt, 4096), "cc_is_subagent=true"),
		}
		w.entries[key] = entry
	} else {
		w.retainedBytes -= entry.retainedBytes
		switch {
		case digest == entry.digest:
		case len(request.Messages) > len(entry.retained.Messages) ||
			size >= entry.retainedBytes+warmAppendFloor:
			entry.sends++
		default:
			entry.sends = 1
		}
	}
	entry.retained = request
	entry.retainedBytes = size
	entry.digest = digest
	entry.model = wireUID
	entry.router = routerUID
	entry.lastTouch = now
	entry.suspectAt = time.Time{}
	// 真流量 = lineage 仍值钱：清降级与 miss 连击，免费重武装。
	entry.demoted = false
	entry.missStreak = 0
	entry.nextDue = w.dueAfterLocked(now)
	w.retainedBytes += size
	w.markSuspectsLocked(key, now)
	w.evictLocked(key)
}

// noteCompleted 用一发已完成响应更新条目：pending 名表 + sub 标记定
// 静默档位（中断/aborted 不走到这里——分类只看干净收尾）；上游自报
// response_model 进观测集；usage 的 input+cache_read 是前缀体量实测，
// 替换 retained 估算值。
func (w *cacheWarmer) noteCompleted(key warmLineageKey, msg *llm.AssistantMessage) {
	if key == (warmLineageKey{}) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	entry := w.entries[key]
	if entry == nil {
		return
	}
	if observed := msg.Usage.Input + msg.Usage.CacheRead; observed > 0 {
		entry.prefixTokens = int(observed)
	}
	entry.tier = entry.classify(msg, w.params)
}

// pushEvent 追加一条 ping 结局事件；At 在这里打戳（测试假钟随簿记
// 同源）。调用方须持 mu。
func (w *cacheWarmer) pushEvent(ev WarmEvent) {
	ev.At = w.now()
	w.events.push(ev)
}

// stats 返回簿记快照；顺带按当前参数统计 promoted/suspect 现值，
// 事件环按新在前序投影（与 GateStats.Events 同一展示序）。
func (w *cacheWarmer) stats() WarmStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	stats := WarmStats{
		Enabled:                w.params.Enabled,
		Entries:                len(w.entries),
		RetainedBytes:          w.retainedBytes,
		PingsSent:              w.pingsSent,
		PingHits:               w.pingHits,
		PingMisses:             w.pingMisses,
		PingSkips:              w.pingSkips,
		PingErrors:             w.pingErrors,
		Retired:                w.retired,
		RetiredByCause:         w.retiredByCause,
		PingMissPrefillTokens:  w.pingMissPrefillTokens,
		PingHitCacheReadTokens: w.pingHitCacheReadTokens,
		FailoverSuspects:       w.failoverSuspects,
	}
	stats.Events = w.events.recent()
	for _, entry := range w.entries {
		if !entry.suspectAt.IsZero() {
			stats.Suspects++
		}
		if entry.demoted {
			stats.Demoted++
		}
		if w.promotedLocked(entry) {
			stats.Promoted++
		}
	}
	return stats
}

// run 是调度协程本体：固定 30s 一拍，Enabled=false 时 sweep 空转。
func (w *cacheWarmer) run() {
	defer close(w.done)
	tick := time.NewTicker(warmSweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-tick.C:
			w.sweep()
		}
	}
}

// sweep 是一轮清扫：先退役（静默超档限/孤儿宽限期满），再对晋升、
// 未降级、非 suspect 且到期的条目按锚龄逐条 ping（最旧接触先打，
// 饱和窗的零星准入槽先给濒死条目）；排空后只退役不收集。suspect
// 不收集——它只剩退役宽限，锚续上也大概率无人用，ping 纯属白烧。
// ping 在锁外发（单发 ~1s、超时 60s，持锁会堵全部簿记入口），结果
// 回锁内结账；条目发送期间被退役/淘汰只结计数器。
func (w *cacheWarmer) sweep() {
	now := w.now()
	w.mu.Lock()
	if !w.params.Enabled {
		w.mu.Unlock()
		return
	}
	var due []*warmEntry
	for _, entry := range w.entries {
		if cause, expired := w.retireDueLocked(entry, now); expired {
			w.removeLocked(entry, cause)
			continue
		}
		if !w.drained && !entry.demoted && entry.suspectAt.IsZero() && w.promotedLocked(entry) && !now.Before(entry.nextDue) {
			due = append(due, entry)
		}
	}
	// 到期条目按锚剩余寿命升序（最后一次上游接触最旧的先打）：
	// 饱和窗闸门只放零星槽时，稀缺准入先给距死透线（~840s 无接触）
	// 最近的条目，而不是 map 序撞到哪条算哪条。
	slices.SortFunc(due, func(a, b *warmEntry) int {
		return a.anchorContact().Compare(b.anchorContact())
	})
	w.mu.Unlock()
	for _, entry := range due {
		// Close 不设 drained——非排空关停（摘 lane、boot 失败退出）靠
		// 这里查 stop 让 sweep 及时收场，否则 due 积压会串行把每条
		// 60s ping 打满才放 run 退，停掉的 lane 还白烧上游配额。
		select {
		case <-w.stop:
			return
		default:
		}
		w.pingEntry(entry)
	}
}

// pingEntry 对单条到期条目执行一轮保温：发送前回锁复查条目仍存活
// 且未排空——sweep 收集到发送之间的退役/淘汰/BeginDrain 都把本轮
// 化为无操作，不给死条目白打上游。闸门 tryAdmit 不排队不偷槽
// （闩内一律拒，闩外按 wait 的 bg 准入同一上界：可发区间内桶位
// 不越过 fg 预留、bg 计数不越过爬坡额度）——被拒
// 跳过本轮，不推进 nextDue，下拍再试；拿到许可才真正发送，发送即
// 消费本轮（成败都推进 nextDue，错误率 bounded 在节拍内）。结果分账：
// 成功记 hit/miss——hit 清 miss 连击，K 连 miss 置 demoted 停 ping
// （降级不删条目，retain 真流量重武装）；错误先喂闩的错误侧再
// 分类——凭证类自愈重发一次，仍 ClientFixable 才退役，其余一律
// 跳过本轮（不清连击：传输失败对锚存活无证据）。
func (w *cacheWarmer) pingEntry(entry *warmEntry) {
	w.mu.Lock()
	if w.drained || w.entries[entry.key] != entry {
		w.mu.Unlock()
		return
	}
	// retained/model/router 在锁内定影：发送全程在锁外，期间 retain 可能
	// 同键 upsert 换走这些字段——快照让本发 ping 读到一致的一份（retain
	// 只整体换 RequestMessages 不改旧切片，浅拷贝即可），结账仍认原 entry。
	snap := *entry
	w.mu.Unlock()
	if admitted, reason := w.adapter.gate.tryAdmit(); !admitted {
		w.mu.Lock()
		w.pingSkips++
		w.pushEvent(WarmEvent{Kind: warmEventSkip, Reason: reason})
		w.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), warmPingTimeout)
	defer cancel()
	cacheRead, err := w.sendOnce(ctx, &snap)
	if err != nil {
		// ping 撞上 resource_exhausted 必须照喂冷却闩：它最先发现上游
		// 饱和，提前上闩反而替真实请求挡枪；成功侧无需上报。
		w.adapter.gate.noteUpstreamError(err)
		if isCredentialFailure(err) {
			// 陈旧 token 或 assignments 里无 TTL 的 JWT 都可能死：
			// 自愈一遍重发，仍败按重发的错误归类。
			w.healCredentials(&snap)
			cacheRead, err = w.sendOnce(ctx, &snap)
			if err != nil {
				w.adapter.gate.noteUpstreamError(err)
			}
		}
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil {
		w.pingsSent++
		if cacheRead > 0 {
			w.pingHits++
			// hit 轮上游自报 cache_read_tokens 是实测账（取流内最后
			// 一个非零帧），与 miss 的 prefill 估计配对成完整燃烧口径。
			w.pingHitCacheReadTokens += cacheRead
			w.pushEvent(WarmEvent{Kind: warmEventHit, CacheReadTokens: cacheRead})
		} else {
			w.pingMisses++
			// miss = 整段前缀重灌：prefill 成本按发送定影的体量
			// 估，实测优先、retained/4 兜底（与晋升判定同口径）。
			prefill := snap.prefixEstimate()
			w.pingMissPrefillTokens += int64(prefill)
			w.pushEvent(WarmEvent{Kind: warmEventMiss, PrefillTokens: prefill})
		}
	} else {
		w.pingErrors++
		w.pushEvent(WarmEvent{Kind: warmEventError, Reason: llm.Classify(err).Code})
	}
	if w.entries[entry.key] != entry {
		return // 发送期间条目已被退役/淘汰，只结计数器
	}
	if err == nil {
		entry.lastPingAt = now
		switch {
		case cacheRead > 0:
			entry.missStreak = 0
		case entry.digest == snap.digest:
			// digest 判等与下方语义退役同一理由：发送期间 retain
			// 换过内容时本发 miss 是旧形态的证据，不算到当前条目
			//（retain 已清过连击，这里跳过后新内容从零重计）。
			entry.missStreak++
			if entry.missStreak >= warmMissDemoteK {
				// K 连 miss = 锚反复丢失（上游 lottery/相位永久
				// 错位），续打只是每拍白烧一发全前缀 prefill——
				// 降级停 ping。条目留表照 maxIdle 退役，retain
				// 真流量重武装；MissDemote 记的是降级次数，条目
				// 不死故不进 Retired 总量。
				entry.demoted = true
				entry.missStreak = 0
				w.retiredByCause.MissDemote++
			}
		}
	}
	entry.nextDue = w.dueAfterLocked(now)
	if err != nil && llm.Classify(err).ClientFixable && entry.digest == snap.digest {
		// 自愈后仍是语义拒绝（invalid_argument/failed_precondition/
		// ContextLength/permission_denied）：前缀形态上游不再接受，
		// 留着只会持续打空——退役。凭证类（unauthenticated）与环境类
		//（传输/限流/取消/超时/未分类）只跳本轮，不作退役证据。
		// digest 判等挡掉「发送期间同键被 retain 换新内容」的误杀：
		// retain 原地改写同一条目，指针复查检不出来——本次 ping 打的
		// 是 snap 旧形态，它的语义拒绝不构成新内容的死刑证据。
		w.removeLocked(entry, warmRetireSemantic)
	}
}

// sendOnce 用 retained 重建 wire 请求发一发 mt=1 ping：走
// buildRequest 原路径让 token/MessageId/step_index/execution_id/
// assignment jwt 全部新鲜，只把 MaxTokens 压成 1。
func (w *cacheWarmer) sendOnce(ctx context.Context, entry *warmEntry) (int64, error) {
	binding := callBinding{Token: w.adapter.currentToken(), Model: entry.model}
	if entry.router != "" {
		// router 解析来的 uid 需绑本次 cascade 的 assignment jwt；
		// assignModel 内部带 (router,cascade) 缓存，命中即免往返。
		_, cascadeID := deriveSessionIDs(entry.retained)
		assignment, err := w.adapter.assignModel(ctx, entry.router, cascadeID)
		if err != nil {
			return 0, err
		}
		binding.ModelAssignmentJWT = assignment.jwt
	}
	req, _, err := buildRequest(entry.retained, w.adapter.CurrentConfig(), binding)
	if err != nil {
		return 0, err
	}
	req.Configuration.MaxTokens = proto.Uint64(1)
	return w.sendPing(ctx, req)
}

// sendWarmPing 是 ping 的生产发送实现：直连 streamClient 开流后把
// 响应流 drain 到 EOF——不提前 cancel：上游 cache touch 记在建流还是
// 读完未知，读满是保守选择（mt=1 一帧即完）。cacheRead 取流内最后
// 一个非零 cache_read_tokens（上游按帧上报，后者更全）。不走
// attemptRunner.send：ping 过的是 tryAdmit 而非 wait、不做瞬时
// 重试、不进 recorder/index——内部流量豁免。
func (adapter *Adapter) sendWarmPing(ctx context.Context, req *devinproto.GetChatMessageRequest) (int64, error) {
	link := adapter.link()
	link.warmer.kickRequest()
	stream, err := link.stream.GetChatMessage(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, err
	}
	var cacheRead int64
	for stream.Receive() {
		if usage := stream.Msg().GetUsage(); usage.GetCacheReadTokens() != 0 {
			cacheRead = int64(usage.GetCacheReadTokens())
		}
	}
	return cacheRead, stream.Err()
}

// healCredentials 执行一次凭证自愈：TokenSource 重读凭据（拿不到新值
// 照常继续——assignment jwt 是另一处可疑陈旧源），并作废该条目
// (router,cascade) 的 AssignModel 缓存让重发重新解析。
func (w *cacheWarmer) healCredentials(entry *warmEntry) {
	w.adapter.reloadToken()
	if entry.router == "" {
		return
	}
	_, cascadeID := deriveSessionIDs(entry.retained)
	w.adapter.invalidateAssignment(entry.router, cascadeID)
}

// isCredentialFailure 判定凭证味的失败：unauthenticated（token 死）与
// permission_denied（上游把模型未授权/内部错误都归并进此 code）——
// 都可能来自陈旧凭据或 assignments 缓存里无 TTL 的 JWT，先自愈一遍
// 再论生死。permission_denied 同属 ClientFixable：自愈后仍拒按语义
// 错误退役而非环境问题，区分由 pingEntry 的 ClientFixable 判定承担。
func isCredentialFailure(err error) bool {
	if isUnauthenticated(err) {
		return true
	}
	failure := llm.Classify(err)
	return failure.Code == "unauthenticated" || failure.Code == "permission_denied"
}

// classify 按最后一发已完成响应的 pending 名表定静默档位：无 pending
// → sub 标记者 subDone、其余 userPaced；pending 全落 userPacedNames
// → userPaced；其余一切 → blocked（catch-all：权限提示与普通工具
// wire 不可分，统归最长档不亏）。SessionKey 为空时等待语义不可判定，
// 恒归 unknown。
func (entry *warmEntry) classify(msg *llm.AssistantMessage, params WarmConfig) warmTier {
	if entry.key.SessionKey == "" {
		return warmTierUnknown
	}
	pending := 0
	for _, block := range msg.Content {
		call, ok := block.(llm.ToolCall)
		if !ok || call.Server {
			continue
		}
		pending++
		if !slices.Contains(params.UserPacedNames, call.Name) {
			return warmTierBlocked
		}
	}
	if pending == 0 && entry.isSub {
		return warmTierSubDone
	}
	return warmTierUserPaced
}

// markSuspectsLocked 把同 SessionKey 内与新到 lineage 恰好一维相异的
// 旧条目标 suspect：一维相异 = compaction/clear 换首消息、auto-update
// 改 system 头、模型漂移这类「旧流从此永久静默」的形态；两维以上相异
// 是异型兄弟/新话题不标。suspect 即停 ping（sweep 不收集）——锚续上
// 也大概率无人用；宽限（2×Interval）是纯退役计时，首标定格不续期
// （与 suspectSession 同规）：compaction 连发的会话若逐次续期，死
// lineage 会无限滞留白占内存。宽限期内真实上行（noteSend/retain）
// 撤标记。调用方须持 mu。
func (w *cacheWarmer) markSuspectsLocked(arrived warmLineageKey, now time.Time) {
	for key, entry := range w.entries {
		if key == arrived || key.SessionKey != arrived.SessionKey {
			continue
		}
		diff := 0
		if key.SysHash != arrived.SysHash {
			diff++
		}
		if key.ToolsHash != arrived.ToolsHash {
			diff++
		}
		if key.MsgHash != arrived.MsgHash {
			diff++
		}
		if key.Model != arrived.Model {
			diff++
		}
		if diff == 1 && entry.suspectAt.IsZero() {
			entry.suspectAt = now
		}
	}
}

// retireDueLocked 判定条目该退役并给出死因：now-lastTouch 超本档
// maxIdle——静默判定只挂客户端可归因上行，ping 不刷 lastTouch
// （否则被晋升的流靠 ping 自刷永不 idle，maxIdle 形同虚设）；或
// suspect 宽限期满（2×Interval）且期间无真实上行。调用方须持 mu。
func (w *cacheWarmer) retireDueLocked(entry *warmEntry, now time.Time) (warmRetireCause, bool) {
	if now.Sub(entry.lastTouch) > w.maxIdleLocked(entry.tier) {
		return warmRetireIdle, true
	}
	if !entry.suspectAt.IsZero() &&
		now.Sub(entry.suspectAt) > 2*w.params.Interval &&
		!entry.lastTouch.After(entry.suspectAt) {
		return warmRetireSuspect, true
	}
	return 0, false
}

// anchorContact 是上游锚寿命的计时零点：最近一次接触时刻——客户端
// 可归因上行（lastTouch）与成功 ping（lastPingAt，hit/miss 都完成
// 一次锚重写）取较新者。距死透线的余量 = ~840s - (now - anchorContact)。
// 调用方须持 mu。
func (entry *warmEntry) anchorContact() time.Time {
	if entry.lastPingAt.After(entry.lastTouch) {
		return entry.lastPingAt
	}
	return entry.lastTouch
}

// maxIdleLocked 取档位的最大静默时长。调用方须持 mu。
func (w *cacheWarmer) maxIdleLocked(tier warmTier) time.Duration {
	switch tier {
	case warmTierBlocked:
		return w.params.BlockedMaxIdle
	case warmTierUserPaced:
		return w.params.UserPacedMaxIdle
	case warmTierSubDone:
		return w.params.SubDoneMaxIdle
	default:
		return w.params.UnknownMaxIdle
	}
}

// prefixEstimate 估该条目前缀 token 体量：最近实测优先（usage 的
// input+cache_read），未观测按 retained 字节/4 粗估——晋升判定与
// miss prefill 账共用同一口径。
func (entry *warmEntry) prefixEstimate() int {
	if entry.prefixTokens != 0 {
		return entry.prefixTokens
	}
	return int(entry.retainedBytes / warmBytesPerToken)
}

// promotedLocked 判定条目是否可保温：第 2 发真追加的成功上行才晋升
// （一次性探针/逐字重发挡在 sends 计数上），且前缀体量达
// MinPrefixTokens——观测过 usage 用实测 input+cache_read，未观测按
// retained 字节/4 估。调用方须持 mu。
func (w *cacheWarmer) promotedLocked(entry *warmEntry) bool {
	if entry.sends < 2 {
		return false
	}
	return entry.prefixEstimate() >= w.params.MinPrefixTokens
}

// evictLocked 把条目表压回容量帽内：先挤 suspect（疑似孤儿），再按
// lastTouch LRU 挤最旧，直到条数与 retained 字节双双回落。protect
// （本轮刚 upsert 的条目）永不作受害者。调用方须持 mu。
func (w *cacheWarmer) evictLocked(protect warmLineageKey) {
	for len(w.entries) > w.params.MaxStreams || w.retainedBytes > w.params.MaxRetainedMB<<20 {
		var suspect, oldest *warmEntry
		for _, entry := range w.entries {
			if entry.key == protect {
				continue
			}
			if !entry.suspectAt.IsZero() && (suspect == nil || entry.lastTouch.Before(suspect.lastTouch)) {
				suspect = entry
			}
			if oldest == nil || entry.lastTouch.Before(oldest.lastTouch) {
				oldest = entry
			}
		}
		victim := suspect
		if victim == nil {
			victim = oldest
		}
		if victim == nil {
			return // 只剩受保护条目，停止
		}
		w.removeLocked(victim, warmRetireCapacity)
	}
}

// warmRetireCause 是条目退役的死因枚举，removeLocked 按它分账进
// WarmRetiredStats 前四桶；MissDemote 不是 removeLocked 死因
// （降级不删条目），由 pingEntry 在连 miss 达 K 时直接记账。
type warmRetireCause int

const (
	warmRetireIdle warmRetireCause = iota
	warmRetireSuspect
	warmRetireSemantic
	warmRetireCapacity
)

// removeLocked 删除条目并结账（retainedBytes 账本、retired 与
// retiredByCause 计数）。调用方须持 mu。
func (w *cacheWarmer) removeLocked(entry *warmEntry, cause warmRetireCause) {
	if w.entries[entry.key] != entry {
		return
	}
	delete(w.entries, entry.key)
	w.retainedBytes -= entry.retainedBytes
	w.retired++
	switch cause {
	case warmRetireIdle:
		w.retiredByCause.Idle++
	case warmRetireSuspect:
		w.retiredByCause.Suspect++
	case warmRetireSemantic:
		w.retiredByCause.Semantic++
	case warmRetireCapacity:
		w.retiredByCause.Capacity++
	}
}

// dueAfterLocked 算从 base 起的下一个 ping 到期时刻：Interval 加
// ±JitterRatio 抖动。调用方须持 mu。
func (w *cacheWarmer) dueAfterLocked(base time.Time) time.Time {
	return base.Add(time.Duration(float64(w.params.Interval) * (1 + w.params.JitterRatio*w.jitter())))
}

// hashHead 取 s 前 n 字节的 sha256 hex；截断口径与 deriveSessionIDs
// 的会话种子一致——头段相同即认为该维稳定。
func hashHead(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// headOf 是 hashHead 同口径的截断原文（给内容包含判定用）。
func headOf(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// firstText 取首条含非空文本块消息的首个文本——与 deriveSessionIDs
// 的消息侧种子同一口径。
func firstText(request llm.RequestMessages) string {
	for _, message := range request.Messages {
		if text := firstMessageText(message); text != "" {
			return text
		}
	}
	return ""
}

// hashTools 把工具声明序列化为规范字节流取 sha256：工具段经
// withToolDescriptions 注入 system 尾，MCP 上下线/权限变更会让注入段
// 漂移而 sysHash4K 不变——声明的全部 wire 字段按序进哈希，任何漂移
// 都换键，旧 retained 自然闲置过期而不是 ping 出自报 hit。
func hashTools(tools []llm.ToolDefinition) string {
	if len(tools) == 0 {
		return ""
	}
	h := sha256.New()
	put, putBytes := hashFieldWriters(h)
	for _, tool := range tools {
		putTool(h, put, putBytes, tool)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashFieldWriters 产出「长度前缀 + 原样字节」的两路写入闭包：put 走
// io.WriteString 免 string→[]byte 转换拷贝，putBytes 直接喂切片——
// schema/arguments 这类 RawMessage 字段按后者写，避开先转 string
// 再转回 []byte 的双份拷贝。crypto 系 hasher 都实现 io.StringWriter。
func hashFieldWriters(h hash.Hash) (put func(string), putBytes func([]byte)) {
	put = func(s string) {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(s)))
		h.Write(length[:])
		_, _ = io.WriteString(h, s)
	}
	putBytes = func(b []byte) {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(b)))
		h.Write(length[:])
		h.Write(b)
	}
	return put, putBytes
}

// toolIdentity 投影一份工具声明的完整身份字段：「哪些 wire 字段构成
// 工具身份」的清单只在这里存在一份。detachedRequestKey 把返回值作
// JSON 投影进语义键（map 键序由 json.Marshal 规范排序）；putTool 把
// 同一字段集按固定二进制编码写进保温指纹与会话种子——覆盖面以这里
// 为准，编码细节以 putTool 为准。ToolDefinition 新增要进身份的字段：
// 此处加键 + putTool 内决定它的编码序。
func toolIdentity(tool llm.ToolDefinition) map[string]any {
	return map[string]any{
		"name":                    tool.Name,
		"description":             tool.Description,
		"input_schema":            tool.InputSchema,
		"custom":                  tool.Custom,
		"server":                  tool.Server,
		"strict":                  tool.Strict,
		"read_only_hint":          tool.ReadOnlyHint,
		"server_name":             tool.ServerName,
		"attribution_field_names": tool.AttributionFieldNames,
	}
}

// putTool 把 toolIdentity 的字段集按固定二进制编码按序写进 h：长度
// 前缀的名/说明/schema/ServerName、四个布尔位打包的 flags、逐条归因
// 名单。编码序与打包方式是 digest 契约——hashTools 经 sessionSeed
// 进 trajectory/cascade/亲和键（同种子契约）与保温指纹，都靠它跨
// 版本稳定；字段覆盖面对齐 toolIdentity，编码细节不得顺手重排。
// put 由调用方提供：fingerprintRequest 的版本顺带累加体量估计。
func putTool(h hash.Hash, put func(string), putBytes func([]byte), tool llm.ToolDefinition) {
	put(tool.Name)
	put(tool.Description)
	putBytes(tool.InputSchema)
	put(tool.ServerName)
	var flags byte
	if tool.Custom {
		flags |= 1
	}
	if tool.Server {
		flags |= 2
	}
	if tool.Strict {
		flags |= 4
	}
	if tool.ReadOnlyHint {
		flags |= 8
	}
	h.Write([]byte{flags})
	for _, name := range tool.AttributionFieldNames {
		put(name)
	}
}

// fingerprintRequest 估算请求体体量并对全量内容取指纹：体量供
// retained 内存账与前缀 token 粗估（~4B/tok），指纹区分「逐字重发/
// 真追加/原地改写」三种再来形态。只覆盖构成 prompt 的字段
// （system/tools/messages 内容块与 output_id 等回传锚点）——
// MaxTokens/TimestampMS 这类不进 wire 的字段差不算改写。
func fingerprintRequest(request llm.RequestMessages) (size int64, digest [32]byte) {
	h := sha256.New()
	rawPut, rawPutBytes := hashFieldWriters(h)
	put := func(s string) {
		rawPut(s)
		size += int64(len(s))
	}
	putBytes := func(b []byte) {
		rawPutBytes(b)
		size += int64(len(b))
	}
	put(request.SystemPrompt)
	for _, tool := range request.Tools {
		putTool(h, put, putBytes, tool)
	}
	for _, message := range request.Messages {
		var content []llm.Content
		switch typed := message.(type) {
		case llm.UserMessage:
			h.Write([]byte{1})
			content = typed.Content
		case llm.AssistantMessage:
			h.Write([]byte{2})
			put(typed.OutputID)
			content = typed.Content
		case llm.ToolResultMessage:
			h.Write([]byte{3})
			put(typed.ToolCallID)
			if typed.IsError {
				h.Write([]byte{1})
			} else {
				h.Write([]byte{0})
			}
			content = typed.Content
		default:
			continue
		}
		for _, block := range content {
			switch typed := block.(type) {
			case llm.TextContent:
				h.Write([]byte{1})
				put(typed.Text)
			case llm.ThinkingContent:
				h.Write([]byte{2})
				put(typed.Thinking)
				put(typed.Signature)
				put(typed.SignatureType)
				if typed.Redacted {
					h.Write([]byte{1})
				} else {
					h.Write([]byte{0})
				}
			case llm.ImageContent:
				h.Write([]byte{3})
				put(typed.Data)
				put(typed.MIMEType)
			case llm.DocumentContent:
				h.Write([]byte{6})
				put(typed.Data)
				put(typed.URL)
				put(typed.MIMEType)
				put(typed.Filename)
			case llm.VideoContent:
				h.Write([]byte{7})
				put(typed.Data)
				put(typed.URL)
				put(typed.MIMEType)
			case llm.ToolCall:
				h.Write([]byte{4})
				put(typed.ID)
				put(typed.Name)
				putBytes(typed.Arguments)
				var flags byte
				if typed.Custom {
					flags |= 1
				}
				if typed.Server {
					flags |= 2
				}
				h.Write([]byte{flags})
			case llm.ServerToolResult:
				h.Write([]byte{5})
				put(typed.ToolCallID)
				put(typed.ToolName)
				put(typed.TextBody())
			}
		}
	}
	h.Sum(digest[:0])
	return size, digest
}
