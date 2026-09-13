# lib-deploy.sh — deploy.sh（macOS/launchd）与 deploy-linux.sh（systemd --user）
# 共用的发布下载、版本校验、健康检查函数。
# 前提：调用方已 cd 到仓库根，且定义了 RUNTIME 与 HEALTH_URL。

REPO_SLUG="$(git remote get-url origin | sed -E 's#.*github.com[:/]([^/]+/[^/.]+)(\.git)?$#\1#')"

# api.github.com 匿名额度很低且私有 repo 需要凭据，依次尝试 gh keyring /
# git credential；公开 repo 拿不到也无妨（仅受匿名速率限制）。
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

latest_release_tag() {
	local token
	token="$(gh_token)"
	curl -sf -m 10 ${token:+-H "Authorization: Bearer ${token}"} \
		"https://api.github.com/repos/${REPO_SLUG}/releases/latest" |
		sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p'
}

# release_asset_name 按当前平台输出 release 资产名（Windows 资产带 .exe，
# 但 deploy 脚本不跑 Windows——裸 exe 手动下载即可）。
release_asset_name() {
	case "$(uname -s)-$(uname -m)" in
	Darwin-arm64) echo "devin-2api-darwin-arm64" ;;
	Darwin-x86_64) echo "devin-2api-darwin-amd64" ;;
	Linux-aarch64 | Linux-arm64) echo "devin-2api-linux-arm64" ;;
	Linux-x86_64) echo "devin-2api-linux-amd64" ;;
	*)
		echo "unsupported platform: $(uname -s)-$(uname -m)" >&2
		return 1
		;;
	esac
}

