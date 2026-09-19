#!/usr/bin/env bash
# install.sh — devin-2api 一键安装/升级入口：无仓库依赖，可 curl | bash。
#
#   curl -sSL https://raw.githubusercontent.com/WncFht/devin2api/main/scripts/install.sh | bash
#
# 本体是薄引导层：按目标版本从 GitHub 拉取 scripts/deploy/* 与
# config.example.yaml 到临时 staging，再交给对应平台的 deploy 脚本
#（Linux→deploy-linux.sh/systemd --user，macOS→deploy.sh/launchd）完成
# 下载校验、服务定义生成与 REUSEPORT 零停机交接——部署逻辑不在这里重写。
#
# 用法:
#   install.sh [install] [-v <tag>]   安装（缺省命令；无 tag 装最新 release）
#   install.sh upgrade [-v <tag>]     升级到最新或指定版本
#   install.sh rollback <tag>         回滚到指定 release
#   install.sh status                 对比 已安装/运行中/最新 版本
#   install.sh list-versions          列出最近 release 版本
#   install.sh uninstall              停用并移除服务与二进制（保留 config 与 logs）
#   install.sh --no-restart ...       只替换二进制不重启（透传给 deploy 脚本）
#
# 覆盖项（env）：
#   DEVIN2API_REPO       release 来源 repo（默认 WncFht/devin2api）
#   DEVIN2API_REF        拉取部署脚本的 git ref（默认：目标 tag，其次 latest，兜底 main）
#   DEVIN2API_STAGE_DIR  复用已有 staging 目录（跳过下载，调试用）
#   DEVIN2API_LABEL / DEVIN2API_BIN_DIR / DEVIN2API_CONFIG_DIR /
#   DEVIN2API_STATE_DIR / DEVIN2API_PORT ——透传给 deploy 脚本
set -euo pipefail

REPO="${DEVIN2API_REPO:-WncFht/devin2api}"
RAW="https://raw.githubusercontent.com/${REPO}"
CONFIG_DIR="${DEVIN2API_CONFIG_DIR:-${XDG_CONFIG_HOME:-${HOME}/.config}/devin-2api}"

if [[ -t 2 ]]; then
	_C_RED=$'\033[31m' _C_YEL=$'\033[33m' _C_RST=$'\033[0m'
else
	_C_RED='' _C_YEL='' _C_RST=''
fi
warn() { echo "${_C_YEL}WARN${_C_RST} $*" >&2; }
die() {
	echo "${_C_RED}ERROR${_C_RST} $*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
用法:
  install.sh [install] [-v <tag>]   安装（缺省命令；无 tag 装最新 release）
  install.sh upgrade [-v <tag>]     升级到最新或指定版本
  install.sh rollback <tag>         回滚到指定 release
  install.sh status                 对比 已安装/运行中/最新 版本
  install.sh list-versions          列出最近 release 版本
  install.sh uninstall              停用并移除服务与二进制（保留 config 与 logs）
  install.sh --no-restart ...       只替换二进制不重启（透传给 deploy 脚本）

覆盖项（env）：DEVIN2API_REPO / DEVIN2API_REF / DEVIN2API_STAGE_DIR /
DEVIN2API_LABEL / DEVIN2API_BIN_DIR / DEVIN2API_CONFIG_DIR /
DEVIN2API_STATE_DIR / DEVIN2API_PORT
EOF
}

CMD=""
TAG=""
NO_RESTART=0
POSITIONAL=()
while [[ $# -gt 0 ]]; do
	case "$1" in
	install | upgrade | rollback | status | check | uninstall | list-versions)
		CMD="$1"
		shift
		;;
	-v | --version)
		TAG="${2:?-v 需要 tag（如 v0.15.0）}"
		shift 2
		;;
	--no-restart)
		NO_RESTART=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		POSITIONAL+=("$1")
		shift
		;;
	esac
done
CMD="${CMD:-install}"

