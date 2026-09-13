#!/usr/bin/env bash
# release.sh — 按 Conventional Commits 计算下一版本并打 tag 发布。
#
#   scripts/release.sh                    # dry-run：打印将要发布的版本与分类 changelog
#   scripts/release.sh --publish          # VERSION 回写 → 等 CI 绿 → 建 annotated tag 推送
#   scripts/release.sh --version vX.Y.Z   # 覆盖自动计算的版本（配合 --publish）
#
# 规则（机械化，不靠人守）：tag 只打在 origin/main 上 CI 已绿的提交；
# --publish 前要求本地 HEAD 已推送，脚本只推送自己的 VERSION bump 提交；
# 打 tag 前重新 fetch 确认 origin/main 没被别人推进。
# 0.x 阶段 feat/破坏性变更升 minor，其余升 patch；1.0 之后破坏性变更升 major。
set -euo pipefail
cd "$(dirname "$0")/.."

PUBLISH=0
OVERRIDE=""
CI_WAIT_SECONDS="${CI_WAIT_SECONDS:-1200}"
CI_POLL_INTERVAL="${CI_POLL_INTERVAL:-20}"
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
VERSION_FILE="cmd/devin-2api/VERSION"

if [[ -n "${LAST_TAG}" ]]; then
	RANGE="${LAST_TAG}..HEAD"
else
	RANGE="HEAD"
fi
# chore(release): 是脚本自己产生的 VERSION 簿记提交，不算内容。
SUBJECTS=()
while IFS= read -r line; do
	[[ "${line}" =~ ^chore\(release\): ]] && continue
	SUBJECTS+=("${line}")
