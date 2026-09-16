// 本文件实现前缀保温：上游 prompt cache 条目按「未接触即衰减」的 TTL
// 过期（实测有效 TTL ~600-780s 漂移），subagent 派发/用户离开造成的静默
// 让整段前缀 re-prefill（冷读 ~48ms/1K tok，生产口径每次命中省 ~4.9s
// TTFB）。cacheWarmer 按 lineage 键登记 sanitize 后的客户端请求
// （retained），对静默条目按节拍发 max_tokens=1 的逐字重放 ping 续命。
// ping 只能续命不能复活——死透条目重放救不回，所以退役只看「客户端
// 可归因上行静默超时」与「凭证自愈后仍语义错误」；ping 的
// cache_read=0 永不作退役证据（相位 miss≠冷 miss，miss 请求本身
// 已完成重写兜底）。设计定稿与实测依据见
// notes/archive/2026-09-15-claude-subagent-cache-cold-ttl.md。
package devin

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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
	suspectAt     time.Time // 非零=被同 session 新一维相异 lineage 标为疑似孤儿
	// observedModels 记上游自报的 response_model 集合——路由相位漂移
	// 的观测面，不进键不参与判定。
	observedModels map[string]struct{}
}

// WarmStats 是保温簿记快照，/admin/runtime-metrics 的 warm 组透出。
// PingsSent 只计打完的 ping；PingHits/Misses 按上游回报 cache_read>0
// 分桶；PingSkips 是闸门 tryAdmit 拒掉的轮次（未触达上游）；
// PingErrors 是发送出错的轮次（不当 miss 证据）；Retired 是条目被
// 移除的累计（静默过期/孤儿宽限期满/语义错误/容量挤）。
type WarmStats struct {
	Enabled       bool  `json:"enabled"`
	Entries       int   `json:"entries"`
	Promoted      int   `json:"promoted"`
	Suspects      int   `json:"suspects"`
	RetainedBytes int64 `json:"retained_bytes"`
	PingsSent     int64 `json:"pings_sent"`
	PingHits      int64 `json:"ping_hits"`
	PingMisses    int64 `json:"ping_misses"`
	PingSkips     int64 `json:"ping_skips"`
	PingErrors    int64 `json:"ping_errors"`
	Retired       int64 `json:"retired"`
}

// cacheWarmer 是前缀保温簿记与调度器：条目表 + 清扫协程。挂 Adapter
// 生命周期（New 创建、Close 停），setParams 随配置热更。全部簿记在
// 内存：重启即清空——留下来的流第一发真实请求自然重暖，持久化只会
// 引入陈旧 JWT/proto 烘焙风险。
type cacheWarmer struct {
	adapter *Adapter
	// sendPing 是 ping 发送出口：生产实现 adapter.sendWarmPing（直连
	// streamClient、绕过 app/recorder——ping 是内部流量，不进
	// index.jsonl/调试目录）；测试注入假实现。
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
}