# rollback <tag> 是唯一吃位置参数的命令；其余命令收到裸参数按错处理。
if [[ "${CMD}" == "rollback" ]]; then
	[[ -z "${TAG}" && ${#POSITIONAL[@]} -gt 0 ]] && TAG="${POSITIONAL[0]}"
elif [[ ${#POSITIONAL[@]} -gt 0 ]]; then
	die "unknown arg: ${POSITIONAL[0]}（--help 查看用法）"
fi

# GitHub API 凭据（可选）：公开 repo 匿名即可，限额紧张或私有 repo 时
# 依次尝试 GH_TOKEN / gh keyring / git credential。
gh_token() {
	local token="${GH_TOKEN:-}"
	if [[ -z "${token}" ]] && command -v gh >/dev/null; then
		token="$(gh auth token 2>/dev/null || true)"
	fi
	if [[ -z "${token}" ]]; then
		token="$(printf 'protocol=https\nhost=github.com\n' | git credential fill 2>/dev/null | awk -F= '/^password=/{print $2}')"
	fi
	printf '%s' "${token}"
}

# latest_tag：/releases/latest 的 302 重定向不吃 api.github.com 匿名限流，
# 失败再退 REST API。
latest_tag() {
	local loc token
	loc="$(curl -sfI -m 10 -o /dev/null -w '%{redirect_url}' "https://github.com/${REPO}/releases/latest" 2>/dev/null || true)"
	if [[ "${loc}" =~ /releases/tag/([^/[:space:]]+) ]]; then
		printf '%s' "${BASH_REMATCH[1]}"
		return 0
	fi
	token="$(gh_token)"
	curl -sf -m 10 ${token:+-H "Authorization: Bearer ${token}"} \
		"https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p'
}

cmd_list_versions() {
	local token
	token="$(gh_token)"
	curl -sf -m 15 ${token:+-H "Authorization: Bearer ${token}"} \
		"https://api.github.com/repos/${REPO}/releases?per_page=20" |
		sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p' ||
		die "release 列表获取失败（网络中断或 API 限流）"
}

OS="$(uname -s)"
case "${OS}" in
Linux) DEPLOY_SCRIPT="deploy-linux.sh" ;;
Darwin) DEPLOY_SCRIPT="deploy.sh" ;;
*) die "install.sh 仅适用 Linux/macOS；Windows 直接下 release exe 用 deploy-windows.ps1" ;;
esac
[[ "$(id -u)" != "0" ]] || die "不需要 sudo——服务以当前用户托管（systemd --user / launchd gui domain），请以普通用户运行"
command -v curl >/dev/null || die "缺少 curl"

if [[ "${CMD}" == "list-versions" ]]; then
	cmd_list_versions
	exit 0
fi

if [[ "${OS}" == "Linux" ]]; then
	command -v systemctl >/dev/null || die "需要 systemd（找不到 systemctl）"
else
	command -v launchctl >/dev/null || die "需要 launchd（找不到 launchctl）"
fi

if [[ "${CMD}" == "rollback" && -z "${TAG}" ]]; then
	die "rollback 需要 tag（install.sh rollback vX.Y.Z；list-versions 看可选版本）"
fi

# 部署脚本来源 ref：显式 DEVIN2API_REF > 目标 tag > latest tag > main。
# 按 tag 拉取保证脚本与被装版本同代；各文件再独立兜底 main 防老 tag 尚无脚本。
REF="${DEVIN2API_REF:-}"
if [[ -z "${REF}" ]]; then
	REF="${TAG:-$(latest_tag || true)}"
	REF="${REF:-main}"
fi

STAGE="${DEVIN2API_STAGE_DIR:-}"
CLEAN_STAGE=0
if [[ -z "${STAGE}" ]]; then
	STAGE="$(mktemp -d)"
	CLEAN_STAGE=1
	trap '[[ "${CLEAN_STAGE}" == "1" ]] && rm -rf "${STAGE}"' EXIT
fi

fetch() { # fetch <repo相对路径>：先按 REF 拉，404 兜底 main
	local path="$1"
	mkdir -p "${STAGE}/$(dirname "${path}")"
	curl -sfL -m 30 -o "${STAGE}/${path}" "${RAW}/${REF}/${path}" 2>/dev/null && return 0
	curl -sfL -m 30 -o "${STAGE}/${path}" "${RAW}/main/${path}" ||
		die "拉取 ${path} 失败（ref=${REF} 与 main 均不可达）"
	[[ "${REF}" == "main" ]] || warn "${path} 在 ${REF} 不存在，已回退 main 版本"
}

if [[ "${CLEAN_STAGE}" == "1" ]]; then
	echo "==> fetch deploy scripts @ ${REF} (${REPO})"
	fetch "scripts/deploy/${DEPLOY_SCRIPT}"
	fetch scripts/deploy/lib-deploy.sh
	fetch scripts/deploy/rotate-logs.sh
	fetch config.example.yaml
	# DEVIN2API_REPO 指向 fork 时给 staging 挂个假 origin，release_slugs 据此
	# 优先 fork 的 release 资产；无 git 环境跳过（兜底常量仍指向上游）。
	if command -v git >/dev/null && ! git -C "${STAGE}" rev-parse --git-dir >/dev/null 2>&1; then
		git -C "${STAGE}" init -q 2>/dev/null || true
		git -C "${STAGE}" remote add origin "https://github.com/${REPO}.git" 2>/dev/null || true
	fi
fi

# live config 拷进 staging：deploy 脚本的 ensure_config/detect_port/preflight
# 读的都是仓库位 config.yaml——有 live 副本时以它为准（升级/卸载/--check
# 路径下端口与凭据判定才不失真），缺失时照旧从 example 生成。
if [[ -f "${CONFIG_DIR}/config.yaml" && ! -f "${STAGE}/config.yaml" ]]; then
	cp "${CONFIG_DIR}/config.yaml" "${STAGE}/config.yaml"
fi

DEPLOY_ARGS=()
case "${CMD}" in
install | upgrade)
	DEPLOY_ARGS+=(--release "${TAG:-latest}")
	;;
rollback)
	DEPLOY_ARGS+=(--release "${TAG}")
	;;
status | check)
	DEPLOY_ARGS+=(--check)
	;;
uninstall)
	DEPLOY_ARGS+=(--uninstall)
	;;
esac
[[ "${NO_RESTART}" == "1" ]] && DEPLOY_ARGS+=(--no-restart)

bash "${STAGE}/scripts/deploy/${DEPLOY_SCRIPT}" "${DEPLOY_ARGS[@]}"
