// 本文件实现 RequestMessages 与 Devin Connect RPC 的双向转换。
//
// Package devin 负责一次 Devin GetChatMessage 调用及其响应事件转换，不执行工具或 agent loop。
package devin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/httpproxy"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
	"github.com/WncFht/devin2api/internal/upstream"
)

// 默认客户端身份常量与真实 Devin CLI 抓包逐字段对齐；上游若开始按
// extension_version 做版本门（新模型 gate），可在 config 的 devin.client_*
// 覆盖而不必发版。
const (
	defaultClientName    = "chisel"
	defaultClientVersion = "3000.2.17"
	defaultClientOS      = "mac"
	// catalogRetryBackoff 是模型目录拉取失败且错误未带 reset hint 时的
	// 冷却时长；带 hint 时按 hint 冷却（上游何时解除它自己最清楚）。
	catalogRetryBackoff = 30 * time.Second
)

// LaneIdentity 是一条上游账号泳道（lane）的身份字段组：号池下逐 lane
// 各异，不参与「全局字段各 lane 一致」约定。Name 进闸门状态键
// （gate:<name>）、日志与面板归因字段，也是 ApplyConfigs 的 lane 名键
// 与 applied 名单（devin.accounts.<name>.token）的词干。
type LaneIdentity struct {
	Name string
	// Token 是 Devin session token；不会写入日志。
	Token string
	// APIKey 是 Devin 平台 durable key（app.devin.ai 签发的 cog_*）：
	// session token 被上游判死且 TokenSource 拿不出新凭据时，lane 用它
	// 经 GetSelfDevinSessionToken 现场铸一枚新 session token（minted
	// 槽位），durable key 本身无内嵌寿命——这是「自动不过期」的来源。
	APIKey string
	// TokenSource 可选：unauthenticated 时回调重新解析凭据。
	// Devin CLI 会续期改写 credentials.toml，静态缓存的 token 会静默失效；
	// 回调应重读同一来源（配置文件或凭证文件），返回空表示无新凭据。
	TokenSource func() string
}

// Endpoint 是烤进 transport 的上游端点参数组：一经 newUpstreamLink 构建
// 即固化进 client 与焐池，热应用时任一字段变化都触发调用束整体重建
// （原子换指针，在途调用持旧引用跑完）。纯值类型、可 == 整比——
// commitConfigLocked 的重建判定与 main 的 reload 端点比对都靠它。
type Endpoint struct {
	// BaseURL 是 Devin Connect 服务的基础地址。
	BaseURL string
	// Proxy 是可选的 HTTP/HTTPS/SOCKS5 代理地址；为空时直连或走系统环境变量。
	Proxy string
	// ForceHTTP1 为 true 时强制 HTTP/1.1，每请求独立连接，避免 HTTP/2 单连接多 stream 并发瓶颈。
	ForceHTTP1 bool
}

// Config 保存 Devin adapter 的配置，字段按所有权分三组：
//   - Identity 是 lane 身份，号池下各 lane 自带一份；
//   - Endpoint 是端点冻结集，换值即重建上游调用束；
//   - 其余为全局可调项，号池下各 lane 共享同一组值——Pool 的
//     first-lane 视图（CurrentConfig/Aliases 等）只读这组与 Endpoint，
//     Identity 类一律走 TokenFuncs/AccountLaneStates 等 per-lane 接口。
type Config struct {
	Identity LaneIdentity
	Endpoint Endpoint
	// Model 是 Devin chat model UID。
	Model string
	// Aliases 是客户端模型名到上游真实 UID 的映射；命中时请求模型被重写。
	Aliases map[string]string
	// ClientName/ClientVersion/ClientOS 是发给上游的 metadata 身份字段；
	// 为空时回落到默认常量（与真实 CLI 抓包一致）。
	ClientName    string
	ClientVersion string
	ClientOS      string
	// Gate 是速率闸门参数组；字段语义与默认值回落见 GateConfig。
	Gate GateConfig
	// GateStateStore 非空时冷却闩截止时刻持久化到 runtime_state
	// （键 store.GateStateKey(Identity.Name)，即 gate:<lane>），进程重启后
	// 未过期的闩被恢复——上游限流器把被拒尝试计入窗口，闩内重启
	// 裸发会把限流续长。这是运行时句柄而非配置值：commitConfigLocked
	// 热应用时沿用旧值，不参与 diff。
	GateStateStore *store.Store
	// Warm 是前缀保温参数组；字段语义与默认值回落见 WarmConfig。
	Warm WarmConfig
	// Priority 是池级排序元数据：值越大越优先被新会话选中，同优先级
	// 内回 rendezvous 钉选序。它不是 lane 运行参数——ApplyConfig 不
	// 消费它，Pool.ApplyConfigs 直接读进 lane 排序键。
	Priority int
	// SessionAffinityTTLSeconds 是会话绑定的滑动 TTL 秒数：命中即续期，
	// 0 回落默认 3600。全局字段各 lane 一致，Pool 读首 lane 值。
	SessionAffinityTTLSeconds int
	// QuotaLowThresholdPercent 是配额降权阈值：weekly 剩余百分比低于
	// 它时 lane 对新会话降档（已绑定会话不受影响）；0 回落默认 15，
	// 负值关闭降权。全局字段各 lane 一致。
	QuotaLowThresholdPercent int
	// NoProgressTimeout 是「产出过内容之后」的无进度期限：上游在工具
	// 调用参数阶段可静默计算 15-25min 只发心跳帧，post-content 档位
	// 必须盖住它（pre-content 档沿用 upstreamNoProgressTimeout——
	// 还没产出就死等价值不大）。<=0 回落默认 45min。
	// 全局字段各 lane 一致。
	NoProgressTimeout time.Duration
	// PreEventNoProgressTimeout 是「产出首个事件之前」每段等待的
	// 无进度期限：与 NoProgressTimeout 对偶，<=0 回落默认 10min。
	// 注意它只缩不扩——pre-event 累计静默另有
	// upstreamPreEventSilenceCap 硬顶（从首发起算、跨重开累计），
	// 配得比 180s 大不会突破累计上限。全局字段各 lane 一致。
	PreEventNoProgressTimeout time.Duration
}

// ClientIdentity 返回请求要携带的客户端身份；空字段回落到与真实
// Devin CLI 抓包一致的默认值。cmd/probe 复用它保持与代理同一指纹。
func (config Config) ClientIdentity() (name, version, os string) {
	name = strings.TrimSpace(config.ClientName)
	if name == "" {
		name = defaultClientName
	}
	version = strings.TrimSpace(config.ClientVersion)
	if version == "" {
		version = defaultClientVersion
	}
	os = strings.TrimSpace(config.ClientOS)
	if os == "" {
		os = defaultClientOS
	}
	return name, version, os
}

// Adapter 调用 Devin 的 ApiServerService/GetChatMessage。
type Adapter struct {
	// configMu 保护 config：ApplyConfig 热路径整体换值，读侧经
	// CurrentConfig 取快照。
	configMu sync.RWMutex
	config   Config
	// token 是声明侧上游凭据（config/TokenSource 管）；minted 是
	// APIKey 现场铸出的 session token——只在内存活（不落盘，重启后
	// 再铸一次即可），非空时优先于 token 服役。unauthenticated 自愈
	// 原地更新，transport 经 tokenFunc 每次请求读取，无需重建 client。
	tokenMu sync.RWMutex
	token   string
	minted  string
	// mintMu 串行化 reloadToken 的铸币段：凭据死亡时 N 个并发
	// unauthenticated 只会付一发 GetSelfDevinSessionToken——排队者
	// 进锁后先按快照比对，生效凭据已被先行自愈换掉就直接复用。
	mintMu sync.Mutex
	// linkPtr 是绑死 base_url/proxy/force_http1 的上游调用束：endpoint
	// 热应用时整体重建换指针（见 ApplyConfig），在途调用持旧引用跑完。
	// 读侧经 link() 取快照；New 之后恒非 nil。
	linkPtr        atomic.Pointer[upstreamLink]
	modelsMu       sync.RWMutex
	models         []adapter.ModelInfo
	modelsExpiry   time.Time
	modelsCacheTTL time.Duration
	// modelsRetryUntil/modelsErr 是目录拉取失败的冷却窗口：失败期间
	// 目录始终为空，不冷却会让每个请求（ensureCatalog）都重试一次
	// GetCliModelConfigs，客户端重试风暴原样穿透到上游（stub 实测
	// 20s 内 1.1 万次）。窗口内有旧缓存回旧值，否则回 modelsErr。
	modelsRetryUntil time.Time
	modelsErr        error
	// modelsErrWarned 标记本次冷却窗的失败已告警：窗内每个
	// ensureCatalog 调用方拿到同一 modelsErr，不记会按请求频率
	// 刷屏；下一次拉取失败提交新冷却时重置（见 ListModels）。
	modelsErrWarned bool
	// modelsFetch 非 nil 表示有目录拉取在锁外进行中：等待者 select 该
	// channel（吃自己的 ctx，断连可中途退出），拉取方提交缓存/冷却
	// 之后 close 它，被唤醒方重走复查路径拿结果。
	modelsFetch chan struct{}
	// warnedAbsentModels 给「模型缺席目录」告警按 uid 去重：别名目标
	// 是配置级事实，每进程警一次足够，不该按请求频率刷屏。
	warnedAbsentModels sync.Map
	// gate 是上游消息速率闸门：令牌桶主动限速 + 上游限流冷却闩。
	// 每次 GetChatMessage 发送（含自愈/重开重试）前都要过闸。
	gate *rateGate
	// warm 是前缀保温簿记与调度器：跟踪 lineage 的上游缓存存活，
	// 静默期按节奏发 mt=1 重放续命。New 中随 adapter 创建。
	warm *cacheWarmer
	// assignments 缓存 (router uid, cascade id) 的 AssignModel 解析结果：
	// assignment jwt 绑 cascade_id（上游实测），同会话内复用省去
	// 每请求一次的解析往返。
	assignmentsMu sync.Mutex
	assignments   map[string]resolvedAssignment
	// assignmentsFetch 登记同键的在飞 AssignModel 调用：同会话并发
	// 请求共享一次解析（模式同 modelsFetch），等待者收 done 后直接
	// 读 flight 上的共享结果，不各发一次 RPC。
	assignmentsFetch map[string]*assignFlight
	// detached 是完成缓存：客户端断开后仍在后台续命的流按语义请求
	// 键登记，同键重试重放已缓冲事件或挂接追帧（见 detached.go）。
	// 号池下逐 lane 各持一份——重试经 SessionAffinity 钉回同 lane
	// 才命中，换 lane 自然未命中走新上游。
	detached *detachedRegistry
}

// resolvedAssignment 是 AssignModel 对单个 router uid 的解析结果。
type resolvedAssignment struct {
	modelUID string
	jwt      string
}

// assignFlight 是一次在飞 AssignModel 调用的共享句柄：done 关闭前
// result/err 已写定（close 建立 happens-before），同键等待者直接取
// 共享结果——失败也随结果广播，不产生逐个重试的串行风暴。
type assignFlight struct {
	done   chan struct{}
	result resolvedAssignment
	err    error
}

// upstreamLink 是一次「上游端点」的固化产物：stream/api 两个 connect
// client 与焐池 connWarmer 绑在同一份 base_url/proxy/force_http1 与同一
// 个 *http.Transport 上。endpoint 配置热应用时整体重建、原子换指针；
// 旧 link 被在途调用持有，直到引用自然散尽。
type upstreamLink struct {
	// transport 是底层拨号/代理 transport，warmer 与两个 client 共享；
	// 退役时 CloseIdleConnections 收掉 idle 池（在途流不受影响）。
	transport *http.Transport
	// stream 无 Client.Timeout（SSE 长连接靠 Transport 层超时兜底）；
	// api 有 610s 整体超时，用于模型目录等普通调用。
	stream devinprotoconnect.ApiServerServiceClient
	api    devinprotoconnect.ApiServerServiceClient
	// warmer 焐住 transport 的 idle 连接池，省掉每请求的 TCP+TLS 握手段。
	warmer *connWarmer
}

var _ adapter.Adapter = (*Adapter)(nil)

// New 创建 Devin adapter。
func New(config Config) (*Adapter, error) {
	if strings.TrimSpace(config.Endpoint.BaseURL) == "" {
		return nil, errors.New("devin base URL is required")
	}
	// token 允许为空：它是运行时字段——unauthenticated 自愈经
	// TokenSource 重读、config reload 热应用都能补进。启动期强校验
	// 会让「先起服务后配凭据」变成没有 reload 端点的死路。
	if strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("devin model is required")
	}
	adapter := &Adapter{
		config:         config,
		token:          config.Identity.Token,
		modelsCacheTTL: 5 * time.Minute,
		gate:           newRateGate(config.Gate, config.GateStateStore, store.GateStateKey(config.Identity.Name)),
		assignments:    make(map[string]resolvedAssignment),
		detached:       newDetachedRegistry(config.GateStateStore, config.Identity.Name),
	}
	link, err := newUpstreamLink(config, adapter.currentToken)
	if err != nil {
		return nil, err
	}
	adapter.linkPtr.Store(link)
	if adapter.token == "" && strings.TrimSpace(config.Identity.APIKey) != "" {
		// api_key-only lane：声明侧没有可服役凭据，第一发请求必然
		// unauthenticated——启动即铸一枚 minted，省掉这发献祭请求。
		// 失败不拦启动：minted 留空，流量进来走 reloadToken 惰性路径。
		if minted, mintErr := adapter.mintSessionToken(strings.TrimSpace(config.Identity.APIKey)); mintErr == nil {
			adapter.minted = minted
		} else {
			slog.Warn("initial session token mint failed", "lane", config.Identity.Name, "error", mintErr)
		}
	}
	adapter.warm = newCacheWarmer(adapter, config.Warm)
	// 交接播种：REUSEPORT 双进程重叠期前任完成的脱钩条目经
	// detached_blobs 灌回本 lane——同键重试在新进程命中即重放，
	// 不再付一次静默上游再生。台账缺席时空转不拦启动。
	adapter.detached.seed()

	return adapter, nil
}