// newCacheWarmer 创建并启动保温调度协程：Enabled 与否都起——开关
// 热更经 setParams 生效，协程本身常驻。停止用 Close（幂等）。
func newCacheWarmer(adapter *Adapter, params WarmConfig) *cacheWarmer {
	w := &cacheWarmer{
		adapter:  adapter,
		sendPing: adapter.sendWarmPing,
		now:      time.Now,
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
	if w == nil {
		return
	}
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
	if w == nil {
		return
	}
	w.mu.Lock()
	w.drained = true
	w.mu.Unlock()
}

// setParams 热更新保温参数：下一轮清扫即按新节拍/上限/档位执行，
// 缩容即刻按同一淘汰序压回帽内。Enabled=false 停调度并清空条目表
// 释放 retained 内存——重开时各流的第一发真实请求自然重建簿记。
func (w *cacheWarmer) setParams(next WarmConfig) {
	if w == nil {
		return
	}
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
	if w == nil {
		return warmLineageKey{}
	}
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
	if w == nil || key == (warmLineageKey{}) {
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

// retain 在客户端请求的上游流成功打开后登记/刷新 retained。新建
// sends=1；同键再来按 retained 指纹分三态：逐字重发（探活/重试）
// 不计 sends——「同 SessionKey+同内容重发」型探针不能靠它晋升；
// 真追加（消息数增长或体量净增 >=64B）sends++；其余原地改写
// （microcompact 类前缀失配）sends 归 1 重新计。随后做超任扫描与
// 容量淘汰。续试变体不入表——调用方只在客户端原形态上调用。
func (w *cacheWarmer) retain(key warmLineageKey, request llm.RequestMessages, wireUID, routerUID string) {
	if w == nil || key == (warmLineageKey{}) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.params.Enabled {
		return
	}
	now := w.now()
	size, digest := fingerprintRequest(request)
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
	if w == nil || key == (warmLineageKey{}) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	entry := w.entries[key]
	if entry == nil {
		return
	}
	if msg.ResponseModel != "" {
		if entry.observedModels == nil {
			entry.observedModels = make(map[string]struct{})
		}
		entry.observedModels[msg.ResponseModel] = struct{}{}
	}
	if observed := msg.Usage.Input + msg.Usage.CacheRead; observed > 0 {
		entry.prefixTokens = int(observed)
	}
	entry.tier = entry.classify(msg, w.params)
}

// stats 返回簿记快照；顺带按当前参数统计 promoted/suspect 现值。
func (w *cacheWarmer) stats() WarmStats {
	if w == nil {
		return WarmStats{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	stats := WarmStats{
		Enabled:       w.params.Enabled,
		Entries:       len(w.entries),
		RetainedBytes: w.retainedBytes,
		PingsSent:     w.pingsSent,
		PingHits:      w.pingHits,
		PingMisses:    w.pingMisses,
		PingSkips:     w.pingSkips,
		PingErrors:    w.pingErrors,
		Retired:       w.retired,
	}
	for _, entry := range w.entries {
		if !entry.suspectAt.IsZero() {
			stats.Suspects++
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

// sweep 是一轮清扫：先退役（静默超档限/孤儿宽限期满），再对晋升且
// 到期的条目逐条 ping；排空后只退役不收集。ping 在锁外发（单发 ~1s、
// 超时 60s，持锁会堵全部簿记入口），结果回锁内结账；条目发送期间
// 被退役/淘汰只结计数器。
func (w *cacheWarmer) sweep() {
	now := w.now()
	w.mu.Lock()
	if !w.params.Enabled {
		w.mu.Unlock()
		return
	}
	var due []*warmEntry
	for _, entry := range w.entries {
		if w.retireDueLocked(entry, now) {
			w.removeLocked(entry)
			continue
		}
		if !w.drained && w.promotedLocked(entry) && !now.Before(entry.nextDue) {
			due = append(due, entry)
		}
	}
	w.mu.Unlock()
	for _, entry := range due {
		w.pingEntry(entry)
	}
}

// pingEntry 对单条到期条目执行一轮保温：发送前回锁复查条目仍存活
// 且未排空——sweep 收集到发送之间的退役/淘汰/BeginDrain 都把本轮
// 化为无操作，不给死条目白打上游。闸门 tryAdmit 不排队不偷槽
// （闩内一律拒，闩外只在可发区间+配额有余+无排队者时放行）——被拒
// 跳过本轮，不推进 nextDue，下拍再试；拿到许可才真正发送，发送即
// 消费本轮（成败都推进 nextDue，错误率 bounded 在节拍内）。结果分账：
// 成功记 hit/miss（cr=0 只观测）；错误先喂闩的错误侧再分类——凭证类
// 自愈重发一次，仍 ClientFixable 才退役，其余一律跳过本轮。
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
	if !w.adapter.gate.tryAdmit() {
		w.mu.Lock()
		w.pingSkips++
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
		} else {
			w.pingMisses++
		}
	} else {
		w.pingErrors++
	}
	if w.entries[entry.key] != entry {
		return // 发送期间条目已被退役/淘汰，只结计数器
	}
	if err == nil {
		entry.lastPingAt = now
	}
	entry.nextDue = w.dueAfterLocked(now)
	if err != nil && llm.Classify(err).ClientFixable {
		// 自愈后仍是语义拒绝（invalid_argument/failed_precondition/
		// ContextLength/permission_denied）：前缀形态上游不再接受，
		// 留着只会持续打空——退役。凭证类（unauthenticated）与环境类
		//（传输/限流/取消/超时/未分类）只跳本轮，不作退役证据。
		w.removeLocked(entry)
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
// getChatMessageWithRetry：ping 过的是 tryAdmit 而非 wait、不做瞬时
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
	w.adapter.assignmentsMu.Lock()
	delete(w.adapter.assignments, entry.router+"|"+cascadeID)
	w.adapter.assignmentsMu.Unlock()
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
// 是异型兄弟/新话题不标。宽限期内真实上行（noteSend/retain）撤标记。
// 调用方须持 mu。
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
		if diff == 1 {
			entry.suspectAt = now
		}
	}
}

// retireDueLocked 判定条目该退役：now-lastTouch 超本档 maxIdle——静默
// 判定只挂客户端可归因上行，ping 不刷 lastTouch（否则被晋升的流靠
// ping 自刷永不 idle，maxIdle 形同虚设）；或 suspect 宽限期满
// （2×Interval）且期间无真实上行。调用方须持 mu。
func (w *cacheWarmer) retireDueLocked(entry *warmEntry, now time.Time) bool {
	if now.Sub(entry.lastTouch) > w.maxIdleLocked(entry.tier) {
		return true
	}
	return !entry.suspectAt.IsZero() &&
		now.Sub(entry.suspectAt) > 2*w.params.Interval &&
		!entry.lastTouch.After(entry.suspectAt)
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

// promotedLocked 判定条目是否可保温：第 2 发真追加的成功上行才晋升
// （一次性探针/逐字重发挡在 sends 计数上），且前缀体量达
// MinPrefixTokens——观测过 usage 用实测 input+cache_read，未观测按
// retained 字节/4 估。调用方须持 mu。
func (w *cacheWarmer) promotedLocked(entry *warmEntry) bool {
	if entry.sends < 2 {
		return false
	}
	tokens := entry.prefixTokens
	if tokens == 0 {
		tokens = int(entry.retainedBytes / warmBytesPerToken)
	}
	return tokens >= w.params.MinPrefixTokens
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
		w.removeLocked(victim)
	}
}

// removeLocked 删除条目并结账（retainedBytes 账本与 retired 计数）。
// 调用方须持 mu。
func (w *cacheWarmer) removeLocked(entry *warmEntry) {
	if w.entries[entry.key] != entry {
		return
	}
	delete(w.entries, entry.key)
	w.retainedBytes -= entry.retainedBytes
	w.retired++
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
// 漂移而 sysHash4K 不变——声明的全部 wire 字段（名/说明/schema/
// custom/server/strict/只读位/MCP 归属/归因名单）按序进哈希，任何
// 漂移都换键，旧 retained 自然闲置过期而不是 ping 出自报 hit。
func hashTools(tools []llm.ToolDefinition) string {
	if len(tools) == 0 {
		return ""
	}
	h := sha256.New()
	put := func(s string) {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(s)))
		h.Write(length[:])
		h.Write([]byte(s))
	}
	for _, tool := range tools {
		put(tool.Name)
		put(tool.Description)
		put(string(tool.InputSchema))
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
	return hex.EncodeToString(h.Sum(nil))
}

// fingerprintRequest 估算请求体体量并对全量内容取指纹：体量供
// retained 内存账与前缀 token 粗估（~4B/tok），指纹区分「逐字重发/
// 真追加/原地改写」三种再来形态。只覆盖构成 prompt 的字段
// （system/tools/messages 内容块与 output_id 等回传锚点）——
// MaxTokens/TimestampMS 这类不进 wire 的字段差不算改写。
func fingerprintRequest(request llm.RequestMessages) (size int64, digest [32]byte) {
	h := sha256.New()
	put := func(s string) {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(s)))
		h.Write(length[:])
		h.Write([]byte(s))
		size += int64(len(s))
	}
	put(request.SystemPrompt)
	for _, tool := range request.Tools {
		put(tool.Name)
		put(tool.Description)
		put(string(tool.InputSchema))
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
				put(typed.ThinkingSignature)
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
			case llm.ToolCall:
				h.Write([]byte{4})
				put(typed.ID)
				put(typed.Name)
				put(string(typed.Arguments))
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
				put(typed.Text)
			}
		}
	}
	h.Sum(digest[:0])
	return size, digest
}
