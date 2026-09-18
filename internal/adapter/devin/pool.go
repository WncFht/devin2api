// 本文件实现多上游账号池：Pool 把 N 条 lane（每条一个完整 *Adapter，
// 含自己的 token 槽/自愈、rateGate、cacheWarmer、assignment 与目录缓存）
// 包成单个 adapter.Adapter，app/handler 层对「多账号」零感知。
//
// 选号是 rendezvous 钉选 + 健康分层：同一会话亲和键（与 trajectory/
// cascade/warm 谱系同种子）对各 lane 打分，健康档整体排前、不健康档
// 只排后不剔除——健康判定是竞态下的近似，最终裁决留给 lane 自己的
// 闸门（闩内秒拒、桶满睡到下窗口）。failover 发生在两级：lane.Stream
// 返回 error（开流前失败，未向客户端提交任何内容）与流内 error 事件
// 先于任何内容事件到达（死 token 的 unauthenticated 就是首帧形态）；
// 后者由 poolStream 在 Recv 里拦转换号，已交付内容后的失败只能透传。
// 两级都按「换号能否改变结果」的词表判定。
package devin

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// badTokenCooldown 是凭据失效冷却的退避基档：lane 内自愈也救不回的
// unauthenticated 把当前 token 标记这么久起步；期间若 TokenSource 重读出
// 不同凭据会提前解禁（见 poolLane.authCooldown），它只是凭据源永远
// 不更新时的兜底解封点。
const badTokenCooldown = 10 * time.Minute

// badTokenCooldownMax 是凭据失效退避的封顶：连败升档最多到这里——
// 死 token 周期性烧一次真实请求的代价封顶在每小时一次。
const badTokenCooldownMax = time.Hour

// genericLaneCooldown 是非凭据类失败的退避基档：permission_denied/
// UpstreamFault/未知错误换号能改变结果但不足以判死凭据——没有这段
// 冷却，钉选到惯犯 lane 的会话每个请求都先烧一次注定失败的上游
// 调用再换号；冷却只压到第三档不剔除，到期自然重试。
const genericLaneCooldown = 90 * time.Second

// genericLaneCooldownMax 是非凭据类退避的封顶。
const genericLaneCooldownMax = 30 * time.Minute

// defaultAffinityTTL 是会话绑定的默认滑动 TTL：与 cliproxyapi 的
// routing.session-affinity-ttl 同量级——远长于单轮对话间隔，短到
// 账号健康变化能在可接受时间内重洗落点。
const defaultAffinityTTL = time.Hour

// poolBindingCap 是绑定表容量上限：号池会话以万计前先逐过期再逐最早
// 到期者——绑定是性能优化不是真值，被逐会话下次请求按普通序重选重绑。
const poolBindingCap = 4096

// defaultQuotaLowThresholdPercent 是配额降权默认阈值：weekly 剩余
// 百分比低于它时 lane 对新会话降档。
const defaultQuotaLowThresholdPercent = 15

// ttfbSampleCap 是 TTFB 滚动窗容量（每 lane 保留的最近样本数）：
// 覆盖够长的相对中位估计窗又不让远古样本常驻。
const ttfbSampleCap = 200

// ttfbConfidenceSamples 是 TTFB 权重置信度缩放的满信样本量：
// n/50 线性升信，冷启动与小样本 lane 的权重回中性 1.0——没观测到
// 慢的 lane 不该被惩罚，没观测到的 lane 也不该白捡便宜。
const ttfbConfidenceSamples = 50

// gatePressureTau 是闸门压力权重的时间常数：期望排队每过一个 τ
// 权重折半级衰减（w=1/(1+E/τ)），τ≈10s 让「预留挡几秒」与「排到
// 下窗」在权重上有数量级区分。
const gatePressureTau = 10 * time.Second

// Pool 是多账号上游池，实现 adapter.Adapter。
type Pool struct {
	// lanes 是当前 lane 集合快照；账号集合热更（ApplyConfigs）整体换
	// 指针，读侧无锁。空集是合法态（账号被面板/配置删光）：读侧视图给
	// 零值，Stream/ListModels 显式报 unavailable 而非取首元素 panic。
	lanes atomic.Pointer[[]*poolLane]
	// bindings 是显式会话绑定表：亲和键 → {lane, expiry}。绑定命中恒
	// 赢于健康分层——会话缓存谱系留在同一 lane 上；绑定 lane 硬故障
	// （池侧两档冷却）才删绑按普通序重选，gate 闩/桶满不算硬故障
	// （粘性区：宁等不换，交给 gate 自己仲裁）。滑动 TTL 命中即续期。
	bindings   map[string]laneBinding
	bindingsMu sync.Mutex
	// inflight 是亲和键的在飞请求指派表：请求在选号循环里逐次登记
	// 当前 lane（pin.setLane），Stream 返回时归还计数。正式绑定要等
	// 前驱开流成功才写——在此之前的同键并发后继按这张表钉到同一
	// lane，不再各自按当时的健康快照散选（prod 实测换 lane 事件
	// 91.5% 是这个窗口的竞态）。纯内存无 TTL：生命周期跟随在飞
	// 请求本身。
	inflight   map[string]*inflightEntry
	inflightMu sync.Mutex
}

// laneBinding 是一条会话绑定：lane 是亲和键当前钉住的泳道，expiry
// 是滑动过期时刻（每次命中按 TTL 续期）。
type laneBinding struct {
	lane   *poolLane
	expiry time.Time
}

// inflightEntry 是同一亲和键的在飞请求集合的当前指派：lane 取最近
// 一次活动指派（前驱 failover 改派时随走——并发同键请求跟随最新
// 指派，与 bind 的 last-writer-wins 同口径），count 是活着的在飞
// 请求数，归零删条目。
type inflightEntry struct {
	lane  *poolLane
	count int
}

// inflightPin 是一次在飞请求的钉选句柄：Stream 选号落定后 acquire，
// 每次 lane 尝试前 setLane 登记当前指派（failover 改派即记录——前驱
// 的缓存谱系实际落在哪，后继就往哪钉），返回时 release 归还计数。
type inflightPin struct {
	pool     *Pool
	affinity string
}

// poolLane 是池里的一条账号泳道：adapter 承载该号全部运行时状态，
// badToken* 是池侧加的凭据失效冷却（lane 内自愈失败后由 noteFailure
// 标记，authCooldown 惰性解禁），lastFailure* 是最近一次换号失败的
// 归因（冷却期外也保留——冷却只压重试，失败史是排障证据），
// failStreak 是连败计数（退避升档与 LaneState 透出用），authMu
// 保护这六组字段。
type poolLane struct {
	name    string
	adapter *Adapter

	authMu       sync.Mutex
	badTokenHash string
	badUntil     time.Time
	// unhealthyUntil 是非凭据类失败的短冷却截止（genericLaneCooldown
	// 起档按 failStreak 退避）；与 badToken 冷却不同键：不看 token
	// 换没换，到点自然解封。
	unhealthyUntil time.Time
	failStreak     int
	// debtSetAt 是最近一次落债（noteFailure 写两档冷却）的时刻：
	// noteSuccess 清账只认债后发起的发送（laneStart 晚于它）——债设立
	// 前已在飞的请求即便成功，证明的也是故障前 lane 能发而非故障后恢复。
	// 不持久化：重启后在飞全灭，任何新发送天然晚于旧债，零值即正确。
	debtSetAt          time.Time
	lastFailureAt      time.Time
	lastFailureCode    string
	lastFailureMessage string
	// priority 是池级排序元数据（Config.Priority 的运行时投影）：
	// 同健康档内 desc 排，ApplyConfigs 热更覆盖。
	priority atomic.Int32
	// quotaLow 是配额降权标记：weekly 剩余低于阈值时 NoteQuotaSample
	// 置位，排序把 lane 降入「健康但配额低」档——只影响新会话落点。
	quotaLow atomic.Bool
	// states 是冷却持久化句柄（与 gate 同一 runtime_state 表，键
	// poolcool:<name>）；nil 时冷却只活在内存。
	states *store.Store
	// ttfb* 是上游侧 TTFB 的滚动样本环（定容，写满覆最旧）与累计
	// 计数：选号的相对中位权重输入，与冷却簿记分开加锁。
	ttfbMu   sync.Mutex
	ttfbRing []time.Duration
	ttfbHead int
}

// LaneState 是单条 lane 的池侧状态快照，/admin/runtime-metrics 的
// accounts.<name>.lane 组透出。Healthy 复刻选中判定的近似值——读
// 时刻与选中时刻之间状态可翻转，是展示快照而非调度承诺。
type LaneState struct {
	Healthy bool `json:"healthy"`
	// AuthCooldownUntil 是凭据失效冷却截止：TokenSource 换出新凭据会
	// 提前解禁，所以它是最晚恢复点而非精确点。
	AuthCooldownUntil *time.Time `json:"auth_cooldown_until,omitempty"`
	// UnhealthyUntil 是非凭据类失败的短冷却截止，到点自然解封。
	UnhealthyUntil *time.Time `json:"unhealthy_until,omitempty"`
	// FailStreak 是连败计数：退避升档的依据，前端可直接显示「连败 N 次」。
	FailStreak int `json:"fail_streak,omitempty"`
	// BoundSessions 是绑定到本 lane 的活会话数（绑定表未过期条目计数）。
	BoundSessions int `json:"bound_sessions,omitempty"`
	// LastFailure* 是最近一次换号失败的归因。
	LastFailureAt      *time.Time `json:"last_failure_at,omitempty"`
	LastFailureCode    string     `json:"last_failure_code,omitempty"`
	LastFailureMessage string     `json:"last_failure_message,omitempty"`
}

var _ adapter.Adapter = (*Pool)(nil)

