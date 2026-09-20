// 本文件实现配额快照采样与燃烧速率预测，以及 /admin/quota、/admin/status
// 两个端点。
//
// /admin/status 每次都会取回日/周配额剩余百分比与重置时间，但看完即弃。
// 这里按固定间隔把快照写入 quota_samples 表，面板据此画出配额曲线，
// 并用最近窗口的消耗速率外推耗尽时刻——回答「按现在的用法还能撑多久」。
package ccpanel

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

// quotaSampler 是配额采样子系统：采样协程生命周期、逐号身份投影、
// 落库重放缓冲与轮次心跳四组状态连同全部采样/落库/预测逻辑收进
// 一处——此前它们以四把锁十余个字段摊在 Handler 上。依赖经 h 反查
// （store 可被测试热换、pool 是装配期注入），读口径与旧字段直读一致。
type quotaSampler struct {
	h *Handler
	// mu/cancel 管采样协程生命周期：setInterval cancel 旧协程按新
	// 间隔重起（配置 reload 热路径）。reqInterval 记最近一次请求的
	// 周期，供设置页回读；drained 是排空闩——置位后 setInterval
	// 只记账不再重起协程，排空窗口内的 reload/设置写入不能把采样
	// 重新武装。
	mu          sync.Mutex
	cancel      context.CancelFunc
	reqInterval time.Duration
	drained     bool
	// userMu/users 是最近一次逐号配额采样顺带取回的账号身份快照
	// （按账号名索引）：只活内存、随采样周期刷新，重启后首个采样点
	// 落盘前缺席——lane 名是主键，身份只是易读别名。
	userMu sync.Mutex
	users  map[string]map[string]any
	// pendingMu/pending 是配额快照落库失败的重放缓冲：写失败的点
	// 挂账回来，下一次落库（定时采样或手动刷新）随新点一并重放；
	// 容量封顶 quotaPersistRetryCap，溢出丢最老点并告警。(account,at)
	// 唯一索引 + INSERT OR IGNORE 使重放幂等——缓冲是写争用期的
	// 安全带而非持久队列。
	// persistFailures/Dropped/Replayed 是同锁内的落库健康账，投到
	// runtime-metrics 的 quota 组——样本写失败此前只有 stderr WARN，
	// 写争用期丢点没有这组计数完全不可见。
	// persistInFlight/persistDone 是在途落库调用（采样协程与手动
	// 刷新共用的 persist 同步路径）的计数与落定信号：进入时 +1
	// （>0 时 persistDone 非 nil），收尾（含失败挂回之后）-1，归零
	// close 并置 nil。flush 凭它在排空时等写落定——失败点挂回缓冲
	// 后才能被冲刷看见。不用 sync.WaitGroup：排空窗口内手动刷新仍
	// 可能新发落库，Add 撞上零计数 Wait 属 misuse。均在 pendingMu
	// 下读写。
	pendingMu       sync.Mutex
	pending         []*store.QuotaSample
	persistFailures int
	persistDropped  int
	persistReplayed int
	persistInFlight int
	persistDone     chan struct{}
	// hbMu 管采样轮心跳簿记：rounds*/lastRound*At 是协程级（每次
	// sample 调用记一轮）计数与时刻，lanes 是逐 lane 的阶段账。
	// 全内存、进程生命周期——quota_samples 静默空洞（进程活着、
	// 零 WARN、行断档）的归因面：调度器冻结/阶段丢失/stderr 丢行
	// 三类形态靠这组账互证区分。
	hbMu                sync.Mutex
	roundsStarted       int64
	roundsAborted       int64
	lastRoundStartedAt  int64
	lastRoundFinishedAt int64
	lanes               map[string]*quotaLaneStats
}

// adminQuota 实现 GET /admin/quota：日/周配额历史曲线与燃烧速率预测。
// 账户计费数据只对 admin 开放。
func (h *Handler) adminQuota(w http.ResponseWriter, r *http.Request) {
	respondOK(w, h.quotaSub().report(r.Context()))
}

// adminStatus 实现 GET /admin/status：上游账户/plan/容量/IDE/模型状态/
// 供应商的六路聚合。整页结果进 statusSnapshot 的短 TTL 缓存。
func (h *Handler) adminStatus(w http.ResponseWriter, r *http.Request) {
	respondOK(w, h.statusSnapshot(r.Context()))
}

// statusSnapshot 返回 TTL 内的 StatusReport 缓存；并发收敛与超时兜底
// 由 statusCache（ttlCache）承担。等待方断连吃 ctx 取消——映射为
// fetch_error 键回给前端，与 StatusReport 单路失败落 *_error 键的
// 既定语义一致。
func (h *Handler) statusSnapshot(ctx context.Context) map[string]any {
	snap, err := h.statusCache.Get(ctx)
	if err != nil {
		return map[string]any{"fetch_error": err.Error()}
	}
	return snap
}

// 配额快照的行类型是 store.QuotaSample——表行与 /admin/quota 的
// points 线格式共用一个形状（字段 JSON tag 与被取代的 quota.jsonl
// 行一致）。Daily/WeeklyRemaining 是 *float64：保留「上游没报」
// （nil/NULL）与「真到 0」的区分，耗尽时刻前端仍能画出 0%。

