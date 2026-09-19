#!/usr/bin/env bash
# codegen.sh — 生成物漂移检查：把 Taskfile 的 generate 任务在临时
# 目录重跑一遍，与提交的 outputs/devin-proto-go/ 逐字节比对。
# 改了 outputs/devin-proto/ 的 proto 源但忘了重新生成时在此拦下。
#
# 依赖：protoc（版本必须与生成时一致——codegen 头部注释带 protoc 版本，
# 不一致会产生虚假漂移）、protoc-gen-go、protoc-gen-connect-go。
# 版本钉法见 Taskfile 注释与 .github/workflows/ci.yml 的 codegen-drift job。
#
# 用法: scripts/check/codegen.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

PROTO_DIR=outputs/devin-proto
GEN_DIR=outputs/devin-proto-go
GO_PKG=local/devinproto

command -v protoc >/dev/null || { echo "需要 protoc（生成时用 36.1）" >&2; exit 1; }
command -v protoc-gen-go >/dev/null || { echo "需要 protoc-gen-go（go install ...@v1.36.11）" >&2; exit 1; }
command -v protoc-gen-connect-go >/dev/null || { echo "需要 protoc-gen-connect-go（go install ...@v1.20.0）" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/gen"

protoc -I "$PROTO_DIR" \
	--go_out="$WORK/gen" --go_opt=module="$GO_PKG" --go_opt=Mall-protos.proto="$GO_PKG" \
	--connect-go_out="$WORK/gen" --connect-go_opt=module="$GO_PKG" --connect-go_opt=Mall-protos.proto="$GO_PKG" \
	"$PROTO_DIR/all-protos.proto"

cd "$WORK/gen"
go mod init "$GO_PKG" >/dev/null
go mod edit -go="$(sed -n 's/^go //p' "$OLDPWD/go.mod")" \
	-require=connectrpc.com/connect@v1.20.0 -require=google.golang.org/protobuf@v1.36.11
go mod tidy >/dev/null
cd "$OLDPWD"

if diff -r "$WORK/gen" "$GEN_DIR"; then
	echo "codegen 与 proto 源同步"
else
	echo "codegen 漂移：proto 源与 outputs/devin-proto-go 不同步，跑 task generate 重新生成并提交" >&2
	exit 1
fi
