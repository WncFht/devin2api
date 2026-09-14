// upstreamstub 是上游断流故障注入桩：以真实 Connect 流式帧格式响应
// GetChatMessage，按场景在 envelope 写到一半时收尾，让客户端读帧器得到
// 「incomplete envelope: unexpected EOF」——与线上 TCP 断流在读帧视角同构。
// 也可模拟静默收尾、上游错误尾帧、挂死、坏帧，用于验证重试链路与错误分层。
// stream 场景提供正常全流（可配 delta 数/大小/间隔/TTFT），
// 作为 perf-snapshot 的确定性压测后端。
package main

import (
	"encoding/binary"
	"flag"
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

func main() {
	listen := flag.String("listen", "127.0.0.1:48090", "监听地址")
	scenario := flag.String("scenario", "precontent",
		"precontent|midcontent|recover|cleaneof|cleaneof-content|bare-end|endstream-error|badframe|badflags|end-hang|heartbeat|stall|stream")
	recoverAfter := flag.Int64("recover-after", 1, "recover 场景下前 N 次请求截断，之后返回完整流")
	deltas := flag.Int("deltas", 200, "stream 场景的 delta 帧数")
	deltaBytes := flag.Int("delta-bytes", 32, "stream 场景每帧 delta 字节数")
	interval := flag.Duration("interval", 0, "stream 场景帧间隔（0 = 连续吐帧）")
	ttfb := flag.Duration("ttfb", 0, "stream 场景首帧前延迟（模拟上游思考 TTFT）")
	flag.Parse()
	// 未知 scenario 拼错不能静默落到某个场景——那会让测试对着错误
	// 行为判结果。启动期直接拒绝。
	validScenarios := map[string]bool{
		"precontent": true, "midcontent": true, "recover": true, "cleaneof": true,
		"cleaneof-content": true, "bare-end": true, "endstream-error": true,
		"badframe": true, "badflags": true, "end-hang": true, "heartbeat": true,
		"stall": true, "stream": true,
	}
	if !validScenarios[*scenario] {
		log.Fatalf("unknown scenario %q", *scenario)
	}

	http.HandleFunc("/exa.api_server_pb.ApiServerService/GetChatMessage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		jsonWire := r.Header.Get("Content-Type") == "application/connect+json"
		n := requestCount.Add(1)
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
			log.Printf("request #%d scenario=stall: headers sent, hanging", n)
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
			body = frame(metaFrame(), jsonWire)
		case "cleaneof-content":
			// 内容帧后干净收尾：已产出内容的静默截断，不可重发。
			body = join(frame(metaFrame(), jsonWire), frame(deltaText("stub: partial"), jsonWire))
		case "bare-end":
			// 有 EndStream 尾帧但无 stopReason：上游「正常结束但没给理由」，
			// 复现线上 "Devin stream ended without stop reason"。
			body = join(frame(metaFrame(), jsonWire), endStream("{}"))
		case "endstream-error":
			// 尾帧携带错误：上游经 EndStream 主动报语义错误（限流形态）。
			body = join(frame(metaFrame(), jsonWire),
				endStream(`{"error":{"code":"resource_exhausted","message":"stub: rate limited"}}`))
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
				frame(metaFrame(), jsonWire),
				frame(deltaText("stub: done"), jsonWire),
				frame(stopFrame(), jsonWire),
				endStream("{}")))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			log.Printf("request #%d scenario=end-hang: stream complete, hanging", n)
			time.Sleep(5 * time.Minute)
			return
		case "heartbeat":
			// 周期无事件帧续命：元数据帧每 3s 一帧永不终止——看门狗若
			// 按「任意帧」判活将永不判死（ccLoad #119 纯 keepalive 挂死类）。
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(http.StatusOK)
			for i := 0; i < 100; i++ {
				_, _ = w.Write(frame(metaFrame(), jsonWire))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(3 * time.Second)
			}
			return
		case "stream":
			// 正常全流：meta → 可选 TTFT 静默 → N 个 delta（逐帧 flush，
			// 真实驱动代理的逐帧投影/编码/下发路径）→ stop → endStream。
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write(frame(metaFrame(), jsonWire))
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
				body = append(frame(metaFrame(), jsonWire), 0x00, 0x00)
			} else {
				body = join(
					frame(metaFrame(), jsonWire),
					frame(deltaText("stub: recovered reply"), jsonWire),
					frame(stopFrame(), jsonWire),
					endStream("{}"))
			}
		default: // precontent：启动期已校验，余下只有它
			body = append(frame(metaFrame(), jsonWire), 0x00, 0x00)
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
		log.Printf("request #%d scenario=%s bytes=%d", n, *scenario, len(body))
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

func marshal(msg *devinproto.GetChatMessageResponse, json bool) ([]byte, error) {
	if json {
		return protojson.Marshal(msg)
	}
	return proto.Marshal(msg)
}

func metaFrame() *devinproto.GetChatMessageResponse {
	return &devinproto.GetChatMessageResponse{
		MessageId: proto.String("bot-stub"),
		RequestId: proto.String("stub-req"),
		Timestamp: &devinproto.GoogleProtobuf_Timestamp{Seconds: proto.Int64(time.Now().Unix())},
		Usage: &devinproto.ExaCodeiumCommonPb_ModelUsageStats{
			ModelUid: proto.String("swe-2-max"),
		},
	}
}

func deltaText(text string) *devinproto.GetChatMessageResponse {
	return &devinproto.GetChatMessageResponse{DeltaText: proto.String(text)}
}

func stopFrame() *devinproto.GetChatMessageResponse {
	return &devinproto.GetChatMessageResponse{
		StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum(),
	}
}

func join(frames ...[]byte) []byte {
	var out []byte
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}