// newUpstreamLink 按端点参数构建上游调用束：transport 经 tokenFunc 每次
// 请求取凭据（unauthenticated 自愈与 token 热应用原地生效，无需重建）。
// proxy 串非法等构建失败返回 error，调用方整体不提交。
func newUpstreamLink(config Config, tokenFunc func() string) (*upstreamLink, error) {
	base, err := httpproxy.NewTransport(config.Endpoint.Proxy, config.Endpoint.ForceHTTP1)
	if err != nil {
		return nil, fmt.Errorf("create proxy transport: %w", err)
	}
	transport := upstream.NewBasicAuthTransportFunc(base, tokenFunc)

	// 上行链路（直连 GCP）单连接吞吐实测仅 ~200KB/s，而 chat 请求体重发
	// 全量上下文常达数百 KB——请求体 gzip 实测把建流到首字从 ~5s 压回 ~1.5s。
	// 用 BestSpeed 档替代默认档：对数百 KB 的 proto 文本压缩率同量级，
	// 压缩 CPU 省约 2/3（profiler 实测默认档占数据面 ~21%）；解压侧工厂
	// 与 connect 内置默认一致，仅替换注册项。
	gzipSend := []connect.ClientOption{
		connect.WithAcceptCompression("gzip",
			func() connect.Decompressor { return &gzip.Reader{} },
			func() connect.Compressor {
				zw, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
				return zw
			}),
		connect.WithSendCompression("gzip"),
	}

	// SSE 流需要长期保持连接，不能设置 Client.Timeout；
	// 但 Transport 层的 ResponseHeaderTimeout 已限制首包等待时间。
	stream := devinprotoconnect.NewApiServerServiceClient(&http.Client{Transport: transport}, config.Endpoint.BaseURL, gzipSend...)

	// 普通 API 调用（如模型目录）设置整体超时，避免慢请求长时间占用 goroutine；
	// 需要大于 ResponseHeaderTimeout，给 body 读取留余量。
	apiHTTPClient := &http.Client{Transport: transport, Timeout: 610 * time.Second}
	api := devinprotoconnect.NewApiServerServiceClient(apiHTTPClient, config.Endpoint.BaseURL, gzipSend...)

	return &upstreamLink{
		transport: base,
		stream:    stream,
		api:       api,
		warmer:    newConnWarmer(base, config.Endpoint.BaseURL),
	}, nil
}

// link 返回当前生效的上游调用束快照；New 之后恒非 nil。
func (adapter *Adapter) link() *upstreamLink {
	return adapter.linkPtr.Load()
}

// Close 停掉焐池协程并收掉 transport 的 idle 连接池（与 finishConfigApply
// 退役旧 link 同一卫生动作）；进程退出是最兜底的生命周期。
func (adapter *Adapter) Close() {
	adapter.warm.Close()
	link := adapter.link()
	link.warmer.Close()
	link.transport.CloseIdleConnections()
}

// BeginDrain 实现 app 排空钩子（可选接口，App.BeginDrain 经断言调用）：
// 排空起点即停发一切保温 ping——排空语义是不再制造新上游工作，保留表
// 留作 stats 观测，条目自然到期退役。脱钩缓存同步闩门：此刻登记的条目
// 随进程退出蒸发，挂接方永远来不了，拒收让断连流直接随客户端死掉。
func (adapter *Adapter) BeginDrain() {
	adapter.warm.BeginDrain()
	adapter.detached.draining.Store(true)
}

// FlushPendingWindows 冲刷闸门窗口行重放缓冲（排空收尾的 best-effort
// 落库）：同步直写，ctx 预算内写不完的行随进程退出丢弃。
func (adapter *Adapter) FlushPendingWindows(ctx context.Context) {
	adapter.gate.FlushPendingWindows(ctx)
}

// currentToken 返回当前生效的上游凭据：minted（APIKey 铸出的 session
// token）非空时优先，否则回落声明侧 token。
func (adapter *Adapter) currentToken() string {
	adapter.tokenMu.RLock()
	defer adapter.tokenMu.RUnlock()
	if adapter.minted != "" {
		return adapter.minted
	}
	return adapter.token
}

// TokenFunc 返回读取当前凭据的函数，供面板等共享同一上游账号的组件
// 跟随 adapter 的 unauthenticated 自愈结果——凭据续期后各方拿到的是
// 同一份新 token，而不是启动时的静态快照。
func (adapter *Adapter) TokenFunc() func() string {
	return adapter.currentToken
}

// CurrentConfig 返回当前生效配置的读快照。
func (adapter *Adapter) CurrentConfig() Config {
	adapter.configMu.RLock()
	defer adapter.configMu.RUnlock()
	return adapter.config
}

// Aliases 返回当前生效的模型别名映射，供面板做目录缺席校验。
func (adapter *Adapter) Aliases() map[string]string {
	return adapter.CurrentConfig().Aliases
}

// GateStats 返回速率闸门状态快照，供面板 stats 端点透出。
func (adapter *Adapter) GateStats() GateStats {
	return adapter.gate.stats()
}

// WarmStats 返回前缀保温簿记快照，供 /admin/runtime-metrics 透出。
func (adapter *Adapter) WarmStats() WarmStats {
	return adapter.warm.stats()
}

// DetachedStats 返回脱钩完成缓存快照，供 /admin/runtime-metrics 透出。
func (adapter *Adapter) DetachedStats() DetachedStats {
	return adapter.detached.stats()
}

// EvictDetachedByOriginDir 按来源调试目录清出脱钩条目：面板 abort 路径
// 补刀 abort-after-detach 残留窗——detach 落册与请求出 activeDirs 之间
// 的 µs 窗口内 abort 到达时，cancel 对 WithoutCancel 的后台泵已无效，
// 被掐死的生成必须移出缓存，否则同键重试会重放尸体。
func (adapter *Adapter) EvictDetachedByOriginDir(dir string) {
	adapter.detached.evictByOriginDir(dir)
}

// ApplyConfig 热应用新配置：读侧每次请求取快照的字段（model、aliases、
// client_*）与闸门参数/token 直接换值即生效；烤进 transport 的
// base_url/proxy/force_http1 变化时整体重建上游调用束并原子换指针，
// 在途调用持旧引用跑完。返回的列表只含值发生变化的字段。
func (adapter *Adapter) ApplyConfig(next Config) (applied []string, err error) {
	adapter.configMu.Lock()
	prev, newLink, err := adapter.commitConfigLocked(next)
	adapter.configMu.Unlock()
	if err != nil {
		return nil, err
	}
	return adapter.finishConfigApply(prev, next, newLink), nil
}

// UpdateConfig 在 configMu 下克隆当前配置交给 mutate 改字段、再走
// ApplyConfig 同一提交路径——克隆与提交之间插不进另一场 ApplyConfig，
// 消灭面板单字段热改与 config reload 的 lost-update。
// 返回的 applied 列表与 ApplyConfig 同语义。
func (adapter *Adapter) UpdateConfig(mutate func(*Config) error) (applied []string, err error) {
	adapter.configMu.Lock()
	next := adapter.config
	if err := mutate(&next); err != nil {
		adapter.configMu.Unlock()
		return nil, err
	}
	prev, newLink, err := adapter.commitConfigLocked(next)
	adapter.configMu.Unlock()
	if err != nil {
		return nil, err
	}
	return adapter.finishConfigApply(prev, next, newLink), nil
}

// commitConfigLocked 是 ApplyConfig/UpdateConfig 共享的持锁段：prev
// 快照、运行时字段继承、端点三件套变化时预构建新调用束（先构建后提交：
// proxy 串非法等失败整体返回错误，旧配置继续服役）、换值。调用方必须
// 持 configMu；解锁后的收尾见 finishConfigApply。
func (adapter *Adapter) commitConfigLocked(next Config) (prev Config, newLink *upstreamLink, err error) {
	prev = adapter.config
	// GateStateStore 是运行时句柄而非配置值：不归热应用管，沿用旧值。
	next.GateStateStore = prev.GateStateStore
	if prev.Endpoint != next.Endpoint {
		newLink, err = newUpstreamLink(next, adapter.currentToken)
		if err != nil {
			return prev, nil, err
		}
	}
	adapter.config = next
	return prev, newLink, nil
}

// finishConfigApply 是解锁后的后提交段：新调用束原子换指针并回收旧
// transport、回写 token/闸门/保温参数、按 prev→next 差集算 applied。
func (adapter *Adapter) finishConfigApply(prev, next Config, newLink *upstreamLink) (applied []string) {
	if newLink != nil {
		old := adapter.linkPtr.Swap(newLink)
		// 旧 transport 的 idle 池收掉；在途流持旧 client 引用跑完。
		old.transport.CloseIdleConnections()
		// warmer 停表要等进行中的 warmOnce（最坏 ~15s），异步收不堵 reload。
		go old.warmer.Close()
	}
	if prev.Endpoint.BaseURL != next.Endpoint.BaseURL || prev.Identity.Token != next.Identity.Token {
		// assignment jwt 绑 cascade_id 且只认签发它的端点与凭据：换端点
		// 或换账号后旧缓存若被复用会撞 jwt↔account 校验（permission_denied
		// 且自愈救不回），清空强制重 assign——清缓存只付一次重解析，
		// 方向安全。assignmentsFetch 同清：在飞调用的提交以「flight 仍是
		// 注册项」为前提，清表即让旧端点/旧凭据在飞的解析结果不落缓存。
		adapter.assignmentsMu.Lock()
		clear(adapter.assignments)
		clear(adapter.assignmentsFetch)
		adapter.assignmentsMu.Unlock()
	}

	if prev.Model != next.Model {
		applied = append(applied, "devin.model")
	}
	if !maps.Equal(prev.Aliases, next.Aliases) {
		applied = append(applied, "devin.aliases")
	}
	if prev.ClientName != next.ClientName {
		applied = append(applied, "devin.client_name")
	}
	if prev.ClientVersion != next.ClientVersion {
		applied = append(applied, "devin.client_version")
	}
	if prev.ClientOS != next.ClientOS {
		applied = append(applied, "devin.client_os")
	}
	if prev.Identity.Token != next.Identity.Token {
		adapter.tokenMu.Lock()
		adapter.token = next.Identity.Token
		// 声明凭据换值即夺回服役位：minted 是按旧声明铸出的，留下会
		// 让新 token 永不服役。
		adapter.minted = ""
		adapter.tokenMu.Unlock()
		applied = append(applied, "devin.accounts."+next.Identity.Name+".token")
	}
	if prev.Identity.APIKey != next.Identity.APIKey {
		// mint key 换值意味着铸币身份可能换号——旧 key 铸出的 minted
		// 一并作废，下一次需要时按新 key 重铸。
		adapter.tokenMu.Lock()
		adapter.minted = ""
		adapter.tokenMu.Unlock()
		applied = append(applied, "devin.accounts."+next.Identity.Name+".api_key")
	}
	adapter.gate.setParams(next.Gate)
	if prev.Gate.MaxRPM != next.Gate.MaxRPM {
		applied = append(applied, "devin.max_rpm")
	}
	if prev.Gate.MaxHold != next.Gate.MaxHold {
		applied = append(applied, "devin.gate_max_hold_seconds")
	}
	if prev.Gate.DripInterval != next.Gate.DripInterval {
		applied = append(applied, "devin.gate_drip_interval_seconds")
	}
	if prev.Gate.DefaultLatch != next.Gate.DefaultLatch {
		applied = append(applied, "devin.gate_default_latch_seconds")
	}
	if prev.Gate.WindowOffset != next.Gate.WindowOffset {
		applied = append(applied, "devin.gate_window_offset_seconds")
	}
	if prev.Gate.WindowGuard != next.Gate.WindowGuard {
		applied = append(applied, "devin.gate_window_guard_seconds")
	}
	if prev.Gate.BgMaxHold != next.Gate.BgMaxHold {
		applied = append(applied, "devin.gate_bg_max_hold_seconds")
	}
	if prev.Gate.BgReserveMargin != next.Gate.BgReserveMargin {
		applied = append(applied, "devin.gate_bg_reserve_margin")
	}
	adapter.warm.setParams(next.Warm)
	if prev.Warm.Enabled != next.Warm.Enabled {
		applied = append(applied, "devin.warm_prefix_enabled")
	}
	if prev.Warm.Interval != next.Warm.Interval {
		applied = append(applied, "devin.warm_prefix_interval_seconds")
	}
	if prev.Warm.JitterRatio != next.Warm.JitterRatio {
		applied = append(applied, "devin.warm_prefix_jitter_ratio")
	}
	if prev.Warm.MaxStreams != next.Warm.MaxStreams {
		applied = append(applied, "devin.warm_prefix_max_streams")
	}
	if prev.Warm.MaxRetainedMB != next.Warm.MaxRetainedMB {
		applied = append(applied, "devin.warm_prefix_max_retained_mb")
	}
	if prev.Warm.MinPrefixTokens != next.Warm.MinPrefixTokens {
		applied = append(applied, "devin.warm_prefix_min_prefix_tokens")
	}
	if prev.Warm.BlockedMaxIdle != next.Warm.BlockedMaxIdle {
		applied = append(applied, "devin.warm_prefix_blocked_max_idle_seconds")
	}
	if prev.Warm.UserPacedMaxIdle != next.Warm.UserPacedMaxIdle {
		applied = append(applied, "devin.warm_prefix_userpaced_max_idle_seconds")
	}
	if prev.Warm.SubDoneMaxIdle != next.Warm.SubDoneMaxIdle {
		applied = append(applied, "devin.warm_prefix_subdone_max_idle_seconds")
	}
	if prev.Warm.UnknownMaxIdle != next.Warm.UnknownMaxIdle {
		applied = append(applied, "devin.warm_prefix_unknown_max_idle_seconds")
	}
	if !slices.Equal(prev.Warm.BlockedNames, next.Warm.BlockedNames) {
		applied = append(applied, "devin.warm_prefix_blocked_names")
	}
	if !slices.Equal(prev.Warm.UserPacedNames, next.Warm.UserPacedNames) {
		applied = append(applied, "devin.warm_prefix_userpaced_names")
	}
	if prev.Endpoint.BaseURL != next.Endpoint.BaseURL {
		applied = append(applied, "devin.base_url")
	}
	if prev.Endpoint.Proxy != next.Endpoint.Proxy {
		applied = append(applied, "devin.proxy")
	}
	if prev.Endpoint.ForceHTTP1 != next.Endpoint.ForceHTTP1 {
		applied = append(applied, "devin.force_http1")
	}
	// 池级调度旋钮：config 整体换值即生效（affinityTTL/NoteQuotaSample
	// 每次经 CurrentConfig 现读），无 adapter 侧回写动作。
	if prev.SessionAffinityTTLSeconds != next.SessionAffinityTTLSeconds {
		applied = append(applied, "devin.session_affinity_ttl_seconds")
	}
	if prev.QuotaLowThresholdPercent != next.QuotaLowThresholdPercent {
		applied = append(applied, "devin.quota_low_threshold_percent")
	}
	if prev.NoProgressTimeout != next.NoProgressTimeout {
		applied = append(applied, "devin.no_progress_timeout_seconds")
	}
	if prev.PreEventNoProgressTimeout != next.PreEventNoProgressTimeout {
		applied = append(applied, "devin.pre_event_no_progress_timeout_seconds")
	}
	return applied
}

