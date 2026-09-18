#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════════════════
# RETIRED 2026-09-18 — 生产实例已迁至 archbox（systemd --user :3033，
# 部署走 scripts/deploy-linux.sh）。Mac 实例与 ~/.cache/devin-2api-staging
# 流程不复存在，本脚本仅留档（worktree→staging 部署模型仍可参考）。
# ═══════════════════════════════════════════════════════════════════════════
# deploy-remote.sh — 在开发机（archbox）上驱动生产机（Mac）的 scripts/deploy.sh。
# 用法见 --help。
#
# 三种部署模式对应三类「要部署的东西」：
#   worktree（默认）：本地工作树原样上生产——git 视角的文件（tracked 含脏改
#     + 未跟踪非忽略）连同 .git 打成 tar 推到远端 staging 重铺，在那里源码
#     构建。staging 里 git describe 的版本号与本地一致（含 -dirty）。
#   --ref：部署「已推送」的状态——远端 staging 仓库（tar 自带 .git）fetch 后
#     checkout --detach -f，本地未推送的提交不会跟过去（部署前对未推送提交
#     给出提示）。
#   --release：远端 deploy.sh 直接下载预编译资产，不碰远端 git 状态。
#
# config.yaml 不进 staging tar：三种模式部署前都把远端在跑实例的 config.yaml
# 复制进 staging——生产配置的权威副本是 live 配置，本地开发配置与 staging
# 里的旧快照都不会盖上生产。staging config 同时是 deploy.sh 预检（端口、
# api_key、token 来源）的读取对象，不刷新会让这些检查打到陈旧值上。
# Mac 端不保留仓库 clone：所有远端操作都落 staging，避免有人在 Mac 仓库里
# 直接 deploy.sh 部署了落后/分叉的历史。
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "${1:-}" != "--help" && "${1:-}" != "-h" ]]; then
  echo "deploy-remote.sh is RETIRED (2026-09-18): prod moved to archbox :3033." >&2
  echo "Use scripts/deploy-linux.sh on archbox. See file header." >&2
  exit 1
fi

# 远端路径表达式在远端 shell 内展开——默认值里的 $HOME 必须原样传到对端，
# 本地不提前展开、不用单引号包死（远端命令里一律放双引号内）。
HOST="${DEVIN2API_HOST:-fht-mba}"
STAGING="${DEVIN2API_STAGING:-\$HOME/.cache/devin-2api-staging}"
CONFIG_LIVE="${DEVIN2API_CONFIG_LIVE:-\$HOME/Library/Application Support/devin-2api/config.yaml}"
REF="origin/main"

if [[ -t 2 ]]; then
  _C_YEL=$'\033[33m' _C_RST=$'\033[0m'
else
  _C_YEL='' _C_RST=''
fi
warn() { echo "${_C_YEL}WARN${_C_RST} $*" >&2; }
die() {
  echo "ERROR $*" >&2
  exit 1
}

usage() {
  cat <<EOF
用法: $(basename "$0") [模式] [透传参数]
  （无参数）    worktree 部署：本地工作树（含未提交改动）推到远端 staging
              后源码构建——部署的就是开发机上看到的代码
  --ref <ref> 远端真实仓库 fetch + checkout --detach <ref>（默认 ${REF}）
              后源码部署——部署已推送的状态，本地未推送提交不含在内
  --release   远端 deploy.sh --release <tag|latest>：装预编译资产
  --check     并排对比生产（远端）与验证（本机）实例的 安装/运行/最新版本
  --uninstall 远端 deploy.sh --uninstall（保留远端 config.yaml 与 logs/）
  --no-restart 透传：只替换二进制，不重启服务
  --help      显示本说明

覆盖项（env）：DEVIN2API_HOST（默认 ${HOST}）/ DEVIN2API_STAGING
（远端暂存目录，默认 ${STAGING}，远端 shell 展开）/ DEVIN2API_CONFIG_LIVE
（远端 live config.yaml 路径，默认 ${CONFIG_LIVE}）。SSH 走 ~/.ssh/config 与
tailscale MagicDNS，要求免密（BatchMode）。
EOF
}

# shq <arg>：单引号包裹的 shell 安全引用，用于拼远端命令字符串。
shq() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\"'\"'/g")"; }

MODE=""
set_mode() {
  [[ -n "${MODE}" && "${MODE}" != "$1" ]] && die "模式冲突：${MODE} 与 $1 只能选一个"
  MODE="$1"
}
FWD=()
while [[ $# -gt 0 ]]; do
  case "$1" in
  --ref)
    set_mode ref
    REF="${2:?--ref 需要 git ref（如 origin/main、v0.10.1）}"
    shift 2
    ;;
  --release)
    set_mode release
    FWD+=(--release "${2:?--release 需要 tag 或 latest}")
    shift 2
    ;;
  --check)
    set_mode check
    shift
    ;;
  --uninstall)
    set_mode uninstall
    FWD+=(--uninstall)
    shift
    ;;
  --no-restart)
    FWD+=(--no-restart)
    shift
    ;;
  --help | -h)
    usage
    exit 0
    ;;
  *)
    die "unknown arg: $1（--help 查看用法；deploy.sh 参数集以外的参数不透传）"
    ;;
  esac
done
MODE="${MODE:-worktree}"

ssh -o BatchMode=yes -o ConnectTimeout=8 "${HOST}" true 2>/dev/null ||
  die "ssh ${HOST} 不可达——检查 tailscale 状态与免密登录（ssh ${HOST}）"
