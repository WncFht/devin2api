# lib-deploy.sh — deploy.sh（macOS/launchd）与 deploy-linux.sh（systemd --user）
# 共用的发布下载、版本校验、健康检查与首装预检函数。
# 前提：调用方已 cd 到仓库根，且定义了 BIN_DIR/CONFIG_DIR/STATE_DIR
# （平台规范布局：二进制入 ~/.local/bin，配置入平台 config dir，logs/ 与
# 状态文件入平台 state dir）与 LEGACY_RUNTIME（上一版单运行目录路径，
# 供迁移与 --check 回退）。PORT/HEALTH_URL 由 detect_port 生成
# （config.yaml 就绪后再调用，首次调用只为 --check）。

# 告警/错误一律走 stderr——多处函数经 $() 捕获 stdout，混入噪音会污染
# 返回值。tty 上加颜色便于人类扫读。
if [[ -t 2 ]]; then
	_C_RED=$'\033[31m' _C_YEL=$'\033[33m' _C_RST=$'\033[0m'
else
	_C_RED='' _C_YEL='' _C_RST=''
fi
warn() { echo "${_C_YEL}WARN${_C_RST} $*" >&2; }
err() { echo "${_C_RED}ERROR${_C_RST} $*" >&2; }
die() {
	err "$*"
	exit 1
}

deploy_usage() {
	cat <<EOF
用法: $(basename "$0") [--release <tag|latest>] [--no-restart] [--check] [--uninstall] [--help]
  --release     安装 GitHub Release 预编译二进制（sha256 校验）；缺省为源码构建
  --no-restart  只替换二进制，不重启服务（下次自然重启时生效）
  --check       只对比 已安装/运行中/最新 release 版本，不做变更
  --uninstall   停用并移除服务与二进制（保留 config.yaml 与 logs/）
  --help        显示本说明

首装与升级同一条命令：服务未安装时自动生成服务定义并拉起；config.yaml
缺失时从 config.example.yaml 生成——写入随机 dashboard.password，终端
下提示粘贴 Devin session token（写入 devin.accounts 首条），否则留空
池——面板 /web/accounts.html 再加号。下游 /v1 令牌不入配置：面板
/web/auth-tokens 创建，明文一次性出示，仓内只存哈希。
覆盖项（env）：DEVIN2API_LABEL / DEVIN2API_BIN_DIR / DEVIN2API_CONFIG_DIR /
DEVIN2API_STATE_DIR / DEVIN2API_PORT（DEVIN2API_RUNTIME 视作 STATE_DIR 的
兼容别名）。
EOF
}

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

# release_slugs 输出 release 候选 repo（每行一个）：origin（若是 GitHub
# 地址）在前，上游 WncFht/devin2api 兜底——fork 克隆的 origin 一般没有
# release 资产，会自动落到上游；无 .git（tarball 解压）也能解析出兜底项。
release_slugs() {
	local origin slug
	origin="$(git remote get-url origin 2>/dev/null || true)"
	slug="$(printf '%s' "${origin}" | sed -nE 's#.*github\.com[-[:alnum:]_.]*[:/]([^/]+/[^/.]+)(\.git)?$#\1#p')"
	{
		[[ -n "${slug}" ]] && printf '%s\n' "${slug}"
		printf '%s\n' "WncFht/devin2api"
	} | awk '!seen[$0]++'
}

# latest_tag_of <slug>：解析该 repo 最新 release tag；失败/无限额返回空。
# 先走 /releases/latest 的 302 重定向——不吃 api.github.com 匿名限流
# （60/h/IP，共享出口易打满）；失败再退到 REST API。
latest_tag_of() {
	local loc token
	loc="$(curl -sfI -m 10 -o /dev/null -w '%{redirect_url}' "https://github.com/$1/releases/latest" 2>/dev/null || true)"
	if [[ "${loc}" =~ /releases/tag/([^/[:space:]]+) ]]; then
		printf '%s' "${BASH_REMATCH[1]}"
		return 0
	fi
	token="$(gh_token)"
	curl -sf -m 10 ${token:+-H "Authorization: Bearer ${token}"} \
		"https://api.github.com/repos/$1/releases/latest" |
		sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p'
}

