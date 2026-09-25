// 本文件定义一次开流的「发送面」：attemptEnv 把原先靠 ctx 走私进闸门
// 与发送路径的环境值收成显式结构，attemptRunner 持有本次开流的全部
// 发送输入（请求投影、簿记句柄、发送序号），让首发与各处续试重发
// （凭据自愈/pre-content 重开/托管续轮/截断续传）收敛到同一组方法上。
package devin

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http/httptrace"
	"time"

	devinproto "local/devinproto"

	"connectrpc.com/connect"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
)

// attemptEnv 是一次开流要带过各次上游发送的环境值集合：闸门回执
// （准入放行时写回、HTTP 层据此盖 X-Gate-* 头）、兄弟 lane 让位探针、
// 跨 lane 完成缓存登记表、调试记录器。这些值此前靠 ctx 走私进
// gate.wait 与发送路径——号池 failover 把新 lane 开在 Recv 的 ctx
// 上时，本包手工挂进开流 ctx 的值必须逐个重注，漏一个即静默丢失。
// env 把集合收成显式字段：Stream 入口 capture 一次，此后按字段取用；
// 跨 ctx 边界的重挂收进 attach 单点，新增环境值即新增一个字段。
type attemptEnv struct {
	// gc 是闸门回执：wait 放行时经它回填 verdict。它仍随请求 ctx 血统
	// 走（app 挂在请求 ctx 上，Recv ctx 与开流 ctx 同源自带）——env
	// 持它是让闸门输入显式化，attach 不重挂。
	gc *adapter.GateContext
	// yield 是「兄弟 lane 此刻能更快放行」的活探针：号池逐 lane 装填
	// （gateYieldProbe.attach），单 lane 为 nil。
	yield func() (siblingEW time.Duration, free bool)
	// peers 是全池 {lane 名→完成缓存} 登记表：本 lane lookup 落空后
	// 据此探测兄弟 lane 是否持有同键条目。
	peers map[string]*detachedRegistry
	// recorder 是本请求的调试记录器。
	recorder *debuglog.Recorder
}

// attemptEnvFrom 把挂在 ctx 上的环境值捕成 env：ctx 只是 Stream 边界
// （接口签名固定）上的运输层，进入本包后一律按 env 字段取用。
func attemptEnvFrom(ctx context.Context) attemptEnv {
	return attemptEnv{
		gc:       adapter.GateContextFrom(ctx),
		yield:    adapter.GateYieldFrom(ctx),
		peers:    detachedPeersFrom(ctx),
		recorder: debuglog.FromContext(ctx),
	}
}

// class 返回本请求在闸门语义里的类别；未挂回执按 fg。
func (env attemptEnv) class() string {
	if env.gc != nil {
		return env.gc.Class
	}
	return adapter.ClassFG
}

// attach 把 env 里「挂接点在本包内部」的字段重挂到新 ctx：换 lane 时
// 新流开在 Recv 的 ctx 上，peers 登记表这类池内挂接值不会自动跟过去。
// gc 不重挂——它由 app 挂在请求 ctx 血统上，且闸门经 env 字段直接取
// 用；号池逐 lane 的 yield 由新 lane 自己的探针装填，不经 env 搬运
// （env.yield 非空时才重挂，覆盖 lane 级 env 整体搬运的场景）。
func (env attemptEnv) attach(ctx context.Context) context.Context {
	if env.yield != nil {
		ctx = adapter.WithGateYield(ctx, env.yield)
	}
	if env.peers != nil {
		ctx = withDetachedPeers(ctx, env.peers)
	}
	if env.recorder != nil {
		ctx = debuglog.WithRecorder(ctx, env.recorder)
	}
	return ctx
}

// errAttemptBuild 标记 resend 内部的请求重建失败：调用方据此区分
// 「重发没建成」（保留原始失败上报）与「重发打出去又败了」（重发
// 错误顶替原错误）。
var errAttemptBuild = errors.New("attempt request build failed")

// maxConnectAttempts 是 GetChatMessage 建立阶段对瞬时传输错误的最大尝试次数。
const maxConnectAttempts = 3

