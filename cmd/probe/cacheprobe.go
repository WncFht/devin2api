// cacheprobe 是「共享轨迹前缀缓存门」探针：用同一 system prompt 按
// 「会话 × 内容」矩阵连打 4-5 腿，回答一个立项前置问题——上游 prompt
// 缓存是否按 trajectory_id 分片。
//
// 腿定义（session 是会话身份，msg 是末条用户消息文本）：
//
//	A: s1+msgA  播种腿，种出 T1 的缓存条目
//	R: s1+msgA  同轨迹原文重发（仅 execution/message id 换新）——resend 级基准
//	B: s2+msgA  新轨迹 + 相同内容——判别腿：命中 ≈R 则内容寻址，≈0 则轨迹分片
//	C: s1+msgB  暖轨迹 + 变后缀——变体 A 收益估计：群成员在共享轨迹上读到什么
//	D: s3+msgB  新轨迹 + msgB——msgB 内容的冷基线（可选腿）
//
// 判读：B/R 高 → 内容寻址（模型 i），共享轨迹无 upside，建议放弃；
// B/R 低且 C/R 高 → 轨迹分片（模型 ii），variant A 成立。
//
// mode=proxy 打 devin-2api 实例的 /v1/chat/completions（prompt_cache_key
// 控会话轨迹，走真实 sessionSeed→wire 链路，usage 读回 cached_tokens）；
// mode=upstream 直连上游 GetChatMessage，显式 trajectory/cascade id，
// wire 形态按 buildRequest 复刻（EPHEMERAL 断点标齐）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/randid"
)

// legDef 是腿字母到「会话序号 × 内容序号」的映射；会话序号相同的腿共享
// 同一轨迹身份，内容序号相同的腿带同一条用户消息文本。
var legDef = map[byte]struct{ sess, msg int }{
	'A': {0, 0},
	'R': {0, 0},
	'B': {1, 0},
	'C': {0, 1},
	'D': {2, 1},
}