// NewPool 按配置逐条构建 lane；任一失败时回收已建 lane 整体报错
// （启动期 fail-fast，不留半初始化池）。configs 可为空——空池合法，
// 「先起服务后配号」是面板引导态。
func NewPool(configs []Config) (*Pool, error) {
	lanes := make([]*poolLane, 0, len(configs))
	for _, config := range configs {
		lane, err := newPoolLane(config)
		if err != nil {
			for _, built := range lanes {
				built.adapter.Close()
			}
			return nil, err
		}
		lanes = append(lanes, lane)
	}
	pool := &Pool{bindings: make(map[string]laneBinding), inflight: make(map[string]*inflightEntry)}
	pool.lanes.Store(&lanes)
	return pool, nil
}

func newPoolLane(config Config) (*poolLane, error) {
	laneAdapter, err := New(config)
	if err != nil {
		return nil, fmt.Errorf("devin account %q: %w", config.Identity.Name, err)
	}
	lane := &poolLane{name: config.Identity.Name, adapter: laneAdapter, states: config.GateStateStore}
	lane.priority.Store(int32(config.Priority))
	lane.restoreCooldown()
	return lane, nil
}

// snapshot 返回当前 lane 集合；空池时为空切片。
func (pool *Pool) snapshot() []*poolLane {
	if lanes := pool.lanes.Load(); lanes != nil {
		return *lanes
	}
	return nil
}

// firstLane 返回配置序首 lane：Endpoint 与 tuning 类全局字段各 lane
// 一致（devinConfigsFrom 逐 lane 复制同一模板），首 lane 视图只读这组；
// Identity 类字段各 lane 各异，走 TokenFuncs/AccountLaneStates 等
// per-lane 接口而不是这里。面板 MVP 也固定绑首号——逐号展示是阶段 2
// 的事。空池返回 nil，调用方给各自视图类型的零值。
func (pool *Pool) firstLane() *poolLane {
	if lanes := pool.snapshot(); len(lanes) > 0 {
		return lanes[0]
	}
	return nil
}

// poolFailoverBudget* 是一次请求内换号重试的累计预算：首个候选恒试，
// 首个换号候选在非硬故障时保底放行——fg 预算 60s ≈ 上游开流 deadline
// 60-72s，首 lane 挂到 deadline 已烧穿预算，此处查账必然拦截，健康
// 兄弟 lane 永远接不到管（实证 ~80/日 终局 504 本可救回）；第 2+ 次
// 换号尝试前查账——lane 内自愈链（reopen/凭据重载/传输重试）在 pool
// 视野内是串行烧时，不封顶会把单 lane 预算逐条烧遍（实证
// 111.5s+120s=231.5s 才回 429）。分档与闸门排队预算同量级（fg
// maxHold ~15s / bg ~120s）。检查只在 lane 间进行，拦不住单 lane
// 内部的等待。var 而非 const：测试可缩短覆盖拦截路径。
var poolFailoverBudgetFG = 60 * time.Second
var poolFailoverBudgetBG = 150 * time.Second

// failoverBudget 返回本请求类（fg/bg）的换号累计预算。
func failoverBudget(ctx context.Context) time.Duration {
	if adapter.RequestClass(ctx) == adapter.ClassBG {
		return poolFailoverBudgetBG
	}
	return poolFailoverBudgetFG
}

// errBoundYield 是选号让位的归因载体：绑定 lane 在 pick 时被严格更
// 健康的兄弟挤下首位不是一次发送失败——LocalGate 标记零上游发送
// （幻影换号，与闸门让位快败同一簿记口径），GateReason=bound_yield
// 经 switchCauseKey 落成 lane_attempt_causes 的 local_gate:bound_yield
// 词，与排队中让位（local_gate:yield）分账。
var errBoundYield = &llm.Failure{
	LocalGate:  true,
	GateReason: "bound_yield",
	Message:    "bound lane yielded at pick: a strictly healthier sibling leads",
}

// Stream 按亲和键选 lane 发起请求，失败按 failoverable 词表换号。
// 开流级换号发生在本函数内；开流成功后返回 poolStream，由它在 Recv
// 里处理流内 error 事件的 pre-content 换号（死 token 的 unauthenticated
// 以首帧到达，lane.Stream 已经返回成功，只能在这一级拦截）。
// account 归因记「产出终局结果的 lane」：成功开流、终审拒绝、failover
// 穷尽都算——失败请求同样有「哪号拒的我」的答案。被试过又放弃的 lane
// 明细在 NoteAccountAttempt。
// 会话绑定表（见 Pool.bindings）让同一会话恒落同 lane：开流成功即写
// 绑定，绑定命中恒居候选首位；绑定 lane 硬故障才删绑按普通序重选。
func (pool *Pool) Stream(ctx context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	entered := time.Now()
	lanes := pool.snapshot()
	if len(lanes) == 0 {
		return nil, errNoUpstreamAccounts()
	}
	recorder := debuglog.FromContext(ctx)
	if len(lanes) == 1 {
		lane := lanes[0]
		stream, err := lane.adapter.Stream(ctx, request)
		recorder.SetUpstreamAccount(lane.name)
		return stream, err
	}
	affinity := SessionAffinityKey(request)
	recorder.SetAffinityHash(affinity)
	// 跨 lane 挂接探测的 ctx 载荷：各 lane 的完成缓存登记表（含本 lane，
	// 探测方自跳过）。lookup 落 nil 后 adapter 据此查兄弟 lane 是否持有
	// 同键条目——换 lane 的重试此前在两侧都静默走新上游。
	ctx = withDetachedPeers(ctx, pool.detachedPeerRegistries())
	ranked := pool.rankLanes(ctx, lanes, affinity)
	// 选号审计：排序落定即登记候选序快照，回答「这次为什么去了这个号」
	//（swap 接管时会以新一轮现场覆盖重写）。
	recorder.NotePoolCandidates(poolCandidateRows(ranked))
	// bound 让位的持久账：绑定 lane 让位后按普通序仍被兄弟压过（真被
	// 挤下首位）时记一笔幻影换号——local_gate:bound_yield 随 logs 行
	// 同事务展开进 lane_attempt_causes 与 account_switches；否则这类
	// 移动只剩 pool_candidates.reason，随 payload 保留期一起淘汰。
	if bi := slices.IndexFunc(ranked, func(c poolCandidate) bool { return c.bound && c.yielded }); bi > 0 {
		// 让位探针量随尝试行落账：bound 侧选号时刻的 expectedWait 与
		// 挤下它的兄弟 ew——与闸门 yield 同一组审计字段。
		attempt := *errBoundYield
		attempt.GateProbeMS = ranked[bi].verdict.expectedWait.Milliseconds()
		attempt.GateSiblingEwMS = ranked[bi].yieldEW.Milliseconds()
		recorder.NoteAccountAttempt(ranked[bi].lane.name, &attempt)
	}
	// 在飞钉选登记：本请求占据亲和键的一个在飞名额，同键并发后继
	// 按此钉到同一 lane；release 与首个成功开流的 bind 同刻发生——
	// 钉选接力给正式绑定，不重叠。
	pin := pool.inflightAcquire(affinity)
	defer pin.release()
	rest := make([]*poolLane, len(ranked))
	for i, c := range ranked {
		rest[i] = c.lane
	}
	class := adapter.RequestClass(ctx)
	var lastErr error
	tried := 0
	for len(rest) > 0 {
		// 换号累计预算：首个候选恒试，首个换号候选（tried==1）在非
		// 硬故障时保底放行——首 lane 挂到上游开流 deadline 已烧穿预
		// 算，此处查账必然拦截换号，健康兄弟 lane 永远接不到管。硬故
		// 障候选不保底：池侧冷却判死的 lane 再点一次只会复烧整条自
		// 愈链。第 2+ 次换号（tried>1）恢复查账——烧满一条再败的场
		// 景不再串行点燃第三条。拦截留痕，否则「为什么没换第二条」
		// 只能靠 elapsed 反推。
		if lastErr != nil && time.Since(entered) > failoverBudget(ctx) &&
			(tried != 1 || rest[0].hardDown()) {
			recorder.AppendJSONL(debuglog.StageDevinResponse, "failover_budget_exhausted", map[string]any{"elapsed_ms": time.Since(entered).Milliseconds(), "skipped": rest[0].name})
			break
		}
		lane := rest[0]
		rest = rest[1:]
		tried++
		pin.setLane(lane)
		// 04 里插账号分界行：各 lane 的上游帧直接续写同一文件，
		// 没有分界行无法区分一段帧属于哪号。
		recorder.AppendJSONL(debuglog.StageDevinResponse, "account_attempt", map[string]any{"account": lane.name})
		laneStart := time.Now()
		probe := newGateYieldProbe(class, rest)
		stream, err := lane.adapter.Stream(probe.attach(ctx), request)
		if err == nil {
			recorder.SetUpstreamAccount(lane.name)
			// 开流成功即写绑定：无论它是否是命中那条——绑定记录的是
			// 「上次产出内容的 lane」，胜者接管会话谱系。
			pool.bind(affinity, lane, request.SessionKey)
			return &poolStream{request: request, recorder: recorder, pool: pool, affinity: affinity, entered: entered, lane: lane, laneStart: laneStart, inner: stream, rest: rest, failovers: tried - 1}, nil
		}
		lastErr = err
		recorder.SetUpstreamAccount(lane.name)
		if !failoverable(ctx, err) {
			return nil, err
		}
		recorder.NoteAccountAttempt(lane.name, err)
		lane.noteFailure(err)
		rest = preferSibling(rest, probe.target.Load(), recorder)
		slog.Warn("devin account lane failed, failing over", "account", lane.name, "error", err)
	}
	return nil, lastErr
}

