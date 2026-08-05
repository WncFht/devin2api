FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine AS builder

ARG TARGETARCH

# proto → Go 绑定生成工具链（见 Taskfile.yml）
RUN apk add --no-cache protobuf \
    && go install github.com/go-task/task/v3/cmd/task@latest \
    && go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11 \
    && go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.19.1

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY Taskfile.yml ./
COPY outputs/devin-proto ./outputs/devin-proto
RUN task generate

COPY cmd ./cmd
COPY internal ./internal
COPY e2e ./e2e

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/devin-2api ./cmd/devin-2api

FROM alpine:3.22

RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /out/devin-2api /app/devin-2api

EXPOSE 8080

ENTRYPOINT ["/app/devin-2api"]