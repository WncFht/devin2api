# 项目结构与 module 管理

## 标准项目布局

```
myproject/
├── cmd/                    # 主应用
│   ├── server/
│   │   └── main.go        # server 入口
│   └── cli/
│       └── main.go        # CLI 工具入口
├── internal/              # 私有应用代码
│   ├── api/              # API handler
│   ├── service/          # 业务逻辑
│   └── repository/       # 数据访问层
├── pkg/                   # 公开库代码
│   └── models/           # 共享 model
├── api/                   # API 定义
│   ├── openapi.yaml      # OpenAPI 规格
│   └── proto/            # Protocol Buffers
├── web/                   # Web 资源
│   ├── static/
│   └── templates/
├── scripts/               # 构建与安装脚本
├── configs/              # 配置文件
├── deployments/          # Docker、K8s 配置
├── test/                 # 额外测试数据
├── docs/                 # 文档
├── go.mod               # module 定义
├── go.sum               # 依赖校验和
├── Makefile             # 构建自动化
└── README.md
```

## go.mod 基础

```go
// 初始化 module
// go mod init github.com/user/project

module github.com/user/myproject

go 1.21

require (
    github.com/gin-gonic/gin v1.9.1
    github.com/lib/pq v1.10.9
    go.uber.org/zap v1.26.0
)

require (
    // 间接依赖（自动管理）
    github.com/bytedance/sonic v1.9.1 // indirect
    github.com/chenzhuoyu/base64x v0.0.0-20221115062448-fe3a3abad311 // indirect
)

// 本地开发用 replace 指令
replace github.com/user/mylib => ../mylib

// 用 retract 指令标记问题版本
retract v1.0.1 // 含严重 bug
```

## module 命令

```bash
# 初始化 module
go mod init github.com/user/project

# 补全缺失依赖
go mod tidy

# 下载依赖
go mod download

# 校验依赖
go mod verify

# 显示 module 依赖图
go mod graph

# 显示为何需要该包
go mod why github.com/user/package

# vendor 依赖（复制到 vendor/）
go mod vendor

# 更新依赖
go get -u github.com/user/package

# 更新到指定版本
go get github.com/user/package@v1.2.3

# 更新全部依赖
go get -u ./...

# 移除未使用的依赖
go mod tidy
```

## internal 包

```go
// internal/ 包只能被父目录树内的代码 import

myproject/
├── internal/
│   ├── auth/           # 只能被 myproject import
│   │   └── jwt.go
│   └── database/
│       └── postgres.go
└── pkg/
    └── models/         # 可被任何人 import
        └── user.go

// 这样可以（同一项目）：
import "github.com/user/myproject/internal/auth"

// 这样不行（不同项目）：
import "github.com/other/project/internal/auth" // 报错！

// internal 子目录
myproject/
└── api/
    └── internal/       # 只能被 api/ 内代码 import
        └── helpers.go
```

## 包组织

```go
// user/user.go——领域包
package user

import (
    "context"
    "time"
)

// User 表示用户实体
type User struct {
    ID        string
    Email     string
    CreatedAt time.Time
}

// Repository 定义数据访问 interface
type Repository interface {
    Create(ctx context.Context, user *User) error
    GetByID(ctx context.Context, id string) (*User, error)
    Update(ctx context.Context, user *User) error
    Delete(ctx context.Context, id string) error
}

// Service 处理业务逻辑
type Service struct {
    repo Repository
}

// NewService 创建新的 user service
func NewService(repo Repository) *Service {
    return &Service{repo: repo}
}

func (s *Service) RegisterUser(ctx context.Context, email string) (*User, error) {
    user := &User{
        ID:        generateID(),
        Email:     email,
        CreatedAt: time.Now(),
    }
    return user, s.repo.Create(ctx, user)
}
```

## 多 module 仓库（monorepo）

```
monorepo/
├── go.work              # workspace 文件
├── services/
│   ├── api/
│   │   ├── go.mod
│   │   └── main.go
│   └── worker/
│       ├── go.mod
│       └── main.go
└── shared/
    └── models/
        ├── go.mod
        └── user.go

// go.work
go 1.21

use (
    ./services/api
    ./services/worker
    ./shared/models
)

// 命令：
// go work init ./services/api ./services/worker
// go work use ./shared/models
// go work sync
```

## build tag 与构建约束

```go
// +build integration
// integration_test.go

package myapp

import "testing"

func TestIntegration(t *testing.T) {
    // 集成测试代码
}

// 构建：go test -tags=integration

// 文件级构建约束（Go 1.17+）
//go:build linux && amd64

package myapp

// 多个约束
//go:build linux || darwin
//go:build amd64

// 取反
//go:build !windows

// 常用 tag：
// linux, darwin, windows, freebsd
// amd64, arm64, 386, arm
// cgo, !cgo
```

## Makefile 示例