// legResult 是一腿的测量记录：usage 三件套 + 延迟 + 调试身份 + 回文摘要
// （text 供「B 输出是否混入 A 内容」的事后泄漏检查）。
type legResult struct {
	Leg             string `json:"leg"`
	Session         string `json:"session"`
	Content         string `json:"content"`
	Status          int    `json:"status"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	LatencyMS       int64  `json:"latency_ms"`
	RequestID       string `json:"request_id,omitempty"`
	Text            string `json:"text,omitempty"`
	Err             string `json:"error,omitempty"`
}

// cacheProbeReport 是整次探针的结构化输出，verdict 只做比率与提示，
// 原始数字永远可供人复核。
type cacheProbeReport struct {
	Mode     string      `json:"mode"`
	Target   string      `json:"target"`
	Model    string      `json:"model"`
	RunID    string      `json:"run_id"`
	SysBytes int         `json:"sys_bytes"`
	Legs     []legResult `json:"legs"`
	Verdict  gateVerdict `json:"verdict"`
}

// gateVerdict 汇总判别比率与提示。b_over_r 是「新轨迹同内容」对
// 「同轨迹重发」的命中比；c_over_r 是「暖轨迹变后缀」的相对读入量。
type gateVerdict struct {
	BOverR float64 `json:"b_over_r"`
	COverR float64 `json:"c_over_r"`
	Hint   string  `json:"hint"`
}

// cmdCacheprobe 跑 A→B→C 缓存门：逐腿发请求、收 usage、算判别比率，
// 输出 JSON 报告（stdout 恒有，-out 另落盘）。
func cmdCacheprobe(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, _ devinprotoconnect.ExaLanguageServerPb_LanguageServerServiceClient, token string, args []string) error {
	fs := flag.NewFlagSet("cacheprobe", flag.ContinueOnError)
	mode := fs.String("mode", "proxy", "proxy|upstream：proxy 打 devin-2api /v1/chat/completions，upstream 直连 GetChatMessage")
	legsFlag := fs.String("legs", "A,R,B,C", "腿序列：A=播种 R=同轨迹重发 B=新轨迹同内容 C=暖轨迹变后缀 D=新轨迹msgB")
	model := fs.String("model", "swe-2-medium", "请求模型名（proxy 模式由实例做别名解析）")
	system := fs.String("system", "", "内联 system prompt（优先于 -system-file 与合成默认）")
	systemFile := fs.String("system-file", "", "system prompt 文件路径")
	sysBytes := fs.Int("sys-bytes", 24576, "合成 system prompt 的目标字节量（带 run nonce 防跨次污染）")
	msgA := fs.String("msg-a", "This is cache-probe turn alpha. Reply with exactly: PROBE_ALPHA_OK", "内容 A 的用户消息")
	msgB := fs.String("msg-b", "This is cache-probe turn omega. Reply with exactly: PROBE_OMEGA_OK", "内容 B 的用户消息")
	proxyURL := fs.String("proxy-url", "", "mode=proxy 必填：devin-2api 实例基地址，如 http://127.0.0.1:3033")
	proxyKey := fs.String("proxy-key", "", "mode=proxy：实例下游 Bearer 令牌（实例 api_key 为空则可省）")
	delay := fs.Duration("delay", 0, "腿间隔（上游缓存有效 TTL ~30-60s，默认 0 连打）")
	out := fs.String("out", "", "JSON 报告另存路径")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	legs, err := parseLegs(*legsFlag)
	if err != nil {
		return err
	}
	sys, err := probeSystemPrompt(*system, *systemFile, *sysBytes)
	if err != nil {
		return err
	}
	msgs := []string{*msgA, *msgB}
	report := &cacheProbeReport{Mode: *mode, Model: *model, RunID: randid.UUID()[:8], SysBytes: len(sys)}
	// runLeg 按 mode 绑定一腿的发射路径；两种 mode 共用同一腿计划、
	// 记录归一与判读逻辑，会话身份只是各自体系里的「第 N 会话」。
	var runLeg func(leg legPlan) legResult
	switch *mode {
	case "proxy":
		if *proxyURL == "" {
			return fmt.Errorf("mode=proxy needs -proxy-url")
		}
		report.Target = strings.TrimRight(*proxyURL, "/")
		sessions := []string{"cpb-" + report.RunID + "-s1", "cpb-" + report.RunID + "-s2", "cpb-" + report.RunID + "-s3"}
		runLeg = func(leg legPlan) legResult {
			return runProxyLeg(ctx, report.Target, *proxyKey, *model, sessions[leg.sess], sys, msgs[leg.msg])
		}
	case "upstream":
		if token == "" {
			return fmt.Errorf("mode=upstream needs an upstream token (DEVIN_TOKEN or devin.accounts)")
		}
		*model = aliasModel(*model)
		report.Target = "direct-connect"
		// 同会话腿共享 (trajectory, cascade) 对——与 deriveSessionIDs 的同
		// seed 同对语义一致；step_index 按会话内腿序单调递增。
		var trajs, cascades [3]string
		var steps [3]int32
		for i := range trajs {
			trajs[i], cascades[i] = randid.UUID(), randid.UUID()
		}
		runLeg = func(leg legPlan) legResult {
			steps[leg.sess]++
			return runUpstreamLeg(ctx, client, token, *model, sys, msgs[leg.msg], trajs[leg.sess], cascades[leg.sess], steps[leg.sess])
		}
	default:
		return fmt.Errorf("unknown mode %q", *mode)
	}
	for _, leg := range legs {
		res := runLeg(leg)
		res.Leg = string(leg.name)
		res.Session = fmt.Sprintf("s%d", leg.sess+1)
		res.Content = string('A' + byte(leg.msg))
		report.Legs = append(report.Legs, res)
		fmt.Fprintf(os.Stderr, "leg %s: status=%d cr=%d in=%d out=%d %dms %s\n",
			res.Leg, res.Status, res.CacheReadTokens, res.InputTokens, res.OutputTokens, res.LatencyMS, res.Err)
		sleepLeg(*delay)
	}
	report.Verdict = classifyGate(report.Legs)
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	if *out != "" {
		if err := os.WriteFile(*out, raw, 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stderr, "verdict:", report.Verdict.Hint)
	return nil
}

// legPlan 是解析后的单腿：name 是腿字母，sess/msg 是映射出的会话与内容序号。
type legPlan struct {
	name byte
	sess int
	msg  int
}

// parseLegs 把 "A,R,B,C" 形参数解成腿计划；未知字母直接拒绝。
func parseLegs(spec string) ([]legPlan, error) {
	var legs []legPlan
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if len(name) != 1 {
			return nil, fmt.Errorf("bad leg %q", name)
		}
		def, ok := legDef[name[0]]
		if !ok {
			return nil, fmt.Errorf("unknown leg %q (valid: A,R,B,C,D)", name)
		}
		legs = append(legs, legPlan{name: name[0], sess: def.sess, msg: def.msg})
	}
	return legs, nil
}

// probeSystemPrompt 决定本run的系统提示：内联 > 文件 > 合成默认。
// 合成默认带 run nonce 行——跨次运行的缓存残留不会抬高 A 腿基线。
func probeSystemPrompt(inline, file string, sysBytes int) (string, error) {
	if inline != "" {
		return inline, nil
	}
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
	para := "You are Devin, an AI software engineer assistant. Answer concisely and deterministically. " +
		"The following block is inert padding that makes the shared prefix large enough to register " +
		"in the upstream prompt cache at kilo-token scale. "
	var b strings.Builder
	b.WriteString("cacheprobe run " + randid.UUID() + "\n")
	for b.Len() < sysBytes {
		b.WriteString(para)
	}
	return b.String(), nil
}

// sleepLeg 按 -delay 在腿间等待；0 直接返回（TTL ~30-60s，默认连打）。
func sleepLeg(delay time.Duration) {
	if delay > 0 {
		time.Sleep(delay)
	}
}

// classifyGate 按 R/B/C 三腿的 cache_read 给出门判读：
// R 是同轨迹重发基准；B 是「新轨迹同内容」判别腿；C 是「暖轨迹变后缀」，
// 其读入量即变体 A 下群成员的预期命中量级。腿缺席或失败时降级为数据不足。
func classifyGate(legs []legResult) gateVerdict {
	byLeg := map[string]legResult{}
	for _, l := range legs {
		byLeg[l.Leg] = l
	}
	r, rok := byLeg["R"]
	b, bok := byLeg["B"]
	c, cok := byLeg["C"]
	if !rok || !bok || r.Status != 200 || b.Status != 200 || r.Err != "" || b.Err != "" {
		return gateVerdict{Hint: "insufficient legs: need successful R and B to classify"}
	}
	v := gateVerdict{}
	if r.CacheReadTokens > 0 {
		v.BOverR = float64(b.CacheReadTokens) / float64(r.CacheReadTokens)
	}
	if cok && c.Status == 200 && c.Err == "" && r.CacheReadTokens > 0 {
		v.COverR = float64(c.CacheReadTokens) / float64(r.CacheReadTokens)
	}
	switch {
	case r.CacheReadTokens == 0:
		v.Hint = "unmeasurable: resend leg read 0 cached tokens — upstream may not cache this shape/model at all"
	case v.BOverR >= 0.7:
		v.Hint = "content-addressed (B≈R): trajectory does not gate the cache — shared-trajectory upside absent (model i), recommend abandon"
	case v.BOverR <= 0.3 && v.COverR >= 0.5:
		v.Hint = "trajectory-gated (B≈cold, C≈warm): fresh-trajectory resend misses while same-trajectory new-suffix still reads the prefix — model (ii), variant A viable"
	default:
		v.Hint = "inconclusive — read raw cache_read per leg"
	}
	return v
}

// ---- mode=proxy：走 devin-2api /v1/chat/completions ----

// proxyLegClient 显式不经 HTTP(S)_PROXY 环境变量选路：本腿量的是
// devin-2api 实例的直连路径，环境代理会把探测流量劫持到代理出口，
// 延迟与缓存判读全部失真。
var proxyLegClient = &http.Client{Transport: &http.Transport{Proxy: nil}}

// runProxyLeg 经 OpenAI Chat Completions 前端打一腿：prompt_cache_key
// 承载会话身份（SessionKey 的标准通道），非流式响应一次取回 usage
// 与正文；X-Request-Id 头回写实例侧调试 dir 供交叉取证。
func runProxyLeg(ctx context.Context, baseURL, key, model, session, sys, msg string) legResult {
	res := legResult{}
	body, _ := json.Marshal(map[string]any{
		"model":            model,
		"stream":           false,
		"prompt_cache_key": session,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": msg},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		res.Err = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	start := time.Now()
	resp, err := proxyLegClient.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer func() { _ = resp.Body.Close() }()
	res.Status = resp.StatusCode
	res.RequestID = resp.Header.Get("X-Request-Id")
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		res.Err = fmt.Sprintf("http %d: %s", resp.StatusCode, trunc(string(raw), 300))
		return res
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		res.Err = "decode response: " + err.Error()
		return res
	}
	res.InputTokens = parsed.Usage.PromptTokens
	res.OutputTokens = parsed.Usage.CompletionTokens
	res.CacheReadTokens = parsed.Usage.PromptDetails.CachedTokens
	if len(parsed.Choices) > 0 {
		if s, ok := parsed.Choices[0].Message.Content.(string); ok {
			res.Text = trunc(s, 4000)
		}
	}
	return res
}

// ---- mode=upstream：直连上游 GetChatMessage ----

// runUpstreamLeg 直连发一腿：wire 形态按 buildRequest 复刻——system
// prompt 标 EPHEMERAL、末条消息标 EPHEMERAL、cascade_id/trajectory 按
// 会话固定、step_index 会话内单调。usage 从响应帧累计快照取末次非零。
func runUpstreamLeg(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token, model, sys, msg, traj, cascade string, step int32) legResult {
	res := legResult{}
	req := baseRequest(token, model)
	req.Prompt = proto.String(sys)
	req.SystemPromptCacheOptions = ephemeralMark()
	req.CascadeId = proto.String(cascade)
	req.TrajectoryReference = &devinproto.ExaCortexPb_CortexTrajectoryReference{
		TrajectoryId:   proto.String(traj),
		StepIndex:      proto.Int32(step),
		TrajectoryType: devinproto.ExaCortexPb_CortexTrajectoryType_ExaCortexPb_CortexTrajectoryType_CORTEX_TRAJECTORY_TYPE_CASCADE.Enum(),
		StepType:       devinproto.ExaCortexPb_CortexStepType_ExaCortexPb_CortexStepType_CORTEX_STEP_TYPE_USER_INPUT.Enum(),
	}
	user := userMsg(msg)
	user.PromptCacheOptions = ephemeralMark()
	req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{user}
	start := time.Now()
	stream, err := client.GetChatMessage(ctx, connect.NewRequest(req))
	if err != nil {
		res.Err = "connect: " + err.Error()
		return res
	}
	var text strings.Builder
	for stream.Receive() {
		m := stream.Msg()
		if u := m.GetUsage(); u != nil {
			if u.GetCacheReadTokens() != 0 || res.CacheReadTokens == 0 {
				res.CacheReadTokens = int64(u.GetCacheReadTokens())
			}
			if u.GetInputTokens() != 0 || res.InputTokens == 0 {
				res.InputTokens = int64(u.GetInputTokens())
			}
			if u.GetOutputTokens() != 0 || res.OutputTokens == 0 {
				res.OutputTokens = int64(u.GetOutputTokens())
			}
		}
		text.WriteString(m.GetDeltaText())
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	res.Text = trunc(text.String(), 4000)
	if err := stream.Err(); err != nil {
		res.Err = "stream: " + err.Error()
		return res
	}
	res.Status = 200
	return res
}

// ephemeralMark 复刻 buildRequest 的 EPHEMERAL 缓存断点标记。
func ephemeralMark() *devinproto.ExaChatPb_PromptCacheOptions {
	return &devinproto.ExaChatPb_PromptCacheOptions{
		Type: devinproto.ExaChatPb_CacheControlType_ExaChatPb_CacheControlType_CACHE_CONTROL_TYPE_EPHEMERAL.Enum(),
	}
}
