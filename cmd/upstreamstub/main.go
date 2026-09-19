// upstreamstub 是上游断流故障注入桩：以真实 Connect 流式帧格式响应
// GetChatMessage，按场景在 envelope 写到一半时收尾，让客户端读帧器得到
// 「incomplete envelope: unexpected EOF」——与线上 TCP 断流在读帧视角同构。
// 也可模拟静默收尾、上游错误尾帧、挂死、坏帧，用于验证重试链路与错误分层。
// stream 场景提供正常全流（可配 delta 数/大小/间隔/TTFT），
// 作为 scripts/perf/snapshot.sh 的确定性压测后端。
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	devinproto "local/devinproto"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var requestCount atomic.Int64

// main 起上游桩服务：GetChatMessage 按 -scenario 产出预设帧形态，
// 其余 RPC 一律断开连接。
func main() {
	listen := flag.String("listen", "127.0.0.1:48090", "监听地址")
	scenario := flag.String("scenario", "precontent",
		"precontent|midcontent|recover|cleaneof|cleaneof-content|bare-end|endstream-error|preframe-error|badframe|badflags|end-hang|heartbeat|stall|stream")
	recoverAfter := flag.Int64("recover-after", 1, "recover 场景下前 N 次请求截断，之后返回完整流")
	resetIn := flag.Int("reset-in", 30, "preframe-error 场景限流文案的 reset 秒数（<0 = 不带 hint，走 defaultLatch）")
	deltas := flag.Int("deltas", 200, "stream 场景的 delta 帧数")
	deltaBytes := flag.Int("delta-bytes", 32, "stream 场景每帧 delta 字节数")
	interval := flag.Duration("interval", 0, "stream 场景帧间隔（0 = 连续吐帧）")
	ttfb := flag.Duration("ttfb", 0, "stream 场景首帧前延迟（模拟上游思考 TTFT）")
	cacheMode := flag.String("cache-mode", "none",
		"none|content|trajectory：模拟上游前缀缓存记账并在 usage 帧回 cache_read——content 跨轨迹内容匹配，trajectory 只匹配同 trajectory_id 的既往请求；匹配按 EPHEMERAL 断点边界计")
	flag.Parse()
	// 未知 scenario 拼错不能静默落到某个场景——那会让测试对着错误
	// 行为判结果。启动期直接拒绝。
	validScenarios := map[string]bool{
		"precontent": true, "midcontent": true, "recover": true, "cleaneof": true,
		"cleaneof-content": true, "bare-end": true, "endstream-error": true,
		"badframe": true, "badflags": true, "end-hang": true, "heartbeat": true,
		"stall": true, "stream": true, "preframe-error": true,
	}
	if !validScenarios[*scenario] {
		log.Fatalf("unknown scenario %q", *scenario)
	}
	cache := &prefixCache{mode: *cacheMode}
	if *cacheMode != "none" && *cacheMode != "content" && *cacheMode != "trajectory" {
		log.Fatalf("unknown cache-mode %q", *cacheMode)
	}

	http.HandleFunc("/exa.api_server_pb.ApiServerService/GetChatMessage", func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		jsonWire := r.Header.Get("Content-Type") == "application/connect+json"
		usage := cache.observe(reqBody, jsonWire, r.Header.Get("Content-Encoding"))
		n := requestCount.Add(1)
		log.Printf("request #%d scenario=%s cache_read=%d input=%d", n, *scenario, usage.GetCacheReadTokens(), usage.GetInputTokens())
		contentType := "application/connect+proto"
		if jsonWire {
			contentType = "application/connect+json"
		}
		var body []byte
		switch *scenario {
		case "stall":
			// 流建立后无限静默：验证静默看门狗与判死重发。睡超时两倍兜底。
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			log.Printf("stall: headers sent, hanging")
			time.Sleep(5 * time.Minute)
			return
		case "midcontent":
			// 先吐内容再断：客户端已产出 delta，属于不可重发场景。
			body = join(
				frame(deltaText("stub: hello "), jsonWire),
				frame(deltaText("world"), jsonWire))
			body = append(body, 0x00, 0x00) // 半帧前缀 → ErrUnexpectedEOF
		case "cleaneof":
			// 元数据帧后协议中途干净收尾（无尾帧）：静默截断，pre-content 应重发。
			body = frame(metaFrame(usage), jsonWire)
		case "cleaneof-content":
			// 内容帧后干净收尾：已产出内容的静默截断，不可重发。
			body = join(frame(metaFrame(usage), jsonWire), frame(deltaText("stub: partial"), jsonWire))
		case "bare-end":
			// 有 EndStream 尾帧但无 stopReason：上游「正常结束但没给理由」，
			// 复现线上 "Devin stream ended without stop reason"。
			body = join(frame(metaFrame(usage), jsonWire), endStream("{}"))
		case "endstream-error":
			// 尾帧携带错误：上游经 EndStream 主动报语义错误（限流形态）。
			body = join(frame(metaFrame(usage), jsonWire),
				endStream(`{"error":{"code":"resource_exhausted","message":"stub: rate limited"}}`))
		case "preframe-error":
			// 建流即拒：200 + 仅一条 EndStream 错误尾帧，前面没有任何数据帧——
			// 真实上游的语义拒绝形态（不在 HTTP 层给限流信号，见
			// docs/upstream-rate-limit.md）。无任何数据帧意味客户端侧
			// noteUpstreamSuccess 不触发，noteUpstreamError 直接上闩/延闩，
			// 覆盖 drip 探针的 extended 路径；reset-in 文案让 RateLimitReset
			// 对齐路径可达（<0 时不带 hint，回落 defaultLatch）。
			body = endStream(rateLimitErrorJSON(*resetIn))
		case "badframe":
			// 垃圾字节充当 envelope：unmarshal/帧级解析失败路径。
			body = []byte{0xff, 0xff, 0xff, 0xff, 0xff}
		case "badflags":
			// 完整 envelope 但 flag 字节非法（0x04 未定义位）：connect-go 报
			// CodeInternal "protocol error: invalid envelope flags"。
			b := frame(deltaText("stub: never read"), jsonWire)
			b[0] = 0x04
			body = b
		case "end-hang":
			// 完整终止序列（stopReason + EndStream）后传输层不收尾：
			// 语义终帧到达即应完成，客户端不该等 TCP 关闭（ccLoad #80 类）。
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(join(
				frame(metaFrame(usage), jsonWire),
				frame(deltaText("stub: done"), jsonWire),
				frame(stopFrame(), jsonWire),
				endStream("{}")))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			log.Printf("end-hang: stream complete, hanging")
			time.Sleep(5 * time.Minute)
			return
		case "heartbeat":
			// 周期无事件帧续命：元数据帧每 3s 一帧永不终止——看门狗若
			// 按「任意帧」判活将永不判死（ccLoad #119 纯 keepalive 挂死类）。
			// 断连后写失败被忽略、循环空转到进程退出：stub 的生命周期
			// 就是拉起它的测试进程，不值得为常驻泄漏加复杂度。
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(http.StatusOK)
			for {
				_, _ = w.Write(frame(metaFrame(usage), jsonWire))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(3 * time.Second)
			}
		case "stream":
			// 正常全流：meta → 可选 TTFT 静默 → N 个 delta（逐帧 flush，
			// 真实驱动代理的逐帧投影/编码/下发路径）→ stop → endStream。
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write(frame(metaFrame(usage), jsonWire))
			if flusher != nil {
				flusher.Flush()
			}
			if *ttfb > 0 {
				time.Sleep(*ttfb)
			}
			// 帧内容固定：只 marshal 一次，桩侧不引入逐帧序列化成本，
			// 压测瓶颈如实落在被测代理的投影/编码路径上。
			deltaFrame := frame(deltaText(strings.Repeat("x", *deltaBytes)), jsonWire)
			for i := 0; i < *deltas; i++ {
				_, _ = w.Write(deltaFrame)
				if flusher != nil {
					flusher.Flush()
				}
				if *interval > 0 {
					time.Sleep(*interval)
				}
			}
			_, _ = w.Write(join(frame(stopFrame(), jsonWire), endStream("{}")))
			if flusher != nil {
				flusher.Flush()
			}
			return
		case "recover":
			if n <= *recoverAfter {
				body = append(frame(metaFrame(usage), jsonWire), 0x00, 0x00)
			} else {
				body = join(
					frame(metaFrame(usage), jsonWire),
					frame(deltaText("stub: recovered reply"), jsonWire),
					frame(stopFrame(), jsonWire),
					endStream("{}"))
			}
		default: // precontent：启动期已校验，余下只有它
			body = append(frame(metaFrame(usage), jsonWire), 0x00, 0x00)
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
		log.Printf("done #%d bytes=%d", n, len(body))
	})
	// 其余 RPC（GetCliModelConfigs 等）不实现：直接断开让调用方走目录缺失路径。
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	})
	log.Printf("upstreamstub listening on %s scenario=%s", *listen, *scenario)
	log.Fatal(http.ListenAndServe(*listen, nil))
}