```makefile
# Makefile
.PHONY: build test lint clean run

# 变量
BINARY_NAME=myapp
BUILD_DIR=bin
GO=go
GOFLAGS=-v

# 构建应用
build:
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/server

# 跑测试
test:
	$(GO) test -v -race -coverprofile=coverage.out ./...

# 跑测试并生成覆盖率报告
test-coverage: test
	$(GO) tool cover -html=coverage.out

# 跑 linter
lint:
	golangci-lint run ./...

# 格式化代码
fmt:
	$(GO) fmt ./...
	goimports -w .

# 运行应用
run:
	$(GO) run ./cmd/server

# 清理构建产物
clean:
	rm -rf $(BUILD_DIR)
	rm -f coverage.out

# 安装依赖
deps:
	$(GO) mod download
	$(GO) mod tidy

# 多平台构建
build-all:
	GOOS=linux GOARCH=amd64 $(GO) build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/server
	GOOS=darwin GOARCH=amd64 $(GO) build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/server
	GOOS=windows GOARCH=amd64 $(GO) build -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe ./cmd/server

# 带 race detector 运行
run-race:
	$(GO) run -race ./cmd/server

# 生成代码
generate:
	$(GO) generate ./...

# Docker 构建
docker-build:
	docker build -t $(BINARY_NAME):latest .

# 帮助
help:
	@echo "Available targets:"
	@echo "  build         - Build the application"
	@echo "  test          - Run tests"
	@echo "  test-coverage - Run tests with coverage report"
	@echo "  lint          - Run linters"
	@echo "  fmt           - Format code"
	@echo "  run           - Run the application"
	@echo "  clean         - Clean build artifacts"
	@echo "  deps          - Install dependencies"
```

## Dockerfile 多阶段构建

```dockerfile
# 构建阶段
FROM golang:1.21-alpine AS builder

WORKDIR /app

# 复制 go mod 文件
COPY go.mod go.sum ./
RUN go mod download

# 复制源码
COPY . .

# 构建二进制
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o server ./cmd/server

# 最终阶段
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

# 从 builder 复制二进制
COPY --from=builder /app/server .

# 需要时复制配置文件
COPY --from=builder /app/configs ./configs

EXPOSE 8080

CMD ["./server"]
```

## 版本信息

```go
// version/version.go
package version

import "runtime"

var (
    // 构建时经 ldflags 注入
    Version   = "dev"
    GitCommit = "none"
    BuildTime = "unknown"
)

// Info 返回版本信息
func Info() map[string]string {
    return map[string]string{
        "version":    Version,
        "git_commit": GitCommit,
        "build_time": BuildTime,
        "go_version": runtime.Version(),
        "os":         runtime.GOOS,
        "arch":       runtime.GOARCH,
    }
}

// 带版本信息构建：
// go build -ldflags "-X github.com/user/project/version.Version=1.0.0 \
//   -X github.com/user/project/version.GitCommit=$(git rev-parse HEAD) \
//   -X github.com/user/project/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

## go generate

```go
// models/user.go
//go:generate mockgen -source=user.go -destination=../mocks/user_mock.go -package=mocks

package models

type UserRepository interface {
    GetUser(id string) (*User, error)
    SaveUser(user *User) error
}

// tools.go——跟踪工具依赖
//go:build tools

package tools

import (
    _ "github.com/golang/mock/mockgen"
    _ "golang.org/x/tools/cmd/stringer"
)

// 安装工具：
// go install github.com/golang/mock/mockgen@latest

// 运行生成：
// go generate ./...
```

## 配置管理

```go
// config/config.go
package config

import (
    "os"
    "time"

    "github.com/kelseyhightower/envconfig"
)

type Config struct {
    Server   ServerConfig
    Database DatabaseConfig
    Redis    RedisConfig
}

type ServerConfig struct {
    Host         string        `envconfig:"SERVER_HOST" default:"0.0.0.0"`
    Port         int           `envconfig:"SERVER_PORT" default:"8080"`
    ReadTimeout  time.Duration `envconfig:"SERVER_READ_TIMEOUT" default:"10s"`
    WriteTimeout time.Duration `envconfig:"SERVER_WRITE_TIMEOUT" default:"10s"`
}

type DatabaseConfig struct {
    URL          string `envconfig:"DATABASE_URL" required:"true"`
    MaxOpenConns int    `envconfig:"DB_MAX_OPEN_CONNS" default:"25"`
    MaxIdleConns int    `envconfig:"DB_MAX_IDLE_CONNS" default:"5"`
}

type RedisConfig struct {
    Addr     string `envconfig:"REDIS_ADDR" default:"localhost:6379"`
    Password string `envconfig:"REDIS_PASSWORD"`
    DB       int    `envconfig:"REDIS_DB" default:"0"`
}

// Load 从环境变量加载配置
func Load() (*Config, error) {
    var cfg Config
    if err := envconfig.Process("", &cfg); err != nil {
        return nil, err
    }
    return &cfg, nil
}
```

## 速查表

| 命令                         | 说明             |
| ---------------------------- | ---------------- |
| `go mod init`                | 初始化 module    |
| `go mod tidy`                | 增删依赖         |
| `go mod download`            | 下载依赖         |
| `go get package@version`     | 添加/更新依赖    |
| `go build -ldflags "-X ..."` | 注入版本信息     |
| `go generate ./...`          | 跑代码生成       |
| `GOOS=linux go build`        | 交叉编译         |
| `go work init`               | 初始化 workspace |
