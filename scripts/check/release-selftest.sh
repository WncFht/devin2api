#!/usr/bin/env bash
# release-selftest.sh — release.sh 的离线演练（ccLoad 模式）：
# 每个场景建一个临时仓库 + 本地 bare origin，url.insteadOf 把假 github URL
# 重写到 bare，使 git fetch/push 全程走本地；PATH 里的 stub curl 按
# FIXTURE_MODE 返回 canned workflow_runs JSON，不外网。覆盖 dry-run 版本
# 计算、publish 全流程、全部拒绝分支，以及 tag 注解中 "## 节标题" 的保真
# 回归（git tag -a -F 默认 cleanup=strip 会吃掉它们）。
set -euo pipefail
REAL_REPO="$(cd "$(dirname "$0")/../.." && pwd)"

FAILED=0
pass() { echo "ok    $1"; }
fail() { echo "FAIL  $1"; FAILED=1; }

# 模板必须自带 XXXXXX：BSD mktemp 会自动补，GNU 没有就报错。
WORK="$(mktemp -d -t devin2api-release-selftest.XXXXXX)"
trap 'rm -rf "${WORK}"' EXIT

# check <描述> <命令串>：bash -c 执行须成功，失败时回显输出。
check() {
	local desc="$1" cmd="$2"
	if bash -c "${cmd}" >"${WORK}/check.out" 2>&1; then
		pass "${desc}"
	else
		fail "${desc}"
		sed 's/^/    /' "${WORK}/check.out"
	fi
}
# check_fail <描述> <命令串>：须非零退出。
check_fail() {
	local desc="$1" cmd="$2"
	if bash -c "${cmd}" >"${WORK}/check.out" 2>&1; then
		fail "${desc}（命令意外成功）"
	else
		pass "${desc}"
	fi
}
# expect_ok / expect_fail：跑真实命令（如 run_release），输出存 last.out。
expect_ok() {
	local desc="$1"; shift
	if "$@" >"${WORK}/last.out" 2>&1; then
		pass "${desc}"
	else
		fail "${desc}"
		sed 's/^/    /' "${WORK}/last.out"
	fi
}
expect_fail() {
	local desc="$1"; shift
	if "$@" >"${WORK}/last.out" 2>&1; then
		fail "${desc}（意外成功）"
	else
		pass "${desc}"
	fi
}

# ---------- stub 工具 ----------
# curl：扫描参数里的 api.github.com URL，按 FIXTURE_MODE 回 JSON；其余请求
# 一律失败（测试不允许真实外网）。CALLS_FILE 记调用次数验证轮询。
STUB="${WORK}/bin"
mkdir -p "${STUB}"
cat > "${STUB}/curl" <<'EOF'
#!/usr/bin/env bash
for arg in "$@"; do
	case "${arg}" in
		*api.github.com*)
			n=0
			[[ -f "${CALLS_FILE:-/dev/null}" ]] && n="$(cat "${CALLS_FILE}")"
			echo $((n + 1)) > "${CALLS_FILE}"
			[[ -n "${SABOTAGE_HOOK:-}" ]] && bash "${SABOTAGE_HOOK}"
			case "${FIXTURE_MODE:-green}" in
				failed)
					echo '{"workflow_runs":[{"name":"CI","status":"completed","conclusion":"failure"}]}' ;;
				pending_then_green)
					if [[ "${n}" -eq 0 ]]; then
						echo '{"workflow_runs":[{"name":"CI","status":"in_progress","conclusion":null}]}'
					else
						echo '{"workflow_runs":[{"name":"CI","status":"completed","conclusion":"success"}]}'
					fi ;;
				*)
					echo '{"workflow_runs":[{"name":"CI","status":"completed","conclusion":"success"}]}' ;;
			esac
			exit 0 ;;
	esac
done
echo "stub curl: 拒绝非 GitHub API 请求: $*" >&2
exit 1
EOF
# gh：只认 auth token；真实 token 也进不了 stub curl，双保险。
cat > "${STUB}/gh" <<'EOF'
#!/usr/bin/env bash
[[ "${1:-} ${2:-}" == "auth token" ]] && { echo fake-token; exit 0; }
exit 1
EOF
chmod +x "${STUB}/curl" "${STUB}/gh"

