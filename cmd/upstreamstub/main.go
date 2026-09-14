// upstreamstub 是上游断流故障注入桩：以真实 Connect 流式帧格式响应
// GetChatMessage，按场景在 envelope 写到一半时收尾，让客户端读帧器得到
// 「incomplete envelope: unexpected EOF」——与线上 TCP 断流在读帧视角同构。
// 用于验证传输断裂的重试链路与错误分层归类。
package main

import (
	"encoding/binary"
	"flag"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	devinproto "local/devinproto"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var requestCount atomic.Int64

func main() {
	listen := flag.String("listen", "127.0.0.1:48090", "监听地址")
	scenario := flag.String("scenario", "precontent", "precontent|midcontent|recover")
	recoverAfter := flag.Int64("recover-after", 1, "recover 场景下前 N 次请求截断，之后返回完整流")
	flag.Parse()

	http.HandleFunc("/exa.api_server_pb.ApiServerService/GetChatMessage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		json := r.Header.Get("Content-Type") == "application/connect+json"
		n := requestCount.Add(1)
		truncate := *scenario != "recover" || n <= *recoverAfter
		var body []byte
		if truncate {
			switch *scenario {
			case "midcontent":
				// 先吐内容再断：客户端已产出 delta，属于不可重发场景。
				body = join(
					frame(deltaText("stub: hello "), json),
					frame(deltaText("world"), json))
			default:
				// precontent/recover 的截断分支：只发不产生内容的元数据帧。
				body = frame(metaFrame(), json)
			}
			// 追加半个 envelope 前缀（5 字节头只写 2 字节）：读帧器
			// ReadFull 得到 ErrUnexpectedEOF，与 TCP 半路断开等效。
			body = append(body, 0x00, 0x00)
		} else {
			body = join(
				frame(metaFrame(), json),
				frame(deltaText("stub: recovered reply"), json),
				frame(stopFrame(), json),
				endStream(),
			)
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		if json {
			w.Header().Set("Content-Type", "application/connect+json")
		}
		_, _ = w.Write(body)
		log.Printf("request #%d scenario=%s truncate=%v bytes=%d", n, *scenario, truncate, len(body))
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

// endStream 编码流终止 envelope：0x02 标志 + JSON 尾帧体。
func endStream() []byte {
	payload := []byte("{}")
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