// reloadToken 在 unauthenticated 后尝试换出一份新凭据，两级来源：
// 声明侧 TokenSource 重读（CLI 续期/配置热改），拿不出新值再回落到
// APIKey mint（durable key 现场铸 session token）。声明侧给出不同
// token 时 minted 一并作废——声明值夺回服役位。换出成功返回 true，
// 调用方据此重试；拿不到时记 Warn——凭据静默失效是排障天敌。
func (adapter *Adapter) reloadToken() bool {
	cfg := adapter.CurrentConfig()
	var sourceToken string
	if source := cfg.Identity.TokenSource; source != nil {
		sourceToken = strings.TrimSpace(source())
	}
	adapter.tokenMu.Lock()
	if sourceToken != "" && sourceToken != adapter.token {
		adapter.token = sourceToken
		adapter.minted = ""
		adapter.tokenMu.Unlock()
		slog.Info("reloaded upstream token after unauthenticated error")
		return true
	}
	// 记下换出前的生效凭据快照：mint 在锁外进行，期间另一个并发自愈
	// 若已换上新凭据，提交时凭快照比对跳过覆盖。
	stale := adapter.minted
	if stale == "" {
		stale = adapter.token
	}
	adapter.tokenMu.Unlock()

	apiKey := strings.TrimSpace(cfg.Identity.APIKey)
	if apiKey == "" {
		if cfg.Identity.TokenSource == nil {
			return false
		}
		if sourceToken == "" {
			slog.Warn("upstream unauthenticated but TokenSource returned no token")
		} else {
			slog.Warn("upstream unauthenticated and TokenSource returned the same token; credential refresh did not help")
		}
		return false
	}
	// 铸币段进 mintMu 串行：等锁期间另一路自愈（声明侧重读或先到的
	// mint）可能已换上新凭据——现值不同于快照即视同步成功，重试吃
	// currentToken 现值，不再各付一发铸币 RPC。
	adapter.mintMu.Lock()
	defer adapter.mintMu.Unlock()
	if adapter.currentToken() != stale {
		return true
	}
	minted, err := adapter.mintSessionToken(apiKey)
	if err != nil {
		slog.Warn("session token mint via api_key failed", "lane", cfg.Identity.Name, "error", err)
		return false
	}
	adapter.tokenMu.Lock()
	defer adapter.tokenMu.Unlock()
	current := adapter.minted
	if current == "" {
		current = adapter.token
	}
	if current != stale {
		// 并发自愈已换上新凭据：mint 出的 token 不必再服役，但仍算
		// 自愈成功——重试直接吃 currentToken 现值。
		return true
	}
	adapter.minted = minted
	slog.Info("minted new session token via api_key after unauthenticated error", "lane", cfg.Identity.Name)
	return true
}

// seatMintTimeout 是 mint 单程调用的预算：它是 lane 自愈的同步段，
// 超了只会让调用方多吃一次失败，不会比上游响应慢更糟。
const seatMintTimeout = 30 * time.Second

