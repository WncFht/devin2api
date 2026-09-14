// loadtest 是本服务的流式压测客户端：固定并发打 /v1/* 端点，
// 逐请求记录 TTFB（首个响应字节）与总时长，聚合输出分位数与吞吐。
// SSE 流的时延特征是首字与逐帧间隔——普通压测工具把整流当一个响应，
// 测不出 TTFB；这里逐请求采首字节时刻。上游侧由 upstreamstub 的
// stream 场景提供确定性负载，不烧真实配额。
//
// 用法：loadtest -url http://localhost:3003/v1/chat/completions -c 8 -n 200
//
//	loadtest -duration 30s -c 16   # 持续模式：压满时长供 pprof 采样
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// result 是一次请求的测量：ttfb 是发出请求到首个响应字节的耗时，
// total 是整个响应（含流式全部帧）完成耗时，bytes 是响应体大小。
type result struct {
	ttfb   time.Duration
	total  time.Duration
	bytes  int64
	errMsg string
}

func main() {
	url := flag.String("url", "http://localhost:3003/v1/chat/completions", "目标端点")
	key := flag.String("key", "", "auth.api_key（空 = 不携带凭据）")
	concurrency := flag.Int("c", 8, "并发 worker 数")
	total := flag.Int("n", 100, "总请求数；与 -duration 互斥，-duration 非零时忽略")
	duration := flag.Duration("duration", 0, "持续压测时长（如 30s）；非零时忽略 -n")
	model := flag.String("model", "stub", "请求体 model 字段")
	stream := flag.Bool("stream", true, "请求体 stream 字段；false 测非流式路径")
	bodyFile := flag.String("body", "", "自定义请求体文件；空用内置 chat 请求")
	flag.Parse()

	body := []byte(fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"load test"}]}`,
		*model, *stream))
	if *bodyFile != "" {
		data, err := os.ReadFile(*bodyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read body file:", err)
			os.Exit(1)
		}
		body = data
	}

	// 共享 client 复用连接池，贴近真实客户端形态；超时只兜底卡死，
	// 不设短超时——长流式响应的时长本身是被测量。
	client := &http.Client{Timeout: 10 * time.Minute}

	var issued atomic.Int64
	var deadline time.Time
	if *duration > 0 {
		deadline = time.Now().Add(*duration)
	}
	shouldIssue := func() bool {
		if *duration > 0 {
			return time.Now().Before(deadline)
		}
		return issued.Add(1) <= int64(*total)
	}

	perWorker := make([][]result, *concurrency)
	var wg sync.WaitGroup
	started := time.Now()
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results := make([]result, 0, 256)
			for shouldIssue() {
				results = append(results, doRequest(client, *url, *key, body))
			}
			perWorker[idx] = results
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(started)

	var results []result
	for _, rs := range perWorker {
		results = append(results, rs...)
	}
	report(results, elapsed)
}

// doRequest 发一次请求并测量 TTFB/总时长/字节数。
// TTFB 口径与排障体系一致：请求发出到首个响应体字节到达（SSE 即首帧）。
func doRequest(client *http.Client, url, key string, body []byte) result {
	started := time.Now()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result{total: time.Since(started), errMsg: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return result{total: time.Since(started), errMsg: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, 32*1024)
	n, readErr := resp.Body.Read(buf)
	ttfb := time.Since(started)
	if readErr != nil && readErr != io.EOF {
		return result{ttfb: ttfb, total: ttfb, errMsg: fmt.Sprintf("read first byte: %v", readErr)}
	}
	written, copyErr := io.CopyBuffer(io.Discard, resp.Body, buf)
	// 读流中途断掉不是成功完成：压测关心「上游/代理会不会半路掐流」，
	// 截断必须显式计为错误，否则结果里的成功率和字节数都虚高。
	if copyErr != nil {
		return result{ttfb: ttfb, total: time.Since(started), bytes: int64(n) + written,
			errMsg: fmt.Sprintf("stream truncated: %v", copyErr)}
	}
	if resp.StatusCode != http.StatusOK {
		return result{ttfb: ttfb, total: time.Since(started), bytes: int64(n) + written,
			errMsg: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	return result{ttfb: ttfb, total: time.Since(started), bytes: int64(n) + written}
}

// report 聚合全部请求输出：分位数 + 吞吐 + 错误摘要。
func report(results []result, elapsed time.Duration) {
	var ttfbs, totals []float64
	var bytesTotal int64
	errors := map[string]int{}
	for _, r := range results {
		bytesTotal += r.bytes
		if r.errMsg != "" {
			errors[r.errMsg]++
			continue
		}
		ttfbs = append(ttfbs, float64(r.ttfb.Microseconds())/1000)
		totals = append(totals, float64(r.total.Microseconds())/1000)
	}
	ok := len(ttfbs)
	fmt.Printf("requests=%d ok=%d errors=%d elapsed=%.1fs rps=%.1f body_mb=%.1f\n",
		len(results), ok, len(results)-ok, elapsed.Seconds(),
		float64(ok)/elapsed.Seconds(), float64(bytesTotal)/1e6)
	if ok > 0 {
		fmt.Printf("ttfb_ms   avg=%.1f p50=%.1f p90=%.1f p99=%.1f\n",
			avg(ttfbs), pct(ttfbs, 50), pct(ttfbs, 90), pct(ttfbs, 99))
		fmt.Printf("total_ms  avg=%.1f p50=%.1f p90=%.1f p99=%.1f\n",
			avg(totals), pct(totals, 50), pct(totals, 90), pct(totals, 99))
	}
	for msg, count := range errors {
		fmt.Printf("error x%d: %s\n", count, msg)
	}
}

func avg(values []float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// pct 返回最近秩分位数（ms）。
func pct(values []float64, p int) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(float64(p) / 100 * float64(len(sorted))))
	return sorted[max(rank-1, 0)]
}