// poolStream 包装一条已开流的 lane，把「流内 error 事件」纳入换号域：
// lane.Stream 的 error 返回只覆盖开流前失败，而上游相当一类拒绝（死
// token 的 unauthenticated、限流、对端断流）以流内首帧 error 事件到达
// ——此时 lane.Stream 已返回成功，不在这里拦截就会直接向客户端下发
// 失败，死 lane 也永远进不了冷却簿记。
// 换号边界与 lane 内 tryReopen 同义：尚未向消费方交付任何非 start 事件
// （start 是本地合成的协议信封，换号后新 lane 的重复 start 被吞掉）。
// 已交付内容后的终局错误透传给客户端，但 lane 证据类失败仍记冷却——
// 本次救不回，后续请求也该避开这条 lane。
type poolStream struct {
	request  llm.RequestMessages
	recorder *debuglog.Recorder
	// pool/affinity 是换号接管写绑定与重登审计所需的回链：
	// swap 成功即把亲和键改绑到新 lane。
	pool     *Pool
	affinity string
	// entered 是 Pool.Stream 的进入时刻：流内换号与开流级换号共用
	// 同一份累计预算（failoverBudget），不是重新起算。
	entered time.Time
	lane    *poolLane
	// laneStart 是当前 lane 的 Stream 调用时刻：TTFB 样本按
	// time.Since(laneStart) 归属该 lane——含它自己的闸门排队（用户
	// 成本），不含别条 lane 的 failover 烧时。
	laneStart time.Time
	inner     llm.ResponseStream
	// rest 是尚未尝试的候选 lane（钉选序尾部）；每条只在换号时试一次。
	rest []*poolLane
	// failovers 是本请求已发起的换号尝试数（含 Stream 开流级与 swap
	// 流内级）：首个换号候选的保底放行每请求只有一次，靠它跨两级
	// 共享——开流级已换过一次后，流内 swap 的首候选恢复查账。
	failovers int
	// startReleased 表示 start 信封已交付给消费方：换号 lane 再产 start
	// 要吞掉，客户端只能见一个。
	startReleased bool
	// committed 表示已交付非 start 事件：内容已部分到达客户端，换号
	// 会产出双份内容，之后的失败只能透传。
	committed bool
}

var _ llm.ResponseStream = (*poolStream)(nil)

// Recv 透传当前 lane 的事件流；在 pre-content 的终局 error 事件上
// 按 failoverableEvent 词表换号重试，直到有 lane 产出内容或候选穷尽。
// 单消费者契约与内层流一致。
func (s *poolStream) Recv(ctx context.Context) (llm.ResponseEvent, error) {
	for {
		event, err := s.inner.Recv(ctx)
		if err != nil {
			// 内层 Recv 的 error 只有 io.EOF 与 ctx 取消两种——都是
			// 本流终态，原样透传（取消下换号毫无意义）。
			return event, err
		}
		if event.Type != llm.ResponseEventError {
			switch {
			case event.Type == llm.ResponseEventStart && s.startReleased:
				// 换号 lane 的重复 start 信封：吞掉继续读下一事件。
				continue
			case event.Type == llm.ResponseEventStart:
				s.startReleased = true
			default:
				if !s.committed {
					// 首个内容事件才是 lane 可用的真实证据（死 token
					// lane 开流也"成功"）——成功清账以内容到达为准，
					// 且只认 laneStart 晚于落债的债后发送；同点采
					// TTFB 样本喂选号的相对中位权重。
					s.lane.noteSuccess(s.laneStart)
					s.lane.noteTTFB(time.Since(s.laneStart))
				}
				s.committed = true
			}
			return event, nil
		}
		// 终局 error 事件：lane 内自愈（reopen/凭据重载）已先跑过一次
		// 没救回才到这里。事件携带分类记录，按同一份词表决策。
		failure := llm.FailureOf(event.Error)
		laneSpecific := failoverableEvent(ctx, failure)
		if laneSpecific {
			s.lane.noteFailure(failure)
		}
		// 无候选时原事件即终局——swap 空手而归会以 nil 错误吞掉它。
		if s.committed || !laneSpecific || len(s.rest) == 0 {
			return event, nil
		}
		s.recorder.NoteAccountAttempt(s.lane.name, failure)
		opened, lastErr := s.swap(ctx)
		// 被吞掉的换号前错误事件记入 05：pump 只记录 Recv 返回的事件，
		// 拦截下来的要由这里补登，否则失败 lane 的死因在事件流里无迹
		// 可查（error.json 是 first-write-wins 已有其一）。预算拦截
		// 未试任何候选时事件原样下发、由 pump 记，此处不预登。
		if opened || lastErr != nil {
			s.recorder.RecordResponseEvent(event)
		}
		if !opened {
			if lastErr == nil {
				// 预算拦截未点燃任何候选：本 lane 的真实错误事件照常
				// 下发，与候选穷尽的透传路径同语义。
				return event, nil
			}
			// 候选全部开流失败：终局错误是最后一次开流错误（与
			// Stream 路径的 lastErr 语义一致），原 error 事件吞掉。
			return llm.ResponseEvent{}, lastErr
		}
	}
}

// swap 在 pre-content 失败后把流换到下一候选 lane：逐条尝试开流，
// 开流级失败走与 Stream 相同的 attempt/冷却/归因记账（failoverable
// 词表，无 code 的本地构建错误不再换号）。返回是否成功接管与最后的
// 开流错误。
func (s *poolStream) swap(ctx context.Context) (bool, error) {
	var lastErr error
	for len(s.rest) > 0 {
		// 与 Stream 开流级同一份累计预算：swap 只在 pre-content 触发
		//（committed 后不再换号），s.entered 起算的 elapsed 全是未产出
		// 内容的烧时。首个换号候选（failovers==0，跨开流/流内两级共
		// 享计数）在非硬故障时保底放行——本 lane 流内烧穿预算后查账
		// 必然拦截，健康兄弟永远接不到管；硬故障候选不保底，池侧冷
		// 却判死的 lane 不值得复烧一条自愈链。第 2+ 次换号恢复查账，
		// 拦住串行点燃第三条。未试候选时返回 (false, nil)，由 Recv
		// 把本 lane 的真实错误事件透传给客户端。
		if time.Since(s.entered) > failoverBudget(ctx) &&
			(s.failovers > 0 || s.rest[0].hardDown()) {
			s.recorder.AppendJSONL(debuglog.StageDevinResponse, "failover_budget_exhausted", map[string]any{"elapsed_ms": time.Since(s.entered).Milliseconds(), "skipped": s.rest[0].name})
			break
		}
		next := s.rest[0]
		s.rest = s.rest[1:]
		s.failovers++
		s.recorder.AppendJSONL(debuglog.StageDevinResponse, "account_attempt", map[string]any{"account": next.name})
		laneStart := time.Now()
		// 调试记录挂 s.recorder（开流时的请求 ctx）而不是指望 Recv 的
		// ctx 恰好携带——换号 lane 的 03 分片等证据必须落本请求目录，
		// 与 swap 自身的 account_attempt 记账同一份句柄。ctx 注值同理
		// 必须重注：gateYield 与跨 lane 挂接探测的 peers 登记表都活在
		// 开流 ctx 上，Recv 的 ctx 是另一个对象。
		probe := newGateYieldProbe(adapter.RequestClass(ctx), s.rest)
		probeCtx := withDetachedPeers(probe.attach(ctx), s.pool.detachedPeerRegistries())
		inner, err := next.adapter.Stream(debuglog.WithRecorder(probeCtx, s.recorder), s.request)
		if err == nil {
			s.lane = next
			s.laneStart = laneStart
			s.inner = inner
			s.recorder.SetUpstreamAccount(next.name)
			// 换号接管即改绑：会话谱系转到新 lane，后续请求直落这里。
			s.pool.bind(s.affinity, next, s.request.SessionKey)
			// 重选审计覆盖首轮快照——meta 留下的是最新一轮决策现场。
			s.recorder.NotePoolCandidates(poolCandidateRows(s.swapRanked(next, adapter.RequestClass(ctx))))
			return true, nil
		}
		lastErr = err
		s.recorder.SetUpstreamAccount(next.name)
		if !failoverable(ctx, err) {
			return false, err
		}
		s.recorder.NoteAccountAttempt(next.name, err)
		next.noteFailure(err)
		s.rest = preferSibling(s.rest, probe.target.Load(), s.recorder)
		slog.Warn("devin account lane failed to open during in-stream failover", "account", next.name, "error", err)
	}
	return false, lastErr
}

// failoverableEvent 是流内 error 事件的换号与归责词表：能走到流内
// 说明请求形状已被 lane 的开流路径接受，失败要么是这条 lane 的环境
// 证据（凭据失效、限流、传输断、看门狗判死——无 code 的本地超时也
// 是 lane 证据，与开流期 codeless=本地构建错误的判定相反），要么是
// 请求级拒绝（ClientFixable）或客户端已走（Canceled）——后两者换号
// 不能改变结果。与 failoverable 的差异只在无 code 错误一项。
func failoverableEvent(ctx context.Context, failure *llm.Failure) bool {
	if ctx.Err() != nil || failure.Canceled {
		return false
	}
	return !failure.ClientFixable
}

// poolCandidate 是排序时的一次性评估快照：verdict 是 lane 当时的健康
// 判定与降级归因，bound 是绑定命中标记，yielded 是本轮绑定让位标记
// （bound 保留审计语义，居首特权被摘掉），yieldEW 是把 bound 挤下
// 首位的兄弟的 expectedWait（bound_yield 尝试行的 gate_sibling_ew_ms
// 来源），pinned 是在飞钉选命中标记，score 是 rendezvous 分数，weight
// 是健康权重（压力×相对 TTFB），key 是加权 HRW 键 u^(1/w)（末位
// 排序键，大者居前——同亲和键下各 lane 的选中概率 ∝ w）。
// 快照语义保证审计行（pool_candidates）与排序决策同源——不在排完序后
// 再评一次，避免两次评估之间的状态翻转让审计与决策对不上。
type poolCandidate struct {
	lane     *poolLane
	score    [32]byte
	key      float64
	weight   float64
	verdict  laneVerdict
	bound    bool
	yielded  bool
	yieldEW  time.Duration
	pinned   bool
	priority int32
}

// boundLead 报告候选本轮是否享有绑定居首特权：绑定命中且未让位。
func (c poolCandidate) boundLead() bool { return c.bound && !c.yielded }