// mintSessionToken 用 durable api_key 经 SeatManagementService/
// GetSelfDevinSessionToken 现场铸一枚新 session token。请求形状与
// Devin Desktop _ensureDevinSessionToken 逆向结论一致：metadata.api_key
// 与 X-Api-Key 头放同一枚 durable key（body 缺 api_key 会被判
// invalid_argument）；metadata 身份固定 windsurf——seat 系 RPC 不认
// lane 的 client_* 指纹（实测低版本号会吃 500）。走 lane 自己的
// transport，代理设置与 chat 路径同源。
func (adapter *Adapter) mintSessionToken(apiKey string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"api_key":           apiKey,
			"extension_name":    "windsurf",
			"extension_version": "1.48.2",
			"ide_name":          "windsurf",
			"ide_version":       "1.48.2",
			"locale":            "en",
			"os":                "windows",
		},
	})
	if err != nil {
		return "", err
	}
	base := strings.TrimRight(adapter.CurrentConfig().Endpoint.BaseURL, "/")
	ctx, cancel := context.WithTimeout(context.Background(), seatMintTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/exa.seat_management_pb.SeatManagementService/GetSelfDevinSessionToken",
		bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("X-Api-Key", apiKey)
	resp, err := (&http.Client{Transport: adapter.link().transport}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		// 错误体可能回显请求字段——入日志前先擦掉 durable key。
		detail := strings.ReplaceAll(string(raw), apiKey, "[redacted]")
		return "", fmt.Errorf("GetSelfDevinSessionToken HTTP %d: %s", resp.StatusCode, truncateRunes(detail, 300))
	}
	var parsed struct {
		SessionToken string `json:"sessionToken"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode GetSelfDevinSessionToken: %w", err)
	}
	token := strings.TrimSpace(parsed.SessionToken)
	if token == "" {
		return "", errors.New("GetSelfDevinSessionToken: empty sessionToken")
	}
	return token, nil
}

// isUnauthenticated 判断错误是否为上游 unauthenticated（凭据失效）。
func isUnauthenticated(err error) bool {
	var connectErr *connect.Error
	return errors.As(err, &connectErr) && connectErr.Code() == connect.CodeUnauthenticated
}

// ResolveModelAlias 把客户端模型名改写为上游 uid：精确命中 → 折叠命中 →
// "*" 兜底键；全部未中时原样返回。折叠比较对大小写不敏感且忽略
// '-'/'_'/'.'，覆盖客户端拼写与目录 uid 的标点变体（'GLM-5-3-Flash'、
// 'glm_5_3_flash' 同命中 'glm-5.3-flash' 键）——仅靠大小写折叠时这类
// 变体逐字上行被上游拒。aliases 来自 config 加载期归一化（键已 trim、
// 链式已展开、折叠等价键被拒——折叠实现同源 config.FoldModelKey），
// 折叠兜底只做线性扫描——别名表规模小，
// 且只在精确未命中时发生。导出供 cmd/probe 与代理保持同一路径语义。
func ResolveModelAlias(aliases map[string]string, model string) string {
	if target, ok := aliases[model]; ok {
		return target
	}
	folded := config.FoldModelKey(model)
	for name, target := range aliases {
		if config.FoldModelKey(name) == folded {
			return target
		}
	}
	if target, ok := aliases["*"]; ok {
		return target
	}
	return model
}

// Stream 将一份中间请求转换为 Devin RPC，并返回一份中间响应事件流。
// request 已在三个 DecodeRequest 末尾过一遍 context.Validate()——
// 唯一调用路径是解码后的 startStreamPump，这里不再重扫（单个
// arguments 的 json.Valid 曾被扫三次）。
func (adapter *Adapter) Stream(ctx context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	request, sanitizeHits := sanitizeRequest(request)
	cfg := adapter.CurrentConfig()
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = cfg.Model
	}
	model = ResolveModelAlias(cfg.Aliases, model)
	// env 把 ctx 走私值（闸门回执/让位探针/peers 登记表/记录器）捕成
	// 显式结构：之后的发送面一律经 attemptRunner 按 env 字段取用。
	env := attemptEnvFrom(ctx)
	recorder := env.recorder
	if request.ServerSearch != nil {
		// 服务端托管搜索侧请求（CC WebSearch）：不经 GetChatMessage，
		// 模型路由/图片校验与本次请求无关，同步执行搜索后返回预成形
		// 事件流——产出前的失败保持真实 HTTP 状态码语义。
		recorder.SetResolvedModel(model)
		return adapter.runServerSearch(ctx, request, model)
	}
	// 目录是 router 判定与能力位校验的依据；懒加载时此处补一次拉取。
	// 相位耗时记进 meta.models_fetch_ms：闸门指标看不到这段闸门前停滞
	// （含等待他人在飞拉取的陪等），缓存命中≈0。
	catalogAt := time.Now()
	adapter.ensureCatalog(ctx)
	recorder.NoteModelsFetchMS(time.Since(catalogAt).Milliseconds())
	// requestedUID 记下路由判定前的 uid：命中 router 时保温条目要用它
	// 重建 assignment jwt（绑 cascade_id），否则 ping 重放丢绑定。
	requestedUID := model
	model, assignmentJWT, err := adapter.resolveModelRouting(ctx, request, model)
	if err != nil {
		// AssignModel 同属上游建连期 RPC：传输断裂与语义拒绝分层。
		stage := debuglog.ErrStageDevinConnect
		if isTransientConnectError(err) {
			stage = debuglog.ErrStageDevinTransport
		}
		recorder.WriteError(stage, err)
		return nil, err
	}
	// 别名与路由判定到此完结：记下发上线 uid，进行中列表即刻
	// 呈现「请求名 → 实际 uid」，不必等响应身份回填。
	recorder.SetResolvedModel(model)
	// 完成缓存查找在一切上游动作之前：同键脱钩条目在场时整段建流
	// 路径（目录校验/构建/闸门/发送）都不发生——重放不消耗上游。
	detachKey := detachedRequestKey(request, model)
	if entry := adapter.detached.lookup(detachKey); entry != nil {
		originDir, state, buffered := entry.marker()
		detail := map[string]any{
			"key":             detachKey,
			"origin_dir":      originDir,
			"state":           state.String(),
			"buffered_events": buffered,
		}
		recorder.AppendJSONL(debuglog.StageDevinResponse, "detached_attach", detail)
		recorder.NoteDetachedEvent("detached_attach", detail)
		return &attachStream{entry: entry}, nil
	}
	// 本地查无此键时探测兄弟 lane 的登记表（号池经 ctx 挂接；裸 New()
	// 与单 lane 无 peers 零成本）：同键请求落错 lane 此前在两侧都不留
	// 痕——本 lane attach_misses 不动、owner 条目只能等移除时记不区分
	// 原因的孤儿。peek 只读探测命中即记账：本 lane 记 cross_lane_misses
	// 与事件环，04 留 marker 行与 pool_candidates 的选号现场互证；
	// owner 条目置 sawCrossLaneRetry 供移除时拆出「来错门」孤儿档。
	// 多 holder（同键条目同时存在多条 lane）各记一次不吞。
	if detachKey != "" {
		for owner, reg := range env.peers {
			if reg == adapter.detached {
				continue
			}
			state, usable, ok, originDir := reg.peek(detachKey)
			if !ok {
				continue
			}
			adapter.detached.noteCrossLaneMiss(detachKey, owner, originDir, state)
			detail := map[string]any{
				"key":         detachKey,
				"owner_lane":  owner,
				"owner_state": state.String(),
				"usable":      usable,
			}
			recorder.AppendJSONL(debuglog.StageDevinResponse, "detached_cross_lane_miss", detail)
			recorder.NoteDetachedEvent("detached_cross_lane_miss", detail)
		}
	}
	// 能力校验与缺席告警作用在解析后的真实 uid 上——router 条目自己的
	// 目录能力位与最终承担请求的模型无关。
	adapter.warnIfModelAbsentFromCatalog(model)
	if err := adapter.validateImagesForModel(request, model); err != nil {
		// 本地校验拒绝在起源点记 request_build：错误继续冒泡会经
		// 流层错误出口被盖成 provider_stream。
		recorder.WriteError(debuglog.ErrStageRequestBuild, err)
		return nil, err
	}
	// binding 携带每次调用可变的字段：model 是别名/路由改写后的最终
	// uid，token 现取（自愈后重试会换），jwt 是本次路由的绑定产物。
	binding := callBinding{Token: adapter.currentToken(), Model: model, ModelAssignmentJWT: assignmentJWT}
	// 保温 lineage 键在本请求定稿后计算：sanitize 后的 request 与解析后
	// 的 wire uid 是重放等价性判定的全部输入。面板探活走真实 /v1 管线
	// 但不属于客户端会话（无 SessionKey、内容固定会撞同一 fallback
	// lineage），按 client_request_id 豁免出簿记。
	warmKey := adapter.warm.keyOf(request, model)
	if recorder.ClientRequestID() == debuglog.ProbeClientRequestID {
		warmKey = warmLineageKey{}
	}
	warmRouter := ""
	if assignmentJWT != "" {
		warmRouter = requestedUID
	}
	// runner 持有本次开流的全部发送输入：首发的分片/序号记账与各处
	// 续试重发（自愈/重开/续轮/续传）都走它，不再各抄骨架。
	runner := &attemptRunner{
		adapter: adapter, env: env, request: request, cfg: cfg,
		binding: binding, warmKey: warmKey,
	}
	protoRequest, repairs, err := buildRequest(request, cfg, binding)
	if err != nil {
		recorder.WriteError(debuglog.ErrStageRequestBuild, err)
		return nil, err
	}
	repairs.SanitizeHits = sanitizeHits
	recorder.SetRepairs(repairs)
	runner.noteSend(protoRequest)
	// streamBase 剥离客户端取消、保留 ctx 值（recorder/请求分类）：
	// detached 语义要求客户端断开后上游泵继续活着（见 detach），
	// connect 流的生命周期绑在开流 ctx 上，必须从剥离后的基底派生。
	streamBase := context.WithoutCancel(ctx)
	// streamCtx 由 responseStream 持有：看门狗判死、客户端断开或
	// 后台泵超时时 cancel 是唯一打断泵协程内阻塞 Receive 的手段。
	streamCtx, cancel := context.WithCancel(streamBase)
	// 开流放进协程里跑：主 goroutine 在 select 里同时盯客户端 ctx。
	// 客户端在闸门排队/建连期断开时 streamCtx 不随客户端取消（它从
	// streamBase 派生），必须主动 cancel 打断在飞 RPC 并等 goroutine
	// 收尾（闸门槽位随返回释放）——否则这次开流会漏成一条无人消费
	// 的上游流。
	type openResult struct {
		stream *connect.ServerStreamForClient[devinproto.GetChatMessageResponse]
		err    error
	}
	openCh := make(chan openResult, 1)
	go func() {
		opened, err := runner.send(streamCtx, protoRequest, false)
		if err != nil && isUnauthenticated(err) && adapter.reloadToken() {
			// 凭据自愈：CLI 会续期改写 credentials.toml，重读 token 后
			// 用新凭据重建请求重试一次。token 未变化时不重试。
			if resent, resendErr := runner.resend(streamCtx, "unauthenticated: token reloaded", nil, false); resendErr == nil {
				opened, err = resent, nil
			} else if !errors.Is(resendErr, errAttemptBuild) {
				// 重发打出去又败的错误顶替原 unauthenticated 上报；
				// 重建失败（errAttemptBuild，retry_failed 已留痕）
				// 保留原始失败。
				err = resendErr
			}
		}
		openCh <- openResult{opened, err}
	}()
	var stream *connect.ServerStreamForClient[devinproto.GetChatMessageResponse]
	select {
	case result := <-openCh:
		stream, err = result.stream, result.err
	case <-ctx.Done():
		cancel()
		<-openCh
		err = context.Cause(ctx)
	}
	if err != nil {
		// 判父 ctx 而非 streamCtx：cancel() 后 streamCtx 必为 canceled，
		// 查它会让整个 WriteError 块成为死代码（rate_gate 阶段名全丢）。
		// 父 ctx 已取消（客户端断连/排空）时不记——外层记
		// client_disconnected，这里抢占首个失败点会把它顶掉。
		parentDone := ctx.Err() != nil
		cancel()
		// 闸门拒绝已在 gate.wait 失败处记 rate_gate；这里只剩上游建连
		// 失败——传输断裂与上游语义拒绝（devin_connect）分层，前者是
		// 连接/帧级事故，后者才是上游配额或参数动作。
		if !parentDone {
			stage := debuglog.ErrStageDevinConnect
			if isTransientConnectError(err) {
				// 建连期的传输断裂与中流断裂同层，不混进上游语义拒绝桶。
				stage = debuglog.ErrStageDevinTransport
			}
			recorder.WriteError(stage, err)
		}
		// 错误分类记录随车携带——下游经 llm.Classify 取回结构事实，
		// 不再按文本反推。
		return nil, llm.Classify(err)
	}
	// 客户端请求成功开流才更新 retained——续试变体（continueEmpty 追加
	// 的合成 "continue"、extend 的内部编码续轮/续传）客户端下一发不会
	// 逐字节复现，存了就是保温死分支。
	adapter.warm.retain(warmKey, request, model, warmRouter)
	serverTools := serverToolNames(request.Tools)
	decoder := newResponseDecoder(model, request.StopSequences, customToolNames(request.Tools), serverTools)
	// postProgressTimeout 解析 post-content 无进度档（工具调用参数的
	// 长静默计算）——cfg <=0 回落默认；Stream 构造的流恒有值，测试
	// 裸流留零走 deadlines.progress 的回落档。preProgressTimeout 是
	// 它的 pre 对偶档，同法回落 upstreamNoProgressTimeout。
	postProgressTimeout := cfg.NoProgressTimeout
	if postProgressTimeout <= 0 {
		postProgressTimeout = defaultPostProgressTimeout
	}
	preProgressTimeout := cfg.PreEventNoProgressTimeout
	if preProgressTimeout <= 0 {
		preProgressTimeout = upstreamNoProgressTimeout
	}
	response := &responseStream{
		frames:   pumpUpstream(streamCtx, stream),
		cancel:   cancel,
		decoder:  decoder,
		recorder: recorder,
		gate:     adapter.gate,
		warm:     adapter.warm,
		warmKey:  warmKey,
		deadlines: streamDeadlines{
			postProgress: postProgressTimeout,
			preProgress:  preProgressTimeout,
			// 累计静默上限的锚点：流刚建立，距首个上游发送只有建流
			// 往返，pre-event 死等预算从这一刻起跨换流累计。
			firstSentAt: time.Now(),
		},
		detachKey: detachKey,
		registry:  adapter.detached,
		entry:     &detachedEntry{notify: make(chan struct{})},
		// 上游流建立后、产出任何内容前的失败允许整体重发一次：
		// 传输层断裂与 unauthenticated（凭据自愈）重试能改变结果；
		// 上游语义拒绝（参数校验/权限/限流）重试只会复现同样失败，直接放行。
		reopen: func(cause error, continueEmpty bool) (<-chan upstreamFrame, context.CancelFunc, error) {
			var causeText string
			var mutate func(*llm.RequestMessages)
			switch {
			case continueEmpty:
				// 空 end_turn（有 stopReason 零内容，上游实测存在的退化形态）：
				// 追加 "continue" 用户消息重发一次，让模型在同一上下文续说。
				mutate = func(request *llm.RequestMessages) {
					request.Messages = append(append([]llm.Message{}, request.Messages...),
						llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "continue"}}})
				}
				causeText = "empty end_turn: continue"
				slog.Warn("reopening stream: upstream ended with empty content")
			case isTransientConnectError(cause):
				causeText = "transport: " + cause.Error()
				slog.Warn("reopening stream: transport error before first content", "error", cause)
			case isUnauthenticated(cause) && adapter.reloadToken():
				causeText = "unauthenticated: token reloaded"
				slog.Info("reopening stream: token reloaded after unauthenticated")
			default:
				return nil, nil, cause
			}
			retryCtx, retryCancel := context.WithCancel(streamBase)
			reopened, err := runner.resend(retryCtx, causeText, mutate, continueEmpty)
			if err != nil {
				retryCancel()
				return nil, nil, err
			}
			return pumpUpstream(retryCtx, reopened), retryCancel, nil
		},
		newDecoder: func() *responseDecoder {
			return newResponseDecoder(model, request.StopSequences, customToolNames(request.Tools), serverTools)
		},
	}
	// extend 以「原始历史 + 追加消息」重发并返回播种旧内容的新流：
	// 托管续轮（handleServerCalls）追加 assistant 回显与结果消息；
	// 截断续传（tryResume）在其后再追加 "continue" 用户消息。续发
	// 清掉 tool_choice——首发期的指名/强制约束会让上游每跳都强发
	// 同一调用（实测 named web_search 滚到 hops 封顶）。
	response.extend = func(cause string, extra []llm.Message, seed []llm.Content) (<-chan upstreamFrame, context.CancelFunc, *responseDecoder, error) {
		nextCtx, nextCancel := context.WithCancel(streamBase)
		next, err := runner.resend(nextCtx, cause, func(request *llm.RequestMessages) {
			request.ToolChoice = nil
			request.Messages = append(append([]llm.Message{}, request.Messages...), extra...)
		}, false)
		if err != nil {
			nextCancel()
			return nil, nil, nil, err
		}
		nextDecoder := newResponseDecoder(model, request.StopSequences, customToolNames(request.Tools), serverTools)
		// start 先跑：partial 元数据初始化后再播种旧内容——客户端
		// 已见过本轮的 start，续发不产第二个（started 已置位，
		// Recv 里 pendingStart 为空）。
		nextDecoder.start()
		nextDecoder.partial.Content = slices.Clone(seed)
		return pumpUpstream(nextCtx, next), nextCancel, nextDecoder, nil
	}
	if len(serverTools) > 0 {
		// 托管工具声明在场才接管：模型发出 Server 调用时由
		// handleServerCalls 代执行（search）并续轮（extend）。
		// 搜索调用的请求记录用 searchN 词干：主文件与 attemptN 编号
		// 已被 chat 首发/续轮占用（见 runWebSearch 的 stem 说明）。
		searchSeq := 0
		response.search = func(ctx context.Context, query string, allowedDomains, blockedDomains []string, limit uint32) (webSearchOutcome, error) {
			searchSeq++
			stem := fmt.Sprintf("%s%d", debuglog.StageDevinSearchStem, searchSeq)
			return adapter.runWebSearch(ctx, query, allowedDomains, blockedDomains, limit, stem, warmKey)
		}
	}
	// 客户端哨兵：streamCtx 从 streamBase 派生不随客户端取消。断连时刻
	// 的脱钩判定有两个执行者——app 泵退场前的交班 Recv（投递点
	// ctx.Done 出口先驱动末次 Recv 再退）与本哨兵，经 detached CAS
	// 定序、先到者赢、后到者见已认领空转。哨兵兜住「泵不再进 Recv」的
	// 残留形态（如未来不排干 items 就退场的消费方）；泵侧交班才是
	// 定序保障——泵退出即蕴含判定已定，消费方排干到 close 才放
	// unwind 进 Complete。
	go response.watchClientCtx(ctx)
	return response, nil
}

// watchClientCtx 是客户端哨兵主体：ctx.Done 醒来先走 admitIntent 占位
// 登记——锁内阻塞段（tryResume/extend/托管搜索的 gate.wait+dial，内层
// maxConnectAttempts 重试，实测剩余 ~95s）会把持 mu 的判定推迟到段末，
// 且段末 finished=true 时 detachable() 已假、登记整段丢失。占位路径只用
// isTransientConnectError 判断错误是否为传输层断裂（可重试、记
// devin_transport）。connect-go 会把底层传输失败统一包成 connect.Error——
// RoundTrip/读写断 → CodeUnavailable（duplex_http_call.go），envelope 帧
// 被截断 → CodeInvalidArgument "protocol error: ..."，流中段裸 EOF →
// CodeUnknown——判据要看 unwrap 链里有没有 io/net 错误，而不是
// 「是不是 connect.Error」。链上不带底层错误的 connect.Error 才是上游
// 语义拒绝（unavailable 固定模板、invalid_argument 参数、
// resource_exhausted、permission_denied），重试只会复现同样失败。
// 例外：connect-go 对「线上字节不构成合法帧」的本地报错都不带 %w，unwrap
// 链干净，只能靠措辞认出——envelope 前缀截断（envelope.go:336）与帧体截断
// （envelope.go:361）译成 CodeInvalidArgument "protocol error: ..."，
// 垃圾 flag 字节（protocol_connect.go:890）译成 CodeInternal
// "protocol error: invalid envelope flags"；"protocol error:" 是它对本地
// 帧解析失败的固定措辞，上游语义错误经 EndStream 尾帧传达、不撞前缀。
// 垃圾前缀会误判进此分支，但重试一次确定性失败代价小，换覆盖全部帧级
// 解析失败形态。
// 另一族措辞：对端 http2 RST_STREAM/GOAWAY。connect-go 把 RST 尾缀
// code 映成语义 code（wrapIfRSTError：REFUSED_STREAM→unavailable、
// ENHANCE_YOUR_CALM→resource_exhausted、PROTOCOL_ERROR/INTERNAL_ERROR
// →internal、INADEQUATE_SECURITY→permission_denied），GOAWAY 建连期
// 以 unavailable 透出——映射 code 只是传输事件的近似，认文案里的本地
// http2 措辞（llm.IsHTTP2TransportError）。顺带说明盲区：上游对
// num_completions>1 回的是 EndStream 携带的 invalid_argument
// "protocol error: incomplete envelope: unexpected EOF"——与本分支本地
// 措辞同形，但该请求形状在本管线不可达（chat 解码面拒绝 n>1，
// responses/anthropic 协议无 n，wire 恒为 NumCompletions=1），即使
// 上游措辞再与本地产文撞车，代价仍是一次确定性重试。
func isTransientConnectError(err error) bool {
	// 调用方取消不是传输故障：context.DeadlineExceeded 自身实现
	// net.Error，不先短路会把客户端断连/超时误判成可重试的断线，
	// 既无谓重发又把 stage 错记成 devin_transport。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 已分类的语义记录（如闸门拒绝、本地校验）不是传输断裂——外层
	// fmt.Errorf 包装会让 unwrap 链上看不到 connect.Error，没有这条
	// 短路会把语义拒绝误判成可重试的断线。
	var failure *llm.Failure
	if errors.As(err, &failure) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return true
	}
	// http2 RST/GOAWAY 措辞先于 code 判定：映射 code（unavailable/
	// resource_exhausted/internal/permission_denied）与真实语义无关。
	if llm.IsHTTP2TransportError(connectErr.Message()) {
		return true
	}
	// h1 连接池形态（force_http1）：池复用到对端已关闭的空闲连接时报
	// "server closed idle connection"，失败发生在任何字节写出之前，
	// 与 RST/GOAWAY 同属传输断裂，重试安全。
	if llm.IsIdleConnClosedError(connectErr.Message()) {
		return true
	}
	code := connectErr.Code()
	return (code == connect.CodeInvalidArgument || code == connect.CodeInternal) &&
		strings.HasPrefix(connectErr.Message(), "protocol error:")
}

// validateImagesForModel 在本地尽早拒绝「无视觉能力模型 + 图片」组合，错误信息对客户端可读。
// 模型目录缓存中有该模型时以目录的 supports_images 为准（上游实测确实回
// invalid_argument），目录未覆盖时退回前缀启发式。
func (adapter *Adapter) validateImagesForModel(request llm.RequestMessages, model string) error {
	if !requestHasImages(request) {
		return nil
	}
	supported, known := adapter.catalogSupportsImages(model)
	if !known {
		supported = modelLikelySupportsImages(model)
	}
	if !supported {
		return &llm.Failure{Code: "invalid_argument", Message: fmt.Sprintf("model %q does not support image inputs (supports_images=false); use a vision-capable model or remove images", model)}
	}
	return nil
}

// catalogSupportsImages 查询模型目录缓存中该 uid 的图片能力。
// 第二个返回值表示目录是否包含该模型。
func (adapter *Adapter) catalogSupportsImages(model string) (supported bool, known bool) {
	adapter.modelsMu.RLock()
	defer adapter.modelsMu.RUnlock()
	for _, m := range adapter.models {
		if m.ID == model {
			return m.SupportsImages, true
		}
	}
	return false, false
}

// warnIfModelAbsentFromCatalog 在目录已加载且目标 uid 缺席时记 Warn。
// 实测 alias 指向死模型时上游只回模糊的 permission_denied: an internal
// error occurred——排障只能靠日志里的这条提示定位到 alias 目标。
// 目录未加载或缺席都放行：用户配置的 model 本就可以不在目录里。
func (adapter *Adapter) warnIfModelAbsentFromCatalog(model string) {
	adapter.modelsMu.RLock()
	defer adapter.modelsMu.RUnlock()
	if len(adapter.models) == 0 {
		return
	}
	for _, m := range adapter.models {
		if m.ID == model {
			return
		}
	}
	// 缺席是配置级事实（别名目标或 client_version 问题），按 uid 每进程
	// 警一次足够——别名改写后每个请求都路过这里，不去重会按请求频率刷屏。
	if _, loaded := adapter.warnedAbsentModels.LoadOrStore(model, struct{}{}); loaded {
		return
	}
	slog.Warn("model absent from upstream catalog; upstream will likely return a vague permission_denied",
		"model", model, "hint", "check devin.aliases target or bump devin.client_version")
}

// ensureCatalog 尽力保证模型目录已加载：router 判定、图片能力位校验与
// 缺席告警都以目录为依据，目录从未加载过时这些检查静默失效。
// TTL 缓存使命中期的调用只是读锁；拉取失败放行，维持「交给上游裁决」的旧行为。
func (adapter *Adapter) ensureCatalog(ctx context.Context) {
	// 调用方 ctx 已死（客户端断连/进程排空）时的失败是噪声不是信号。
	if _, err := adapter.ListModels(ctx); err != nil && ctx.Err() == nil {
		// 冷却窗内每个请求都拿到同一 modelsErr：告警按失败场次去重，
		// 一场冷却只打一条，否则窗内时长等于按请求频率刷屏。
		adapter.modelsMu.Lock()
		if adapter.modelsErrWarned {
			adapter.modelsMu.Unlock()
			return
		}
		adapter.modelsErrWarned = true
		adapter.modelsMu.Unlock()
		slog.Warn("model catalog unavailable; router detection skipped", "error", err)
	}
}

// resolveModelRouting 对目录里标了 is_model_router 的 uid 调 AssignModel
// 解出真实 model_uid 与绑定 cascade_id 的 assignment jwt——router uid
// 直连上游只回 unavailable: third-party model provider，伪装成瞬时错误
// 的永久失败。目录未覆盖该模型时按原样放行，交给上游裁决。
func (adapter *Adapter) resolveModelRouting(ctx context.Context, request llm.RequestMessages, model string) (resolved string, assignmentJWT string, err error) {
	adapter.modelsMu.RLock()
	isRouter := false
	for _, m := range adapter.models {
		if m.ID == model {
			isRouter = m.IsModelRouter
			break
		}
	}
	adapter.modelsMu.RUnlock()
	if !isRouter {
		return model, "", nil
	}
	// jwt 绑 cascade_id：必须用与本请求 wire 一致的派生值。
	_, cascadeID := deriveSessionIDs(request)
	// AssignModel 相位耗时记进 meta.assign_model_ms（含共享 flight 陪等）：
	// 闸门前停滞在 gate 指标里不可见，只有这里能量化。
	assignAt := time.Now()
	assignment, err := adapter.assignModel(ctx, model, cascadeID)
	debuglog.FromContext(ctx).NoteAssignModelMS(time.Since(assignAt).Milliseconds())
	if err != nil {
		return "", "", err
	}
	slog.Info("resolved model router via AssignModel", "router", model, "model", assignment.modelUID)
	return assignment.modelUID, assignment.jwt, nil
}

// preGateTimeout 是闸门前同步上游调用（AssignModel 路由解析、模型目录
// 拉取）的硬顶：这些调用在速率闸门之外跑，上游挂起时没有整形兜底——
// send-silent 故障实测 flight 等待者陪等近 60s、恢复后 ~1s 齐放。取值
// 低于 fg maxHold(30s)：超时失败在闸门排队预算内落定，号池下
// deadline_exceeded 经 failoverable 换 lane 重解析；同键等待者经
// flight.done 广播共享同一失败，不各陪一条超时。var 供测试缩短。
var preGateTimeout = 10 * time.Second

// assignModel 调上游 AssignModel 把 router uid 解析为真实模型 + assignment
// jwt，结果按 (router uid, cascade id) 缓存。错误分类见
// docs/upstream-protocol.md 路由节：非 router uid → invalid_argument，
// 不存在的 router → not_found。
//
// 同键并发收敛为单次上游调用（模式同 ListModels 的 modelsFetch）：在飞
// 调用 detach 自首发者 ctx——结果是键级共享状态，一个客户端断连不该
// 让全体等待者吃 context.Canceled；等待者吃自己的 ctx 可随时退出。
// detach 的在飞调用由 preGateTimeout 兜底，等待者经 done 广播共享
// 同一超时失败，不必各自设限。
// 提交只在 flight 仍是注册项时生效：配置清空（换端点/换凭据）后在飞
// 解析结果落进缓存就是把陈旧 jwt 借尸还魂。
func (adapter *Adapter) assignModel(ctx context.Context, routerUID, cascadeID string) (resolvedAssignment, error) {
	key := routerUID + "|" + cascadeID
	adapter.assignmentsMu.Lock()
	if cached, ok := adapter.assignments[key]; ok {
		adapter.assignmentsMu.Unlock()
		return cached, nil
	}
	if flight := adapter.assignmentsFetch[key]; flight != nil {
		adapter.assignmentsMu.Unlock()
		select {
		case <-flight.done:
			return flight.result, flight.err
		case <-ctx.Done():
			return resolvedAssignment{}, ctx.Err()
		}
	}
	if adapter.assignmentsFetch == nil {
		adapter.assignmentsFetch = make(map[string]*assignFlight)
	}
	flight := &assignFlight{done: make(chan struct{})}
	adapter.assignmentsFetch[key] = flight
	adapter.assignmentsMu.Unlock()

	result, err := adapter.callAssignModel(context.WithoutCancel(ctx), routerUID, cascadeID)

	adapter.assignmentsMu.Lock()
	if adapter.assignmentsFetch[key] == flight {
		delete(adapter.assignmentsFetch, key)
		if err == nil {
			// 有界缓存：会话级键随运行时长累积，触顶整体清空让会话重新解析。
			if len(adapter.assignments) >= 4096 {
				adapter.assignments = make(map[string]resolvedAssignment)
			}
			adapter.assignments[key] = result
		}
	}
	flight.result, flight.err = result, err
	adapter.assignmentsMu.Unlock()
	close(flight.done)
	return result, err
}

// callAssignModel 执行一次 AssignModel RPC 并整形结果；在飞去重、缓存
// 提交与失败广播都归 assignModel。
func (adapter *Adapter) callAssignModel(ctx context.Context, routerUID, cascadeID string) (resolvedAssignment, error) {
	name, version, os := adapter.CurrentConfig().ClientIdentity()
	link := adapter.link()
	link.warmer.kickRequest()
	ctx, cancel := context.WithTimeout(ctx, preGateTimeout)
	defer cancel()
	resp, err := link.api.AssignModel(ctx, connect.NewRequest(&devinproto.AssignModelRequest{
		Metadata:       upstream.BuildMetadata(adapter.currentToken(), name, version, os, 366),
		ModelRouterUid: proto.String(routerUID),
		CascadeId:      proto.String(cascadeID),
	}))
	if err != nil {
		// 归因写进 Message——Classify 经 errors.As 直取内层记录，
		// fmt.Errorf 包装文本不会进客户端可见文案。
		failure := llm.Classify(err)
		failure.Message = fmt.Sprintf("AssignModel(%s): %s", routerUID, failure.Message)
		return resolvedAssignment{}, failure
	}
	assignment := resp.Msg.GetAssignment()
	resolved := strings.TrimSpace(assignment.GetModelUid())
	if resolved == "" || assignment.GetAssignmentJwt() == "" {
		return resolvedAssignment{}, &llm.Failure{Code: "invalid_argument", Message: fmt.Sprintf("AssignModel(%s) returned empty assignment", routerUID)}
	}
	return resolvedAssignment{modelUID: resolved, jwt: assignment.GetAssignmentJwt()}, nil
}

// invalidateAssignment 作废 (router uid, cascade id) 的 AssignModel 解析
// 缓存：assignment jwt 绑 cascade_id 且无 TTL，凭据味失败后留着它会让
// 重发复用同一份可疑 jwt——删掉后下一次 assignModel 重新走上游解析。
// 键格式（router|cascade）与 assignModel 共享，全仓仅此两处拼装。
func (adapter *Adapter) invalidateAssignment(routerUID, cascadeID string) {
	adapter.assignmentsMu.Lock()
	delete(adapter.assignments, routerUID+"|"+cascadeID)
	adapter.assignmentsMu.Unlock()
}

// requestHasImages 判断请求是否含图片块（用户消息与工具结果两类），
// 供 validateImagesForModel 在无图时跳过目录能力检查。
func requestHasImages(request llm.RequestMessages) bool {
	for _, message := range request.Messages {
		var content []llm.Content
		switch m := message.(type) {
		case llm.UserMessage:
			content = m.Content
		case llm.ToolResultMessage:
			content = m.Content
		default:
			continue
		}
		for _, block := range content {
			if _, ok := block.(llm.ImageContent); ok {
				return true
			}
		}
	}
	return false
}

// modelLikelySupportsImages 用已知无视觉模型名单；不确定时放行让上游裁决。
func modelLikelySupportsImages(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return true
	}
	// 与 GetCascadeModelConfigs.supports_images=false 的常见 uid 对齐。
	// 前缀必须带边界（相等或 -/_ 续形）：裸 HasPrefix 会把 "o10" 一类
	// 同头异名 uid 误判成无视觉模型。
	noVisionPrefixes := []string{
		"glm-5-2", "glm-5", "glm-4.7", "glm-4-7", "glm-4",
		"deepseek", "kimi-k2", "qwen3-coder",
		"o1", "o3-mini", "o4-mini",
	}
	for _, p := range noVisionPrefixes {
		if m == p || strings.HasPrefix(m, p+"-") || strings.HasPrefix(m, p+"_") {
			return false
		}
	}
	return true
}

// ListModels 通过 GetCliModelConfigs 拉取可用模型目录，结果带 TTL 缓存。
// 并发 miss 收敛为单次上游调用（singleflight）：拉取在锁外进行且 detach
// 自调用方 ctx——目录是 adapter 级共享状态，一个客户端断连不该掐死
// 全体等待者共享的拉取；等待者吃自己的 ctx，可随时退出。
// 缓存/冷却先于 close(fetch) 提交，被唤醒方走复查只会看到已提交状态。
// CLI 版响应比 Cascade 版多 subagent_default_model_uid/default_override_model_config，
// 且 modelInfo.modelFeatures 提供 tool_calls/thinking/parallel 能力位。
func (a *Adapter) ListModels(ctx context.Context) ([]adapter.ModelInfo, error) {
	for {
		a.modelsMu.RLock()
		if a.models != nil && time.Now().Before(a.modelsExpiry) {
			cached := a.models
			a.modelsMu.RUnlock()
			return cached, nil
		}
		a.modelsMu.RUnlock()

		a.modelsMu.Lock()
		if a.models != nil && time.Now().Before(a.modelsExpiry) {
			models := a.models
			a.modelsMu.Unlock()
			return models, nil
		}
		// 失败冷却期不再打上游：有旧值回旧值，空缓存回上次错误。
		if time.Now().Before(a.modelsRetryUntil) {
			if a.models != nil {
				models := a.models
				a.modelsMu.Unlock()
				return models, nil
			}
			err := a.modelsErr
			a.modelsMu.Unlock()
			return nil, err
		}
		if fetch := a.modelsFetch; fetch != nil {
			a.modelsMu.Unlock()
			select {
			case <-fetch:
				continue // 拉取方已提交缓存或冷却，复查拿结果
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		a.modelsFetch = make(chan struct{})
		a.modelsMu.Unlock()

		models, err := a.fetchModelCatalog(context.WithoutCancel(ctx))

		a.modelsMu.Lock()
		done := a.modelsFetch
		a.modelsFetch = nil
		if err != nil {
			// fetch 的 ctx 经 WithoutCancel detach，调用方取消传不进来
			//（Canceled 实际不可达，留作兜底判据）；可达的 ctx 错误是
			// preGateTimeout 的 DeadlineExceeded——那是上游挂起的
			// 形态，按上游失败进冷却，否则挂起期每个 ensureCatalog
			// 调用方都吸附陪等到超时。
			if !errors.Is(err, context.Canceled) {
				backoff := catalogRetryBackoff
				if failure := llm.Classify(err); failure.RetryAfterSeconds > 0 {
					backoff = time.Duration(failure.RetryAfterSeconds) * time.Second
				}
				a.modelsRetryUntil = time.Now().Add(backoff)
				a.modelsErr = err
				// 新一场失败冷却开启：告警去重标记清零，本场第一条
				// ensureCatalog 告警仍会落盘（见 modelsErrWarned）。
				a.modelsErrWarned = false
			}
			// 目录刷新失败但有旧缓存时回旧值：catalog 缺席会让面板与
			// 能力位校验同时失去依据，比数据稍旧危害更大。
			stale := a.models
			a.modelsMu.Unlock()
			close(done)
			if stale != nil {
				// 调用方 ctx 已死（排空/断连）时的失败属噪声不报。
				if ctx.Err() == nil {
					slog.Warn("model catalog refresh failed; serving stale cache", "error", err)
				}
				return stale, nil
			}
			return nil, err
		}
		a.models = models
		a.modelsExpiry = time.Now().Add(a.modelsCacheTTL)
		a.modelsRetryUntil = time.Time{}
		a.modelsErr = nil
		a.modelsMu.Unlock()
		close(done)
		return models, nil
	}
}

// fetchModelCatalog 执行一次 GetCliModelConfigs 拉取并整形目录（去重、
// 配置模型补位、别名条目合并）。锁外运行——并发收敛、缓存提交与失败
// 冷却都归 ListModels。
func (a *Adapter) fetchModelCatalog(ctx context.Context) ([]adapter.ModelInfo, error) {
	// config 经 CurrentConfig 取快照：写路径是 ApplyConfig 持 configMu
	// 整体换值，modelsMu 管不到 config——裸读会与热应用竞争。
	cfg := a.CurrentConfig()
	name, version, os := cfg.ClientIdentity()
	ctx, cancel := context.WithTimeout(ctx, preGateTimeout)
	defer cancel()
	resp, err := a.link().api.GetCliModelConfigs(ctx, connect.NewRequest(&devinproto.GetCliModelConfigsRequest{
		Metadata: upstream.BuildMetadata(a.currentToken(), name, version, os, 0),
	}))
	if err != nil {
		return nil, fmt.Errorf("devin GetCliModelConfigs: %w", err)
	}
	now := time.Now().Unix()
	models := make([]adapter.ModelInfo, 0, len(resp.Msg.GetClientModelConfigs()))
	seen := make(map[string]struct{}, len(resp.Msg.GetClientModelConfigs()))
	for _, c := range resp.Msg.GetClientModelConfigs() {
		if c.GetDisabled() {
			continue
		}
		uid := c.GetModelUid()
		if uid == "" && c.GetModelOrAlias() != nil {
			uid = c.GetModelOrAlias().GetModelUid()
		}
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		ownedBy := "devin"
		if p := c.GetProvider().String(); p != "" {
			if i := strings.LastIndex(p, "_"); i >= 0 && i+1 < len(p) {
				ownedBy = strings.ToLower(p[i+1:])
			}
		}
		info := adapter.ModelInfo{
			ID: uid, Created: now, OwnedBy: ownedBy, SupportsImages: c.GetSupportsImages(),
			ContextTokens: int(c.GetMaxTokens()),
		}
		if modelInfo := c.GetModelInfo(); modelInfo != nil {
			info.MaxOutputTokens = int(modelInfo.GetMaxOutputTokens())
			info.IsModelRouter = modelInfo.GetIsModelRouter()
			if info.ContextTokens == 0 {
				info.ContextTokens = int(modelInfo.GetMaxTokens())
			}
			if features := modelInfo.GetModelFeatures(); features != nil {
				info.SupportsToolCalls = features.GetSupportsToolCalls()
				info.SupportsParallelToolCalls = features.GetSupportsParallelToolCalls()
				info.SupportsThinking = features.GetSupportsThinking()
				info.PreserveThinking = features.GetPreserveThinking()
				if !info.SupportsImages {
					info.SupportsImages = features.GetSupportsImages()
				}
			}
		}
		models = append(models, info)
	}
	// 用户显式配置的 model（如 gpt5.6）即使不在 Devin 返回的列表中，也应可被发现和调用。
	if configured := strings.TrimSpace(cfg.Model); configured != "" {
		if _, ok := seen[configured]; !ok {
			models = append(models, adapter.ModelInfo{
				ID: configured, Created: now, OwnedBy: "devin",
				// 配置模型无法从 Devin 获取图片能力，默认按支持图片处理更友好。
				SupportsImages: true,
			})
		}
	}

	// 别名条目进目录：按 /v1/models 选模型的客户端才能发现别名。
	// "*" 是兜底匹配符而非可命名模型，不进列表。能力位继承自目标
	// 条目（别名请求实际跑的是目标）；目标缺席时退回与配置模型同策
	// 的占位。别名键撞上真实 uid 时改写原条目为 alias_of 形态——
	// 该名字的请求已被改道，展示目标能力位才是真实行为。
	byID := make(map[string]int, len(models))
	for i, m := range models {
		byID[m.ID] = i
	}
	aliasNames := make([]string, 0, len(cfg.Aliases))
	for name := range cfg.Aliases {
		if name != "*" {
			aliasNames = append(aliasNames, name)
		}
	}
	sort.Strings(aliasNames)
	for _, name := range aliasNames {
		target := cfg.Aliases[name]
		entry := adapter.ModelInfo{ID: name, Created: now, OwnedBy: "devin", AliasOf: target, SupportsImages: true}
		if i, ok := byID[target]; ok {
			t := models[i]
			entry.SupportsImages = t.SupportsImages
			entry.SupportsToolCalls = t.SupportsToolCalls
			entry.SupportsParallelToolCalls = t.SupportsParallelToolCalls
			entry.SupportsThinking = t.SupportsThinking
			entry.PreserveThinking = t.PreserveThinking
			entry.IsModelRouter = t.IsModelRouter
			entry.ContextTokens = t.ContextTokens
			entry.MaxOutputTokens = t.MaxOutputTokens
		}
		if i, ok := byID[name]; ok {
			entry.Created = models[i].Created
			entry.OwnedBy = models[i].OwnedBy
			models[i] = entry
		} else {
			byID[name] = len(models)
			models = append(models, entry)
		}
	}
	return models, nil
}

// startHoldTimeout 是 start 事件（message_start/response.created）允许被
// 扣留的最长时间。扣留的目的是给上游「产出内容前就失败」留一个返回真实
// HTTP 状态码的窗口——实测这类失败全部在 ~9s 内落定；而下游客户端在
// ~30s 无数据时弃连，且中间网关只在首个协议事件后才向客户端放通字节
// （保活注释行不算）。15s 位于两者之间：快速失败仍拿到真实状态码，
// 长思考则先把 start 发出去让客户端保持存活。
var startHoldTimeout = 15 * time.Second

// upstreamFrameBuffer 是泵协程可超前读取的帧数：上游生产与客户端
// 消费解耦，同时保留对上游的背压上限。
const upstreamFrameBuffer = 64

// upstreamFrame 是泵协程的一次产出：response 为正常数据帧；
// response 为 nil 表示流终止，err 为终止错误（正常 EOF 时为 nil）。
type upstreamFrame struct {
	response *devinproto.GetChatMessageResponse
	err      error
}

// pumpUpstream 把阻塞的 Receive 归一化为 channel 帧序列：Receive 只能被
// ctx 取消打断，交给协程后 Recv 才能在等待期间响应静默看门狗与客户端断开。
// 流终止时 Err() 作为最后一帧无条件投递：终帧只有一帧，阻塞等消费方
// 排空缓冲，丢弃它会以「正常 EOF」的形态吃掉真实流错误（含静默截断）。
// 所有发送带 ctx.Done 分支：消费方放弃后协程必须能退出。
func pumpUpstream(ctx context.Context, upstream devinResponseReceiver) <-chan upstreamFrame {
	frames := make(chan upstreamFrame, upstreamFrameBuffer)
	go func() {
		defer close(frames)
		for upstream.Receive() {
			select {
			case frames <- upstreamFrame{response: upstream.Msg()}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case frames <- upstreamFrame{err: upstream.Err()}:
		case <-ctx.Done():
		}
	}()
	return frames
}

// responseStream 从泵协程读取上游帧并依次返回 decoder 生成的事件。
type responseStream struct {
	// mu 串行化全部内部状态访问：单消费者契约之外还有两个潜在并发
	// 线程——客户端哨兵（断开后消费方未必再进 Recv，由它就地判定
	// 脱钩/杀泵）与脱钩后的后台泵（原消费方退场前可能与之短暂重叠）。
	// Recv 全程持锁，哨兵与淘汰路径持锁后才碰流字段。
	mu sync.Mutex
	// frames 是泵协程产出的上游帧通道；终止帧 response 为 nil。
	frames <-chan upstreamFrame
	// cancel 中止上游流：看门狗判死、客户端 ctx 取消或流正常结束时调用，
	// 打断泵协程内可能仍阻塞的 Receive。swap/tryResume 换流时在 mu 下
	// 重赋值；锁外调用方经 kill() 拿当前值，裸读写 func 值有撕裂风险。
	cancel context.CancelFunc
	// decoder 将一个 Devin protobuf 帧转换为零个或多个中间响应事件。
	decoder *responseDecoder
	// recorder 记录 Devin 原始响应帧；nil 表示禁用调试日志。
	recorder *debuglog.Recorder
	// started 表示是否已经请求 decoder 产生 start 事件。
	started bool
	// pendingStart 保存 decoder.start() 生成但尚未下发的事件。
	// start 推迟到第一批真实事件前发出：上游在产出内容前报错时，
	// 首个对外事件是 error，HTTP 层才能返回真实错误状态码，
	// 而不是已提交的 200 + SSE error（下游网关会把后者误判为渠道故障）。
	// 扣留上限由 startHold 控制：超时后 start 单独下发。
	pendingStart []llm.ResponseEvent
	// startHold 在 pendingStart 填充时武装，到期释放扣留的 start。
	startHold *time.Timer
	// startReleased 标记 start 已下发给客户端：pre-content 重试重建
	// 解码器后必须丢弃新 start，否则客户端会收到第二个 message_start。
	startReleased bool
	// finished 表示 decoder 已经生成最终事件，不再读取上游。atomic：
	// 断连哨兵在锁外做占位登记判定时读它——只 false→true 单向翻转
	//（swap/tryResume 的 =false 写在已 false 的值上），锁外读不会看
	// 到反方向的过期值。
	finished atomic.Bool
	// queue 保存已经转换、等待调用方读取的中间响应事件。
	queue []llm.ResponseEvent
	// producedEvents 表示上游帧已产出过任何事件：一旦为真说明内容已
	// 开始对外流动，此后失败只能透传，不能整体重发。atomic：哨兵在
	// 锁外读它判定占位登记资格，单调 false→true 保证读到 true 恒为
	// 已成立事实。
	producedEvents atomic.Bool
	// upstreamConfirmed 标记上游已产出首个非错误帧：限流闩以此为据
	// 提前解闩（边际态下拒绝是概率执行，成功帧即窗口已过的证据）。
	upstreamConfirmed bool
	// gate 是上游消息速率闸门：流内 resource_exhausted 也要喂冷却闩。
	gate *rateGate
	// warm/warmKey 是本流所属保温 lineage 的簿记句柄：流正常收尾的
	// Done 事件触发条目分类与 prefix 尺寸观测（见 release）。
	warm    *cacheWarmer
	warmKey warmLineageKey
	// retry 是两类流级重试（pre-content 整体重开 / 截断续传）的预算
	// 计数与准入判定，纯值类型见 streampolicy.go。
	retry retryPolicy
	// reopen 在可重试的 pre-content 失败（传输断裂、凭据自愈后的
	// unauthenticated、静默看门狗判死）时重发请求并返回新泵；
	// continueEmpty 表示空 end_turn 续传：追加 "continue" 用户消息。
	reopen func(cause error, continueEmpty bool) (<-chan upstreamFrame, context.CancelFunc, error)
	// newDecoder 重建响应解码器供重试使用；nil 时不可重试。
	newDecoder func() *responseDecoder
	// search 是服务端托管搜索的执行入口（runWebSearch）；nil 表示
	// 本请求没有托管工具声明，handleServerCalls 不会触发。
	search func(ctx context.Context, query string, allowedDomains, blockedDomains []string, limit uint32) (webSearchOutcome, error)
	// extend 以「历史 + 追加消息」重发请求并返回播种旧内容的新流：
	// 托管续轮（handleServerCalls）与截断续传（tryResume）共用；
	// seed 是含结果块的完整内容（新解码器的 ContentIndex 种子）。
	// nil 只在测试构造的裸流上出现。
	extend func(cause string, extra []llm.Message, seed []llm.Content) (<-chan upstreamFrame, context.CancelFunc, *responseDecoder, error)
	// hops 是已执行的服务端托管续轮数，封顶见 maxServerSearchHops。
	hops int
	// costsCarry 累计已完成的托管续轮跳的上游 CreditCost：上游按跳
	// 分别记账，收尾时并入最终 Done 的 Usage（见 applyCostsCarry）。
	costsCarry int64
	// stall 是跨 Recv 复用的静默看门狗计时器；首次等待时创建。
	stall *time.Timer
	// progress 是「无内容进度」期限计时器：只有产出事件的帧喂它，
	// latency 活性帧/元数据帧不喂——退化上游的零事件帧续命会被它兜底。
	// 与 stall 同为跨 Recv 复用计时器：Recv 入等待前 Reset 续期，
	// tryReopen 换流后重置窗口；不随 Recv 返回 Stop——窗口语义是
	// 「消费方活跃等待期间零事件」，Stop 会让首个内容事件后的
	// 零事件帧续命逃过看门狗，流无限挂起。
	progress *time.Timer
	// deadlines 收拢两个看门狗的全部期限算术：分档档值（pre/post
	// 无进度窗）与累计静默上限的锚点（首发起算、跨换流累计）。
	// 纯值类型零 I/O 零锁，方法与语义见 streampolicy.go。
	deadlines streamDeadlines
	// detachKey/registry/entry 是完成缓存挂接面：key 是语义请求
	// 哈希（detachedRequestKey），entry 自建流起经 Recv 返回点 tee
	// 累积全部下发事件（重试方需要含前缀的完整序列），registry 持
	// 命中判定与条目生命周期。三者由 Adapter.Stream 注入；测试裸流
	// 留空 → detachable() 恒假 → 客户端断开行为与旧实现一致。
	detachKey string
	registry  *detachedRegistry
	entry     *detachedEntry
	// detached 标记本流已与客户端解耦、由后台泵续命：无进度看门狗
	// 退役（耐心是它的全部意义，running TTL 是存活上界），静默
	// 看门狗仍在岗——零帧意味着连接真死而非算得慢。atomic：它同时
	// 是「生命周期处置权」的唯一认领位——断连哨兵在锁外 CAS 占位
	// 登记（绕过 mu 内可达分钟级的阻塞段），持 mu 的消费方/哨兵慢路
	// 经同一 CAS 兑入：赢家或登记进缓存或就地杀泵，后到者见此标记
	// 直接退场。置位只保证「已有人处置」，不保证「已登记」。
	detached atomic.Bool
}

// devinResponseReceiver 描述 responseStream 消费 Devin 服务端流所需的最小能力。
type devinResponseReceiver interface {
	// Receive 前进到下一帧，并报告是否成功取得消息。
	Receive() bool
	// Msg 返回最近一次成功取得的响应帧。
	Msg() *devinproto.GetChatMessageResponse
	// Err 返回流结束时的错误；正常 EOF 返回 nil。
	Err() error
}

// Recv 前进到下一个中间响应事件。单消费者契约仍成立，但锁内串行是
// 硬保证：decoder/queue/看门狗全部在 stream.mu 下访问，客户端哨兵
// （断开后消费方未必再进 Recv，由它就地判定脱钩/杀泵）与脱钩后的
// 后台泵也经同一把锁进场，三者不会交错读写内部状态。ctx 取消让等待
// 中的 Recv 返回取消错误，泵协程同时被 stream.cancel 打断。
func (stream *responseStream) Recv(ctx context.Context) (llm.ResponseEvent, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	// 静默计时器挂在流上跨 Recv 复用：每次入等待循环前 Reset 覆盖
	// 帧间隔。Go 1.23+ 计时器通道无缓冲，Stop/Reset 后不会投递陈旧触发，
	// 已触发（stall.C 分支）的计时器 Reset 重新武装即可。
	stall := stream.stall
	if stall == nil {
		stall = time.NewTimer(stream.deadlines.stall(stream.decoder.hasStopReason, stream.upstreamConfirmed))
		stream.stall = stall
	} else {
		stall.Reset(stream.deadlines.stall(stream.decoder.hasStopReason, stream.upstreamConfirmed))
	}
	defer stall.Stop()
	// progress 与 stall 同构：计时器跨 Recv 复用，消费方每次进入等待
	// 前 Reset 续期——窗口只覆盖「活跃等待期间」的零事件时长，消费方
	// 去忙别的事不计入，也不能随 Recv 返回停表。脱钩流不武装它：
	// 耐心是后台泵的全部意义，存活上界由条目 running TTL 兜住；
	// progressC 为 nil 时 select 的该分支永不触发。
	var progress *time.Timer
	var progressC <-chan time.Time
	if !stream.detached.Load() {
		progress = stream.progress
		if progress == nil {
			progress = time.NewTimer(stream.deadlines.progress(stream.producedEvents.Load(), time.Now()))
			stream.progress = progress
		} else {
			progress.Reset(stream.deadlines.progress(stream.producedEvents.Load(), time.Now()))
		}
		progressC = progress.C
	} else if stream.progress != nil {
		// 占位路径脱钩不持锁、停不了消费方留下的旧表：首个持锁进场的
		// 脱钩方顺手回收——无进度看门狗对脱钩流已退役。
		stream.progress.Stop()
		stream.progress = nil
	}
	for len(stream.queue) == 0 && !stream.finished.Load() {
		if err := ctx.Err(); err != nil {
			// streamCtx 从 streamBase 派生不随客户端取消：早退路径
			// 必须显式决定上游泵的去向——已产出内容就脱钩续命进缓存，
			// 否则杀掉（pre-content 流进缓存没有重放价值）。detached
			// CAS 是处置权的唯一仲裁：哨兵占位登记若抢先落子，泵已归
			// 后台，这里误杀会把刚到手的条目收成截断前缀。
			if stream.detachable() {
				stream.detach(ctx)
			} else if stream.detached.CompareAndSwap(false, true) {
				stream.cancel()
			}
			return llm.ResponseEvent{}, err
		}
		if !stream.started {
			// start() 初始化 decoder.partial，必须先于 decode 调用；
			// 事件本身扣留在 pendingStart，等待第一批真实事件一起下发。
			stream.started = true
			stream.pendingStart = stream.decoder.start()
			if stream.startReleased {
				// 重试流上客户端已见过一个 start，重复下发会违反协议。
				stream.pendingStart = nil
			} else if stream.startHold == nil {
				stream.startHold = time.NewTimer(startHoldTimeout)
			} else {
				stream.startHold.Reset(startHoldTimeout)
			}
			continue
		}
		// 等待窗口按语义状态分档（stallDeadline）：上游首帧确认前是
		// 无心跳覆盖段取保守窗；确认后帧间隔被上游 ~60s 心跳封顶可
		// 收紧；stopReason 之后只剩尾帧（实测 <1ms 到达）再缩到尾部
		// 宽限——connect-go 排空 body 等传输 EOF 时上游不关连接会把
		// 正常收尾拖成 stall。
		stall.Reset(stream.deadlines.stall(stream.decoder.hasStopReason, stream.upstreamConfirmed))
		var startHold <-chan time.Time
		if stream.startHold != nil {
			startHold = stream.startHold.C
		}
		select {
		case <-startHold:
			// 上游建流后静默超时：先把扣留的 start 发出去——对客户端
			// 这是首个可见字节，链路各段的空闲计时器随之刷新。
			if len(stream.pendingStart) == 0 {
				continue
			}
			stream.queue = stream.pendingStart
			stream.pendingStart = nil
			stream.startReleased = true
			continue
		case frame, ok := <-stream.frames:
			stall.Stop()
			if !ok || frame.response == nil {
				var upstreamErr error
				if ok {
					upstreamErr = frame.err
				} else if ctxErr := context.Cause(ctx); ctxErr != nil {
					// ok==false 只剩「泵协程随 ctx 取消退出」一种来源
					//（终帧无条件投递）。把取消透传给 finish，避免以
					// 正常 EOF 的形态吞掉被截断的流。
					upstreamErr = ctxErr
				}
				if upstreamErr != nil && stream.tryReopen(upstreamErr, false) {
					continue
				}
				resumeCause := upstreamErr
				if resumeCause == nil {
					// 干净 EOF 无 stopReason = 静默截断，与传输断裂同级续传。
					resumeCause = errors.New("devin stream ended without stop reason")
				}
				if stream.tryResume(resumeCause) {
					continue
				}
				events := stream.release(stream.decoder.finish(upstreamErr))
				if upstreamErr == nil && stream.handleServerCalls(ctx, &events) {
					// 续轮换流前先把本跳尾帧（toolcall_end/托管结果）下发——
					// 直接 continue 会把它们吞掉，客户端的调用项永远不收口。
					stream.queue = events
					continue
				}
				stream.applyCostsCarry(events)
				if upstreamErr == nil && emptyEndTurn(events) && stream.tryReopen(nil, true) {
					continue
				}
				stream.recordUpstreamFailure(upstreamErr)
				stream.queue = events
				stream.finished.Store(true)
				continue
			}
			if !stream.upstreamConfirmed {
				stream.upstreamConfirmed = true
				stream.gate.noteUpstreamSuccess()
			}
			// 脱钩后盘上不再追写：帧已由 Recv 返回点 tee 进完成缓存供
			// 重放，此时原 dir 多已 Complete，04 续写只会被 closed 门口
			// 拒收计进 late_writes/dropped 噪声。
			if !stream.detached.Load() {
				recordProtoJSON(stream.recorder, debuglog.StageDevinResponse, frame.response)
			}
			events := stream.decoder.decode(frame.response)
			stream.recordSchemaDrift()
			if len(events) > 0 {
				stream.producedEvents.Store(true)
				if progress != nil {
					progress.Reset(stream.deadlines.progress(stream.producedEvents.Load(), time.Now()))
				}
			}
			stream.queue = stream.release(events)
		case <-stall.C:
			// 上游静默超时：取消底层流打断泵协程；已缓冲未消费的帧
			// 补记进原始日志留证，然后按传输错误收尾。
			stream.cancel()
			stream.drainFrames()
			if stream.decoder.hasStopReason {
				// 语义内容已齐、只是传输尾帧没到（上游不关 body 时
				// connect-go 的排空会一直等）——按正常 EOF 收尾。
				slog.Warn("upstream held connection after stop reason; finishing after tail grace")
				events := stream.release(stream.decoder.finish(nil))
				if stream.handleServerCalls(ctx, &events) {
					stream.queue = events
					continue
				}
				stream.applyCostsCarry(events)
				if emptyEndTurn(events) && stream.tryReopen(nil, true) {
					continue
				}
				stream.queue = events
				stream.finished.Store(true)
				continue
			}
			stallErr := fmt.Errorf("devin stream stalled: no frames for %s", stream.deadlines.stall(stream.decoder.hasStopReason, stream.upstreamConfirmed))
			if stream.tryReopen(stallErr, false) {
				continue
			}
			if stream.tryResume(stallErr) {
				continue
			}
			stream.recordUpstreamFailure(stallErr)
			stream.queue = stream.release(stream.decoder.finish(stallErr))
			stream.finished.Store(true)
		case <-progressC:
			// 有帧流动但长期零内容进度（上游 latency 活性帧不算
			// 进度）：退化形态兜底——pre-content 可整体重发，
			// post-content 按传输错误收尾。
			stream.cancel()
			stream.drainFrames()
			progressErr := fmt.Errorf("devin stream made no progress for %s", stream.deadlines.progressBound(stream.producedEvents.Load(), time.Now()))
			if stream.tryReopen(progressErr, false) {
				continue
			}
			if stream.tryResume(progressErr) {
				continue
			}
			stream.recordUpstreamFailure(progressErr)
			stream.queue = stream.release(stream.decoder.finish(progressErr))
			stream.finished.Store(true)
		case <-ctx.Done():
			stall.Stop()
			if stream.detachable() {
				// 已产出内容的流不随客户端一起死：脱钩进完成缓存由
				// 后台泵续命，同键重试重放缓冲。取消错误原样返回给
				// 消费方（与早退 ctx.Err() 分支同形态）。
				stream.detach(ctx)
				return llm.ResponseEvent{}, context.Cause(ctx)
			}
			if !stream.detached.CompareAndSwap(false, true) {
				// 泵已归后台（哨兵占位登记抢先，或本流早已脱钩）：
				// finish/杀泵都归后台泵的退出路径负责，本消费方原样
				// 收取消退场。
				return llm.ResponseEvent{}, context.Cause(ctx)
			}
			stream.cancel()
			stream.queue = stream.release(stream.decoder.finish(context.Cause(ctx)))
			stream.finished.Store(true)
		}
	}
	if stream.finished.Load() {
		stream.cancel()
		// progress/startHold 是跨 Recv 复用的看门狗，不能随 Recv 返回
		// 停表（语义见 progress 字段注释）；finished 后等待循环不再进入，
		// 窗口语义终结——此处是终局退出点，Stop 回收计时器，否则每条
		// 完成的流留 ~2 个挂起计时器直到自然触发。
		if stream.progress != nil {
			stream.progress.Stop()
		}
		if stream.startHold != nil {
			stream.startHold.Stop()
		}
	}
	if len(stream.queue) > 0 {
		event := stream.queue[0]
		stream.queue = stream.queue[1:]
		// 下发即缓冲：脱钩后重试方需要含前缀的完整事件序列，
		// tee 在返回点才能覆盖 start 扣留在内的全部对外事件。
		stream.teeDetached(event)
		return event, nil
	}
	return llm.ResponseEvent{}, io.EOF
}

// tryReopen 在「上游已失败但尚未产出任何内容」时整体重发请求一次：
// 此时客户端只见过扣留的 start 事件，重发没有可见副作用。返回 true
// 表示新流已接管，调用方重置解码器后继续消费。
// continueEmpty 为空 end_turn 续传：流正常结束但零内容时重发并
// 追加 "continue" 用户消息（空轮是上游实测退化形态，CPA#4886 同构）。
func (stream *responseStream) tryReopen(cause error, continueEmpty bool) bool {
	if stream.reopen == nil ||
		!stream.retry.reopenable(stream.producedEvents.Load(), cause, continueEmpty, stream.deadlines.silenceCapExhausted(time.Now())) {
		return false
	}

	frames, cancel, err := stream.reopen(cause, continueEmpty)
	if err != nil {
		return false
	}
	stream.retry.reopened = true
	stream.swap(frames, cancel, stream.newDecoder())
	return true
}

// swap 把流换到一组新泵与解码器上：先杀旧泵（stall 重开时旧泵可能还
// 堵在 Receive 上，不 cancel 它就带着旧 gRPC 流陪跑到请求结束），交接
// 帧通道与 cancel，再把消费状态归零——start 扣留、事件队列、完工标记、
// 上游确认与无进度窗口都按新流重新计起。换流不变量集中在这一个方法里；
// 调用方的差异只在解码器播种方式：pre-content 重开经 newDecoder 重建，
// 托管续轮传入已播种旧内容的解码器。
func (stream *responseStream) swap(frames <-chan upstreamFrame, cancel context.CancelFunc, decoder *responseDecoder) {
	stream.cancel()
	stream.frames = frames
	stream.cancel = cancel
	stream.decoder = decoder
	stream.started = false
	stream.pendingStart = nil
	stream.finished.Store(false)
	stream.queue = nil
	// 新流的首个非错误帧重新获得解闩资格——上一流的确认不能
	// 替代这次重试是否真的打穿了限流。
	stream.upstreamConfirmed = false
	// 新流的无进度窗口重新武装：旧流的计时器（可能刚触发排空）
	// 不沿用。post-event 档换流重获完整窗口；pre-event 档仍被
	// firstSentAt 的累计静默上限截顶——重开只继承剩余额度，不是
	// 重置预算。脱钩流无 progress 看门狗（nil），跳过武装。
	if stream.progress != nil {
		stream.progress.Reset(stream.deadlines.progress(stream.producedEvents.Load(), time.Now()))
	}
}

// tryResume 在「内容已部分下发、上游流被截断」时续传：在飞块物化进
// partial 后拼成 assistant 回显、追加 "continue" 用户消息整体重发——
// 上游实测能从半截回合接续生成（含句中截断，probe edge
// stall-resume-*）。返回 true 表示新流已接管；在飞块的 end 事件
// 已入队作为接缝先于续流事件下发。
//
// 不可续的形态：在飞工具调用（arguments 仍是截断 JSON，回传会被
// 上游参数校验拒掉，丢弃又让客户端已见的调用与上游历史分叉）、已收
// stopReason 或被本地停止序列截断的流（语义内容已齐，续传会在
// 停止标记之后再长出一块内容）。
func (stream *responseStream) tryResume(cause error) bool {
	sealed := stream.decoder.hasStopReason || stream.decoder.stoppedByPattern
	if stream.extend == nil ||
		!stream.retry.resumable(stream.producedEvents.Load(), sealed, len(stream.decoder.tools) > 0) {
		return false
	}
	// 物化在飞块并产接缝事件：end 让客户端看到干净块边界，续流
	// 内容开新块（ContentIndex 由播种接续）。
	thinkingOpen := stream.decoder.thinkingOpen
	seam := stream.decoder.endThinking(stream.decoder.endText(nil))
	partial := stream.decoder.partial
	wireContent := make([]llm.Content, 0, len(partial.Content))
	var results []llm.Message
	for _, block := range partial.Content {
		if result, isResult := block.(llm.ServerToolResult); isResult {
			// 托管结果块不进 assistant 回显，按内容序转 TOOL 消息——
			// 与 handleServerCalls 续轮的 wire 形态同构。
			results = append(results, llm.ToolResultMessage{
				ToolCallID: result.ToolCallID, IsError: result.IsError, TimestampMS: time.Now().UnixMilli(),
				Content: []llm.Content{llm.TextContent{Text: result.Text}},
			})
			continue
		}
		wireContent = append(wireContent, block)
	}
	if thinkingOpen && len(wireContent) > 0 {
		// 在飞 thinking 块的签名是截断残片，回显剥掉——无签名
		// thinking 上游实测接受；半截签名可能被验签拒掉。
		if thinking, ok := wireContent[len(wireContent)-1].(llm.ThinkingContent); ok {
			thinking.ThinkingSignature = ""
			thinking.SignatureType = ""
			wireContent[len(wireContent)-1] = thinking
		}
	}
	assistant := partial
	assistant.Content = wireContent
	extra := make([]llm.Message, 0, len(results)+2)
	extra = append(extra, assistant)
	extra = append(extra, results...)
	extra = append(extra, llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "continue"}}})
	frames, cancel, decoder, err := stream.extend("resume: "+cause.Error(), extra, partial.Content)
	if err != nil {
		return false
	}
	// 截断流已上报的 credit 并入 costsCarry（与托管续轮同账法）。
	if costs := partial.Usage.Costs; costs != nil {
		stream.costsCarry += costs.CreditCost
	}
	stream.retry.resumes++
	slog.Warn("resuming truncated stream", "attempt", stream.retry.resumes, "error", cause)
	// 换流同 tryReopen：杀旧泵、重置窗口，新解码器已播种旧内容。
	stream.cancel()
	stream.frames = frames
	stream.cancel = cancel
	stream.decoder = decoder
	stream.started = false
	stream.finished.Store(false)
	stream.upstreamConfirmed = false
	if stream.progress != nil {
		stream.progress.Reset(stream.deadlines.progress(stream.producedEvents.Load(), time.Now()))
	}
	stream.queue = seam
	return true
}

// emptyEndTurn 判断 finish 产出的事件是否构成「正常 stop 但零内容」：
// 上游偶发直接以 stopReason 收尾且不带任何 delta。StopSequence 不算——
// 零内容命中停止序列更可能是预期的截断而非退化轮。
func emptyEndTurn(events []llm.ResponseEvent) bool {
	for _, event := range events {
		if event.Type != llm.ResponseEventDone {
			continue
		}
		return event.Message != nil &&
			event.Message.StopReason == llm.StopReasonStop &&
			len(event.Message.Content) == 0
	}
	return false
}

// recordUpstreamFailure 把不可重试的上游侧失败记为该请求调试记录的首个失败点：
// 传输层断裂记 devin_transport——含 connect.Error 包装的 EOF/帧截断/
// 连接重置，判定见 isTransientConnectError；上游语义错误记 devin_connect。
// ctx 取消不记——客户端断连由 HTTP 外层记 client_disconnected，不应被
// 上游 stage 抢占。
// WriteError 是 first-write-wins，此处记录后外层 http_stream 只作补充。
func (stream *responseStream) recordUpstreamFailure(cause error) {
	if cause == nil {
		return
	}
	// 限流结论与日志开关无关：上游报了 resource_exhausted 就上闩。
	stream.gate.noteUpstreamError(cause)
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return
	}
	stage := debuglog.ErrStageDevinConnect
	if isTransientConnectError(cause) {
		stage = debuglog.ErrStageDevinTransport
	}
	stream.recorder.WriteError(stage, cause)
	// 走到这里说明 reopen/resume 都已拒绝：把两侧门禁快照落成 04 标记行，
	// 「为什么没续」（典型：在飞工具调用）不必靠反推 retries=0。
	stream.recorder.AppendJSONL(debuglog.StageDevinResponse, "retry_declined", map[string]any{
		"retried":            stream.retry.reopened,
		"produced_events":    stream.producedEvents.Load(),
		"tools_in_flight":    len(stream.decoder.tools),
		"has_stop_reason":    stream.decoder.hasStopReason,
		"stopped_by_pattern": stream.decoder.stoppedByPattern,
		"resume_attempts":    stream.retry.resumes,
	})
}

// drainFrames 把看门狗判死时已缓冲未消费的上游帧补记进原始日志——
// 「死前最后输出了什么」是判断上游挂死形态的关键证据。补记的帧未经
// decode，这里顺手跑一次 schema 漂移检查，让临死帧也能留 drift 标记。
func (stream *responseStream) drainFrames() {
	for {
		select {
		case frame, ok := <-stream.frames:
			if !ok || frame.response == nil {
				stream.recordSchemaDrift()
				return
			}
			// 与 Recv 主路径同闸：脱钩流的临死帧同样不追写盘上。
			if !stream.detached.Load() {
				recordProtoJSON(stream.recorder, debuglog.StageDevinResponse, frame.response)
			}
			stream.decoder.noteSchemaDrift(frame.response)
		default:
			stream.recordSchemaDrift()
			return
		}
	}
}

// recordSchemaDrift 把解码器首次检出的上游 schema 漂移落成 04 的
// schema_drift 标记行——stderr 告警会被日志流冲掉，标记行随调试目录
// 留存且与帧序同档可查。消费即清 decoder.drift：换解码器的重开/续轮
// 各自最多标一次。
func (stream *responseStream) recordSchemaDrift() {
	drift := stream.decoder.drift
	if drift == nil {
		return
	}
	stream.decoder.drift = nil
	stream.recorder.AppendJSONL(debuglog.StageDevinResponse, "schema_drift", map[string]any{
		"scope":  drift.Scope,
		"fields": drift.Fields,
	})
}

// release 把 decoder 产出的第一批事件交给调用方：非错误批次前置扣留的
// start 事件；若首批就是错误事件（上游在产出内容前失败），丢弃 start，
// 让错误成为流的第一个对外事件。
func (stream *responseStream) release(events []llm.ResponseEvent) []llm.ResponseEvent {
	// Done 只在正常收尾路径存在（fail 产 Error 事件）——拿最终消息进
	// 保温簿记：pending 调用名表决定保温档位，usage 的 input+cache_read
	// 是前缀尺寸的真实读数。托管续轮的中间跳 Done 也过这里：其 pending
	// 只含 Server 调用（不算客户端 pending），会被最终跳覆写，无害。
	for _, event := range events {
		if event.Type == llm.ResponseEventDone && event.Message != nil {
			stream.warm.noteCompleted(stream.warmKey, event.Message)
		}
	}
	if len(events) == 0 || len(stream.pendingStart) == 0 {
		return events
	}
	start := stream.pendingStart
	stream.pendingStart = nil
	if events[0].Type == llm.ResponseEventError {
		return events
	}
	return append(start, events...)
}

// protoJSON 把 protojson 序列化推迟到日志写协程：recordProtoJSON 的调用方
// 是上游泵/解码 goroutine，同步 marshal 每帧会挤占流处理；包装成
// json.Marshaler 后 sanitize 在 worker 内 marshal+预筛+脱敏。marshal 失败
// 的兜底是记录文件里的 serialization_error 条目（实际不可达：protojson
// 对构造好的消息不报错）。
type protoJSON struct{ message proto.Message }

// MarshalJSON 实现 json.Marshaler：把 protojson 序列化推迟到日志
// worker 执行，调用方 goroutine 不承担 marshal 成本。
func (p protoJSON) MarshalJSON() ([]byte, error) { return protojson.Marshal(p.message) }

// recordProtoJSON 把 proto 消息记入调试日志；.jsonl 文件名走追加，
// 其余整写。recorder 可为 nil（未开调试日志）——Recorder 方法对
// nil 接收者安全。message 在全部调用点都已保证非空。
func recordProtoJSON(recorder *debuglog.Recorder, name string, message proto.Message) {
	if strings.HasSuffix(name, ".jsonl") {
		// JSONL 阶段文件的帧行走 JSONLRecord 信封（event="frame"）：
		// seq/elapsed_ms 给每帧本地到达序与时标——帧间隔重建不再依赖
		// 帧内上游 timestamp（~±0.1s 偏移），pre-frame0 截断也能从
		// 「有帧行/无帧行」直接判读。
		recorder.AppendJSONL(name, "frame", protoJSON{message})
		return
	}
	recorder.WriteJSON(name, protoJSON{message})
}
