#!/usr/bin/env bash
# bench.sh — 跑全部微基准并存基线：自动找出含 Benchmark 的包，
# -count=10 -benchmem 输出落 outputs/bench/<label>.txt。
# 优化前后各跑一遍，用 benchstat 做统计显著性对比：
#   scripts/perf/bench.sh before   # 改前基线
#   scripts/perf/bench.sh after    # 改后
#   benchstat outputs/bench/before.txt outputs/bench/after.txt
# （benchstat: go install golang.org/x/perf/cmd/benchstat@latest）
set -euo pipefail
cd "$(dirname "$0")/../.."

LABEL="${1:-bench-$(git rev-parse --short HEAD 2>/dev/null || echo local)}"
COUNT="${2:-10}"

# macOS 自带 bash 3.2 无 mapfile：while-read 逐行收集。
# 目录名需加 ./ 前缀，裸路径会被 go 当成 stdlib 包解析。
PKGS=()
while IFS= read -r pkg; do
	PKGS+=("./$pkg")
done < <(grep -rl "func Benchmark" --include="*_test.go" internal/ | xargs -n1 dirname | sort -u)
[[ ${#PKGS[@]} -gt 0 ]] || { echo "没有找到基准包" >&2; exit 1; }

mkdir -p outputs/bench
OUT="outputs/bench/$LABEL.txt"
echo "== go test -bench=. -benchmem -count=$COUNT: ${PKGS[*]} =="
go test -run='^$' -bench=. -benchmem -count="$COUNT" -timeout 30m "${PKGS[@]}" | tee "$OUT"
echo "== 已存 ${OUT}（对比: benchstat <old>.txt ${OUT}）=="
