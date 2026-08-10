// 真并发压测：goroutine + WaitGroup，所有请求同一时刻发出
// 测量每个请求的首字到达时间和总时间
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

func main() {
	n := 10
	url := "http://localhost:3543/v1/chat/completions"
	body := []byte(`{"model":"glm-5-2","stream":true,"max_tokens":30,"messages":[{"role":"user","content":"hi"}]}`)

	// 用一个共享 client（连接池），更接近真实客户端
	client := &http.Client{Timeout: 60 * time.Second}

	type result struct {
		Idx       int
		FirstByte int64 // ms
		Total     int64 // ms
		Err       string
	}

	// barrier：所有 goroutine 同时开始
	var ready sync.WaitGroup
	ready.Add(n)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	done.Add(n)
	results := make([]result, n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			defer done.Done()
			ready.Done()
			start.Wait() // 等主 goroutine 放行，所有请求同一时刻发出

			sw := time.Now()
			req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				results[idx] = result{Idx: idx, Total: time.Since(sw).Milliseconds(), Err: err.Error()}
				return
			}
			defer resp.Body.Close()

			// 读第一个字节
			buf := make([]byte, 4096)
			r, err := resp.Body.Read(buf)
			if err != nil && err != io.EOF {
				results[idx] = result{Idx: idx, Total: time.Since(sw).Milliseconds(), Err: err.Error()}
				return
			}
			firstByte := time.Since(sw).Milliseconds()
			if r == 0 {
				firstByte = -1
			}
			// 读完全部
			for {
				_, err := resp.Body.Read(buf)
				if err != nil {
					break
				}
			}
			total := time.Since(sw).Milliseconds()
			results[idx] = result{Idx: idx, FirstByte: firstByte, Total: total}
		}(i)
	}

	// 等所有 goroutine 就绪，然后同时放行
	ready.Wait()
	t0 := time.Now()
	start.Done()
	done.Wait()

	// 输出结果
	fmt.Println("\nIdx | FirstByte(ms) | Total(ms) | Error")
	fmt.Println("----|---------------|-----------|------")
	var ok int
	var minFB, maxFB, sumFB int64
	for _, r := range results {
		errStr := ""
		if r.Err != "" {
			errStr = r.Err
		} else {
			ok++
			if minFB == 0 || r.FirstByte < minFB {
				minFB = r.FirstByte
			}
			if r.FirstByte > maxFB {
				maxFB = r.FirstByte
			}
			sumFB += r.FirstByte
		}
		fmt.Printf("%3d | %13d | %9d | %s\n", r.Idx, r.FirstByte, r.Total, errStr)
	}

	wall := time.Since(t0).Milliseconds()
	fmt.Printf("\nOK: %d/%d\n", ok, n)
	if ok > 0 {
		fmt.Printf("FirstByte ms - min: %d, avg: %d, max: %d, spread: %d\n",
			minFB, sumFB/int64(ok), maxFB, maxFB-minFB)
	}
	fmt.Printf("Wall time (all done): %d ms\n", wall)
}
