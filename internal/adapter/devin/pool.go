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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
)

// badTokenCooldown 是凭据失效的池侧冷却时长：lane 内自愈也救不回的
// unauthenticated 把当前 token 标记这么久；期间若 TokenSource 重读出
// 不同凭据会提前解禁（见 poolLane.authCooldown），它只是凭据源永远
// 不更新时的兜底解封点。
const badTokenCooldown = 10 * time.Minute

// genericLaneCooldown 是非凭据类失败的短冷却：permission_denied/
// UpstreamFault/未知错误换号能改变结果但不足以判死凭据——没有这段
// 冷却，钉选到惯犯 lane 的会话每个请求都先烧一次注定失败的上游
// 调用再换号；冷却只压到第二档不剔除，到期自然重试。
const genericLaneCooldown = 90 * time.Second

// Pool 是多账号上游池，实现 adapter.Adapter。
type Pool struct {
	// lanes 是当前 lane 集合快照；账号集合热更（ApplyConfigs）整体换
	// 指针，读侧无锁。不变式：非空。
	lanes atomic.Pointer[[]*poolLane]
}

// poolLane 是池里的一条账号泳道：adapter 承载该号全部运行时状态，
// badToken* 是池侧加的凭据失效冷却（lane 内自愈失败后由 noteFailure
// 标记，authCooldown 惰性解禁），lastFailure* 是最近一次换号失败的
// 归因（冷却期外也保留——冷却只压重试，失败史是排障证据），authMu
// 保护这四组字段。
type poolLane struct {
	name    string
	adapter *Adapter

	authMu       sync.Mutex
	badTokenHash string
	badUntil     time.Time
	// unhealthyUntil 是非凭据类失败的短冷却截止（genericLaneCooldown）；
	// 与 badToken 冷却不同键：不看 token 换没换，到点自然解封。
	unhealthyUntil     time.Time
	lastFailureAt      time.Time
	lastFailureCode    string
	lastFailureMessage string
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
	// LastFailure* 是最近一次换号失败的归因。
	LastFailureAt      *time.Time `json:"last_failure_at,omitempty"`
	LastFailureCode    string     `json:"last_failure_code,omitempty"`
	LastFailureMessage string     `json:"last_failure_message,omitempty"`
}

var _ adapter.Adapter = (*Pool)(nil)

// NewPool 按配置逐条构建 lane；任一失败时回收已建 lane 整体报错
// （启动期 fail-fast，不留半初始化池）。configs 至少一条。
func NewPool(configs []Config) (*Pool, error) {
	if len(configs) == 0 {
		return nil, errors.New("devin pool requires at least one account")
	}
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
	pool := &Pool{}
	pool.lanes.Store(&lanes)
	return pool, nil
}

func newPoolLane(config Config) (*poolLane, error) {
	laneAdapter, err := New(config)
	if err != nil {
		return nil, fmt.Errorf("devin account %q: %w", config.Name, err)
	}
	return &poolLane{name: config.Name, adapter: laneAdapter}, nil
}

// snapshot 返回当前 lane 集合；NewPool 之后恒非空。
func (pool *Pool) snapshot() []*poolLane {
	if lanes := pool.lanes.Load(); lanes != nil {
		return *lanes
	}
	return nil
}

// firstLane 返回配置序首 lane：别名/客户端指纹/闸门参数等全局字段各
// lane 一致，面板 MVP 也固定绑首号——逐号展示是阶段 2 的事。
func (pool *Pool) firstLane() *poolLane {
	return pool.snapshot()[0]
}