done < <(git log --format='%s' "${RANGE}")
if [[ ${#SUBJECTS[@]} -eq 0 && -z "${OVERRIDE}" ]]; then
	echo "自 ${LAST_TAG:-仓库起点} 以来没有新提交，无可发布内容" >&2
	exit 1
fi

# --- 计算下一版本（0.x：feat/! → minor，其余 → patch；>=1.x：! → major） ---
BUMP=patch
for s in ${SUBJECTS[@]+"${SUBJECTS[@]}"}; do
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

# --- 分类 changelog → tag 注解 → release.yml 取注解作 release body ---
FEATS=(); FIXES=(); OTHERS=()
for s in ${SUBJECTS[@]+"${SUBJECTS[@]}"}; do
	if [[ "${s}" =~ ^feat(\(.+\))?!?: ]]; then
		FEATS+=("${s}")
	elif [[ "${s}" =~ ^(fix|perf)(\(.+\))?!?: ]]; then
		FIXES+=("${s}")
	else
		OTHERS+=("${s}")
	fi
done
NOTES_FILE="$(mktemp -t devin2api-release-notes.XXXXXX)"
trap 'rm -f "${NOTES_FILE}"' EXIT
{
	echo "devin-2api ${NEXT}"
	echo
	if [[ -n "${LAST_TAG}" ]]; then
		echo "**Full Changelog**: https://github.com/${REPO_SLUG}/compare/${LAST_TAG}...${NEXT}"
		echo
	fi
	emit_section() {
		local title="$1"; shift
		[[ $# -eq 0 ]] && return 0
		echo "## ${title}"
		echo
		printf -- '- %s\n' "$@"
		echo
	}
	emit_section "Features" ${FEATS[@]+"${FEATS[@]}"}
	emit_section "Fixes" ${FIXES[@]+"${FIXES[@]}"}
	emit_section "Other" ${OTHERS[@]+"${OTHERS[@]}"}
} > "${NOTES_FILE}"

# --- CI 门禁：查名为 CI 的 workflow 在指定 sha 上的结论；轮询到 completed ---
gh_token() {
	if [[ -n "${GH_TOKEN:-}" ]]; then
		echo "${GH_TOKEN}"
	elif command -v gh >/dev/null; then
		gh auth token 2>/dev/null || true
	else
		printf 'protocol=https\nhost=github.com\n' | git credential fill 2>/dev/null | awk -F= '/^password=/{print $2}'
	fi
}

# ci_state <sha>：输出 green / FAILED / pending / unknown
ci_state() {
	local token json
	token="$(gh_token)"
	json="$(curl -sf -m 10 ${token:+-H "Authorization: Bearer ${token}"} \
		"https://api.github.com/repos/${REPO_SLUG}/actions/runs?head_sha=$1&per_page=20" 2>/dev/null || true)"
	[[ -z "${json}" ]] && { echo unknown; return; }
	CI_JSON="${json}" python3 -c '
import json, os
d = json.loads(os.environ["CI_JSON"])
# 只看名为 CI 的 workflow——其它 workflow 的失败与发布质量无关
runs = [r for r in d.get("workflow_runs", []) if r["name"] == "CI"]
if not runs:
    print("pending")
elif runs[0]["status"] != "completed":
    print("pending")
elif runs[0]["conclusion"] == "success":
    print("green")
else:
    print("FAILED")
'
}

# --- 输出计划 ---
echo "repo:     ${REPO_SLUG}"
echo "last tag: ${LAST_TAG:-<none>}"
echo "next:     ${NEXT}  (bump: ${BUMP})"
echo "commits:  ${#SUBJECTS[@]}"
echo
cat "${NOTES_FILE}"

[[ "${PUBLISH}" == "1" ]] || {
	echo "dry-run。确认无误后执行: scripts/release.sh --publish"
	exit 0
}

# --- publish：VERSION 回写（tag 自指版本）→ 等 CI 绿 → 打 tag ---
if ! git diff --quiet || ! git diff --cached --quiet; then
	echo "拒绝发布：工作区有未提交改动，先自行整理" >&2
	exit 1
fi
git fetch origin --quiet
if [[ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]]; then
	echo "拒绝发布：本地 HEAD 未推送到 origin/main，先 git push origin main" >&2
	exit 1
fi
if [[ "$(cat "${VERSION_FILE}")" != "${NEXT}" ]]; then
	echo "${NEXT}" > "${VERSION_FILE}"
	git add "${VERSION_FILE}"
	git commit -m "chore(release): bump VERSION to ${NEXT}"
	git push origin main
	echo "==> VERSION 已回写并推送，等待该提交的 CI"
fi

HEAD_SHA="$(git rev-parse HEAD)"
echo "==> 等待 ${HEAD_SHA:0:7} 的 CI（最长 ${CI_WAIT_SECONDS}s）"
DEADLINE=$((SECONDS + CI_WAIT_SECONDS))
while true; do
	STATE="$(ci_state "${HEAD_SHA}")"
	case "${STATE}" in
		green) echo "==> CI green"; break ;;
		FAILED)
			echo "拒绝发布：CI 失败——https://github.com/${REPO_SLUG}/actions" >&2
			exit 1
			;;
	esac
	if [[ ${SECONDS} -ge ${DEADLINE} ]]; then
		echo "拒绝发布：等待 CI 超时（state=${STATE}）" >&2
		exit 1
	fi
	sleep "${CI_POLL_INTERVAL}"
done

# 打 tag 前复查：期间若有人推进 origin/main，本次 HEAD 已不是它，拒绝。
git fetch origin --quiet
if [[ "${HEAD_SHA}" != "$(git rev-parse origin/main)" ]]; then
	echo "拒绝发布：等 CI 期间 origin/main 被推进，重新跑一遍" >&2
	exit 1
fi

# --cleanup=verbatim：默认 strip 会把 "## Features" 这类行当注释吃掉。
git tag -a "${NEXT}" -F "${NOTES_FILE}" --cleanup=verbatim
git push origin "${NEXT}"
echo
echo "已推送 ${NEXT} → release.yml 开始发布："
echo "  https://github.com/${REPO_SLUG}/actions"