// swapRanked 构造换号接管后的审计快照：接管 lane 居首（bound），其余
// 候选按剩余序附上当时的降级归因。
func (s *poolStream) swapRanked(taken *poolLane, class string) []poolCandidate {
	ranked := make([]poolCandidate, 0, len(s.rest)+1)
	ranked = append(ranked, poolCandidate{lane: taken, verdict: taken.verdict(class), bound: true})
	for _, lane := range s.rest {
		ranked = append(ranked, poolCandidate{lane: lane, verdict: lane.verdict(class)})
	}
	return ranked
}

// poolCandidateRows 把候选快照投影成审计行：bound lane 的 Reason 在
// 降级归因后附 "bound"（让位时改附 "bound_yield"，Bound 字段仍为真）、
// pinned lane 记 "inflight"（两者都是粘性区语义）；其余 lane 的
// Reason 是降级归因的有序叠加——取全部适用词连写而非首个主因，多因
// 并存时（如冷却+闩、死区+桶满）完整保留现场。
func poolCandidateRows(ranked []poolCandidate) []debuglog.PoolCandidate {
	rows := make([]debuglog.PoolCandidate, len(ranked))
	for i, c := range ranked {
		rows[i] = debuglog.PoolCandidate{
			Name:    c.lane.name,
			Healthy: c.verdict.healthy,
			Bound:   c.bound,
			Pinned:  c.pinned,
			Weight:  c.weight,
			Reason:  strings.Join(c.verdict.reasons, ","),
		}
		if c.bound && !c.yielded {
			rows[i].Reason = strings.Join(append(c.verdict.reasons, "bound"), ",")
		}
		if c.yielded {
			rows[i].Reason = strings.Join(append(c.verdict.reasons, "bound_yield"), ",")
		}
	}
	return rows
}

// rankLanes 给出候选序的完整评估快照，排序键从高到低：
// bound-hit（未让位）→ 在飞钉选 → 健康档（绿 → 配额低 → 病）→
// priority desc → 加权 HRW 键降序 → rendezvous 分数升序兜底。
// 三区语义：绑定 lane 默认居首（粘性区「宁等不换」——正式绑定是
// 「上次产出内容的 lane」的确认记录，留原 lane 继续吃 warm/cache
// 连续性红利），但已判病而有可发兄弟、或期望排队比最优兄弟高出
// 一个 τ 时本轮让位回本档排序：桶满睡到 maxHold（fg ~15s /
// bg ~120s）再 failover 是实测最贵的错配，换边损失只是一次绑定的
// 连续性；闩内 bound 同理让位，省掉一次必败的过闸评估与幻影 attempt
// 账。让位不解绑不动 bound
// 标记：它若按普通序仍最优照旧赢，换边开流成功后 bind 照常把谱系
// 记到胜者 lane。在飞钉选 lane 居次位同区语义（同亲和键有在飞请求
// 时后继钉同一 lane——正式绑定落地前的并发窗口不再各自散选）；
// 配额低 lane 仍在健康档内但降一级，只影响新会话落点；不健康不剔除
// 只排后：判定是近似快照，全不健康时仍回分数序，由 lane 闸门自己走
// wait/快败（客户端拿 Retry-After，与单号一致）。
func (pool *Pool) rankLanes(ctx context.Context, lanes []*poolLane, affinity string) []poolCandidate {
	bound := pool.boundLane(affinity)
	// 绑定恒赢于在飞钉选——绑定是「已产出内容的 lane」的确认记录，
	// 在飞指派只是未确认的当前尝试；两者天然互斥（有绑定不查在飞表）。
	var pinned *poolLane
	if bound == nil {
		pinned = pool.inflightLane(affinity)
	}
	class := adapter.RequestClass(ctx)
	// TTFB 相对权重需要跨 lane 的最小中位：先全量取样再评候选——
	// 无样本 lane 回中性 1.0（置信度缩放同样回落），只拉已有观测
	// 的 lane 之间的相对差。
	medians := make([]time.Duration, len(lanes))
	counts := make([]int, len(lanes))
	minMedian := time.Duration(math.MaxInt64)
	for i, lane := range lanes {
		medians[i], counts[i] = lane.ttfbStats()
		if counts[i] > 0 && medians[i] > 0 && medians[i] < minMedian {
			minMedian = medians[i]
		}
	}
	candidates := make([]poolCandidate, 0, len(lanes))
	for i, lane := range lanes {
		v := lane.verdict(class)
		score := sha256.Sum256([]byte(affinity + "|" + lane.name))
		weight := laneWeight(v.expectedWait, medians[i], counts[i], minMedian)
		candidates = append(candidates, poolCandidate{
			lane:     lane,
			score:    score,
			key:      hrwKey(score, weight),
			weight:   weight,
			verdict:  v,
			bound:    lane == bound,
			pinned:   lane == pinned,
			priority: lane.priority.Load(),
		})
	}
	// 绑定让位判定——落点兄弟必须严格更健康才接得住让位（实测两类
	// 坏让位：搬上同满兄弟零容量增益，bind-on-open 还把谱系拖向新
	// 饱和 lane 驱动追饱和振荡；搬上同病兄弟吸收 p50≈5.1s 反而比
	// 留守病 bound 的 ~2.1s 更差——病态 lane 的 expectedWait 在满桶
	// 折算下系统性偏乐观，「账面更快」是假信号）。判病兄弟一律不接；
	// 健康兄弟按两条径判定：
	//   - 快照级：bound 判病（闩中/死区/桶满）时，兄弟期望排队落进
	//     让位快败阈值（gateEarlyRelease——与 gateYield 探针「兄弟
	//     此刻能更快放行吗」同一本账）且确实比 bound 快 → 让位。仅
	//     二进制 healthy 不算富余：1 槽余量加深队也报 healthy；
	//   - 期望排队级：bound 的 expectedWait 比最优兄弟高出一个 τ →
	//     让位回本档排序。闩剩余、桶满到下一窗、前队拥堵都已折算进
	//     同一本账；bound 自身等得短（闩将尽、队将排空）时维持粘性。
	// 无合格落点时绑定维持居首：全 lane 同病/同满时让位只是换地方
	// 排队，留守保住绑定连续性，等窗交给 lane 闸门自己仲裁——排队
	// 途中 gateYield 活探针仍会接住半途回春的兄弟。
	// 让位不解绑不动 bound 标记：它若按普通序仍最优照旧赢，换边开流
	// 成功后 bind 照常把谱系记到胜者 lane。
	if bi := slices.IndexFunc(candidates, func(c poolCandidate) bool { return c.bound }); bi >= 0 {
		bv := candidates[bi].verdict
		for i, c := range candidates {
			if i == bi {
				continue
			}
			sv := c.verdict
			if !sv.healthy {
				continue
			}
			yield := sv.expectedWait+gatePressureTau < bv.expectedWait ||
				(!bv.healthy && sv.expectedWait <= gateEarlyRelease && sv.expectedWait < bv.expectedWait)
			if yield {
				candidates[bi].yielded = true
				candidates[bi].yieldEW = sv.expectedWait
				break
			}
		}
	}
	slices.SortStableFunc(candidates, func(a, b poolCandidate) int {
		if a.boundLead() != b.boundLead() {
			if a.boundLead() {
				return -1
			}
			return 1
		}
		if a.pinned != b.pinned {
			if a.pinned {
				return -1
			}
			return 1
		}
		if a.verdict.bucket != b.verdict.bucket {
			return cmp.Compare(a.verdict.bucket, b.verdict.bucket)
		}
		if a.priority != b.priority {
			return cmp.Compare(b.priority, a.priority)
		}
		if a.key != b.key {
			return cmp.Compare(b.key, a.key)
		}
		return bytes.Compare(a.score[:], b.score[:])
	})
	return candidates
}

// detachedPeerRegistries 建 {lane 名→完成缓存} 全量登记表：跨 lane 挂接
// 探测的 ctx 载荷（withDetachedPeers）。登记表含本 lane——探测方按
// reg != adapter.detached 跳过自身，省去按调用点剔除的簿记。
func (pool *Pool) detachedPeerRegistries() map[string]*detachedRegistry {
	lanes := pool.snapshot()
	peers := make(map[string]*detachedRegistry, len(lanes))
	for _, lane := range lanes {
		peers[lane.name] = lane.adapter.detached
	}
	return peers
}

// gateYieldProbe 是一次 lane 尝试的「兄弟 lane 此刻能更快放行吗」
// 活探针：闸门预计排队超 gateEarlyRelease 时在锁外调用 eval，任一
// 剩余候选的 expectedWait 落进阈值即让位快败交给 failover。答真时
// eval 顺手把当时的 argmin 兄弟记入 target——>2 lane 时探针达标的
// 那条不一定是 rest[0]，failover 据此把落点提为首选（preferSibling），
// 省掉盲落首位再多烧一次入闸评估的自我纠正。rest 是克隆快照——谓词
// 可能活在泵协程上到 swap 已推进 rest，快照语义稳定免锁竞争；略陈旧
// 的候选集只让让位偏积极（换号目标若已不在 rest，preferSibling 原序
// 不动）。空候选集 attach 不挂接，闸门走原有排队语义。
type gateYieldProbe struct {
	class string
	rest  []*poolLane
	// target 是最近一次答真时算出的最优兄弟：只在 eval 答出 free 的
	// 当次写入——答真后闸门立即让位快败，所以 target 非空即本次尝试
	// 的死因是让位，failover 可安全按它重排。泵协程上的迟到 eval
	// 与 failover 侧的读并发交错，走原子指针。
	target atomic.Pointer[poolLane]
}

func newGateYieldProbe(class string, siblings []*poolLane) *gateYieldProbe {
	return &gateYieldProbe{class: class, rest: slices.Clone(siblings)}
}

// attach 把探针挂进 ctx；空候选集不挂接。
func (p *gateYieldProbe) attach(ctx context.Context) context.Context {
	if len(p.rest) == 0 {
		return ctx
	}
	return adapter.WithGateYield(ctx, p.eval)
}