// SetQuotaInterval 设定后台配额采样周期；interval<=0 或持久层未注入时
// 停采。可被重复调用（配置 reload 热路径）：cancel 旧协程按新间隔重起，
// 变更点多采一个点——无害，反而给曲线留了变更标记。BeginDrain 置位
// 排空闩后本函数退化为纯簿记：reqInterval 照常记录请求值，协程
// 不再重起。
// 采样失败只记一行进程日志，不影响面板与请求链路。
func (h *Handler) SetQuotaInterval(interval time.Duration) {
	h.quotaSub().setInterval(interval)
}

func (q *quotaSampler) setInterval(interval time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	// 记录最近一次请求值（含停采的 <=0）：ticker 起跑后自身不暴露周期，
	// 面板设置页回读生效值要靠这个簿记。
	q.reqInterval = interval
	if q.cancel != nil {
		q.cancel()
		q.cancel = nil
	}
	if interval <= 0 || q.h.store == nil || q.drained {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel = cancel
	go func() {
		q.stampWriter()
		q.sample(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				q.sample(ctx)
			}
		}
	}()
}

// QuotaInterval 返回最近一次 SetQuotaInterval 请求的采样周期（<=0 表示
// 已停采），供面板设置页回读生效值。
func (h *Handler) QuotaInterval() time.Duration {
	return h.quotaSub().interval()
}

func (q *quotaSampler) interval() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reqInterval
}

// drainFlushTimeout 是排空起点落库冲刷的总预算：闸门窗口行（全部
// lane 共享）与配额样本重放点共用——写连接被批量事务占压时超时
// 返回，进程关停不被拖住；预算内写不完的行与今日一样随退出丢弃。
const drainFlushTimeout = 5 * time.Second

// BeginDrain 实现 app 排空钩子（可选接口，App.BeginDrain 经断言调用）：
// 停掉配额采样协程——采样每轮对每个 lane 打一次上游并写 quota_samples，
// 是排空语义「不再制造新上游工作」该收的后台生产者；在途轮次随 ctx
// 取消收束。只停协程不动 reqInterval 簿记：进程随即退出，生效值
// 回读仍应反映配置而非「被排空归零」。幂等。drained 闩置位后
// 不可逆：排空窗口内的 config reload 与设置写入仍走 SetQuotaInterval，
// 闩保证它们只记账、不把已收束的上游生产者重新武装。
// 顺带冲刷两类落库重放缓冲：闸门窗口行挂账平时等下一窗口翻页重放，
// 配额样本挂账等下一轮采样/刷新重放，进程退出即丢——排空起点给它们
// 最后一轮同步落库机会（best-effort，drainFlushTimeout 内写不完照样
// 丢）。闸门先行：扇出到各 lane 并行写，健康连接毫秒级收工，把预算
// 大头留给配额冲刷的在途落定等待；连接真被占压时两轮写都注定超时，
// 顺序不改变损失。冲刷放在采样锁外：同步落库可能吃满整份预算，
// 持锁会堵排空窗口内 SetQuotaInterval 的簿记。
func (h *Handler) BeginDrain() {
	q := h.quotaSub()
	q.beginDrain()
	ctx, cancel := context.WithTimeout(context.Background(), drainFlushTimeout)
	defer cancel()
	if h.pool != nil && h.pool.FlushGates != nil {
		h.pool.FlushGates(ctx)
	}
	q.flush(ctx)
}

// beginDrain 置排空闩并停掉采样协程：只动生命周期，簿记（reqInterval）
// 保留——进程随即退出，生效值回读仍应反映配置而非「被排空归零」。
func (q *quotaSampler) beginDrain() {
	q.mu.Lock()
	q.drained = true
	if q.cancel != nil {
		q.cancel()
		q.cancel = nil
	}
	q.mu.Unlock()
}

// stampWriter 把当前进程的采样写入者身份刻进 runtime_state
// （key=quota_writer，JSON 值 {pid, boot_at, version, grid_epoch}）。
// quota_samples 行本身不带写入者——2026-09-15→18 的样本断档归因
// 只能靠在 at 列上做网格取证反推未受管进程；留下身份后「谁在写、
// 断档后换成了谁」一次查询即答。采样没有显式相位锚，ticker 各轮
// at ≈ 武装时刻+n·interval，故 grid_epoch 取本次武装时刻——它即
// 网格相位。每次武装写一次（SetState upsert）：进程内身份不变，
// 重复武装仅刷新相位锚；失败仅记日志，不挡采样。
func (q *quotaSampler) stampWriter() {
	raw, err := json.Marshal(map[string]any{
		"pid":        os.Getpid(),
		"boot_at":    q.h.startedAt.Unix(),
		"version":    q.h.Version(),
		"grid_epoch": time.Now().Unix(),
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), quotaPersistBudget)
	defer cancel()
	if err := q.h.store.SetState(ctx, "quota_writer", string(raw)); err != nil {
		slog.Warn("quota writer stamp failed", "error", err)
	}
}