// Stream 按亲和键钉选 lane 发起请求，失败按 failoverable 词表换号。
// 开流级换号发生在本函数内；开流成功后返回 poolStream，由它在 Recv
// 里处理流内 error 事件的 pre-content 换号（死 token 的 unauthenticated
// 以首帧到达，lane.Stream 已经返回成功，只能在这一级拦截）。
// account 归因记「产出终局结果的 lane」：成功开流、终审拒绝、failover
// 穷尽都算——失败请求同样有「哪号拒的我」的答案。被试过又放弃的 lane
// 明细在 NoteAccountAttempt。
func (pool *Pool) Stream(ctx context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	lanes := pool.snapshot()
	recorder := debuglog.FromContext(ctx)
	if len(lanes) == 1 {
		lane := lanes[0]
		stream, err := lane.adapter.Stream(ctx, request)
		recorder.SetUpstreamAccount(lane.name)
		return stream, err
	}
	rest := pool.orderedLanes(lanes, SessionAffinityKey(request))
	var lastErr error
	for len(rest) > 0 {
		lane := rest[0]
		rest = rest[1:]
		// 04 里插账号分界行：各 lane 的上游帧直接续写同一文件，
		// 没有分界行无法区分一段帧属于哪号。
		recorder.AppendJSONL(debuglog.StageDevinResponse, "account_attempt", map[string]any{"account": lane.name})
		stream, err := lane.adapter.Stream(ctx, request)
		if err == nil {
			recorder.SetUpstreamAccount(lane.name)
			return &poolStream{request: request, recorder: recorder, lane: lane, inner: stream, rest: rest}, nil
		}
		lastErr = err
		recorder.SetUpstreamAccount(lane.name)
		if !failoverable(ctx, err) {
			return nil, err
		}
		recorder.NoteAccountAttempt(lane.name, err)
		lane.noteFailure(err)
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
	lane     *poolLane
	inner    llm.ResponseStream
	// rest 是尚未尝试的候选 lane（钉选序尾部）；每条只在换号时试一次。
	rest []*poolLane
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
		// 被吞掉的换号前错误事件也记入 05：pump 只记录 Recv 返回的
		// 事件，拦截下来的要由这里补登，否则失败 lane 的死因在事件
		// 流里无迹可查（error.json 是 first-write-wins 已有其一）。
		s.recorder.RecordResponseEvent(event)
		opened, lastErr := s.swap(ctx)
		if !opened {
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
		next := s.rest[0]
		s.rest = s.rest[1:]
		s.recorder.AppendJSONL(debuglog.StageDevinResponse, "account_attempt", map[string]any{"account": next.name})
		inner, err := next.adapter.Stream(ctx, s.request)
		if err == nil {
			s.lane = next
			s.inner = inner
			s.recorder.SetUpstreamAccount(next.name)
			return true, nil
		}
		lastErr = err
		s.recorder.SetUpstreamAccount(next.name)
		if !failoverable(ctx, err) {
			return false, err
		}
		s.recorder.NoteAccountAttempt(next.name, err)
		next.noteFailure(err)
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

// orderedLanes 给出候选序：rendezvous 分数（sha256(亲和键|lane 名)）
// 决定档内顺序——同亲和键恒得同序，即钉选；健康档整体排在不健康档
// 之前。不健康不剔除只排后：判定是近似快照，全不健康时仍回钉选序，
// 由 lane 闸门自己走 wait/快败（客户端拿 Retry-After，与单号一致）。
func (pool *Pool) orderedLanes(lanes []*poolLane, affinity string) []*poolLane {
	type candidate struct {
		lane    *poolLane
		score   [32]byte
		healthy bool
	}
	candidates := make([]candidate, 0, len(lanes))
	for _, lane := range lanes {
		candidates = append(candidates, candidate{
			lane:    lane,
			score:   sha256.Sum256([]byte(affinity + "|" + lane.name)),
			healthy: lane.healthy(),
		})
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int {
		if a.healthy != b.healthy {
			if a.healthy {
				return -1
			}
			return 1
		}
		return bytes.Compare(a.score[:], b.score[:])
	})
	ordered := make([]*poolLane, len(candidates))
	for i, c := range candidates {
		ordered[i] = c.lane
	}
	return ordered
}

// healthy 报告 lane 当前是否「立即可发」：闸门未闩、分钟桶可发且未满、
// 不在凭据失效冷却。三者都是选中前一刻仍可能翻转的近似判定。
func (lane *poolLane) healthy() bool {
	if lane.authCooldown() || lane.genericCooldown() {
		return false
	}
	stats := lane.adapter.GateStats()
	if stats.Latched {
		return false
	}
	if stats.WindowQuota > 0 && (!stats.Sendable || stats.WindowUsed >= stats.WindowQuota) {
		return false
	}
	return true
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
		return false
	}
	return true
}

// noteFailure 在 lane 失败后更新池侧冷却，两档：
// unauthenticated 走到这里意味着 lane 内自愈（reloadToken+重试）也没
// 救回这份凭据，按当前 token 哈希记鉴权冷却——token 为空同样标记
// （tokenHash("") 作冷却键，凭据源补进真 token 哈希即变、自动解禁）。
// 其余可换号失败记 genericLaneCooldown 短冷却。后到失败只延长不缩短。
// 注意窗口：Stream 返回后才读 currentToken——同 lane 并发请求的自愈
// 恰好在这间隙换上新 token 时会把新 token 误标冷却；最长 10 分钟或
// 下一次轮换自愈，可接受。
func (lane *poolLane) noteFailure(err error) {
	failure := llm.Classify(err)
	if failure == nil {
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
	if failure.Code == "unauthenticated" {
		lane.badTokenHash = tokenHash(lane.adapter.currentToken())
		if until := time.Now().Add(badTokenCooldown); until.After(lane.badUntil) {
			lane.badUntil = until
		}
		return
	}
	if until := time.Now().Add(genericLaneCooldown); until.After(lane.unhealthyUntil) {
		lane.unhealthyUntil = until
	}
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
		models, err := lane.adapter.ListModels(ctx)
		if err == nil {
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
// endpoint 换绑同一生死模型）。任一构建/应用失败即返回错误，已应用
// 的 lane 不回滚——与单 lane ApplyConfig 的失败语义一致。
func (pool *Pool) ApplyConfigs(configs []Config) ([]string, error) {
	if len(configs) == 0 {
		return nil, errors.New("devin pool requires at least one account")
	}
	old := pool.snapshot()
	byName := make(map[string]*poolLane, len(old))
	for _, lane := range old {
		byName[lane.name] = lane
	}
	appliedSet := map[string]bool{}
	next := make([]*poolLane, 0, len(configs))
	var built []*poolLane
	for _, config := range configs {
		if lane, ok := byName[config.Name]; ok {
			applied, err := lane.adapter.ApplyConfig(config)
			if err != nil {
				for _, lane := range built {
					lane.adapter.Close()
				}
				return nil, fmt.Errorf("devin account %q: %w", config.Name, err)
			}
			for _, field := range applied {
				appliedSet[field] = true
			}
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
// applied 取各 lane 差集并集。lane 身份字段（Name/Token/TokenSource）
// 在 mutate 后强制恢复——面板契约只改全局字段，护栏挡住误写把同一
// 身份值铺到全部 lane。
func (pool *Pool) UpdateConfig(mutate func(*Config) error) ([]string, error) {
	appliedSet := map[string]bool{}
	for _, lane := range pool.snapshot() {
		identity := lane.adapter.CurrentConfig()
		applied, err := lane.adapter.UpdateConfig(func(cfg *Config) error {
			if err := mutate(cfg); err != nil {
				return err
			}
			cfg.Name = identity.Name
			cfg.Token = identity.Token
			cfg.TokenSource = identity.TokenSource
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
// 用 TokenFuncs。
func (pool *Pool) TokenFunc() func() string {
	return func() string {
		return pool.firstLane().adapter.TokenFunc()()
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

// GateStats 返回首 lane 闸门快照（顶层 gate 段的后兼容形态）。
func (pool *Pool) GateStats() GateStats {
	return pool.firstLane().adapter.GateStats()
}

// WarmStats 返回首 lane 保温快照（顶层 warm 段的后兼容形态）。
func (pool *Pool) WarmStats() WarmStats {
	return pool.firstLane().adapter.WarmStats()
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

// AccountLaneStates 返回各 lane 的池侧状态快照（按账号名索引），
// /admin/runtime-metrics 的 accounts.<name>.lane 组透出。
func (pool *Pool) AccountLaneStates() map[string]LaneState {
	lanes := pool.snapshot()
	states := make(map[string]LaneState, len(lanes))
	for _, lane := range lanes {
		states[lane.name] = lane.state()
	}
	return states
}

// Aliases 返回模型别名映射：全局字段各 lane 一致，取首 lane。
func (pool *Pool) Aliases() map[string]string {
	return pool.firstLane().adapter.Aliases()
}

// CurrentConfig 返回首 lane 的配置快照：全局字段各 lane 一致；
// Name/Token/TokenSource/GateStatePath 是该 lane 自己的值，调用方
// 展示用要意识到这点（面板 MVP 绑首号，语义恰好正确）。
func (pool *Pool) CurrentConfig() Config {
	return pool.firstLane().adapter.CurrentConfig()
}
