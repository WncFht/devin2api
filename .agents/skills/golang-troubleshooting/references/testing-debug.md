# 测试驱动调试

修 bug 前必须先写一个失败测试。写失败测试往往是最快的调试路径——它给你一个可复现、被隔离的环境。

## 在测试里复现 bug

```go
func TestBugDescription(t *testing.T) {
    // 准备：触发 bug 的精确条件
    svc := NewService(testConfig)

    // 执行：失败的那个操作
    result, err := svc.Process(badInput)

    // 断言：应该发生什么
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if result.Status != "ok" {
        t.Errorf("got status %q, want %q", result.Status, "ok")
    }
}
```

## 用表测试扩展边界用例

调试时加边界用例，找出 bug 的作用边界：

```go
tests := []struct {
    name    string
    input   string
    want    time.Duration
    wantErr bool
}{
    {"valid", "5s", 5 * time.Second, false},
    {"empty", "", 0, true},
    {"negative", "-1s", -time.Second, false},
    {"zero", "0s", 0, false},
    {"overflow", "99999999h", 0, true},
    {"whitespace", " 5s ", 0, true},
}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        got, err := ParseDuration(tt.input)
        if (err != nil) != tt.wantErr {
            t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
        }
        if got != tt.want {
            t.Errorf("got %v, want %v", got, tt.want)
        }
    })
}
```

## 有用的测试 flag

```bash
go test -v ./...                          # 详细输出
go test -run TestName -v ./pkg/...        # 单个测试
go test -count=1 ./...                    # 禁用缓存
go test -timeout 10s ./...                # 短超时（找挂死）
go test -parallel 1 ./...                 # 顺序执行
go test -race ./...                       # 竞态检测器
go test -cover ./...                      # 覆盖率汇总
go test -coverprofile=c.out ./... && go tool cover -html=c.out  # 覆盖率报告
go test -failfast ./...                   # 首个失败即停
go test -shuffle=on ./...                # 随机化测试顺序（Go 1.17+）
```

## 调试不稳定测试

不稳定测试（时过时败）通常是这些原因之一：

1. **测试间共享可变状态**——全局变量、包级 map、单例
    - 修：在 `TestMain` 里重置状态，或用 `t.Cleanup`
2. **测试顺序依赖**——某个测试准备的状态被另一个测试依赖
    - 诊断：`go test -run TestSuspect -count=1`（单独跑）
    - 诊断：`go test -shuffle=on`（随机顺序）
    - 修：每个测试自己准备前置条件
3. **时序敏感**——测试里的 `time.Sleep`、goroutine 间的竞态
    - 修：用 channel/waitgroup 同步，别用 sleep
4. **端口冲突**——测试绑固定端口
    - 修：绑端口 `0`，读实际分配到的端口
5. **文件系统污染**——测试写共享临时目录
    - 修：用 `t.TempDir()` 拿每个测试独立的目录

```bash
# 多跑确认确实不稳定
go test -count=100 -run TestSuspect ./pkg/... -failfast

# 查并行问题
go test -parallel 1 -count=10 ./pkg/...

# 查顺序依赖
go test -shuffle=on ./pkg/...
```