// sample 对每个账号各拉取一次状态并把 plan_status 快照写入
// quota_samples（每行带 account 字段，两号曲线分开画）。账号间按名序
// 逐个采——间隔默认 5 分钟，串行两次上游调用无并发必要。ctx 是采样
// 协程的生命周期：SetQuotaInterval 停采/重起会打断在途轮次。
// 每轮首尾各记一次协程级心跳：ticker 还在不在触发、轮次是否被 ctx
// 中断，投 runtime-metrics 的 quota 组——静默空洞期调度器死活靠它
// 与逐 lane 账互证。
func (q *quotaSampler) sample(ctx context.Context) {
	q.noteRoundStart()
	aborted := false
	for _, account := range q.accounts() {
		if ctx.Err() != nil {
			aborted = true
			break
		}
		q.sampleAccount(ctx, account.name, account.token)
	}
	q.noteRoundFinish(aborted)
}

// quotaAccount 是配额采样的一个账号视角：name 落 quota_samples 的
// account 列，token 是该 lane 的当前凭据。
type quotaAccount struct {
	name  string
	token string
}

// accounts 返回本轮要采样的账号清单，按三种状态分别处置：
//   - 号池未接线（nil）：回退面板首号凭据源的单号匿名
//     采样，account 字段留空——与历史上无号池时的行格式一致；
//   - 已接线但空池（快照无 lane）：返回空清单整轮跳过——再往下走
//     tokenFunc→firstLane 会裸取下标 panic，且每周期写一条
//     account="" 的上游 401 失败行污染 default 桶；
//   - 有号：逐号采，按名序输出稳定。
func (q *quotaSampler) accounts() []quotaAccount {
	ps, ok := q.h.poolSnapshot()
	if !ok {
		return []quotaAccount{{token: q.h.tokenFunc()}}
	}
	funcs := ps.TokenFuncs
	if len(funcs) == 0 {
		// 空池整轮跳过必须留声——静默曾让一次空池故障三天零样本零告警；
		// 每轮复述即信号本身，刻意不去重。
		slog.Warn("quota round skipped: no samplable lanes")
		return nil
	}
	names := make([]string, 0, len(funcs))
	for name := range funcs {
		names = append(names, name)
	}
	slices.Sort(names)
	accounts := make([]quotaAccount, 0, len(names))
	for _, name := range names {
		accounts = append(accounts, quotaAccount{name: name, token: funcs[name]()})
	}
	return accounts
}