# ---------- 场景基建 ----------
SEQ=0
# setup_repo <名>：建 <名>/（工作仓库）与 <名>-origin.git（bare origin）。
# remote URL 是假 github 地址，insteadOf 重写到本地 bare——这样 release.sh
# 解析出的 REPO_SLUG=test/repo 合法，而 fetch/push 实际走文件协议。
setup_repo() {
	local dir="${WORK}/$1"
	git init -q --bare -b main "${dir}-origin.git"
	git init -q -b main "${dir}"
	git -C "${dir}" config user.email selftest@dev
	git -C "${dir}" config user.name selftest
	git -C "${dir}" remote add origin https://github.com/test/repo.git
	git -C "${dir}" config url."file://${dir}-origin.git".insteadOf "https://github.com/test/repo.git"
	mkdir -p "${dir}/scripts/release" "${dir}/cmd/devin-2api"
	cp "${REAL_REPO}/scripts/release/release.sh" "${dir}/scripts/release/"
	echo v0.5.0 > "${dir}/cmd/devin-2api/VERSION"
	echo readme > "${dir}/README.md"
	git -C "${dir}" add -A
	git -C "${dir}" commit -qm "chore: init"
	git -C "${dir}" tag -a v0.5.0 -m "v0.5.0"
	git -C "${dir}" push -qu origin main v0.5.0
}
commit_to() {
	local dir="${WORK}/$1" subject="$2"
	SEQ=$((SEQ + 1))
	echo "${SEQ}" > "${dir}/f${SEQ}.txt"
	git -C "${dir}" add -A
	git -C "${dir}" commit -qm "${subject}"
}
push_main() { git -C "${WORK}/$1" push -q origin main; }
# run_release <名> [release.sh 参数...]：stub 环境跑被测脚本。
run_release() {
	local dir="${WORK}/$1"; shift
	(
		cd "${dir}"
		export PATH="${STUB}:${PATH}"
		export GH_TOKEN=fake-token
		export CI_WAIT_SECONDS=30 CI_POLL_INTERVAL=0
		export FIXTURE_MODE="${FIXTURE_MODE:-green}"
		export CALLS_FILE="${CALLS_FILE:-${WORK}/calls-default}"
		export SABOTAGE_HOOK="${SABOTAGE_HOOK:-}"
		bash scripts/release/release.sh "$@"
	)
}

echo "== dry-run：版本计算 =="
setup_repo d1
commit_to d1 "feat(core): add thing"
expect_ok "dry-run feat 退出" run_release d1
check "feat → minor v0.6.0" "grep -qE 'next: +v0\.6\.0' '${WORK}/last.out'"
check "changelog 有 Features 节" "grep -qF '## Features' '${WORK}/last.out'"
check_fail "dry-run 不打 tag" "git -C '${WORK}/d1' rev-parse --verify --quiet refs/tags/v0.6.0"

setup_repo d2
commit_to d2 "fix(adapter): edge case"
expect_ok "dry-run fix 退出" run_release d2
check "fix → patch v0.5.1" "grep -qE 'next: +v0\.5\.1' '${WORK}/last.out'"

setup_repo d3
commit_to d3 "feat!: drop old api"
expect_ok "dry-run feat! 退出" run_release d3
check "0.x 破坏性 → minor v0.6.0" "grep -qE 'next: +v0\.6\.0' '${WORK}/last.out'"

setup_repo d4
commit_to d4 "chore(release): bump VERSION to v0.5.0"
commit_to d4 "fix: real fix"
expect_ok "dry-run chore 过滤" run_release d4
check "chore(release) 不计入 commits" "grep -qE 'commits: +1' '${WORK}/last.out'"
check_fail "chore(release) 不进 changelog" "grep -qF 'chore(release): bump' '${WORK}/last.out'"

setup_repo d5
expect_fail "无新提交 → 拒绝" run_release d5
check "报无可发布内容" "grep -qF '没有新提交' '${WORK}/last.out'"

setup_repo d6
commit_to d6 "fix: something"
expect_ok "dry-run --version 覆盖" run_release d6 --version v9.9.9
check "覆盖版本生效" "grep -qE 'next: +v9\.9\.9' '${WORK}/last.out'"

setup_repo d7
# v0.6.0 必须打在 HEAD 祖先之外的提交上：若打在 HEAD 上，git describe
# 直接返回它，RANGE 为空会先撞"没有新提交"而不是 tag 冲突检查。
git -C "${WORK}/d7" checkout -qb side v0.5.0
echo side > "${WORK}/d7/side.txt"
git -C "${WORK}/d7" add -A
git -C "${WORK}/d7" commit -qm "feat: side branch"
git -C "${WORK}/d7" tag -a v0.6.0 -m pre-existing
git -C "${WORK}/d7" checkout -q main
commit_to d7 "feat: x"
expect_fail "tag 已存在 → 拒绝" run_release d7
check "报 tag 已存在" "grep -qF '已存在' '${WORK}/last.out'"