// frame 编码一条 Connect 流式 envelope：0x00 标志 + 4 字节大端长度 + 消息体。
func frame(msg *devinproto.GetChatMessageResponse, json bool) []byte {
	payload, err := marshal(msg, json)
	if err != nil {
		log.Fatalf("marshal response: %v", err)
	}
	out := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	return append(out, payload...)
}

// endStream 编码流终止 envelope：0x02 标志 + JSON 尾帧体
// （EndStreamResponse：{"error":..,"metadata":..} 或 {}）。
func endStream(payload string) []byte {
	out := make([]byte, 5, 5+len(payload))
	out[0] = 0x02
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	return append(out, payload...)
}

// rateLimitErrorJSON 造 preframe-error 场景的 EndStream 尾帧体，文案对齐
// 真实上游限流指纹（无 RetryInfo detail，hint 只在文案）。resetIn<0 时
// 省略 reset 声明。
func rateLimitErrorJSON(resetIn int) string {
	if resetIn < 0 {
		return `{"error":{"code":"resource_exhausted","message":"Reached overall message rate limit. Please try again later."}}`
	}
	return fmt.Sprintf(`{"error":{"code":"resource_exhausted","message":"Reached overall message rate limit. Please try again later. Your limit will reset in %d seconds."}}`, resetIn)
}