// sampleAccount 拉取一个账号的状态并写入一行配额快照；ctx 挂在
// 采样协程生命周期上，单号上限 120s——只约束上游拉取与投影，落库
// 在 persist 里自带独立预算，不分享这段余额。
// 首尾各记一次逐 lane 心跳：轮次走到 fetch/persist 哪一步、最近一次
// 错误文本，供静默空洞期判别「调度器没跑」与「跑了没写」。
// seat-gated 号（individual plan 对 SeatManagementService 整面 403）
// TTL 内跳过上游拉取：那是 plan 属性不是瞬时故障，每轮白打只产 WARN。
func (q *quotaSampler) sampleAccount(ctx context.Context, account, token string) {
	q.noteLaneStart(account)
	if q.seatGatedFresh(account) {
		q.noteSeatGatedRound(account)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	_, plan, persistErr, err := q.capture(ctx, account, token)
	q.noteLaneFinish(account, plan, persistErr, err)
	if err != nil {
		slog.Warn("quota sample failed", "account", account, "error", err)
	}
}

// seatGatedTTL 是「individual plan 无 seat 面」判定在采样侧的缓存
// 时长：期内跳过该号的上游拉取，到期重探一次——升 team 后自行恢复，
// 仍 gated 则重新记戳。手动 quota/refresh 不走此闩（用户显式要求即
// 打），且 fetch 成功时 users 投影整体换新、旗标自然清除。
const seatGatedTTL = time.Hour

// noteSeatGated 在该号的 users 身份投影上合并打 seat-gated 标：
// 采样侧据 seatGatedFresh 周期性跳拉取，accounts 视图经 report.user
// 透出受限态（user.seat_gated）。合并写保住已有 name/email 等字段，
// 附带判定时刻供 TTL 重探。
func (q *quotaSampler) noteSeatGated(account string) {
	q.userMu.Lock()
	defer q.userMu.Unlock()
	if q.users == nil {
		q.users = map[string]map[string]any{}
	}
	u := q.users[account]
	if u == nil {
		u = map[string]any{}
		q.users[account] = u
	}
	u["seat_gated"] = true
	u["seat_gated_at"] = time.Now().Unix()
}

// seatGatedFresh 报该号 seat-gated 判定是否仍在有效期内（跳拉取依据）。
func (q *quotaSampler) seatGatedFresh(account string) bool {
	q.userMu.Lock()
	defer q.userMu.Unlock()
	u := q.users[account]
	if u == nil || u["seat_gated"] != true {
		return false
	}
	at, _ := u["seat_gated_at"].(int64)
	return time.Since(time.Unix(at, 0)) < seatGatedTTL
}

// capture 是逐号配额采样内核：拉取该号 userStatus、更新
// users 身份投影、把 planStatus 快照写入 quota_samples，返回
// 投影后的 (user, plan)。定时采样与手动刷新共用——后者把返回值回
// 显给操作者。plan 为 nil 表示上游 200 但未携带 planStatus：身份
// 投影照常更新，本轮只是无配额点可写，不算错误。persistErr 汇报
// 本号新点的落库结局（nil=落库或本轮无点可写）；采样侧按它记
// persist 阶段账，刷新侧忽略——点写失败已挂重放缓冲，不算刷新错误。
func (q *quotaSampler) capture(ctx context.Context, account, token string) (user, plan map[string]any, persistErr, err error) {
	rawUser, plan, _, err := q.h.fetchUserStatusAs(ctx, token)
	if err != nil {
		// seat plan-gate（individual plan 整面 403）记标进投影：定时
		// 采样在 TTL 内跳过白打，视图侧透出受限态。
		if isSeatPlanGate(err) {
			q.noteSeatGated(account)
		}
		return nil, nil, nil, err
	}
	user = map[string]any{
		"name":             strAny(rawUser["name"]),
		"email":            strAny(rawUser["email"]),
		"pro":              rawUser["pro"],
		"teams_tier":       strAny(rawUser["teams_tier"]),
		"plan_name":        strAny(plan["plan_name"]),
		"billing_strategy": strAny(plan["billing_strategy"]),
	}
	q.userMu.Lock()
	if q.users == nil {
		q.users = map[string]map[string]any{}
	}
	q.users[account] = user
	q.userMu.Unlock()
	if plan == nil {
		// 上游 200 但缺 planStatus：不写点也不报错会让曲线静默断档，
		// 留一行痕迹说明「拉到了但无配额数据」。
		slog.Warn("quota sample skipped: userStatus carried no planStatus", "account", account)
		return user, nil, nil, nil
	}
	point := &store.QuotaSample{
		At:                time.Now().Unix(),
		Account:           account,
		DailyRemaining:    planFloat(plan, "daily_quota_remaining"),
		WeeklyRemaining:   planFloat(plan, "weekly_quota_remaining"),
		DailyResetAt:      int64(floatAny(plan["daily_quota_reset"])),
		WeeklyResetAt:     int64(floatAny(plan["weekly_quota_reset"])),
		PromptCredits:     floatAny(plan["available_prompt_credits"]),
		FlowCredits:       floatAny(plan["available_flow_credits"]),
		FlexCredits:       floatAny(plan["available_flex_credits"]),
		ACUConsumed:       floatAny(plan["acu_consumed"]),
		ACULimit:          floatAny(plan["acu_limit"]),
		UsedPromptCredits: floatAny(plan["used_prompt_credits"]),
		UsedFlowCredits:   floatAny(plan["used_flow_credits"]),
		UsedFlexCredits:   floatAny(plan["used_flex_credits"]),
		// overage_balance_micros 是 micros 粒度的欠费账本（负值=负债），
		// 比整数百分比细得多——付费燃烧走 overage 通道时百分比不动它动。
		OverageBalanceMicros: int64(floatAny(plan["overage_balance_micros"])),
		// plan["grace_period_status"] 已经 fetchUserStatus 的 shortEnum
		// 缩成尾段；grace_period_end 是归一后的 RFC3339，转回 unix 秒。
		GracePeriodStatus:         strAny(plan["grace_period_status"]),
		GracePeriodEnd:            rfc3339Unix(plan["grace_period_end"]),
		WasReducedByOrphanedUsage: boolAny(plan["was_reduced_by_orphaned_usage"]),
	}
	if tu, ok := plan["top_up_status"].(map[string]any); ok {
		point.TopUpEnabled = boolAny(tu["enabled"])
		point.TopUpTransactionStatus = strAny(tu["transaction_status"])
	}
	persistErr = q.persist(point)
	q.noteSignal(account, plan)
	return user, plan, persistErr, nil
}

// quotaPersistRetryCap 是配额快照写失败后的重放缓冲深度：采样默认
// 5 分钟一轮、每号一行，深度 4 让失败点搭上后两轮采样（双号 ~10 分钟
// 覆盖批量日志事务/部署交接的写争用波）；更深的缓冲重放的是曲线
// 价值已衰减的陈旧点，溢出丢最老点并告警。
const quotaPersistRetryCap = 4

// quotaPersistBudget 是落库（含重放积压批）的独立预算，与上游拉取
// 分账：历史上 fetch 与写共享 120s 单号预算，慢 fetch 把预算耗尽后
// 写死于 context deadline exceeded（2026-09-19 02:02 bravo 丢点
// 事故）。无竞争时单行 INSERT 毫秒级；15s 覆盖 quotaPersistRetryCap+1
// 行重放批与常规写锁排队仍宽裕，更长也堵不住分钟级争用窗——那部分
// 归挂账重放管。与 debuglog storeCtx/modelreg storeOpTimeout 同款
// 约定：写库拿 Background 派生的独立死线，不随调用方生命周期陪葬。
const quotaPersistBudget = 15 * time.Second

// persist 落一个新配额点并把上轮写失败挂账的点一并重放
// （取走即清，点集独占移交本调用；首个失败即停手，剩余尾部整段挂回
// 缓冲等下一轮——争用期里同批后续点大概率同病）。(account,at) 唯一
// 索引 + INSERT OR IGNORE 使重放幂等：上轮看似失败实则落库的点重放
// 时静默跳过，不写双份。定时采样与手动刷新共用本路径——刷新也是
// 争用期内的恢复通道。写库不继承调用方 ctx：采样侧的 120s 可能已被
// 慢 fetch 耗尽，refresh 侧的 request ctx 可能随操作者断连取消，
// 已取回的数据点不该为这些生命周期陪葬。
// 返回停批的那条写错误；nil 表示新点已落库（含 OR IGNORE 幂等命中）。
// 败在挂账重放行时新点未尝试即挂回——返回值同样是那行错误，采样侧
// 据它记 failed_persist 与 last_error。
func (q *quotaSampler) persist(point *store.QuotaSample) error {
	q.pendingMu.Lock()
	q.persistInFlight++
	if q.persistDone == nil {
		q.persistDone = make(chan struct{})
	}
	pending := q.pending
	q.pending = nil
	q.pendingMu.Unlock()
	// 计数落定放在收尾（含失败挂回）之后：flush
	// 等到落定才取缓冲，挂回的点必须赶在它取走前入帐。
	defer q.notePersistSettled()
	rows := append(pending, point)
	ctx, cancel := context.WithTimeout(context.Background(), quotaPersistBudget)
	defer cancel()
	for i, r := range rows {
		if err := q.h.store.InsertQuotaSample(ctx, r); err != nil {
			slog.Warn("quota sample persist failed", "account", r.Account, "error", err)
			q.noteReplayed(min(i, len(pending)))
			q.stash(rows[i:])
			return err
		}
	}
	q.noteReplayed(len(pending))
	return nil
}

// notePersistSettled 落定一次在途落库：计数 -1，归零时 close
// 落定信号并置 nil 等下一批在途重建。
func (q *quotaSampler) notePersistSettled() {
	q.pendingMu.Lock()
	q.persistInFlight--
	if q.persistInFlight == 0 {
		close(q.persistDone)
		q.persistDone = nil
	}
	q.pendingMu.Unlock()
}

// flush 在排空起点对未落库的配额样本点做最后一轮同步冲刷：先等
// 在途落库调用落定（失败点会挂回 pending，跳过等待直接取缓冲会
// 漏掉它们），再取走缓冲整批写。全程共用调用方的短 ctx——排空有
// 时限，写连接卡死不能拖住关停；预算内写不完的点与进程直接退出
// 一样丢弃（best-effort，不是持久化保证），失败点照常挂回缓冲并
// 计 persistFailures，写成的挂账点计 persistReplayed。
func (q *quotaSampler) flush(ctx context.Context) {
	if q.h.store == nil {
		return
	}
	q.pendingMu.Lock()
	done := q.persistDone
	q.pendingMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
	q.pendingMu.Lock()
	rows := q.pending
	q.pending = nil
	q.pendingMu.Unlock()
	for i, r := range rows {
		if err := q.h.store.InsertQuotaSample(ctx, r); err != nil {
			slog.Warn("quota sample drain flush failed", "account", r.Account, "error", err)
			q.noteReplayed(i)
			q.stash(rows[i:])
			return
		}
	}
	q.noteReplayed(len(rows))
}

// stash 把未落库的配额点挂回重放缓冲；超出深度的最老点
// 丢弃并告警——缓冲是争用期安全带，永久丢失要留痕迹。写尝试失败
// 按批计一次 persistFailures（与 stderr 告警一一对应：首个失败即
// 停手，同批余点未尝试不计失败），溢出丢弃按点计 persistDropped。
func (q *quotaSampler) stash(rows []*store.QuotaSample) {
	q.pendingMu.Lock()
	defer q.pendingMu.Unlock()
	q.persistFailures++
	q.pending = append(q.pending, rows...)
	for len(q.pending) > quotaPersistRetryCap {
		dropped := q.pending[0]
		q.pending = q.pending[1:]
		q.persistDropped++
		slog.Warn("quota sample persist buffer full: dropping oldest sample",
			"account", dropped.Account, "at", dropped.At)
	}
}

// noteReplayed 记账本轮落库中救回的挂账点数：失败下标之前的
// pending 前缀与全量成功两种情形都经它计 persistReplayed；INSERT OR
// IGNORE 的幂等命中同样算救回（点已在库即救援成立）。n=0 快进返回，
// 省一次锁。
func (q *quotaSampler) noteReplayed(n int) {
	if n == 0 {
		return
	}
	q.pendingMu.Lock()
	q.persistReplayed += n
	q.pendingMu.Unlock()
}

// quotaLaneStats 是单 lane 的定时采样轮心跳簿记（hbMu 内）：
// 每轮各阶段计数 + 最近起止时刻与错误文本。只记定时采样路径——手动
// 刷新共用 capture 内核但不记这里，保住「调度器活没活、这轮走到哪
// 步」的判读纯度。lastStartedAt>lastFinishedAt 即在飞轮或楔死轮。
type quotaLaneStats struct {
	roundsStarted   int64  // 轮次起跑（sampleAccount 入口）
	roundsFetchOK   int64  // userStatus 拉取成功（含无 planStatus 轮）
	roundsPersistOK int64  // 本号新点落库（含 OR IGNORE 幂等命中）
	roundsSeatGated int64  // seat plan-gate 跳过的轮数（TTL 内不再白打）
	failedFetch     int64  // 拉取失败（对应 WARN quota sample failed）
	failedNoPlan    int64  // 拉到但缺 planStatus（WARN ...skipped）
	failedPersist   int64  // 落库批停于写错误（WARN ...persist failed）
	lastStartedAt   int64  // 最近一轮起跑时刻（unix 秒）
	lastFinishedAt  int64  // 最近一轮收束时刻（unix 秒）
	lastError       string // 最近一次失败文本；跨成功保留，恢复后仍可溯源
}

// noteRoundStart/noteRoundFinish 记一轮定时采样的协程级
// 心跳：rounds_started 每次 sample 调用都计（含空号池轮），
// rounds_aborted 只计被 ctx 中途截断的轮——停采/重起/排空取消在途
// 轮次是它唯一的成因，据此与「ticker 干脆不触发」（last_round_
// started_at 冻结）区分开。
func (q *quotaSampler) noteRoundStart() {
	q.hbMu.Lock()
	q.roundsStarted++
	q.lastRoundStartedAt = time.Now().Unix()
	q.hbMu.Unlock()
}

func (q *quotaSampler) noteRoundFinish(aborted bool) {
	q.hbMu.Lock()
	if aborted {
		q.roundsAborted++
	}
	q.lastRoundFinishedAt = time.Now().Unix()
	q.hbMu.Unlock()
}

// noteLaneStart/noteLaneFinish 记单 lane 轮次的阶段账，
// 配合协程级心跳把静默空洞归因到三类形态：「调度器没起跑」（各
// lane lastStartedAt 同刻冻结）、「fetch 与 persist 之间丢了」
// （fetch_ok 涨而 persist_ok/failed_persist 都不动）、「WARN 写了
// 但 stderr 丢了」（failed_* 涨而无对应日志行）。
func (q *quotaSampler) noteLaneStart(account string) {
	q.hbMu.Lock()
	defer q.hbMu.Unlock()
	st := q.laneLocked(account)
	st.roundsStarted++
	st.lastStartedAt = time.Now().Unix()
}

func (q *quotaSampler) noteLaneFinish(account string, plan map[string]any, persistErr, err error) {
	q.hbMu.Lock()
	defer q.hbMu.Unlock()
	st := q.laneLocked(account)
	st.lastFinishedAt = time.Now().Unix()
	switch {
	case err != nil:
		st.failedFetch++
		st.lastError = err.Error()
	case plan == nil:
		st.roundsFetchOK++
		st.failedNoPlan++
		st.lastError = "userStatus carried no planStatus"
	case persistErr != nil:
		st.roundsFetchOK++
		st.failedPersist++
		st.lastError = persistErr.Error()
	default:
		st.roundsFetchOK++
		st.roundsPersistOK++
	}
}

// noteSeatGatedRound 记一轮「seat plan-gate 命中 TTL、跳过拉取」：
// 不计失败（gated 是 plan 属性非故障），但轮次须留痕——静默空洞
// 归因要能区分「调度器没跑」与「跑了但被 gated 跳过」。
func (q *quotaSampler) noteSeatGatedRound(account string) {
	q.hbMu.Lock()
	defer q.hbMu.Unlock()
	st := q.laneLocked(account)
	st.lastFinishedAt = time.Now().Unix()
	st.roundsSeatGated++
}

// laneLocked 取该号的逐 lane 账簿，缺则建（hbMu
// 内调用）；匿名空串按 ”/default 折叠归名，与 report 同口径。
func (q *quotaSampler) laneLocked(account string) *quotaLaneStats {
	if q.lanes == nil {
		q.lanes = map[string]*quotaLaneStats{}
	}
	name := account
	if name == "" {
		name = "default"
	}
	st, ok := q.lanes[name]
	if !ok {
		st = &quotaLaneStats{}
		q.lanes[name] = st
	}
	return st
}

// persistStats 返回配额样本落库健康账与采样轮心跳快照，投
// runtime-metrics 的 quota 组。计数口径与 gate 组 persist_* 对齐：
// persist_failures 按写尝试计（首个失败即停手、剩余整段挂回——一批
// 至多记一次失败），persist_dropped/persist_replayed 按点计；
// pending_samples 是重放缓冲当前深度，回答「此刻还有没有未落库的
// 欠账」。rounds_*/last_round_*_at 是协程级心跳，lanes 是逐 lane
// 阶段账（rounds_failed=三类失败合计，failures 分原因）——静默
// 空洞期判别调度器死活与各 lane 止步阶段的唯一面。
func (q *quotaSampler) persistStats() map[string]any {
	q.pendingMu.Lock()
	out := map[string]any{
		"persist_failures": q.persistFailures,
		"persist_dropped":  q.persistDropped,
		"persist_replayed": q.persistReplayed,
		"pending_samples":  len(q.pending),
	}
	q.pendingMu.Unlock()
	q.hbMu.Lock()
	out["rounds_started"] = q.roundsStarted
	out["rounds_aborted"] = q.roundsAborted
	out["last_round_started_at"] = q.lastRoundStartedAt
	out["last_round_finished_at"] = q.lastRoundFinishedAt
	lanes := make(map[string]any, len(q.lanes))
	for name, st := range q.lanes {
		lane := map[string]any{
			"rounds_started":    st.roundsStarted,
			"rounds_fetch_ok":   st.roundsFetchOK,
			"rounds_persist_ok": st.roundsPersistOK,
			"rounds_seat_gated": st.roundsSeatGated,
			"rounds_failed":     st.failedFetch + st.failedNoPlan + st.failedPersist,
			"failures": map[string]any{
				"fetch":   st.failedFetch,
				"no_plan": st.failedNoPlan,
				"persist": st.failedPersist,
			},
			"last_started_at":  st.lastStartedAt,
			"last_finished_at": st.lastFinishedAt,
		}
		if st.lastError != "" {
			lane["last_error"] = st.lastError
		}
		lanes[name] = lane
	}
	q.hbMu.Unlock()
	out["lanes"] = lanes
	return out
}

// noteSignal 把一次成功探测的日/周剩余百分比回灌给池侧
// 降权簿记；两键俱缺时不喂——weekly 缺报按 0 喂会把 lane 误判进降权档。
func (q *quotaSampler) noteSignal(account string, plan map[string]any) {
	if q.h.pool == nil || q.h.pool.NoteQuota == nil || plan == nil {
		return
	}
	daily := planFloat(plan, "daily_quota_remaining")
	weekly := planFloat(plan, "weekly_quota_remaining")
	if daily == nil || weekly == nil {
		return
	}
	q.h.pool.NoteQuota(account, *daily, *weekly)
}

// refresh 即采一次指定账号配额：与定时采样共用
// capture 内核（拉 userStatus、更新 users 投影、
// 落 quota_samples 行），把 {account, user, plan} 回给
// /admin/accounts/{name}/quota/refresh 作响应体——plan 直出
// fetchUserStatusAs 归一化后的 planStatus 子集，与采样落库的字段名
// 同口径。上游失败返回 error（handler 映 502）；plan 为 nil 表示
// 上游没报 planStatus。与定时采样同秒撞 (account,at) 唯一索引时
// INSERT OR IGNORE 静默丢点，不算失败。
func (q *quotaSampler) refresh(ctx context.Context, account, token string) (map[string]any, error) {
	user, plan, _, err := q.capture(ctx, account, token)
	if err != nil {
		return nil, err
	}
	return map[string]any{"account": account, "user": user, "plan": plan}, nil
}

// quotaHistoryCap 是单次读取的历史样本数上限；默认 5 分钟间隔下约覆盖
// 34 天。
const quotaHistoryCap = 10000

// readHistory 读 quota_samples 尾部 quotaHistoryCap 条（at 升序，
// 截断下推 SQL LIMIT）。按号分组的裁剪在 report 侧做；库查询失败
// 按无历史降级。
func (q *quotaSampler) readHistory(ctx context.Context) []*store.QuotaSample {
	if q.h.store == nil {
		return nil
	}
	points, err := q.h.store.ListQuotaSamples(ctx, "", 0, quotaHistoryCap)
	if err != nil {
		slog.Warn("quota history read failed", "error", err)
		return nil
	}
	return points
}

// forecast 用最近 lookback 窗口内的逐相邻样本差分估算燃烧速率与耗尽
// 时刻：分子分母同步累计——只把「两端都报了数且 remaining 未上升」的
// 相邻段计入消耗与时长；remaining 上升的相邻段是周期重置边界，跳过
// （首尾两点差分遇到跨重置窗口会把回满错算成负消耗，烧着却报烧不完）；
// pick 返回 NaN 表示「上游没报」，含 NaN 端点的段不可测、不计入——
// 把 nil 当 0% 会伪造一次烧到 0 的差分。
// 配额只剩百分比语义：日配额在 daily_reset_at 重置，周配额同理；
// 「耗尽」指按当前速率在重置前把剩余百分比烧完。
func forecast(points []*store.QuotaSample, lookback time.Duration, pick func(*store.QuotaSample) float64, resetAt func(*store.QuotaSample) int64) map[string]any {
	if len(points) < 2 {
		return nil
	}
	last := points[len(points)-1]
	cutoff := last.At - int64(lookback.Seconds())
	start := 0
	for i := len(points) - 2; i >= 0; i-- {
		if points[i].At <= cutoff {
			break
		}
		start = i
	}
	var consumed float64
	var measured int64
	for i := start + 1; i < len(points); i++ {
		prev, cur := pick(points[i-1]), pick(points[i])
		if math.IsNaN(prev) || math.IsNaN(cur) || cur > prev {
			continue
		}
		consumed += prev - cur
		measured += points[i].At - points[i-1].At
	}
	if measured <= 0 {
		return nil
	}
	hours := float64(measured) / 3600
	rate := consumed / hours // 百分比/小时，消耗为正
	out := map[string]any{
		"window_hours":  hours,
		"reset_at":      resetAt(last),
		"burn_per_hour": rate,
		"burn_per_day":  rate * 24,
	}
	if rem := pick(last); !math.IsNaN(rem) {
		out["remaining"] = rem
		if rate > 0 {
			hoursLeft := rem / rate
			exhaustedAt := last.At + int64(hoursLeft*3600)
			// 外推的耗尽时刻越过重置点就没有物理意义：配额在 reset_at
			// 先回满，本周期烧不完——报 survives_until_reset 而非一个
			// 不可能发生的 exhausted_at。reset_at 未知或已过期时无法
			// 判定边界，按原样报 exhausted_at。
			if reset := resetAt(last); reset > last.At && exhaustedAt > reset {
				out["survives_until_reset"] = true
			} else {
				out["exhausted_at"] = exhaustedAt
			}
			out["hours_left"] = hoursLeft
		}
	}
	return out
}

// report 返回配额历史曲线与按最近窗口燃烧速率外推的预测。
// 号池下每号配额独立：accounts 组按名给各自的曲线与预测（冻结序列
// 只留曲线与 stale 标记，见 seriesReport），顶层
// points/daily/weekly 镜像尾点 At 最大（最新鲜）的那条序列作后
// 兼容视图——单号部署时与升级前输出逐字段一致（历史无 account
// 字段的行归入 "default" 桶，与隐式单 lane 同名自然合流）。
func (q *quotaSampler) report(ctx context.Context) map[string]any {
	byAccount := map[string][]*store.QuotaSample{}
	for _, point := range q.readHistory(ctx) {
		name := point.Account
		if name == "" {
			// logs 逐号聚合同样按 COALESCE(NULLIF(account,''),'default')
			// 对齐此口径——''/default/真名三群取值两侧折叠一致。
			name = "default"
		}
		byAccount[name] = append(byAccount[name], point)
	}
	names := make([]string, 0, len(byAccount))
	for name := range byAccount {
		names = append(names, name)
	}
	slices.Sort(names)
	accounts := make(map[string]any, len(names))
	for _, name := range names {
		accounts[name] = q.seriesReport(name, byAccount[name])
	}
	out := map[string]any{"accounts": accounts}
	// 镜像跟随最新鲜的序列而非名序首个：被移出号池的号曲线停更，
	// 名序首个可能恰是那条冻住的序列，兼容视图会定格在旧数据上。
	// 尾点 At 相同取名序靠前者——names 已排序，先到最大值的胜出。
	freshest, freshestAt := "", int64(-1)
	for _, name := range names {
		if at := byAccount[name][len(byAccount[name])-1].At; at > freshestAt {
			freshest, freshestAt = name, at
		}
	}
	if freshest != "" {
		mirror := accounts[freshest].(map[string]any)
		out["points"] = mirror["points"]
		out["daily"] = mirror["daily"]
		out["weekly"] = mirror["weekly"]
	}
	return out
}

// quotaSeriesStaleFloor 是冻结序列判定的龄期下限：尾点距今超过
// max(3×采样周期, quotaSeriesStaleFloor) 的序列视为冻结。
const quotaSeriesStaleFloor = time.Hour

// seriesReport 用一条样本序列构建单号报告：points 曲线 + 日/周
// forecast，并并入该号的身份快照（采样顺带取回；只对确有记录的 lane
// 投影，重启后首个采样点落盘前的缺席交给前端渲染成未知）。单号视图
// （accounts 写端点回包）也走它，免去为一条序列扫全表。
// 冻结序列（尾点过旧：lane 被移出号池、采样停摆、历史空串行归入
// default 的遗留桶）不喂 forecast——它的外推锚在死尾点上，耗尽
// 时刻会落进过去；只留曲线并打 stale 标记，消费方据此判读。bound
// 随采样周期缩放、下限一小时：进程重启或短暂停采造成的缺口不误判，
// 单号部署下持续写入的 account 空串（→default）活跃序列不受影响。
func (q *quotaSampler) seriesReport(name string, series []*store.QuotaSample) map[string]any {
	staleAfter := quotaSeriesStaleFloor
	if scaled := 3 * q.interval(); scaled > staleAfter {
		staleAfter = scaled
	}
	stale := time.Now().Unix()-series[len(series)-1].At > int64(staleAfter.Seconds())
	report := map[string]any{
		"points": series,
		"stale":  stale,
		"daily":  nil,
		"weekly": nil,
	}
	if !stale {
		report["daily"] = forecast(series, 24*time.Hour, func(p *store.QuotaSample) float64 { return remainingOrNaN(p.DailyRemaining) }, func(p *store.QuotaSample) int64 { return p.DailyResetAt })
		report["weekly"] = forecast(series, 7*24*time.Hour, func(p *store.QuotaSample) float64 { return remainingOrNaN(p.WeeklyRemaining) }, func(p *store.QuotaSample) int64 { return p.WeeklyResetAt })
	}
	q.userMu.Lock()
	if u, ok := q.users[name]; ok {
		report["user"] = u
	}
	q.userMu.Unlock()
	return report
}

// floatAny 把 fetchUserStatus 产出的宽松数值统一成 float64。
func floatAny(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

// planFloat 取 planStatus 里的数值字段；键缺席返回 nil——QuotaSample 的
// 指针字段靠它保住「未上报」与「0%」的区分。
func planFloat(plan map[string]any, key string) *float64 {
	v, ok := plan[key]
	if !ok {
		return nil
	}
	f := floatAny(v)
	return &f
}

// floatOr0 解引用配额指针，nil（上游未上报）按 0 返回——只用于展示性
// 读取；forecast 差分走 remainingOrNaN，那里必须把「没报」与「真到 0」
// 区分开。
func floatOr0(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// remainingOrNaN 解引用配额指针，nil 编码为 NaN——forecast 靠它识别
// 「这段不可测」而不把缺席伪造成 0%（那会谎报一次烧尽的差分）。
func remainingOrNaN(v *float64) float64 {
	if v == nil {
		return math.NaN()
	}
	return *v
}

// rfc3339Unix 把 planStatus 里归一化后的 RFC3339 时刻转回 unix 秒；
// 缺席或畸形记 0——omitempty 让快照里该键消失，与「未上报」口径一致。
func rfc3339Unix(v any) int64 {
	t, err := time.Parse(time.RFC3339, strAny(v))
	if err != nil {
		return 0
	}
	return t.Unix()
}