echo "== publish：green 全流程 =="
setup_repo p1
commit_to p1 "feat(api): new endpoint"
push_main p1
expect_ok "publish green 退出" run_release p1 --publish
check "tag 已到 origin" "git -C '${WORK}/p1-origin.git' rev-parse --verify --quiet refs/tags/v0.6.0"
check "VERSION 已回写 origin/main" "[[ \"\$(git -C '${WORK}/p1-origin.git' show main:cmd/devin-2api/VERSION)\" == v0.6.0 ]]"
check "VERSION bump 提交在 origin/main" "git -C '${WORK}/p1-origin.git' log -1 --format=%s main | grep -qF 'chore(release): bump VERSION to v0.6.0'"
check "tag 注解保留 ## Features（verbatim 回归）" "git -C '${WORK}/p1-origin.git' tag -l v0.6.0 --format='%(contents)' | grep -qF '## Features'"
check "注解含 compare 链接" "git -C '${WORK}/p1-origin.git' tag -l v0.6.0 --format='%(contents)' | grep -qF 'compare/v0.5.0...v0.6.0'"

echo "== publish：pending → green 轮询 =="
setup_repo p2
commit_to p2 "fix: poll me"
push_main p2
if FIXTURE_MODE=pending_then_green CALLS_FILE="${WORK}/calls-p2" \
	run_release p2 --publish >"${WORK}/last.out" 2>&1; then
	pass "publish 轮询后成功"
else
	fail "publish 轮询后成功"
	sed 's/^/    /' "${WORK}/last.out"
fi
check "轮询至少调用了 2 次 API" "[[ \$(cat '${WORK}/calls-p2') -ge 2 ]]"
check "tag 已到 origin" "git -C '${WORK}/p2-origin.git' rev-parse --verify --quiet refs/tags/v0.5.1"

echo "== publish：CI 失败拒绝 =="
setup_repo p3
commit_to p3 "fix: will fail ci"
push_main p3
if FIXTURE_MODE=failed run_release p3 --publish >"${WORK}/last.out" 2>&1; then
	fail "CI failed → 拒绝（意外成功）"
else
	pass "CI failed → 拒绝"
fi
check "报 CI 失败" "grep -qF 'CI 失败' '${WORK}/last.out'"
check_fail "不留 tag" "git -C '${WORK}/p3-origin.git' rev-parse --verify --quiet refs/tags/v0.5.1"
# 已知行为：VERSION bump 簿记提交先于 CI 等待推送；失败后留在 main 上无害，
# 下次发布会识别 VERSION==NEXT 跳过重复提交。
check "VERSION bump 提交已推送（已知行为）" "git -C '${WORK}/p3-origin.git' log -1 --format=%s main | grep -qF 'chore(release)'"

echo "== publish：HEAD 未推送拒绝 =="
setup_repo p4
commit_to p4 "feat: local only"
expect_fail "未推送 → 拒绝" run_release p4 --publish
check "报未推送" "grep -qF '未推送' '${WORK}/last.out'"

echo "== publish：等 CI 期间 origin/main 被推进（TOCTOU）=="
setup_repo p5
commit_to p5 "feat: will race"
push_main p5
# stub curl 响应前先把一个提交推上 origin/main：release.sh 打 tag 前复查
# HEAD==origin/main 应当失败。
cat > "${WORK}/sabotage-p5.sh" <<EOF
#!/usr/bin/env bash
[[ -f "${WORK}/sabotage-p5.done" ]] && exit 0
touch "${WORK}/sabotage-p5.done"
t="\$(mktemp -d -t sabotage.XXXXXX)"
git clone -q "file://${WORK}/p5-origin.git" "\${t}/r"
git -C "\${t}/r" config user.email s@d
git -C "\${t}/r" config user.name s
echo race > "\${t}/r/race.txt"
git -C "\${t}/r" add -A
git -C "\${t}/r" commit -qm "feat: raced commit"
git -C "\${t}/r" push -q origin HEAD:main
EOF
chmod +x "${WORK}/sabotage-p5.sh"
if SABOTAGE_HOOK="${WORK}/sabotage-p5.sh" \
	run_release p5 --publish >"${WORK}/last.out" 2>&1; then
	fail "origin 被推进 → 拒绝（意外成功）"
else
	pass "origin 被推进 → 拒绝"
fi
check "报被推进" "grep -qF '被推进' '${WORK}/last.out'"
check_fail "不留 tag" "git -C '${WORK}/p5-origin.git' rev-parse --verify --quiet refs/tags/v0.6.0"
check "race 提交确实落在 origin/main" "git -C '${WORK}/p5-origin.git' log -1 --format=%s main | grep -qF 'feat: raced commit'"

echo
if [[ "${FAILED}" == "1" ]]; then
	echo "release-selftest 存在失败项" >&2
	exit 1
fi
echo "全部通过"
