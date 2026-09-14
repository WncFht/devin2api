# FROM 的 Go 版本必须 ≥ go.mod 的 go 指令：官方镜像 GOTOOLCHAIN=local
# 不会自动换 toolchain，低于指令时 go mod download 直接失败。
# deploy-assets.test.sh 有对应断言，改 go.mod 时记得同步这里。
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS builder

ARG TARGETARCH
# VERSION 由 release 流水线注入（git tag），本地构建缺省 dev
ARG VERSION=dev

WORKDIR /src

# 生成绑定已随仓库提交（outputs/devin-proto-go 是 go.mod replace 目标），
# 先拷它与 go.mod/go.sum 以利用 go mod download 的层缓存。
COPY go.mod go.sum ./
COPY outputs/devin-proto-go ./outputs/devin-proto-go
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/devin-2api ./cmd/devin-2api

FROM alpine:3.22

RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /out/devin-2api /app/devin-2api

EXPOSE 8080

ENTRYPOINT ["/app/devin-2api"]