// eval 求兄弟侧期望排队最小值（谓词本体——锁外求值，不得依赖调用方
// 持锁）；落进让位阈值时把 argmin 兄弟记入 target 作 failover 首选。
func (p *gateYieldProbe) eval() (time.Duration, bool) {
	minEW := time.Duration(math.MaxInt64)
	var best *poolLane
	for _, lane := range p.rest {
		if ew := lane.verdict(p.class).expectedWait; ew < minEW {
			minEW, best = ew, lane
		}
	}
	free := minEW <= gateEarlyRelease
	if free {
		p.target.Store(best)
	}
	return minEW, free
}

// preferSibling 把让位探针命中的兄弟提为换号首选：target 为 nil、
// 已不在剩余候选或已在首位时原序不动。移动发生时留一行落点依据，
// 否则「为什么跳过 rest[0]」只能靠当时各 lane 的 ew 反推。
func preferSibling(rest []*poolLane, target *poolLane, recorder *debuglog.Recorder) []*poolLane {
	if target == nil {
		return rest
	}
	i := slices.Index(rest, target)
	if i <= 0 {
		return rest
	}
	recorder.AppendJSONL(debuglog.StageDevinResponse, "yield_failover_target", map[string]any{"lane": target.name})
	reordered := make([]*poolLane, 0, len(rest))
	reordered = append(reordered, target)
	reordered = append(reordered, rest[:i]...)
	reordered = append(reordered, rest[i+1:]...)
	return reordered
}

// orderedLanes 是 rankLanes 的 lane 投影，供测试与只关心顺序的调用方使用。
func (pool *Pool) orderedLanes(ctx context.Context, lanes []*poolLane, affinity string) []*poolLane {
	ranked := pool.rankLanes(ctx, lanes, affinity)
	ordered := make([]*poolLane, len(ranked))
	for i, c := range ranked {
		ordered[i] = c.lane
	}
	return ordered
}

// laneWeight 算 lane 的选号健康权重 w ∈ (0,1]：
//   - w_pressure = 1/(1+expectedWait/τ)：本类请求进该 lane 闸门的期望
//     排队折成的衰减，τ≈10s——「预留挡几秒」与「睡到下一窗口」在权重
//     上有数量级区分，覆盖换号主因（本地闸门饱和，非 lane 失败）；
//   - w_ttfb = minMedian/laneMedian：滚动上游侧 TTFB 中位的相对惩罚，
//     按 n/50 线性置信度缩放——冷启动与小样本 lane 回中性 1.0，没观测
//     到慢的 lane 不被惩罚、没观测到的 lane 也不白捡便宜。
//
// minMedian<=0（全池无样本）时 w_ttfb 恒 1。
func laneWeight(expectedWait, median time.Duration, n int, minMedian time.Duration) float64 {
	w := 1.0 / (1.0 + expectedWait.Seconds()/gatePressureTau.Seconds())
	if minMedian > 0 && median > 0 {
		raw := float64(minMedian) / float64(median)
		w *= 1 + (raw-1)*min(1.0, float64(n)/ttfbConfidenceSamples)
	}
	return w
}

// hrwKey 是加权 rendezvous 键：u 取 score 高位投影到 (0,1]（1-均匀值
// 仍均匀），k=u^(1/w) 降序等价于按 w 加权的 rendezvous 抽取——首位命中
// 概率 ∝ w。取 1-u 而非 u 是为保旧序：w 相等时 k 降序 = score 字节序
// 升序，与加权前的钉选序逐位一致（确定性不变量），投影再平才落回
// score 字节序兜底。
func hrwKey(score [32]byte, w float64) float64 {
	u := 1 - float64(binary.BigEndian.Uint64(score[:8])>>11)*(1.0/(1<<53))
	return math.Pow(u, 1.0/w)
}

// laneVerdict 是 lane 一次评估的结论：healthy 是「立即可发」近似判定
// （无冷却、闸门未闩、分钟桶可发且未满），bucket 是健康档
// （0=绿 1=配额低 2=病），reasons 是降级归因词表（选号审计的
// PoolCandidate.Reason 来源），hardDown 是绑定判死词表——只含池侧
// 两档冷却，gate 状态不算（粘性区只被硬故障打破）。expectedWait 是
// 本类请求进该 lane 闸门的期望排队估计（选号压力权重输入）。
type laneVerdict struct {
	healthy      bool
	hardDown     bool
	bucket       int
	reasons      []string
	expectedWait time.Duration
}

// verdict 对 lane 做一次完整健康评估：降级原因按固定序叠加
// （auth_cooldown → generic_cooldown → gate_latched → gate_window_deadzone
// → gate_window_full → quota_low），调用方各取所需（排序取 bucket、审计取 reasons、
// healthy() 取 healthy、权重取 expectedWait）。class 决定闸门期望
// 排队按哪条准入轨估计；healthy()/state() 等只关心健康面的调用方
// 传 fg（默认视图——健康判定本身与类无关，expectedWait 才分轨）。
// quotaLow 不进 hardDown/病档——它是降权不是故障，配额低 lane 留在
// 健康档内降一级。
func (lane *poolLane) verdict(class string) laneVerdict {
	var v laneVerdict
	if lane.authCooldown() {
		v.hardDown = true
		v.reasons = append(v.reasons, "auth_cooldown")
	}
	if lane.genericCooldown() {
		v.hardDown = true
		v.reasons = append(v.reasons, "generic_cooldown")
	}
	snap := lane.adapter.gate.admissionSnapshot(class)
	v.expectedWait = snap.ExpectedWait
	if snap.Latched {
		v.reasons = append(v.reasons, "gate_latched")
	}
	// 死区与桶满分记：死区是窗界两侧的停发段（整形——同池 lane 同相
	// 判病，不含本 lane 容量信号），桶满是本 lane 配额真耗尽。死区内
	// 桶仍满时两词并存，审计据此区分整形态停发与真实饱和。
	windowDeadzone := snap.WindowQuota > 0 && !snap.Sendable
	windowSaturated := snap.WindowQuota > 0 && snap.WindowUsed >= snap.WindowQuota
	if windowDeadzone {
		v.reasons = append(v.reasons, "gate_window_deadzone")
	}
	if windowSaturated {
		v.reasons = append(v.reasons, "gate_window_full")
	}
	windowBlocked := windowDeadzone || windowSaturated
	v.healthy = !v.hardDown && !snap.Latched && !windowBlocked
	v.bucket = 2
	if v.healthy {
		v.bucket = 0
	}
	if lane.quotaLow.Load() {
		v.reasons = append(v.reasons, "quota_low")
		if v.healthy {
			v.bucket = 1
		}
	}
	return v
}

// hardDown 报告 lane 是否池侧硬故障（凭据失效冷却或非凭据冷却中）——
// 绑定判死词表；gate 闩/桶满不算，那是粘性区该等的整形态。
func (lane *poolLane) hardDown() bool {
	return lane.authCooldown() || lane.genericCooldown()
}

// healthy 报告 lane 当前是否「立即可发」：闸门未闩、分钟桶可发且未满、
// 不在任一档冷却。三者都是选中前一刻仍可能翻转的近似判定。
func (lane *poolLane) healthy() bool {
	return lane.verdict(adapter.ClassFG).healthy
}

// noteTTFB 记录一次上游侧 TTFB 样本（自本 lane 的 Stream 调用到首个
// 内容事件——含该 lane 自己的闸门排队，那正是用户成本）。样本进定容
// 环，写满覆最旧。
func (lane *poolLane) noteTTFB(d time.Duration) {
	lane.ttfbMu.Lock()
	if len(lane.ttfbRing) < ttfbSampleCap {
		lane.ttfbRing = append(lane.ttfbRing, d)
	} else {
		lane.ttfbRing[lane.ttfbHead] = d
		lane.ttfbHead = (lane.ttfbHead + 1) % ttfbSampleCap
	}
	lane.ttfbMu.Unlock()
}

// ttfbStats 返回滚动 TTFB 中位与当前样本量；无样本回 (0,0)。
func (lane *poolLane) ttfbStats() (median time.Duration, n int) {
	lane.ttfbMu.Lock()
	defer lane.ttfbMu.Unlock()
	n = len(lane.ttfbRing)
	if n == 0 {
		return 0, 0
	}
	sorted := slices.Clone(lane.ttfbRing)
	slices.Sort(sorted)
	return sorted[n/2], n
}

// genericCooldown 报告 lane 是否处于非凭据类失败的短冷却窗：到期自动
// 解封，无需任何凭据变化信号。
func (lane *poolLane) genericCooldown() bool {
	lane.authMu.Lock()
	defer lane.authMu.Unlock()
	return time.Now().Before(lane.unhealthyUntil)
}

// authCooldown 报告 lane 是否处于凭据失效冷却。标记只在「当前 token
// 仍是被判死的那份」期间生效：TokenSource 重读出不同凭据（CLI 续期、
// 配置文件被改）即视为新凭据就地解禁——同刻并发标死也不会卡着新
// token 不放行；badUntil 只是凭据源永不更新时的兜底。
func (lane *poolLane) authCooldown() bool {
	lane.authMu.Lock()
	defer lane.authMu.Unlock()
	if lane.badTokenHash == "" {
		return false
	}
	token := lane.adapter.currentToken()
	if time.Now().After(lane.badUntil) || (token != "" && tokenHash(token) != lane.badTokenHash) {
		lane.badTokenHash = ""
		lane.badUntil = time.Time{}
		// 解禁同步落盘：内存态与落盘态同生死——冷却自然到期或凭据
		// 换出后若不重写，重启会把已失效的判死键复活。重写而非删除：
		// failStreak 与 lastFailure 证据仍是有效簿记要留住。
		lane.persistCooldownLocked()
		return false
	}
	return true
}