# sha256_file 兼容 Linux（sha256sum）与 macOS（shasum）。
sha256_file() {
	if command -v sha256sum >/dev/null; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# download_release_binary <tag> <output>：下载当前平台资产并按 checksums.txt
# 校验；输出文件权限置为可执行。
download_release_binary() {
	local tag="$1" out="$2" asset base token expected actual
	asset="$(release_asset_name)" || return 1
	base="https://github.com/${REPO_SLUG}/releases/download/${tag}"
	token="$(gh_token)"
	# 进度走 stderr：本函数会被 build_or_download 在 $() 里调用，
	# stdout 留给版本号返回值，混入进度会污染捕获结果。
	echo "==> download ${asset} @ ${tag}" >&2
	# 公开 repo 下 token 为空也无妨；留着 auth 头以兼容 repo 转 private 的场景，
	# github.com 重定向到 S3 预签名 URL 时 curl 不会跨主机转发 Authorization。
	# -C - 断点续传：失败留下半成品，重跑接着下；若残留的是别的版本残片，
	# 下面的 sha256 校验会拦下并删除。
	curl -fL -C - ${token:+-H "Authorization: Bearer ${token}"} -o "${out}" "${base}/${asset}" || {
		echo "download failed — ${tag} 无二进制资产（老发版只有镜像）或网络中断" >&2
		return 1
	}
	expected="$(curl -sfL ${token:+-H "Authorization: Bearer ${token}"} "${base}/checksums.txt" | awk -v a="${asset}" '$2==a {print $1}')"
	actual="$(sha256_file "${out}")"
	[[ -n "${expected}" && "${expected}" == "${actual}" ]] || {
		echo "sha256 mismatch (expected ${expected:-<missing>}, got ${actual})" >&2
		rm -f "${out}"
		return 1
	}
	chmod +x "${out}"
}

# build_or_download <release_tag_or_empty>：--release 走下载，否则源码构建；
# 产物一律写 devin-2api.new，版本号经 stdout 返回（进度输出走 stderr）。
build_or_download() {
	local tag="$1" version
	if [[ -n "${tag}" ]]; then
		if [[ "${tag}" == "latest" ]]; then
			tag="$(latest_release_tag)" || {
				echo "failed to resolve latest release tag (auth or rate limit)" >&2
				return 1
			}
		fi
		download_release_binary "${tag}" devin-2api.new || return 1
		version="${tag}"
	else
		version="$(git describe --tags --always --dirty)"
		echo "==> build devin-2api ${version}" >&2
		go build -ldflags "-X main.version=${version}" -o devin-2api.new ./cmd/devin-2api || return 1
	fi
	printf '%s' "${version}"
}

# smoke_version <binary> <expected>：新二进制 -version 必须精确回包。
smoke_version() {
	echo "==> smoke: 新二进制 -version"
	"$1" -version | grep -qx "$2" || {
		echo "version mismatch in new binary" >&2
		rm -f "$1"
		return 1
	}
}

# install_binary <new_binary>：装入 RUNTIME 并同步仓库 config.yaml；
# 仓库内 logs 符号链接指向运行目录，排障路径与 AGENTS.md 约定一致。
install_binary() {
	mkdir -p "${RUNTIME}/logs"
	mv "$1" "${RUNTIME}/devin-2api"
	cmp -s config.yaml "${RUNTIME}/config.yaml" 2>/dev/null ||
		echo "==> config.yaml 与运行目录不一致，以仓库版本覆盖"
	cp config.yaml "${RUNTIME}/config.yaml"
	# logs 已是真实目录（本地 -config config.yaml 跑过）则不动，避免吞掉现场。
	if [[ -L logs || ! -e logs ]]; then
		ln -sfn "${RUNTIME}/logs" logs
	fi
	echo "==> installed ${RUNTIME}/devin-2api (config.yaml synced from repo)"
}

# config_listen_port 从 config.yaml 的 server.listen 提取端口（取最后一个
# 冒号后的数字，兼容 ":3003" 与 "127.0.0.1:8080" 写法）；解析不到返回空。
config_listen_port() {
	sed -n 's/^ *listen:.*:\([0-9]\{1,5\}\).*/\1/p' config.yaml | head -1
}

# warn_strays <keep_pid>：列出非服务托管的 devin-2api 进程（单实例约定——
# 它们会抢端口、分流请求，且不受优雅退出保护）。
# 按可执行名精确匹配（comm）：pgrep -f 会把 cmdline 里含 "devin-2api"
# 的 bash/grep（含本函数自己的管道与外层 `cd devin-2api` 的 shell）
# 误报为 stray。
warn_strays() {
	local pids
	pids="$(pgrep -x devin-2api | grep -vx "${1:-0}" || true)"
	[[ -z "${pids}" ]] && return 0
	echo "WARN: 非服务托管的 devin-2api 进程（单实例约定，建议 kill <pid> 优雅关闭）:" >&2
	ps -o pid=,args= -p "$(printf '%s\n' "${pids}" | paste -sd, -)" >&2
}

# healthz_version 返回 /healthz 的 version 字段；未运行/解析失败返回空。
healthz_version() {
	curl -sf -m 2 "$1" 2>/dev/null | sed -n 's/.*"version" *: *"\([^"]*\)".*/\1/p'
}

# wait_healthz_version <health_url> <want_version> [seconds]
# 优雅重启期间旧进程继续应答 healthz（旧版本 + draining 标记），
# 首次 200 不代表新实例已接管——必须轮询到版本匹配才确认。
# 成功时 stdout 输出运行中版本；超时输出最后看到的版本并返回 1。
wait_healthz_version() {
	local url="$1" want="$2" secs="${3:-90}" running="" _
	for _ in $(seq $((secs * 2))); do
		running="$(healthz_version "${url}")"
		[[ "${running}" == "${want}" ]] && {
			printf '%s' "${running}"
			return 0
		}
		sleep 0.5
	done
	printf '%s' "${running:-<none>}"
	return 1
}

# check_versions 对比 已安装/运行中/最新 release 版本；不一致返回 1。
check_versions() {
	local installed running latest
	installed="$("${RUNTIME}/devin-2api" -version 2>/dev/null || echo '<未安装>')"
	running="$(healthz_version "${HEALTH_URL}")"
	latest="$(latest_release_tag 2>/dev/null || echo '<查询失败>')"
	printf 'installed: %s\nrunning:   %s\nlatest:    %s\n' "${installed}" "${running:-<未运行>}" "${latest}"
	[[ "${installed}" == "${latest}" ]]
}

# parse_deploy_args 解析三个脚本共用的 --release/--no-restart/--check。
RELEASE_TAG=""
NO_RESTART=0
CHECK=0
parse_deploy_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--release)
			RELEASE_TAG="${2:?--release 需要 tag（如 v0.2.0）或 latest}"
			shift 2
			;;
		--no-restart)
			NO_RESTART=1
			shift
			;;
		--check)
			CHECK=1
			shift
			;;
		*)
			echo "unknown arg: $1" >&2
			exit 2
			;;
		esac
	done
}
