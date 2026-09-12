#!/usr/bin/env bash
# release.sh — 按 Conventional Commits 计算下一版本并打 tag 发布。
#
#   scripts/release.sh                    # dry-run：打印将要发布的版本与提交清单
#   scripts/release.sh --publish          # 实际创建 annotated tag 并推送（触发 release.yml）
#   scripts/release.sh --version vX.Y.Z   # 覆盖自动计算的版本（配合 --publish）
#
# 规则：tag 只打在已推送 origin/main 且 CI 已绿的提交上；0.x 阶段
# feat/破坏性变更升 minor，其余升 patch；1.0 之后破坏性变更升 major。
set -euo pipefail
cd "$(dirname "$0")/.."

PUBLISH=0
OVERRIDE=""
while [[ $# -gt 0 ]]; do
	case "$1" in
		--publish) PUBLISH=1 ;;
		--version)
			OVERRIDE="${2:?--version 需要参数}"
			shift
			;;
		*) echo "unknown arg: $1" >&2; exit 2 ;;
	esac
	shift
done

REPO_SLUG="$(git remote get-url origin | sed -E 's#.*github.com[:/]([^/]+/[^/.]+)(\.git)?$#\1#')"
LAST_TAG="$(git describe --tags --abbrev=0 2>/dev/null || true)"

if [[ -n "${LAST_TAG}" ]]; then
	RANGE="${LAST_TAG}..HEAD"
else
	RANGE="HEAD"
fi
SUBJECTS=()
while IFS= read -r line; do
	SUBJECTS+=("${line}")
done < <(git log --format='%s' "${RANGE}")
if [[ ${#SUBJECTS[@]} -eq 0 && -z "${OVERRIDE}" ]]; then
	echo "自 ${LAST_TAG:-仓库起点} 以来没有新提交，无可发布内容" >&2
	exit 1
fi

# --- 计算下一版本（0.x：feat/! → minor，其余 → patch；>=1.x：! → major） ---
BUMP=patch
for s in "${SUBJECTS[@]}"; do
	if [[ "${s}" =~ ^[a-z]+(\(.+\))?!: ]]; then
		BUMP=major
		break
	elif [[ "${s}" =~ ^feat(\(.+\))?: ]]; then
		BUMP=minor
	fi
done
if [[ "${BUMP}" != "major" ]] && git log --format='%b' "${RANGE}" | grep -q "BREAKING CHANGE"; then
	BUMP=major
fi

if [[ -z "${LAST_TAG}" ]]; then
	NEXT="v0.1.0"
else
	V="${LAST_TAG#v}"
	MAJOR="${V%%.*}"; REST="${V#*.}"; MINOR="${REST%%.*}"; PATCH="${REST#*.}"
	PATCH="${PATCH%%-*}"
	case "${BUMP}" in
		major)
			if [[ "${MAJOR}" == "0" ]]; then MINOR=$((MINOR + 1)); PATCH=0; else MAJOR=$((MAJOR + 1)); MINOR=0; PATCH=0; fi
			;;
		minor) MINOR=$((MINOR + 1)); PATCH=0 ;;
		patch) PATCH=$((PATCH + 1)) ;;
	esac
	NEXT="v${MAJOR}.${MINOR}.${PATCH}"
fi
[[ -n "${OVERRIDE}" ]] && NEXT="${OVERRIDE}"

if git rev-parse --verify --quiet "refs/tags/${NEXT}" >/dev/null; then
	echo "tag ${NEXT} 已存在" >&2
	exit 1
fi

# --- 门禁：HEAD 必须已推送、CI 必须已绿 ---
git fetch origin --quiet
HEAD_SHA="$(git rev-parse HEAD)"
REMOTE_SHA="$(git rev-parse origin/main)"
CI_STATE="unknown"
if [[ "${HEAD_SHA}" != "${REMOTE_SHA}" ]]; then
	CI_STATE="blocked: HEAD (${HEAD_SHA:0:7}) 未推送到 origin/main (${REMOTE_SHA:0:7})"
else
	TOKEN="${GH_TOKEN:-}"
	if [[ -z "${TOKEN}" ]]; then
		TOKEN="$(printf 'protocol=https\nhost=github.com\n' | git credential fill 2>/dev/null | awk -F= '/^password=/{print $2}')"
	fi
	CI_JSON="$(curl -sf -m 10 ${TOKEN:+-H "Authorization: Bearer ${TOKEN}"} \
		"https://api.github.com/repos/${REPO_SLUG}/commits/${HEAD_SHA}/check-runs" 2>/dev/null || true)"
	if [[ -n "${CI_JSON}" ]]; then
		CI_STATE="$(CI_JSON="${CI_JSON}" python3 -c '
import json, os
d = json.loads(os.environ["CI_JSON"])
runs = [r for r in d.get("check_runs", []) if r["name"] == "CI" or r.get("app", {}).get("slug") == "github-actions"]
if not runs:
    print("unknown (no check runs)")
elif all(r["status"] == "completed" and r["conclusion"] == "success" for r in runs):
    print("green")
elif any(r["conclusion"] in ("failure", "cancelled") for r in runs):
    print("FAILED")
else:
    print("pending")
' 2>/dev/null || echo unknown)"
	fi
fi

# --- 输出计划 ---
echo "repo:     ${REPO_SLUG}"
echo "last tag: ${LAST_TAG:-<none>}"
echo "next:     ${NEXT}  (bump: ${BUMP})"
echo "commits:  ${#SUBJECTS[@]}"
echo "CI:       ${CI_STATE}"
echo
echo "---- changelog ----"
for s in "${SUBJECTS[@]}"; do echo "  ${s}"; done
echo "-------------------"

[[ "${PUBLISH}" == "1" ]] || {
	echo
	echo "dry-run。确认无误后执行: scripts/release.sh --publish"
	exit 0
}

if [[ "${HEAD_SHA}" != "${REMOTE_SHA}" ]]; then
	echo "拒绝发布：本地 HEAD 未推送。先 git push origin main" >&2
	exit 1
fi
if [[ "${CI_STATE}" == "FAILED" ]]; then
	echo "拒绝发布：HEAD 的 CI 失败" >&2
	exit 1
fi
if [[ "${CI_STATE}" != "green" ]]; then
	echo "警告：CI 状态为 ${CI_STATE}，仍继续发布"
fi

TAG_MSG="devin-2api ${NEXT}"$'\n\n'"$(printf -- '- %s\n' "${SUBJECTS[@]}")"
git tag -a "${NEXT}" -m "${TAG_MSG}"
git push origin "${NEXT}"
echo
echo "已推送 ${NEXT} → release.yml 开始发布："
echo "  https://github.com/${REPO_SLUG}/actions"