// backoffDuration 算连败退避档：base×2^(streak-1) 封顶 max。
// streak<1 按首档计（恢复出的 0 值与新失败语义一致）。
func backoffDuration(base, max time.Duration, streak int) time.Duration {
	d := base
	for i := 1; i < streak; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return d
}

// noteFailure 在 lane 失败后更新池侧冷却簿记，两类豁免先进：
// Canceled 是请求方行为不是 lane 健康信号（failoverable 已挡主路径，
// 这里兜 ListModels 等直调路径），连证据都不记；LocalGate 是本地闸门
// 快败——未触达上游且 gate 自身已是惩罚，双冷却会把忙 lane 判死、害
// 绑定丢失，只记 lastFailure 证据不进冷却。
// 两档冷却都走连败退避：unauthenticated 走到这里意味着 lane 内自愈
// （reloadToken+重试）也没救回这份凭据，按当前 token 哈希记鉴权冷却
// （token 为空同样标记——tokenHash("") 作冷却键，凭据源补进真 token
// 哈希即变、自动解禁）；其余可换号失败记 generic 短冷却——限流类失败
// 带 "reset in N" 自述时对齐上游复位点（与 gate 闩同一本账），无自述
// 才用连败退避档。
// 复用窗口规则：新失败到达时旧冷却尚未到期 → streak 不升、只按当前档
// 延长——同一故障期的并发失败不连升档。后到失败只延长不缩短。
// 注意窗口：Stream 返回后才读 currentToken——同 lane 并发请求的自愈
// 恰好在这间隙换上新 token 时会把新 token 误标冷却；最长一个退避档或
// 下一次轮换自愈，可接受。
func (lane *poolLane) noteFailure(err error) {
	failure := llm.Classify(err)
	if failure == nil || failure.Canceled {
		return
	}
	lane.authMu.Lock()
	defer lane.authMu.Unlock()
	lane.lastFailureAt = time.Now()
	lane.lastFailureCode = failure.Code
	lane.lastFailureMessage = failure.Message
	if len(lane.lastFailureMessage) > 300 {
		lane.lastFailureMessage = lane.lastFailureMessage[:300]
	}
	if failure.LocalGate {
		lane.persistCooldownLocked()
		return
	}
	now := time.Now()
	// 落债时刻：以下两档分支都会写实债（LocalGate 已在上方早退，
	// 只记证据不动此钟）。清账门槛以它区分「债后探针」与「在飞陈旧」。
	lane.debtSetAt = now
	if failure.Code == "unauthenticated" {
		lane.badTokenHash = tokenHash(lane.adapter.currentToken())
		if !now.Before(lane.badUntil) {
			// 旧窗已过期：这是一次新故障期，连败升档。
			lane.failStreak++
		}
		if until := now.Add(backoffDuration(badTokenCooldown, badTokenCooldownMax, lane.failStreak)); until.After(lane.badUntil) {
			lane.badUntil = until
		}
		lane.persistCooldownLocked()
		return
	}
	if !now.Before(lane.unhealthyUntil) {
		lane.failStreak++
	}
	until := now.Add(backoffDuration(genericLaneCooldown, genericLaneCooldownMax, lane.failStreak))
	if failure.RateLimited {
		// 上游限流自述复位点是最优恢复点估计（分钟 hint 已被
		// RateLimitReset 对齐 :59 桶界），用它替代固定退避档——
		// 否则 90s 基档比上游真实复位（实测 18-40s）多压 ~30-60s，
		// 与 gate 闩同一报文两本账。无 hint 保持退避兜底。
		if resetAt, ok := failure.RateLimitReset(now); ok {
			until = resetAt
		}
	}
	if until.After(lane.unhealthyUntil) {
		lane.unhealthyUntil = until
	}
	lane.persistCooldownLocked()
}

// noteSuccess 在 lane 产出内容后清账：连败归零、两档冷却与判死键一并
// 清掉——真恢复不需要等冷却自然到期。清账门槛是「证据新于债」：
// laneStart（本尝试选定 lane 的发送时刻）必须晚于最近一次落债时刻
// debtSetAt；早于它的成功来自债设立前已发出的在飞请求，证明的是故障
// 前 lane 能发而非故障后恢复，账目原样保留等真探针或自然到期。持久行
// 同步删除走同一条件：成功已证 lane 可用，重启后不该复活一笔已被清掉
// 的旧账。常态路径（本就无账，debtSetAt 零值恒过闸）是纯内存快路径，
// 不碰状态库。
func (lane *poolLane) noteSuccess(laneStart time.Time) {
	lane.authMu.Lock()
	if !laneStart.After(lane.debtSetAt) {
		lane.authMu.Unlock()
		return
	}
	settled := lane.failStreak > 0 || lane.badTokenHash != "" ||
		!lane.badUntil.IsZero() || !lane.unhealthyUntil.IsZero()
	lane.failStreak = 0
	lane.badTokenHash = ""
	lane.badUntil = time.Time{}
	lane.unhealthyUntil = time.Time{}
	lane.debtSetAt = time.Time{}
	lane.authMu.Unlock()
	if settled {
		lane.deleteCooldownState()
	}
}

// poolCooldownKey 是池侧冷却在 runtime_state 里的键名约定：poolcool:<name>。
func poolCooldownKey(lane string) string {
	return "poolcool:" + lane
}

// poolCooldownState 是池侧冷却的持久化形态（runtime_state 的值 JSON）：
// 重启后死 token lane 不该立即再吃一轮真实流量。
type poolCooldownState struct {
	BadTokenHash       string `json:"bad_token_hash"`
	BadUntilMS         int64  `json:"bad_until_ms"`
	UnhealthyUntilMS   int64  `json:"unhealthy_until_ms"`
	FailStreak         int    `json:"fail_streak"`
	LastFailureAtMS    int64  `json:"last_failure_at_ms"`
	LastFailureCode    string `json:"last_failure_code"`
	LastFailureMessage string `json:"last_failure_message"`
}

// persistCooldownLocked 把冷却簿记写进 runtime_state；须在 authMu 下
// 调用——与 ClearCooldown/noteSuccess 的删除同锁序化，否则「删后写回」
// 交错会把已清掉的冷却复活成幽灵行。写失败只记日志，不挡请求路径。
func (lane *poolLane) persistCooldownLocked() {
	if lane.states == nil {
		return
	}
	data, _ := json.Marshal(poolCooldownState{
		BadTokenHash:       lane.badTokenHash,
		BadUntilMS:         unixMilliOrZero(lane.badUntil),
		UnhealthyUntilMS:   unixMilliOrZero(lane.unhealthyUntil),
		FailStreak:         lane.failStreak,
		LastFailureAtMS:    unixMilliOrZero(lane.lastFailureAt),
		LastFailureCode:    lane.lastFailureCode,
		LastFailureMessage: lane.lastFailureMessage,
	})
	ctx, cancel := context.WithTimeout(context.Background(), lockedStateStoreTimeout)
	defer cancel()
	if err := lane.states.SetState(ctx, poolCooldownKey(lane.name), string(data)); err != nil {
		slog.Warn("pool cooldown state persist failed", "account", lane.name, "error", err)
	}
}

// deleteCooldownState 删除持久化冷却行；行不存在不算错误。
func (lane *poolLane) deleteCooldownState() {
	if lane.states == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockedStateStoreTimeout)
	defer cancel()
	if err := lane.states.DeleteState(ctx, poolCooldownKey(lane.name)); err != nil {
		slog.Warn("pool cooldown state delete failed", "account", lane.name, "error", err)
	}
}

// restoreCooldown 在 lane 构建时从 runtime_state 装载冷却簿记：
// 未过期的冷却原样恢复（过期的由 genericCooldown/authCooldown 的惰性
// 判定自然失效，badTokenHash 的凭据换出解禁语义不变）；行缺失或损坏
// 静默按无冷却处理——持久化是防重启续判的保险，不阻塞建 lane。
func (lane *poolLane) restoreCooldown() {
	if lane.states == nil {
		return
	}
	value, ok, err := lane.states.GetState(context.Background(), poolCooldownKey(lane.name))
	if err != nil || !ok {
		return
	}
	var state poolCooldownState
	if err := json.Unmarshal([]byte(value), &state); err != nil {
		return
	}
	lane.badTokenHash = state.BadTokenHash
	lane.badUntil = milliTime(state.BadUntilMS)
	lane.unhealthyUntil = milliTime(state.UnhealthyUntilMS)
	lane.failStreak = state.FailStreak
	lane.lastFailureAt = milliTime(state.LastFailureAtMS)
	lane.lastFailureCode = state.LastFailureCode
	lane.lastFailureMessage = state.LastFailureMessage
}

// unixMilliOrZero 把零值时刻落成 0 而不是远古负毫秒——JSON 里 0
// 与「未设置」同读。
func unixMilliOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// milliTime 是 unixMilliOrZero 的逆读：0 回零值。
func milliTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// affinityTTL 读全局会话绑定 TTL（首 lane config——全局字段各 lane
// 一致）；未配置回落默认 1h。
func (pool *Pool) affinityTTL() time.Duration {
	if seconds := pool.CurrentConfig().SessionAffinityTTLSeconds; seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultAffinityTTL
}

// boundLane 取亲和键的绑定 lane：命中即滑动续期返回；绑定已过期或
// lane 硬故障（池侧两档冷却——gate 闩/桶满不算）时删绑返回 nil，
// 调用方按普通序重选。
func (pool *Pool) boundLane(affinity string) *poolLane {
	pool.bindingsMu.Lock()
	defer pool.bindingsMu.Unlock()
	binding, ok := pool.bindings[affinity]
	if !ok {
		return nil
	}
	now := time.Now()
	if now.After(binding.expiry) || binding.lane.hardDown() {
		delete(pool.bindings, affinity)
		return nil
	}
	binding.expiry = now.Add(pool.affinityTTL())
	pool.bindings[affinity] = binding
	return binding.lane
}