// attemptRunner 是一次开流的发送面：持有本请求全部发送输入——投影源
// （request/cfg/binding）、簿记句柄（env/warmKey）与最近分配的发送序号
// （attempt）。首发之外的全部续试重发（凭据自愈/pre-content 重开/托管
// 续轮/截断续传）原先在 Stream/responseStream 里各抄一套「换 token→
// 重建→记续试账→发送→失败留痕」骨架，漂移出分界行字段不一致——现
// 收敛到 resend 一处。
type attemptRunner struct {
	adapter *Adapter
	env     attemptEnv
	request llm.RequestMessages
	cfg     Config
	binding callBinding
	warmKey warmLineageKey
	// seedSum 是 Stream 入口冻结的会话种子哈希（降级前形态）：
	// 附件降级改写首消息文本，而 router 的 assignment jwt 绑的是
	// 降级前派生的 cascade_id——重发/续试必须沿用冻结值。
	seedSum []byte
	// attempt 是最近分配的上游发送序号（recorder 发的第 N 次发送号），
	// 供 retry_failed 标记行回填「当时走到第几次发送」。
	attempt int
}

// noteSend 记首发的请求分片与发送序号：序号由 recorder 分配、跨 lane
// 共享——号池 failover 后本 lane 的首发续占 attemptN 分片，不以基座
// 名覆写上一 lane 的 wire 体。
func (r *attemptRunner) noteSend(message *devinproto.GetChatMessageRequest) {
	r.attempt = r.env.recorder.NextDevinSendOrdinal()
	stage := debuglog.StageDevinRequest
	if r.attempt > 1 {
		stage = debuglog.StageDevinRequestAttempt(r.attempt)
	}
	recordProtoJSON(r.env.recorder, stage, message)
}

// noteAttempt 统一续试记账：序号分配、index retries、04 分界行与
// 03.attemptN 分片在同一点落盘——两处调用方曾各写一套，漂移出
// 分界行字段不一致（continue_empty 只有一边写）。
func (r *attemptRunner) noteAttempt(cause string, message *devinproto.GetChatMessageRequest, continueEmpty bool) {
	r.attempt = r.env.recorder.NextDevinSendOrdinal()
	r.env.recorder.NoteRetryAttempt(r.attempt, cause)
	r.env.recorder.AppendJSONL(debuglog.StageDevinResponse, "retry_attempt", map[string]any{
		"attempt":        r.attempt,
		"cause":          cause,
		"continue_empty": continueEmpty,
	})
	recordProtoJSON(r.env.recorder, debuglog.StageDevinRequestAttempt(r.attempt), message)
}

// noteRetryFailed 给「重发自身撞到的错误」留痕：error.json 是
// first-write-wins 只记原始失败点，重发的死因只在 04 的标记行里
// 找得到。三个重发点统一口径：构建失败与发送失败都记。
func (r *attemptRunner) noteRetryFailed(err error) {
	r.env.recorder.AppendJSONL(debuglog.StageDevinResponse, "retry_failed", map[string]any{
		"attempt": r.attempt,
		"error":   err.Error(),
	})
}

// rebuild 按当前凭据重投请求体：自愈/续试都可能换 token（凭据漂移、
// 池内换 lane 后 binding 各异），mutate 应用请求级变形（continue
// 追加/续轮消息/tool_choice 清除）。
func (r *attemptRunner) rebuild(mutate func(*llm.RequestMessages)) (*devinproto.GetChatMessageRequest, error) {
	request := r.request
	if mutate != nil {
		mutate(&request)
	}
	r.binding.Token = r.adapter.currentToken()
	built, _, err := buildRequestSeeded(request, r.cfg, r.binding, r.seedSum)
	return built, err
}

// demoteAttachments 把 r.request 里指定种类的附件降级为文本并写回
// request——rebuild 是浅拷贝不写回，续试（extend/tryResume）要继承
// 降级形态必须改源。写回安全：所有续试重发都串行（开流 goroutine
// 在返回前、reopen/extend 在 stream.mu 下）。返回降级块数；0 表示
// 请求里已没有该种附件可降。
func (r *attemptRunner) demoteAttachments(ctx context.Context, kind string) int {
	demoted, n := demoteRequestAttachments(ctx, r.request,
		kind == attachmentKindDocument, kind == attachmentKindVideo, nil)
	if n > 0 {
		r.request = demoted
	}
	return n
}