if [[ "${MODE}" != "worktree" ]]; then
  ssh -o BatchMode=yes "${HOST}" "test -d \"${STAGING}/.git\"" ||
    die "远端 staging 不存在：${HOST}:${STAGING}——先跑一次 worktree 部署落盘"
fi
# ref/release 都要把 live config 同步进 staging 再部署（见文件头说明）。
if [[ "${MODE}" == "ref" || "${MODE}" == "release" ]]; then
  ssh -o BatchMode=yes "${HOST}" "test -f \"${CONFIG_LIVE}\"" ||
    die "远端缺 live config.yaml（${HOST}:${CONFIG_LIVE}）"
fi

# 远端命令一律用 && 串联（对端登录 shell 是 fish，不能用 set -e/{ } 等
# bash 语法）；deploy.sh 显式以 bash 执行。
case "${MODE}" in
check)
  rc=0
  echo "== 生产实例（${HOST}）=="
  ssh -o BatchMode=yes "${HOST}" "cd \"${STAGING}\" && bash scripts/deploy.sh --check" || rc=1
  echo
  echo "== 验证实例（本机）=="
  if [[ "$(uname -s)" == "Linux" ]]; then
    bash scripts/deploy-linux.sh --check || rc=1
  else
    echo "（本机非 Linux，跳过验证实例检查）"
  fi
  exit "${rc}"
  ;;
uninstall)
  cmd="bash \"${STAGING}/scripts/deploy.sh\""
  for a in ${FWD[@]+"${FWD[@]}"}; do cmd+=" $(shq "$a")"; done
  exec ssh -o BatchMode=yes "${HOST}" "${cmd}"
  ;;
release)
  # 同 ref 分支：先把 staging config 刷成 live 副本，deploy.sh 的预检
  # （端口/api_key/token 来源）与 install_binary 都读它。
  cmd="cp \"${CONFIG_LIVE}\" \"${STAGING}/config.yaml\""
  cmd+=" && bash \"${STAGING}/scripts/deploy.sh\""
  for a in ${FWD[@]+"${FWD[@]}"}; do cmd+=" $(shq "$a")"; done
  exec ssh -o BatchMode=yes "${HOST}" "${cmd}"
  ;;
ref)
  # 提示但未推送不拦：ref 部署的语义本来就是「部署已推送状态」。
  if git fetch origin --quiet 2>/dev/null; then
    ahead="$(git rev-list --count origin/main..HEAD 2>/dev/null || echo 0)"
    [[ "${ahead}" != "0" ]] &&
      warn "本地领先 origin/main ${ahead} 个提交——远端将部署 ${REF}，不含未推送工作"
  else
    warn "本地 git fetch 失败——跳过未推送提交检查"
  fi
  cmd="git -C \"${STAGING}\" fetch origin --tags --quiet"
  cmd+=" && git -C \"${STAGING}\" checkout --detach -f $(shq "${REF}")"
  # checkout -f 不清未跟踪文件：此前 worktree 部署的残留（如他人未提交
  # WIP 的 .go）会混进包目录把源码构建带炸。ref 语义是「部署已推送
  # 状态」，故清干净；gitignore 项（staging config.yaml）不受影响。
  cmd+=" && git -C \"${STAGING}\" clean -fd"
  # staging config.yaml 是 gitignore 项，checkout/clean 都不碰——残留的是
  # 上一次部署时的快照。deploy.sh 预检（端口/api_key/token 来源）读它，
  # install_binary 又拿它同步 live；不先刷成 live 副本会把 live 侧的
  # 改动（如 devin.accounts）静默回滚成旧快照。
  cmd+=" && cp \"${CONFIG_LIVE}\" \"${STAGING}/config.yaml\""
  cmd+=" && bash \"${STAGING}/scripts/deploy.sh\""
  for a in ${FWD[@]+"${FWD[@]}"}; do cmd+=" $(shq "$a")"; done
  exec ssh -o BatchMode=yes "${HOST}" "${cmd}"
  ;;
worktree)
  echo "==> worktree deploy → ${HOST}（$(git describe --tags --always --dirty 2>/dev/null || echo '?')）"
  ssh -o BatchMode=yes "${HOST}" "test -f \"${CONFIG_LIVE}\"" ||
    die "远端缺 live config.yaml（${HOST}:${CONFIG_LIVE}）"
  # staging.new 重铺后原子换名：staging 是本脚本独占目录，可随时整个重铺；
  # .git 尾随进 tar（ls-files 不列目录，显式追加目录项 tar 会递归打包）。
  cmd="rm -rf \"${STAGING}.new\" && mkdir -p \"${STAGING}.new\""
  cmd+=" && tar -xf - -C \"${STAGING}.new\""
  cmd+=" && cp \"${CONFIG_LIVE}\" \"${STAGING}.new/config.yaml\""
  cmd+=" && rm -rf \"${STAGING}\" && mv \"${STAGING}.new\" \"${STAGING}\""
  cmd+=" && bash \"${STAGING}/scripts/deploy.sh\""
  for a in ${FWD[@]+"${FWD[@]}"}; do cmd+=" $(shq "$a")"; done
  { git ls-files -z --cached --others --exclude-standard && printf '.git\0'; } |
    tar --null --files-from=- -cf - |
    ssh -o BatchMode=yes "${HOST}" "${cmd}"
  ;;
esac