// marshal 按客户端请求的 wire 编码（connect+proto 或 connect+json）序列化响应。
func marshal(msg *devinproto.GetChatMessageResponse, json bool) ([]byte, error) {
	if json {
		return protojson.Marshal(msg)
	}
	return proto.Marshal(msg)
}

// metaFrame 造流首的元数据响应帧（message_id/request_id/timestamp/usage）。
// usage 为 nil 时只带 ModelUid——cache-mode=none 下桩不编造 token 计数。
func metaFrame(usage *devinproto.ExaCodeiumCommonPb_ModelUsageStats) *devinproto.GetChatMessageResponse {
	if usage == nil {
		usage = &devinproto.ExaCodeiumCommonPb_ModelUsageStats{}
	}
	usage.ModelUid = proto.String("swe-2-max")
	return &devinproto.GetChatMessageResponse{
		MessageId: proto.String("bot-stub"),
		RequestId: proto.String("stub-req"),
		Timestamp: &devinproto.GoogleProtobuf_Timestamp{Seconds: proto.Int64(time.Now().Unix())},
		Usage:     usage,
	}
}

// deltaText 造一帧增量文本响应。
func deltaText(text string) *devinproto.GetChatMessageResponse {
	return &devinproto.GetChatMessageResponse{DeltaText: proto.String(text)}
}

// stopFrame 造带 stop_reason 的终止响应帧。
func stopFrame() *devinproto.GetChatMessageResponse {
	return &devinproto.GetChatMessageResponse{
		StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum(),
	}
}