// send 过闸并发起一次 GetChatMessage 建流调用：瞬时传输错误最多重试
// maxConnectAttempts 次（只对建立阶段重试——CallServerStream 只回客户端
// 侧 send/close 错误，上游语义拒绝一律经 EndStream 尾帧从泵侧暴露，
// 由 reopen 吸收，不在这个函数的重试域内）。发送前过模型拥塞准入：
// 拥塞窗开着时在飞探针被 cap 闸住，reopen 的重发与首发一起排队打穿。
// resend 标记本次发送是同一请求在同 lane 上的续试重发：闸门把放行
// 计入窗口 retry_admits，与首发区分（号池 failover 后新 lane 的
// 首发不挂——对那条 lane 它不是续试）。
func (r *attemptRunner) send(ctx context.Context, protoRequest *devinproto.GetChatMessageRequest, resend bool) (*connect.ServerStreamForClient[devinproto.GetChatMessageResponse], error) {
	var lastErr error
	link := r.adapter.link()
	link.warmer.kickRequest()
	// sent/open 埋点幂等（CAS -1）：重试时 sent 留在首次发送、open 记首个
	// 成功的建流，sent→open 的差值如实包含退避重试耗时。
	recorder := r.env.recorder
	for attempt := 0; attempt < maxConnectAttempts; attempt++ {
		// 每次真实发送（含瞬时错误重试）都要过速率闸：被拒尝试
		// 会推后上游恢复时刻，本地整形是唯一止损点。attempt>0 与
		// 外层 resend 标记同属续试，计入窗口 retry_admits。
		if err := r.adapter.gate.wait(ctx, r.env, resend || attempt > 0); err != nil {
			// 闸门快败在起源点记 rate_gate（WriteError first-write-wins）：
			// 本函数被首发与 reopen 重试共用，reopen 路径的错误会继续
			// 冒泡经流层出口——不在此处落 stage 会被盖成 provider_stream，
			// 本地限流被误归上游责任。
			var failure *llm.Failure
			if errors.As(err, &failure) && failure.LocalGate {
				recorder.WriteError(debuglog.ErrStageRateGate, err)
			}
			return nil, err
		}
		if attempt > 0 {
			// ±25% 抖动：上游瞬时拥塞时固定节拍的重试会相互叠加。
			base := time.Duration(attempt) * 400 * time.Millisecond
			backoff := time.Duration(float64(base) * (0.75 + 0.5*rand.Float64()))
			select {
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			case <-time.After(backoff):
			}
		}
		// 模型拥塞准入放在闸门与退避之后、真实发送之前：槽只在发送
		// 瞬间持有，排队/等待时不占用——拥塞期该模型的在飞探针被
		// cap 闸住，等待者按到达序补位。
		release, err := r.adapter.congest.acquire(ctx, r.binding.Model)
		if err != nil {
			return nil, err
		}
		recorder.NoteUpstreamSend()
		// 保温簿记的 lastTouch 只看客户端可归因上行：每次真实发送
		//（含瞬时重试）都刷新——ping 不走本函数，记独立的 lastPingAt。
		r.adapter.warm.noteSend(r.warmKey)
		// httptrace 随 ctx 进 transport：GotConn 报告本次发送拿到的是
		// 复用连接还是新握手——connect 段偏慢时据此区分「dial+TLS 成本」
		// 与「上游响应头延迟」两类成因。
		var conn httptrace.GotConnInfo
		traceCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { conn = info },
		})
		stream, err := link.stream.GetChatMessage(traceCtx, connect.NewRequest(protoRequest))
		release()
		if err == nil {
			recorder.NoteUpstreamOpen()
			recorder.NoteUpstreamConn(conn.Reused, conn.IdleTime)
			return stream, nil
		}
		lastErr = err
		if !isTransientConnectError(err) {
			break
		}
	}
	r.adapter.noteModelDenied(r.binding.Model, lastErr)
	r.adapter.noteAttachmentDenied(r.binding.Model, lastErr)
	r.adapter.gate.noteUpstreamError(lastErr)
	return nil, lastErr
}

// resend 是一次完整续试：换 token 重建请求体（mutate 应用请求级变形）、
// 记续试账、过闸发送。构建失败与发送失败都记 retry_failed 标记行——
// 原三处重发点只有 reopen 两侧都记（自愈只记构建失败、extend 两侧都
// 不记），此处统一口径；构建失败按 errAttemptBuild 回传，调用方据此
// 决定保留原始失败还是顶替。
func (r *attemptRunner) resend(ctx context.Context, cause string, mutate func(*llm.RequestMessages), continueEmpty bool) (*connect.ServerStreamForClient[devinproto.GetChatMessageResponse], error) {
	built, err := r.rebuild(mutate)
	if err != nil {
		r.noteRetryFailed(err)
		return nil, fmt.Errorf("%w: %w", errAttemptBuild, err)
	}
	r.noteAttempt(cause, built, continueEmpty)
	stream, err := r.send(ctx, built, true)
	if err != nil {
		r.noteRetryFailed(err)
		return nil, err
	}
	return stream, nil
}