// bind 把亲和键绑到 lane：开流成功与换号接管是仅有的两个写点——
// 绑定记录的是「上次产出内容的 lane」。lane 已硬故障（两档冷却）
// 时整段跳过：那是判死前已选中它的在飞开流迟报，写绑会把会话钉回
// 死 lane，下个请求 boundLane 删绑再重绑、来回拍翅；suspect 标脏
// 同样免——死 lane 的迟报不该动兄弟 lane 的保温条目，会话下次请求
// 按普通序重绑。容量触顶先扫过期再逐最早到期者；被逐会话下次请求
// 按普通序重选重绑。落点即谱系归属：会话流量只会再经过本 lane，
// 其余 lane 上同 SessionKey 的保温条目已成跨 lane 孤儿，顺手标
// suspect 让它们走宽限退役而非骑满 maxIdle 白烧 ping（对稳态重绑
// 是无害复读——别 lane 的同会话条目本来就只能是陈旧孤儿）。
func (pool *Pool) bind(affinity string, lane *poolLane, sessionKey string) {
	if lane.hardDown() {
		return
	}
	pool.bindingsMu.Lock()
	if _, ok := pool.bindings[affinity]; !ok && len(pool.bindings) >= poolBindingCap {
		now := time.Now()
		for key, binding := range pool.bindings {
			if now.After(binding.expiry) {
				delete(pool.bindings, key)
			}
		}
		if len(pool.bindings) >= poolBindingCap {
			var oldestKey string
			var oldest time.Time
			for key, binding := range pool.bindings {
				if oldestKey == "" || binding.expiry.Before(oldest) {
					oldestKey, oldest = key, binding.expiry
				}
			}
			delete(pool.bindings, oldestKey)
		}
	}
	pool.bindings[affinity] = laneBinding{lane: lane, expiry: time.Now().Add(pool.affinityTTL())}
	pool.bindingsMu.Unlock()
	for _, other := range pool.snapshot() {
		if other != lane {
			other.adapter.warm.suspectSession(sessionKey)
		}
	}
}

// unbindLane 清掉一条 lane 的全部绑定与在飞指派：lane 被摘除
// （ApplyConfigs 差集）时调用，避免绑定/钉选指向已不在快照里的死
// lane。在飞条目删掉后持 pin 的请求照旧释放（release 对缺失条目
// 空操作），其 setLane 不再重建——摘除的 lane 不该再吸新流量。
func (pool *Pool) unbindLane(lane *poolLane) {
	pool.bindingsMu.Lock()
	for key, binding := range pool.bindings {
		if binding.lane == lane {
			delete(pool.bindings, key)
		}
	}
	pool.bindingsMu.Unlock()
	pool.inflightMu.Lock()
	for key, entry := range pool.inflight {
		if entry.lane == lane {
			delete(pool.inflight, key)
		}
	}
	pool.inflightMu.Unlock()
}

// inflightLane 返回亲和键的在飞钉选 lane；无条目或 lane 已硬故障
// （池侧两档冷却）按无钉选处理——持条目的在飞请求正走在 failover
// 或终局路径上，会自行改派/释放，读侧只不引用它。
func (pool *Pool) inflightLane(affinity string) *poolLane {
	pool.inflightMu.Lock()
	defer pool.inflightMu.Unlock()
	entry := pool.inflight[affinity]
	if entry == nil || entry.lane == nil || entry.lane.hardDown() {
		return nil
	}
	return entry.lane
}

// inflightAcquire 为一次在飞请求登记钉选名额：同键首个请求建条目，
// 后来者只加计数——并发同键请求共享条目，lane 随最新指派走。
func (pool *Pool) inflightAcquire(affinity string) *inflightPin {
	pool.inflightMu.Lock()
	entry := pool.inflight[affinity]
	if entry == nil {
		entry = &inflightEntry{}
		pool.inflight[affinity] = entry
	}
	entry.count++
	pool.inflightMu.Unlock()
	return &inflightPin{pool: pool, affinity: affinity}
}

// setLane 更新本请求所在 lane：选号循环每次尝试前调用，failover
// 改派即改记——在飞钉选跟踪的是「前驱此刻在哪条 lane」。
func (pin *inflightPin) setLane(lane *poolLane) {
	pin.pool.inflightMu.Lock()
	if entry := pin.pool.inflight[pin.affinity]; entry != nil {
		entry.lane = lane
	}
	pin.pool.inflightMu.Unlock()
}

// release 归还在飞计数：归零删条目，同键后继回到分数序/绑定语义。
// 条目可能已被 unbindLane 摘掉——缺失时空操作。
func (pin *inflightPin) release() {
	pin.pool.inflightMu.Lock()
	if entry := pin.pool.inflight[pin.affinity]; entry != nil {
		entry.count--
		if entry.count <= 0 {
			delete(pin.pool.inflight, pin.affinity)
		}
	}
	pin.pool.inflightMu.Unlock()
}

// boundSessionCounts 统计各 lane 当前绑定的活会话数（未过期条目计数），
// 顺手惰性清掉过期行——读路径顺带收账，不靠专职清扫协程。
func (pool *Pool) boundSessionCounts() map[*poolLane]int {
	pool.bindingsMu.Lock()
	defer pool.bindingsMu.Unlock()
	counts := make(map[*poolLane]int, len(pool.bindings))
	now := time.Now()
	for key, binding := range pool.bindings {
		if now.After(binding.expiry) {
			delete(pool.bindings, key)
			continue
		}
		counts[binding.lane]++
	}
	return counts
}

// state 读 lane 的池侧状态快照。先跑 healthy()——它内部的
// authCooldown 会把「凭据已换出」的死标惰性清掉，之后读到的
// badUntil 才是真实生效的冷却窗。
func (lane *poolLane) state() LaneState {
	s := LaneState{Healthy: lane.healthy()}
	lane.authMu.Lock()
	defer lane.authMu.Unlock()
	now := time.Now()
	if lane.badTokenHash != "" && now.Before(lane.badUntil) {
		until := lane.badUntil
		s.AuthCooldownUntil = &until
	}
	if now.Before(lane.unhealthyUntil) {
		until := lane.unhealthyUntil
		s.UnhealthyUntil = &until
	}
	s.FailStreak = lane.failStreak
	if !lane.lastFailureAt.IsZero() {
		at := lane.lastFailureAt
		s.LastFailureAt = &at
		s.LastFailureCode = lane.lastFailureCode
		s.LastFailureMessage = lane.lastFailureMessage
	}
	return s
}

// tokenHash 是凭据的冷却判等键：不存原文，哈希足够区分「换没换」。
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// failoverable 判定 lane.Stream 的失败能否换号重试，判据围绕「换号
// 能不能改变结果」：客户端取消与确定性的请求形状错误不能（换号只会
// 复现同一拒绝）；限流、传输断裂、凭据与权限问题都能（两号配额/
// seat/凭据各自独立）；未知错误兜底允许——lane 数很小，多试一次的
// 代价是一条上游调用。
func failoverable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	failure := llm.Classify(err)
	if failure == nil || failure.Canceled {
		return false
	}
	if failure.LocalGate || failure.RateLimited || failure.UpstreamFault {
		return true
	}
	// 无 code 的错误是本地确定性失败（请求投影/参数校验在触达上游
	// 之前炸）——换号只会逐 lane 复现同一拒绝，还会把每条 lane 都
	// 打上非凭据冷却。传输断裂类无 code 错误已被 UpstreamFault 接走。
	if failure.Code == "" {
		return false
	}
	switch failure.Code {
	case "unauthenticated", "permission_denied":
		return true
	}
	return !failure.ClientFixable
}