// join 把多条已编码 envelope 顺序拼接成一个响应体。
func join(frames ...[]byte) []byte {
	var out []byte
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

// ---- 前缀缓存模拟（-cache-mode）----
//
// 记账模型：每个 GetChatMessageRequest 拆成「内容单元」序列——单元 0 是
// system prompt，其后每条 ChatMessagePrompt 一个单元。可复用前缀只算到
// EPHEMERAL 断点边界（SystemPromptCacheOptions / PromptCacheOptions），
// 与真实上游「断点声明缓存边界」语义对齐：读侧与写侧都要把该位置标成
// 断点才算可读条目。命中量取最长共享断点前缀的字节数，折 4 字节≈1 token
// 报进 usage 帧。message_id 是每请求重生的易变字段（生产重发也换 id 仍
// 命中），进单元哈希前摘除；trajectory 模式按 trajectory_id 隔离命中域。
type prefixCache struct {
	mode string
	seen []cacheEntry
}

// cacheEntry 是一条请求的可缓存前缀视图：traj 是隔离域键，keys/lens/marks
// 三元组按单元序对齐（marks[i] = 第 i 单元结尾是否标了 EPHEMERAL）。
type cacheEntry struct {
	traj  string
	keys  [][sha256.Size]byte
	lens  []int
	marks []bool
}

// observe 登记一条请求并算本次该得的 cache_read/input 计数；
// mode=none 或请求解不开时返回 nil（usage 帧退回只带 ModelUid 的旧形态）。
func (c *prefixCache) observe(body []byte, jsonWire bool, contentEncoding string) *devinproto.ExaCodeiumCommonPb_ModelUsageStats {
	if c.mode == "none" {
		return nil
	}
	req := decodeChatRequest(body, jsonWire, contentEncoding)
	if req == nil {
		return nil
	}
	entry := extractEntry(req)
	best := 0
	for _, prior := range c.seen {
		if c.mode == "trajectory" && prior.traj != entry.traj {
			continue
		}
		if matched := matchPrefixBytes(prior, entry); matched > best {
			best = matched
		}
	}
	c.seen = append(c.seen, entry)
	input := 0
	for _, l := range entry.lens {
		input += l
	}
	return &devinproto.ExaCodeiumCommonPb_ModelUsageStats{
		InputTokens:     proto.Uint64(uint64(input / 4)),
		CacheReadTokens: proto.Uint64(uint64(best / 4)),
	}
}

// decodeChatRequest 从请求体还原 GetChatMessageRequest：connect 流式请求
// 是 envelope 序列（1B flags + 4B 大端长度 + payload），发送压缩按 payload
// 逐个 gzip 且 flags 置 0x01（0x02 是 trailer）；unary 形态则由
// Content-Encoding: gzip 标整体压缩。取第一条数据帧。
func decodeChatRequest(body []byte, jsonWire bool, contentEncoding string) *devinproto.GetChatMessageRequest {
	if contentEncoding == "gzip" {
		if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			if decoded, err := io.ReadAll(zr); err == nil {
				body = decoded
			}
			_ = zr.Close()
		}
		return unmarshalChatReq(body, jsonWire)
	}
	for len(body) >= 5 {
		flags := body[0]
		n := int(binary.BigEndian.Uint32(body[1:5]))
		if len(body) < 5+n {
			break
		}
		payload := body[5 : 5+n]
		body = body[5+n:]
		if flags&0x02 != 0 {
			continue
		}
		if flags&0x01 != 0 {
			zr, err := gzip.NewReader(bytes.NewReader(payload))
			if err != nil {
				return nil
			}
			decoded, err := io.ReadAll(zr)
			_ = zr.Close()
			if err != nil {
				return nil
			}
			payload = decoded
		}
		return unmarshalChatReq(payload, jsonWire)
	}
	return nil
}

// unmarshalChatReq 按 wire 编码解一帧请求消息；失败只丢本次记账。
func unmarshalChatReq(payload []byte, jsonWire bool) *devinproto.GetChatMessageRequest {
	req := &devinproto.GetChatMessageRequest{}
	var err error
	if jsonWire {
		err = protojson.Unmarshal(payload, req)
	} else {
		err = proto.Unmarshal(payload, req)
	}
	if err != nil {
		log.Printf("cache-mode: unmarshal request: %v", err)
		return nil
	}
	return req
}

// extractEntry 把请求投影为内容单元序列：system prompt 作单元 0（断点取
// SystemPromptCacheOptions），每条消息一个单元（断点取 PromptCacheOptions，
// MessageId/PromptCacheOptions 不参与哈希——标记与易变 id 都不是内容）。
func extractEntry(req *devinproto.GetChatMessageRequest) cacheEntry {
	e := cacheEntry{traj: req.GetTrajectoryReference().GetTrajectoryId()}
	if prompt := req.GetPrompt(); prompt != "" {
		e.keys = append(e.keys, sha256.Sum256([]byte(prompt)))
		e.lens = append(e.lens, len(prompt))
		e.marks = append(e.marks, req.GetSystemPromptCacheOptions() != nil)
	}
	for _, msg := range req.GetChatMessagePrompts() {
		clone := proto.Clone(msg).(*devinproto.ExaChatPb_ChatMessagePrompt)
		clone.MessageId = nil
		clone.PromptCacheOptions = nil
		raw, _ := proto.Marshal(clone)
		e.keys = append(e.keys, sha256.Sum256(raw))
		e.lens = append(e.lens, len(raw))
		e.marks = append(e.marks, msg.GetPromptCacheOptions() != nil)
	}
	return e
}

// matchPrefixBytes 返回 prior 与 cur 共享的最长「断点闭合」前缀字节量：
// 逐单元比对到首个分歧得公共前缀，再取其中最大的两侧都标了断点的边界。
func matchPrefixBytes(prior, cur cacheEntry) int {
	common := 0
	for common < len(prior.keys) && common < len(cur.keys) && prior.keys[common] == cur.keys[common] {
		common++
	}
	boundary := -1
	for i := 0; i < common; i++ {
		if prior.marks[i] && cur.marks[i] {
			boundary = i
		}
	}
	if boundary < 0 {
		return 0
	}
	sum := 0
	for j := 0; j <= boundary; j++ {
		sum += prior.lens[j]
	}
	return sum
}