# latest_release_tag 按候选序取首个能解析出 tag 的 repo。
latest_release_tag() {
	local slug tag
	for slug in $(release_slugs); do
		tag="$(latest_tag_of "${slug}")"
		[[ -n "${tag}" ]] && {
			printf '%s' "${tag}"
			return 0
		}
	done
	return 1
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

# download_release_binary <slug> <tag> <output>：下载指定 repo 的当前平台
# 资产并按 checksums.txt 校验；输出文件权限置为可执行。
download_release_binary() {
	local slug="$1" tag="$2" out="$3" asset base token expected actual
	asset="$(release_asset_name)" || return 1
	base="https://github.com/${slug}/releases/download/${tag}"
	token="$(gh_token)"
	# 进度走 stderr：本函数会被 build_or_download 在 $() 里调用，
	# stdout 留给版本号返回值，混入进度会污染捕获结果。
	echo "==> download ${asset} @ ${tag} (${slug})" >&2
	# 公开 repo 下 token 为空也无妨；留着 auth 头以兼容 repo 转 private 的场景，
	# github.com 重定向到 S3 预签名 URL 时 curl 不会跨主机转发 Authorization。
	# -C - 断点续传：失败留下半成品，重跑接着下；若残留的是别的版本残片，
	# 下面的 sha256 校验会拦下并删除。
	curl -fL -C - ${token:+-H "Authorization: Bearer ${token}"} -o "${out}" "${base}/${asset}" || {
		echo "download failed — ${slug} @ ${tag} 无二进制资产或网络中断" >&2
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
# 下载按 release_slugs 候选序尝试：显式 tag 在 origin 无资产时落到上游。
build_or_download() {
	local want="$1" slug tag version=""
	if [[ -n "${want}" ]]; then
		for slug in $(release_slugs); do
			tag="${want}"
			if [[ "${want}" == "latest" ]]; then
				tag="$(latest_tag_of "${slug}")"
				[[ -n "${tag}" ]] || continue
			fi
			if download_release_binary "${slug}" "${tag}" devin-2api.new; then
				version="${tag}"
				break
			fi
		done
		[[ -n "${version}" ]] || {
			echo "release ${want} 解析或下载失败（tag 不存在、无该平台资产或网络中断）" >&2
			return 1
		}
	else
		version="$(git describe --tags --always --dirty)" || {
			echo "git describe 失败——非 git 环境请用 --release latest" >&2
			return 1
		}
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

# install_binary <new_binary>：装入 BIN_DIR 并把仓库 config.yaml 同步到
# CONFIG_DIR。live config 是权威副本——deploy-remote 各模式在 deploy.sh
# 前已把它刷进 staging，正常路径下 cmp 必相等。只在 live 缺失时从仓库
# 副本恢复；live 存在且不一致说明同步被绕过（手动 deploy.sh、staging
# 残留旧快照），告警并保留 live——绝不把陈旧快照盖回 live。仓库内 logs
# 符号链接指向 STATE_DIR/logs，排障路径与 AGENTS.md 约定一致。
install_binary() {
	mkdir -p "${BIN_DIR}" "${CONFIG_DIR}" "${STATE_DIR}/logs"
	mv "$1" "${BIN_DIR}/devin-2api"
	if [[ ! -f "${CONFIG_DIR}/config.yaml" ]]; then
		warn "${CONFIG_DIR}/config.yaml 缺失——从仓库副本恢复"
		cp config.yaml "${CONFIG_DIR}/config.yaml"
	elif ! cmp -s config.yaml "${CONFIG_DIR}/config.yaml" 2>/dev/null; then
		warn "config.yaml 与 ${CONFIG_DIR} 不一致——live 是权威副本故保留，仓库侧差异被忽略"
	fi
	# logs 已是真实目录（本地 -config config.yaml 跑过）则不动，避免吞掉现场。
	if [[ -L logs || ! -e logs ]]; then
		ln -sfn "${STATE_DIR}/logs" logs
	fi
	echo "==> installed ${BIN_DIR}/devin-2api (config.yaml → ${CONFIG_DIR})"
}

# install_rotate_script：把 copytruncate 轮转脚本装到 BIN_DIR（脱离仓库路径
# 也能被定时任务引用——worktree 部署会整个重铺 staging，仓库路径不可靠）。
install_rotate_script() {
	install -m 755 scripts/rotate-logs.sh "${BIN_DIR}/devin-2api-logrotate"
}

# migrate_legacy_runtime <旧运行目录>：把上一版「单运行目录」布局迁到拆分
# 布局——config.yaml 入 CONFIG_DIR、logs/ 逐项并入 STATE_DIR/logs，删旧
# 二进制；旧布局是独立目录时另回收其 .handoff.pid（登记在册的残留交接
# 进程 SIGTERM 退场）。同名冲突不覆盖——.jsonl 属追加日志把旧尾部接上，
# 其余留给人工。可重入：迁移跑在老实例排空前，在途请求仍会往旧路径补写
# 尾账，各 deploy 脚本在重启验证后再调一次收编。macOS 下旧运行目录与
# CONFIG_DIR/STATE_DIR 同路径，只剩删旧二进制一件事——那里的
# .handoff.pid 是当前布局的交接登记表：本函数在部署收尾还会被调一遍，
# 此时当次交接进程可能仍在排空，把它当残留清掉等于强杀在途请求（排空期
# 第二发信号走默认动作立即退出），必须跳过。
migrate_legacy_runtime() {
	local old="$1" pid item base
	[[ -d "${old}" ]] || return 0
	if [[ "${old}" != "${CONFIG_DIR}" && "${old}" != "${STATE_DIR}" ]]; then
		pid="$(cat "${old}/.handoff.pid" 2>/dev/null || true)"
		rm -f "${old}/.handoff.pid"
		# pid 复用防护与 retire_stale_transient 同理：确认仍是 devin-2api 再发信号。
		if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null &&
			pgrep -x devin-2api | grep -qx "${pid}"; then
			echo "==> 旧布局残留的交接进程 pid=${pid} 退场（SIGTERM）" >&2
			kill "${pid}" 2>/dev/null || true
		fi
	fi
	rm -f "${old}/devin-2api"
	[[ "${old}" == "${CONFIG_DIR}" || "${old}" == "${STATE_DIR}" ]] && return 0
	if [[ -f "${old}/config.yaml" && ! -f "${CONFIG_DIR}/config.yaml" ]]; then
		mkdir -p "${CONFIG_DIR}"
		mv "${old}/config.yaml" "${CONFIG_DIR}/config.yaml"
		echo "==> 迁移 ${old}/config.yaml → ${CONFIG_DIR}/" >&2
	fi
	if [[ -d "${old}/logs" ]]; then
		mkdir -p "${STATE_DIR}/logs"
		for item in "${old}/logs"/*; do
			[[ -e "${item}" ]] || continue
			base="$(basename "${item}")"
			if [[ ! -e "${STATE_DIR}/logs/${base}" ]]; then
				mv "${item}" "${STATE_DIR}/logs/${base}"
			elif [[ -f "${item}" && "${base}" == *.jsonl ]]; then
				cat "${item}" >>"${STATE_DIR}/logs/${base}" && rm -f "${item}"
				echo "==> 合并 ${old}/logs/${base} 尾部 → ${STATE_DIR}/logs/" >&2
			fi
		done
		rmdir "${old}/logs" 2>/dev/null || true
	fi
	rmdir "${old}" 2>/dev/null ||
		warn "旧运行目录 ${old} 有残留（同名冲突不覆盖）——确认后可手动删除"
}

# yaml_scalar <key>：取 config.yaml 里首个 "key: value" 的值（去单/双
# 引号、截断行内空格/注释）。只够读本项目扁平的 key: value 行，不是通用解析。
yaml_scalar() {
	[[ -f config.yaml ]] || return 0
	sed -nE "s/^ *$1: *['\"]?([^'\"# ]*)['\"]?.*/\1/p" config.yaml | head -1
}

# config_credentials_files <file>：列出指定配置文件里全部
# credentials_file 值（每行一个，去引号）。账号池每条目可各指一个文件，
# yaml_scalar 只取首个不够用；同款 sed，只覆盖块式 key: value 写法——
# flow 式漏网条目由交接金丝雀在部署期兜底。
config_credentials_files() {
	[[ -f "$1" ]] || return 0
	sed -nE "s/^ *credentials_file: *['\"]?([^'\"# ]*)['\"]?.*/\1/p" "$1"
}

# config_listen_port 从 config.yaml 的 server.listen 提取端口（取最后一个
# 冒号后的数字，兼容 ":3003" 与 "127.0.0.1:8080" 写法）；解析不到返回空。
config_listen_port() {
	[[ -f config.yaml ]] || return 0
	sed -n 's/^ *listen:.*:\([0-9]\{1,5\}\).*/\1/p' config.yaml | head -1
}

# sed_inplace 兼容 GNU（sed -i）与 BSD（sed -i ''）的就地改写。
sed_inplace() {
	if sed --version >/dev/null 2>&1; then
		sed -i "$@"
	else
		sed -i '' "$@"
	fi
}

# set_yaml_scalar <key> <value> <file>：就地替换首个 key 行为 key: "<value>"
# ——YAML 映射要求冒号后有空格，重写整行顺带把 key:"v" 这类写法归一。
set_yaml_scalar() {
	local esc
	esc="$(printf '%s' "$2" | sed 's/[&\\]/\\&/g')"
	sed_inplace "s|^\\( *\\)$1:.*|\\1$1: \"${esc}\"|" "$3"
}

gen_secret() {
	openssl rand -hex 16 2>/dev/null || od -An -N16 -tx1 /dev/urandom | tr -d ' \n'
}

# ensure_config：config.yaml 缺失时从 config.example.yaml 生成——写入随机
# dashboard.password（示例默认 ":8080" 全网卡监听，空面板密码等于把面板
# 开放给 LAN）；终端下提示粘贴 Devin session token，粘贴了就在 devin:
# 段首注入 devin.accounts 首条（名 main），否则留空池——空池合法，面板
# /web/accounts.html 随时加号。下游令牌不再落配置：首装令牌仓为空即
# /v1 开放准入，非回环监听由 preflight 的令牌仓检查告警。已有
# config.yaml 时不动——用户手写优先。
ensure_config() {
	[[ -f config.yaml ]] && return 0
	[[ -f config.example.yaml ]] ||
		die "config.yaml 缺失且找不到 config.example.yaml——请在仓库根目录运行"
	echo "==> first install: 从 config.example.yaml 生成 config.yaml" >&2
	cp config.example.yaml config.yaml
	chmod 600 config.yaml
	set_yaml_scalar password "$(gen_secret)" config.yaml
	local token=""
	# -r/-w 测的是权限不是控制终端——无 tty 环境下 open /dev/tty 才失败。
	if (exec 3<>/dev/tty) 2>/dev/null; then
		printf 'Devin session token（devin-session-token$...，留空起空池后面板加号）: ' >/dev/tty
		IFS= read -r -s token </dev/tty || true
		printf '\n' >/dev/tty
	fi
	# accounts 是列表嵌套块，set_yaml_scalar 只能写标量——awk 在 devin:
	# 行后整体插入（map 内键序无关）。token 用 yaml 单引号包裹。
	[[ -n "${token}" ]] &&
		awk -v tok="${token}" '
			{ print }
			!done && /^devin:/ {
				print "  accounts:";
				print "    - name: '"'"'main'"'"'";
				print "      token: '"'"'" tok "'"'"'";
				done=1
			}' config.yaml >config.yaml.tmp &&
		mv config.yaml.tmp config.yaml
}

# token_source_desc 描述启动时凭据将来自何处；无处可寻返回空。
# 凭据只认 devin.accounts 声明（env/credentials.toml 发现链已不进
# load 路径）；面板库里的 panel 行不在此探——脚本视角只报 config 层。
token_source_desc() {
	[[ -n "$(yaml_scalar token)" ]] && {
		printf 'config.yaml devin.accounts[].token'
		return 0
	}
	local creds
	creds="$(yaml_scalar credentials_file)"
	[[ -n "${creds}" ]] && {
		printf 'config.yaml devin.accounts[].credentials_file (%s)' "${creds}"
		return 0
	}
	printf ''
}

# listen_is_loopback：server.listen 绑在回环上返回 0。
listen_is_loopback() {
	case "$(yaml_scalar listen)" in
	127.* | localhost:* | \[::1\]*) return 0 ;;
	*) return 1 ;;
	esac
}

# preflight_deploy：开工前把"必炸"与"装了也白装"的场景拦下或喊出来——
# 每个失败点原地给出修复路径，而不是让错误漂到 90s 后的 healthz 超时。
# 依赖 RELEASE_TAG 已解析（决定要不要 git/go）。
preflight_deploy() {
	# 两平台都是用户级服务；sudo 会把 unit/二进制写到 root 名下。
	[[ "$(id -u)" == "0" ]] &&
		die "不需要 sudo——launchd gui 域与 systemd --user 都属当前用户，请以普通用户运行"
	command -v curl >/dev/null || die "缺少 curl"
	if [[ -z "${RELEASE_TAG}" ]]; then
		command -v git >/dev/null || die "源码构建需要 git；也可用 --release latest 免构建"
		command -v go >/dev/null || die "源码构建需要 go 工具链；也可用 --release latest 免构建"
	fi

	ensure_config

	# 模板占位 token 会让上游全部 401/403——拒绝部署。
	grep -q 'devin-session-token\$mock-token' config.yaml &&
		die "config.yaml 仍是模板占位 token（mock-token）：请在 devin.accounts 填真实号凭据"

	local src
	src="$(token_source_desc)"
	if [[ -z "${src}" ]]; then
		warn "config.yaml 未声明 devin.accounts（面板已建的号除外）"
		warn "空池起跑的实例 /v1/* 不可用——面板 /web/accounts.html 加号即热上线，免重启"
	else
		echo "==> 凭据来源: ${src}" >&2
	fi

	# 启动期 config.Load 实读每个 credentials_file——文件被外部摘除会让
	# 新实例死在加载期（9-18 断流 2h55m 根因）。部署强制冷启动，build
	# 之前先拦死引用。新实例实际加载的是 ${CONFIG_DIR}/config.yaml
	# （live 权威副本），live 缺失时才会由仓库副本顶上。
	local cfg_file cred
	cfg_file="config.yaml"
	[[ -f "${CONFIG_DIR}/config.yaml" ]] && cfg_file="${CONFIG_DIR}/config.yaml"
	while IFS= read -r cred; do
		[[ -n "${cred}" ]] || continue
		cred="${cred/#~\//${HOME}/}"
		[[ "${cred}" == /* ]] || cred="$(dirname "${cfg_file}")/${cred}"
		if [[ ! -f "${cred}" ]]; then
			die "devin.accounts[].credentials_file 不存在: ${cred}（声明于 ${cfg_file}）——新实例加载 config 必失败，中止部署"
		fi
	done < <(config_credentials_files "${cfg_file}")

	# 非回环监听 + 令牌仓实际开放（零行或存在匿名通道行）= 配额对网络全开。
	# 准入只看 auth_tokens 表（行内存 sha256 全 hex；匿名行 = sha256("")，
	# 现场算不嵌常量），查不动（无 sqlite3 CLI、库未建、表缺席）一律按
	# 开放告警——宁可多喊一次，不把裸奔判成安全。
	if ! listen_is_loopback; then
		local closed="" anon_hash
		anon_hash="$(printf '' | sha256sum | cut -d' ' -f1)"
		if command -v sqlite3 >/dev/null && [[ -f "${STATE_DIR}/devin-2api.db" ]]; then
			closed="$(sqlite3 -readonly "${STATE_DIR}/devin-2api.db" \
				"SELECT CASE WHEN COUNT(*)>0 AND SUM(token='${anon_hash}')=0 THEN 1 ELSE 0 END FROM auth_tokens" 2>/dev/null || true)"
		fi
		[[ "${closed}" == "1" ]] ||
			warn "server.listen 非回环且令牌仓未闭合（空仓或存在匿名通道行）——/v1 配额对网络开放，先在面板 /web/auth-tokens 建令牌"
	fi
}

# detect_port <default>：生成 PORT 与 HEALTH_URL。须在 config.yaml 就绪后
# 调用（ensure_config 之后），否则只能拿到缺省值。
detect_port() {
	PORT="${DEVIN2API_PORT:-$(config_listen_port)}"
	PORT="${PORT:-$1}"
	HEALTH_URL="http://localhost:${PORT}/healthz"
}

# check_port_available <managed_pid>：端口被占用时判别占用者——
# 能回 healthz 版本的是 devin-2api：托管实例（升级场景）放行，
# 非托管（stray）与外来进程都拦下，省得服务等 90s 才超时。
check_port_available() {
	(exec 3<>"/dev/tcp/127.0.0.1/${PORT}") 2>/dev/null || return 0
	local v
	v="$(healthz_version "${HEALTH_URL}")"
	if [[ -n "${v}" ]]; then
		[[ -n "${1:-}" && "${1}" != "0" ]] && return 0
		die "端口 ${PORT} 已被一个非服务托管的 devin-2api 实例占用（${v}）——先 kill 掉它（见上方 stray 列表）"
	fi
	die "端口 ${PORT} 被非 devin-2api 进程占用——改 server.listen 或先释放端口"
}

# warn_strays <keep_pid...>：列出非服务托管的 devin-2api 进程（单实例约定——
# 它们会抢端口、分流请求，且不受优雅退出保护）。参数均为豁免 pid（托管
# 实例、已知交接进程）。
# 按可执行名精确匹配（comm）：pgrep 匹配 cmdline 会把含 "devin-2api"
# 的 bash/grep（含本函数自己的管道与外层 `cd devin-2api` 的 shell）
# 误报为 stray。
warn_strays() {
	local pids
	pids="$(pgrep -x devin-2api | grep -vxF -f <(printf '%s\n' "$@") || true)"
	[[ -z "${pids}" ]] && return 0
	warn "非服务托管的 devin-2api 进程（单实例约定，建议 kill <pid> 优雅关闭）:"
	ps -o pid=,args= -p "$(printf '%s\n' "${pids}" | paste -sd, -)" >&2
}

# healthz_version 返回 /healthz 的 version 字段；未运行/解析失败返回空。
healthz_version() {
	curl -sf -m 2 "$1" 2>/dev/null | sed -n 's/.*"version" *: *"\([^"]*\)".*/\1/p'
}

# healthz_active 返回 /healthz 的 active_requests 字段；未运行/老版本无此
# 字段时返回空（缺字段视为不可知，不当作 0 等）。
healthz_active() {
	curl -sf -m 2 "$1" 2>/dev/null | sed -n 's/.*"active_requests" *: *\([0-9]*\).*/\1/p'
}

# wait_inflight_idle <health_url> [seconds]：kickstart 前等在途请求清空。
# 排空期新请求全部吃 503——挑一个空闲窗口重启，把 503 窗口压到进程切换
# 间隙本身。字段缺失（旧二进制）直接跳过；超时仍有在途请求时警告并放行，
# 排空机制会护住它们（在途请求照常跑完，只是新到请求被拒）。
wait_inflight_idle() {
	local url="$1" secs="${2:-30}" active _ warned=0
	for _ in $(seq $((secs * 2))); do
		active="$(healthz_active "${url}")"
		[[ -z "${active}" ]] && return 0
		[[ "${active}" == "0" ]] && return 0
		if [[ "${warned}" == "0" ]]; then
			warned=1
			echo "==> ${active} 个在途请求，等空闲窗口再重启（最多 ${secs}s）..."
		fi
		sleep 0.5
	done
	warn "等待 ${secs}s 后仍有 ${active} 个在途请求——照常重启，排空会保护在途请求但新请求在窗口内会拿到 503"
	return 0
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

# ===================== SO_REUSEPORT 重叠交接 =====================
# 语义（本机实测，Darwin）：reuseport 组内新连接派给最先绑定且仍存活的
# socket，它退出或关闭 listener 后按绑定序轮到下一个；Linux 则按四元组
# 哈希分流。两种语义下，「交接进程先入队 → 旧实例 drain 起点即关闭
# listener → 托管新实例拉起入队 → 交接进程退场」这条链里任意时刻都有
# 健康 socket 接新连接——零 503、零拒绝、在途不受打断。
# 前提：组内所有 socket 都开了 SO_REUSEPORT（env 注入给托管实例与交接
# 进程；裸跑二进制拿不到 env，单实例端口冲突保护不变）。在跑的旧实例
# 没有 env 时交接进程 bind 必失败——spawn 探测失败后自动退化为经典重启。

# handoff_pidfile：交接进程 pid 记录——部署中断残留供下次部署回收。
handoff_pidfile() { printf '%s' "${STATE_DIR}/.handoff.pid"; }

# retire_stale_transient：回收上次部署中断留下的交接进程。它排在托管
# 实例之后绑定，且 drain 起点已关 listener——不抢流量，SIGTERM 让它在
# 自己的排空期内自然退场（不等，上限与主进程一致）。
# pidfile 跨重启持久，记录的是数字 pid——进程死后 pid 会被系统复用，
# kill -0 通过不代表还是交接进程；发信号前用 pgrep -x（与 warn_strays
# 同款的可执行名精确匹配）确认它仍是 devin-2api，避免误杀无关进程。
retire_stale_transient() {
	local pf pid
	pf="$(handoff_pidfile)"
	[[ -f "${pf}" ]] || return 0
	pid="$(cat "${pf}" 2>/dev/null || true)"
	rm -f "${pf}"
	if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null &&
		pgrep -x devin-2api | grep -qx "${pid}"; then
		echo "==> 回收上次残留的交接进程 pid=${pid}（SIGTERM，自行排空退出）" >&2
		kill "${pid}" 2>/dev/null || true
	fi
}

# spawn_handoff：起交接进程（同 config、同日志文件、reuseport env）并入队。
# 就绪判据：进程活着 + stderr.log 出现新的「HTTP server listening」行——
# bind 成功只是第一关，adapter 装配/索引回放卡住时踢掉旧实例会整段拒绝。
# stdout 只输出 pid。返回码即死因分诊：0=就绪；2=bind 撞 EADDRINUSE（旧
# 实例没开 reuseport，调用方可回退经典重启）；1=其余一切早夭/超时（新
# 实例当前起不来的实证，调用方必须中止而不是碰旧实例）。分诊依据是
# startline 之后的新鲜 stderr 段——交接进程与托管实例加载同一份 config，
# 它死在启动期等于预告托管实例也会死（9-18 断流事故的根因）。
spawn_handoff() {
	local logf startline pid
	logf="${STATE_DIR}/logs/stderr.log"
	startline=0
	[[ -f "${logf}" ]] && startline="$(wc -l <"${logf}" | tr -d ' ')"
	(
		# TZ 显式剥掉：经 ssh 拉起的会话可能带调用方 TZ（实测注入 UTC），
		# 与托管实例（无 TZ，走 /etc/localtime）的日志时区不一致。unset 后
		# 两侧同走系统时区。DEVIN2API_HANDOFF 标记这是短命占位实例——
		# 后台维护（遗留调试目录导入）只该由托管实例跑，两进程并发导入
		# 会在 UNIQUE 上互相打断。
		unset TZ
		cd "${STATE_DIR}" && exec env DEVIN2API_REUSEPORT=1 DEVIN2API_HANDOFF=1 \
			"${BIN_DIR}/devin-2api" -config "${CONFIG_DIR}/config.yaml" -state-dir "${STATE_DIR}"
	) >>"${STATE_DIR}/logs/stdout.log" 2>>"${logf}" &
	pid=$!
	printf '%s' "${pid}" >"$(handoff_pidfile)"
	local _
	for _ in $(seq 40); do
		kill -0 "${pid}" 2>/dev/null || break
		if tail -n "+$((startline + 1))" "${logf}" 2>/dev/null | grep -q 'msg="HTTP server listening"'; then
			printf '%s' "${pid}"
			return 0
		fi
		sleep 0.25
	done
	kill "${pid}" 2>/dev/null || true
	rm -f "$(handoff_pidfile)"
	if tail -n "+$((startline + 1))" "${logf}" 2>/dev/null | grep -q 'msg="port already in use"'; then
		return 2
	fi
	echo "==> 交接进程死于启动期（非端口冲突）——stderr 新增段摘录：" >&2
	tail -n "+$((startline + 1))" "${logf}" 2>/dev/null | tr -d '\0' | tail -n 8 | sed 's/^/    /' >&2
	return 1
}

# healthz_pid：/healthz 的 pid 字段；旧版本无此字段返回空。
healthz_pid() {
	curl -sf -m 2 "$1" 2>/dev/null | sed -n 's/.*"pid" *: *\([0-9]*\).*/\1/p'
}

# wait_healthz_pid <health_url> <want_pid> <secs>：轮询到应答进程的 pid
# 匹配——交接期间两侧 version 相同，pid 是确认「谁在接流量」的唯一信号。
wait_healthz_pid() {
	local url="$1" want="$2" secs="$3" got _
	for _ in $(seq $((secs * 2))); do
		got="$(healthz_pid "${url}")"
		[[ -n "${got}" && "${got}" == "${want}" ]] && return 0
		sleep 0.5
	done
	return 1
}

# wait_managed_pid <secs> <exclude_pid...>：等托管器拉起的新进程 pid。
# 旧实例退出前 svc_pid 仍报旧值，退出到重拉之间为空；一个不在排除列表
# 且连续两次读到的 pid 才算稳定接管。svc_pid 由各 deploy 脚本定义。
wait_managed_pid() {
	local secs="$1" pid="" stable="" _
	shift
	for _ in $(seq $((secs * 2))); do
		pid="$(svc_pid)"
		if [[ -n "${pid}" && "${pid}" != "0" ]] && ! printf '%s\n' "$@" | grep -qx "${pid}"; then
			[[ "${pid}" == "${stable}" ]] && {
				printf '%s' "${pid}"
				return 0
			}
			stable="${pid}"
		fi
		sleep 0.5
	done
	return 1
}

# wait_managed_listen <baseline> <mpid> <secs>：Darwin 版「托管实例已绑定」
# 证明——等 stderr.log 在 baseline 行数之后落出新 msg="HTTP server
# listening" 行且 mpid 仍存活。Darwin reuseport 只派新连接给最先存活
# socket，交接桥不死托管实例永远接不到 healthz 探测，listening 行是杀桥
# 前唯一可得的绑定证据；baseline 由调用方在 restart 前截取。
wait_managed_listen() {
	local base="$1" mpid="$2" secs="$3" logf _
	logf="${STATE_DIR}/logs/stderr.log"
	for _ in $(seq $((secs * 2))); do
		if tail -n "+$((base + 1))" "${logf}" 2>/dev/null | grep -q 'msg="HTTP server listening"' &&
			kill -0 "${mpid}" 2>/dev/null; then
			return 0
		fi
		sleep 0.5
	done
	return 1
}

# handoff_restart <old_pid> [restart_fn]：reuseport 重叠交接重启。
# restart_fn 是「让新托管实例跑起来」的动作：常规是 svc_restart（systemd
# restart 顺带载入已 daemon-reload 的新 unit 定义），plist 变更时 macOS 侧
# 传 bootout+bootstrap 的封装。spawn_handoff 的返回码决定路径：2=旧实例
# 没开 reuseport，交接桥无从谈起，退化为「等空闲 + restart_fn + 外层等
# healthz 版本」的经典路径；1=交接进程死于启动期——它是新实例的金丝雀
# （同 config 同二进制），此时回退重启等于杀掉健康旧实例换一个必死的
# 新实例（9-18 断流 2h55m 的根因），必须中止部署。
handoff_restart() {
	local old_pid="$1" restart_fn="${2:-svc_restart}" tpid mpid _ spawn_rc=0
	tpid="$(spawn_handoff)" || spawn_rc=$?
	if [[ "${spawn_rc}" == "2" ]]; then
		echo "==> 交接进程不可用（在跑实例未开 reuseport）——回退经典重启" >&2
		wait_inflight_idle "${HEALTH_URL}" 30
		"${restart_fn}"
		return 0
	fi
	[[ "${spawn_rc}" == "0" ]] ||
		die "交接进程启动失败（上方 stderr 段）——新实例当前起不来，中止部署；旧实例未受影响仍在服役"
	echo "==> 交接进程就绪 pid=${tpid}；重启托管实例（其 drain 起点即让出监听，在途继续排空）" >&2
	# Darwin 分支杀桥前靠 stderr 新 listening 行作证，基线必须在 restart 之前
	# 取——托管实例的首条 listening 可能早于 wait_managed_pid 返回。
	local listen_base=0
	[[ -f "${STATE_DIR}/logs/stderr.log" ]] &&
		listen_base="$(wc -l <"${STATE_DIR}/logs/stderr.log" | tr -d ' ')"
	# restart 失败不能放任 set -e 把脚本掐死在交接半途——交接进程已
	# 接管服役，旧实例未被信号触及仍在跑，提示后交给外层 healthz 检查。
	if ! "${restart_fn}"; then
		warn "重启命令失败——交接进程 pid=${tpid} 与旧实例并存服役，请检查托管状态"
		return 0
	fi
	if ! wait_healthz_pid "${HEALTH_URL}" "${tpid}" 90; then
		warn "交接进程未接管（可能已崩）——退化为等托管新实例直接上线"
		return 0
	fi
	echo "==> 交接进程已接管全部新连接；等托管新实例拉起（旧实例排空，上限 ~660s）" >&2
	if ! mpid="$(wait_managed_pid 660 "${old_pid}" "${tpid}")"; then
		warn "托管实例未在预期内复活——交接进程 pid=${tpid} 继续服役，pidfile 保留供下次部署回收"
		return 0
	fi
	# 先证接管再放桥：MainPID 在 fork 即赋值，不代表到 listening——慢死型
	# 崩溃循环里此刻杀桥等于亲手制造无绑窗口。Linux reuseport 按四元组
	# 哈希分流，healthz 探测几秒内必被 mpid 应答；Darwin 新连接只派给
	# 最先绑定且仍存活的 socket，tpid 不死 mpid 永远接不到探测，只能以
	# stderr 新 listening 行 + mpid 存活作证。
	if [[ "$(uname -s)" == "Darwin" ]]; then
		wait_managed_listen "${listen_base}" "${mpid}" 120
	else
		wait_healthz_pid "${HEALTH_URL}" "${mpid}" 120
	fi || {
		warn "托管实例 ${mpid} 未证实接管——交接进程 pid=${tpid} 保留服役（降级模式），pidfile 留下次部署回收"
		return 0
	}
	echo "==> 托管新实例 pid=${mpid} 已接管；交接进程开始退场" >&2
	kill "${tpid}" 2>/dev/null || true
	if ! wait_healthz_pid "${HEALTH_URL}" "${mpid}" 60; then
		warn "托管实例未及时接管应答——终态以外层 healthz 版本检查为准"
	fi
	# 交接进程自行排空退出（上限同主进程）；等 10s 仍活着则留 pidfile。
	for _ in $(seq 20); do
		kill -0 "${tpid}" 2>/dev/null || {
			rm -f "$(handoff_pidfile)"
			return 0
		}
		sleep 0.5
	done
	warn "交接进程 ${tpid} 仍在排放在途请求——pidfile 保留，退出后由下次部署清理"
	return 0
}

# smoke_upstream：部署后打一发 /v1/models——healthz 绿只证明进程活着，
# 上游 token 无效/缺失在这一层才暴露。/v1 准入全看令牌仓：空仓或含匿名
# 通道行时无凭据即过；仓非空时库里只有哈希、存量明文取不回——经面板
# admin API（Bearer 取 live config 的 dashboard.password，实例只读 live）
# 铸一条临时令牌顶上，用完即删。返回非零表示上游鉴权未通过。
smoke_upstream() {
	local key="" tid="" code src resp
	code="$(curl -s -o /dev/null -m 20 -w '%{http_code}' \
		"http://localhost:${PORT}/v1/models" 2>/dev/null || true)"
	if [[ "${code}" == "401" || "${code}" == "403" ]]; then
		local pw cfg_file
		cfg_file="config.yaml"
		[[ -f "${CONFIG_DIR}/config.yaml" ]] && cfg_file="${CONFIG_DIR}/config.yaml"
		pw="$(sed -nE "s/^ *password: *['\"]?([^'\"# ]*)['\"]?.*/\1/p" "${cfg_file}" | head -1)"
		resp="$(curl -s -m 5 -X POST -H "Authorization: Bearer ${pw}" \
			-H 'Content-Type: application/json' -d '{"description":"deploy: smoke"}' \
			"http://localhost:${PORT}/admin/auth-tokens" 2>/dev/null || true)"
		key="$(printf '%s' "${resp}" | sed -n 's/.*"token" *: *"\([^"]*\)".*/\1/p')"
		tid="$(printf '%s' "${resp}" | sed -n 's/.*"id" *: *\([0-9]*\).*/\1/p')"
		if [[ -n "${key}" ]]; then
			code="$(curl -s -o /dev/null -m 20 -w '%{http_code}' \
				-H "X-Api-Key: ${key}" "http://localhost:${PORT}/v1/models" 2>/dev/null || true)"
		fi
		[[ -n "${tid}" ]] &&
			curl -s -o /dev/null -m 5 -X DELETE -H "Authorization: Bearer ${pw}" \
				"http://localhost:${PORT}/admin/auth-tokens/${tid}" 2>/dev/null || true
	fi
	if [[ "${code}" == "200" ]]; then
		echo "==> upstream auth verified (GET /v1/models 200)"
		return 0
	fi
	case "${code}" in
	401 | 403)
		warn "服务已运行但 /v1/models 返回 HTTP ${code}——下游令牌准入被拒或上游 token 无效"
		;;
	*)
		warn "服务已运行但 /v1/models 返回 HTTP ${code:-<timeout>}——上游链路未通过"
		;;
	esac
	src="$(token_source_desc)"
	if [[ -z "${src}" ]]; then
		warn "未配置 token：见 README「提供 Devin token」；空 token 启动的实例配置后须重启"
	else
		warn "token 来源 ${src}——可能已过期；排障看 /web 日志页或 sqlite3 查 logs 表"
	fi
	return 1
}

# dump_recent_log：失败时把服务 stderr 尾部打到调用方终端，省一次翻文件。
dump_recent_log() {
	local f="${STATE_DIR}/logs/stderr.log"
	[[ -f "${f}" ]] || return 0
	echo "--- tail ${f} ---" >&2
	tail -n 15 "${f}" >&2
}

# check_versions 对比 已安装/运行中/最新 release 版本；不一致返回 1。
# 已安装版本先看 BIN_DIR（新布局），不存在再试 LEGACY_RUNTIME（旧布局），
# 让迁移前的 --check 也能报真实状态。
check_versions() {
	local installed running latest
	installed="$("${BIN_DIR}/devin-2api" -version 2>/dev/null ||
		"${LEGACY_RUNTIME:-/nonexistent}/devin-2api" -version 2>/dev/null || echo '<未安装>')"
	running="$(healthz_version "${HEALTH_URL}")"
	latest="$(latest_release_tag 2>/dev/null || echo '<查询失败>')"
	printf 'installed: %s\nrunning:   %s\nlatest:    %s\n' "${installed}" "${running:-<未运行>}" "${latest}"
	[[ "${installed}" == "${latest}" ]]
}

# remove_installed_binary：删 BIN_DIR 二进制与旧布局残留；config.yaml 与
# logs/ 保留。删了返回 0，本就不存在返回 1。
remove_installed_binary() {
	local removed=1
	if [[ -f "${BIN_DIR}/devin-2api" ]]; then
		rm -f "${BIN_DIR}/devin-2api"
		echo "==> removed ${BIN_DIR}/devin-2api"
		removed=0
	fi
	if [[ -n "${LEGACY_RUNTIME:-}" && -f "${LEGACY_RUNTIME}/devin-2api" ]]; then
		rm -f "${LEGACY_RUNTIME}/devin-2api"
		echo "==> removed ${LEGACY_RUNTIME}/devin-2api (旧布局)"
		removed=0
	fi
	return "${removed}"
}

# print_summary <version> <服务管理命令>：收尾报告——装在哪、怎么停、
# 日志与面板在哪，把"接下来怎么办"直接写出来。
print_summary() {
	cat <<EOF
==> deployed $1
    二进制   : ${BIN_DIR}/devin-2api
    配置     : ${CONFIG_DIR}/config.yaml（权威副本——直接编辑此文件，仓库侧只是部署期镜像）
    状态/日志: ${STATE_DIR}/logs（仓库 logs/ 软链同指）
    监听     : http://localhost:${PORT}（面板 /web，凭据见 config.yaml）
    服务管理 : $2
    日志     : tail -f ${STATE_DIR}/logs/stderr.log
EOF
}

# parse_deploy_args 解析三个脚本共用的 --release/--no-restart/--check/
# --uninstall/--help。
RELEASE_TAG=""
NO_RESTART=0
CHECK=0
UNINSTALL=0
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
		--uninstall)
			UNINSTALL=1
			shift
			;;
		--help | -h)
			deploy_usage
			exit 0
			;;
		*)
			echo "unknown arg: $1（--help 查看用法）" >&2
			exit 2
			;;
		esac
	done
}