// ListModels 返回首个健康 lane 的目录：各号 seat/套餐可不同，目录本来
// 就是每 lane 各自缓存的；全失败回最后一个错误（与单号语义一致）。
func (pool *Pool) ListModels(ctx context.Context) ([]adapter.ModelInfo, error) {
	lanes := pool.snapshot()
	if len(lanes) == 0 {
		return nil, errNoUpstreamAccounts()
	}
	ordered := make([]*poolLane, 0, len(lanes))
	var unhealthy []*poolLane
	for _, lane := range lanes {
		// 一趟分区：healthy() 评两次的话两次之间翻转会让 lane
		// 被试两次或完全跳过。
		if lane.healthy() {
			ordered = append(ordered, lane)
		} else {
			unhealthy = append(unhealthy, lane)
		}
	}
	ordered = append(ordered, unhealthy...)
	var lastErr error
	for _, lane := range ordered {
		laneStart := time.Now()
		models, err := lane.adapter.ListModels(ctx)
		if err == nil {
			lane.noteSuccess(laneStart)
			return models, nil
		}
		lastErr = err
		lane.noteFailure(err)
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// ApplyConfigs 热应用账号集合，diff 键是 lane 名：同名 lane 复用旧
// adapter 走 ApplyConfig（token 与 TokenSource 都是热换值字段，warm
// 谱系/assignment/目录缓存与在途流全保住）；新增 lane 先构建再入列；
// 被删 lane 摘出后异步 Close（只停后台协程，在途流持引用跑完——与
// endpoint 换绑同一生死模型）。空集合法，等同全部 lane 被删。任一
// 构建/应用失败即返回错误，已应用的 lane 不回滚——与单 lane
// ApplyConfig 的失败语义一致。
func (pool *Pool) ApplyConfigs(configs []Config) ([]string, error) {
	old := pool.snapshot()
	byName := make(map[string]*poolLane, len(old))
	for _, lane := range old {
		byName[lane.name] = lane
	}
	appliedSet := map[string]bool{}
	next := make([]*poolLane, 0, len(configs))
	var built []*poolLane
	for _, config := range configs {
		if lane, ok := byName[config.Identity.Name]; ok {
			applied, err := lane.adapter.ApplyConfig(config)
			if err != nil {
				for _, lane := range built {
					lane.adapter.Close()
				}
				return nil, fmt.Errorf("devin account %q: %w", config.Identity.Name, err)
			}
			for _, field := range applied {
				appliedSet[field] = true
			}
			// priority 是池级元数据不进 adapter 的 applied 差集——
			// 复用 lane 原地换值即生效。
			lane.priority.Store(int32(config.Priority))
			next = append(next, lane)
			continue
		}
		lane, err := newPoolLane(config)
		if err != nil {
			for _, lane := range built {
				lane.adapter.Close()
			}
			return nil, err
		}
		built = append(built, lane)
		next = append(next, lane)
	}
	kept := make(map[*poolLane]bool, len(next))
	for _, lane := range next {
		kept[lane] = true
	}
	pool.lanes.Store(&next)
	for _, lane := range old {
		if !kept[lane] {
			go lane.adapter.Close()
			// 摘除 lane 的会话绑定一并清——绑定指向已不在快照里的
			// 死 lane 会把会话钉在不再存在的 lane 上。
			pool.unbindLane(lane)
		}
	}
	applied := make([]string, 0, len(appliedSet))
	for field := range appliedSet {
		applied = append(applied, field)
	}
	slices.Sort(applied)
	return applied, nil
}

// UpdateConfig 把同一 mutate 应用到每条 lane（全局字段语义不变）；
// applied 取各 lane 差集并集。lane 身份在 mutate 后整组恢复——面板
// 契约只改全局字段，护栏按子结构一次赋回，挡住误写把同一身份值铺到
// 全部 lane；今后 Identity 新增字段自动进护栏，无需维护字段清单。
func (pool *Pool) UpdateConfig(mutate func(*Config) error) ([]string, error) {
	appliedSet := map[string]bool{}
	for _, lane := range pool.snapshot() {
		prev := lane.adapter.CurrentConfig()
		applied, err := lane.adapter.UpdateConfig(func(cfg *Config) error {
			if err := mutate(cfg); err != nil {
				return err
			}
			cfg.Identity = prev.Identity
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, field := range applied {
			appliedSet[field] = true
		}
	}
	applied := make([]string, 0, len(appliedSet))
	for field := range appliedSet {
		applied = append(applied, field)
	}
	slices.Sort(applied)
	return applied, nil
}

// Close 停掉全部 lane 的后台资源。
func (pool *Pool) Close() {
	for _, lane := range pool.snapshot() {
		lane.adapter.Close()
	}
}

// BeginDrain 向全部 lane 透传排空起点（各停各的保温调度）。
func (pool *Pool) BeginDrain() {
	for _, lane := range pool.snapshot() {
		lane.adapter.BeginDrain()
	}
}

// TokenFunc 返回「首 lane 当前凭据」的读取函数：每次求值重解析
// firstLane——热更摘掉首号或模式切换后，面板 seat/状态类调用
// 落到当前首 lane 而不是已关闭 lane 的冻结 token。要按号取凭据
// 用 TokenFuncs。空池求值回 ""（面板 seat 调用发空 Bearer 拿 401，
// 是「号还没配」的引导态）。
func (pool *Pool) TokenFunc() func() string {
	return func() string {
		if lane := pool.firstLane(); lane != nil {
			return lane.adapter.TokenFunc()()
		}
		return ""
	}
}

// TokenFuncs 返回各 lane 的凭据读取函数（按账号名索引）：
// recentTokens 脱敏环必须收编全部 lane 的当前 token——漏遮任一号的
// 凭据都是日志泄露。
func (pool *Pool) TokenFuncs() map[string]func() string {
	lanes := pool.snapshot()
	funcs := make(map[string]func() string, len(lanes))
	for _, lane := range lanes {
		funcs[lane.name] = lane.adapter.TokenFunc()
	}
	return funcs
}

// GateStats 返回首 lane 闸门快照（顶层 gate 段的后兼容形态）；空池回零值。
func (pool *Pool) GateStats() GateStats {
	if lane := pool.firstLane(); lane != nil {
		return lane.adapter.GateStats()
	}
	return GateStats{}
}

// WarmStats 返回首 lane 保温快照（顶层 warm 段的后兼容形态）；空池回零值。
func (pool *Pool) WarmStats() WarmStats {
	if lane := pool.firstLane(); lane != nil {
		return lane.adapter.WarmStats()
	}
	return WarmStats{}
}

// AccountGateStats 返回各 lane 的闸门快照（按账号名索引），
// /admin/runtime-metrics 的 accounts 段透出。
func (pool *Pool) AccountGateStats() map[string]GateStats {
	lanes := pool.snapshot()
	stats := make(map[string]GateStats, len(lanes))
	for _, lane := range lanes {
		stats[lane.name] = lane.adapter.GateStats()
	}
	return stats
}

// AccountWarmStats 返回各 lane 的保温快照（按账号名索引）。
func (pool *Pool) AccountWarmStats() map[string]WarmStats {
	lanes := pool.snapshot()
	stats := make(map[string]WarmStats, len(lanes))
	for _, lane := range lanes {
		stats[lane.name] = lane.adapter.WarmStats()
	}
	return stats
}

// DetachedStats 返回全 lane 聚合的脱钩完成缓存快照（顶层 detached
// 段：计数逐 lane 求和、事件环按时刻归并——脱钩簿记全是可加口径，
// 与 gate/warm 的闩态/分位数不同，没有不可聚合字段）；空池回零值。
// 逐号视图见 AccountDetachedStats。
func (pool *Pool) DetachedStats() DetachedStats {
	return mergeDetachedStats(pool.AccountDetachedStats())
}

// AccountDetachedStats 返回各 lane 的脱钩完成缓存快照（按账号名索引），
// /admin/runtime-metrics 的 accounts.<name>.detached 组透出——缓存
// per-lane，跨 lane 重试恒 miss，孤儿/attach 率必须逐号看。
func (pool *Pool) AccountDetachedStats() map[string]DetachedStats {
	lanes := pool.snapshot()
	stats := make(map[string]DetachedStats, len(lanes))
	for _, lane := range lanes {
		stats[lane.name] = lane.adapter.DetachedStats()
	}
	return stats
}

// EvictDetachedByOriginDir 逐 lane 按来源调试目录清脱钩条目（面板 abort
// 补刀）：条目归属 lane 由选号决定、abort 侧不可预知，按
// AccountDetachedStats 同型 fan-out 全池调用，不匹配 lane 上是空操作。
func (pool *Pool) EvictDetachedByOriginDir(dir string) {
	for _, lane := range pool.snapshot() {
		lane.adapter.EvictDetachedByOriginDir(dir)
	}
}

// AccountLaneStates 返回各 lane 的池侧状态快照（按账号名索引），
// /admin/runtime-metrics 的 accounts.<name>.lane 组透出。
func (pool *Pool) AccountLaneStates() map[string]LaneState {
	lanes := pool.snapshot()
	boundCounts := pool.boundSessionCounts()
	states := make(map[string]LaneState, len(lanes))
	for _, lane := range lanes {
		state := lane.state()
		state.BoundSessions = boundCounts[lane]
		states[lane.name] = state
	}
	return states
}

// Aliases 返回模型别名映射：全局字段各 lane 一致，取首 lane；空池回 nil。
func (pool *Pool) Aliases() map[string]string {
	if lane := pool.firstLane(); lane != nil {
		return lane.adapter.Aliases()
	}
	return nil
}

// CurrentConfig 返回首 lane 的配置快照：Endpoint 与 tuning 类全局字段
// 各 lane 一致；Identity 是该 lane 自己的值，调用方展示用要意识到这点
// （面板 MVP 绑首号，语义恰好正确）。空池回零值 Config——消费侧
// （settings 默认值兜底、/admin/config 视图）把它当「未配置」基线处理。
func (pool *Pool) CurrentConfig() Config {
	if lane := pool.firstLane(); lane != nil {
		return lane.adapter.CurrentConfig()
	}
	return Config{}
}

// errNoUpstreamAccounts 是空池（含「全 lane 被 disabled/墓碑摘出
// 生效集」的同形态）的统一失败：没有可触达的上游。非 ClientFixable——
// /v1 侧映射 5xx。每次新建实例：Failure 惯例上不共享可复用对象。
func errNoUpstreamAccounts() *llm.Failure {
	return &llm.Failure{Code: "unavailable", Message: "no upstream accounts configured"}
}

// ClearCooldown 清该名 lane 的池侧冷却并立即回候选：两档冷却窗
// （badUntil 凭据冷却 + unhealthyUntil 短冷却）连同 badTokenHash 判死键
// 与连败计数一并清掉——语义是人工宣布「已处理，回候选」，token 若仍坏
// 会在下一次 unauthenticated 重新进冷却。持久行同步删除。保留
// lastFailure* 证据，不动 gate 闩（上游推导的真值，本地无权清）。
// 无该名活 lane 返 false。
func (pool *Pool) ClearCooldown(name string) bool {
	for _, lane := range pool.snapshot() {
		if lane.name != name {
			continue
		}
		lane.authMu.Lock()
		lane.badTokenHash = ""
		lane.badUntil = time.Time{}
		lane.unhealthyUntil = time.Time{}
		lane.failStreak = 0
		lane.debtSetAt = time.Time{}
		lane.authMu.Unlock()
		lane.deleteCooldownState()
		return true
	}
	return false
}

// NoteQuotaSample 按配额采样刷新该名 lane 的降权标记：weekly 剩余
// 百分比低于阈值（QuotaLowThresholdPercent，0→默认 15，负值关闭）
// 时 quotaLow 置位——排序把 lane 降入「健康但配额低」档，只影响新
// 会话落点，已绑定会话不受影响（绑定命中恒居首）。无该名 lane 忽略。
// boot 不补种：下个采样周期生效即可。
func (pool *Pool) NoteQuotaSample(name string, dailyRemainingPct, weeklyRemainingPct float64) {
	threshold := pool.CurrentConfig().QuotaLowThresholdPercent
	if threshold == 0 {
		threshold = defaultQuotaLowThresholdPercent
	}
	low := threshold > 0 && weeklyRemainingPct < float64(threshold)
	for _, lane := range pool.snapshot() {
		if lane.name == name {
			lane.quotaLow.Store(low)
			return
		}
	}
}
