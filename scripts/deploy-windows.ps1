# deploy-windows.ps1 — deploy.sh / deploy-linux.sh 的 Windows 对应物。
# Windows 不做服务化（docs/deployment.md）：实例在独立控制台窗口前台跑，
# Ctrl+C 优雅排空；关窗与 Stop-Process/taskkill 都是强杀，会掐断在途请求。
#
# 用法: scripts\deploy-windows.ps1 [-Release <tag|latest>] [-NoStart] [-Check] [-Uninstall] [-Force] [-Help]
#   -Release    安装 GitHub Release 预编译 zip（sha256 校验）；缺省为源码构建（需 go+git）
#   -NoStart    只安装，不启动实例
#   -Check      只对比 已安装/运行中/最新 release 版本，不做变更
#   -Uninstall  移除 exe 等安装产物（保留 config.yaml 与 logs\）
#   -Force      允许强杀正在运行的实例（等价关窗，在途请求会断；否则提示手动 Ctrl+C）
#   -RuntimeDir 覆盖 exe 安装目录（默认 %LOCALAPPDATA%\Programs\devin-2api）
#   -Help       显示用法
#
# 首装与升级同一条命令：config.yaml 缺失时自动从 config.example.yaml 生成——
# 写入随机 auth.api_key / dashboard.password，listen 绑 127.0.0.1 自选空闲
# 端口（避免 Windows 防火墙弹窗与裸暴露），token 提示粘贴或留空走自动发现。
# 注意：经 SSH 远程执行时，启动的实例会随会话结束被系统回收（job object）
# ——本脚本面向本机交互会话使用。
[CmdletBinding()]
param(
    [string]$Release = "",
    [string]$RuntimeDir = "",
    [switch]$NoStart,
    [switch]$Check,
    [switch]$Uninstall,
    [switch]$Force,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0
$ProgressPreference = 'SilentlyContinue'   # IWR 进度条在非交互会话又慢又吵

# ---------- 输出 ----------
function Note([string]$m) { Write-Host "==> $m" }
function Warn([string]$m) { Write-Host "WARN $m" -ForegroundColor Yellow }
function Die([string]$m) { Write-Host "ERROR $m" -ForegroundColor Red; exit 1 }
function Usage { Get-Content $PSCommandPath -TotalCount 20 | Where-Object { $_ -match '^#' } | ForEach-Object { $_.TrimStart('# ').TrimStart('#') } }
if ($Help) { Usage; exit 0 }

# ---------- 布局 ----------
# 平台规范布局（Microsoft 分法）：exe 入 %LOCALAPPDATA%\Programs\devin-2api
# （per-user Program Files），config.yaml 入 %APPDATA%\devin-2api（roaming
# 配置随账号走），logs\ 与状态文件入 %LOCALAPPDATA%\devin-2api（machine-local
# 输出）。仓库内运行（scripts\ 下）时仓库 config.yaml 是权威副本、由部署
# 同步进 ConfigDir；单文件下载运行时直接生成。env 覆盖：
# DEVIN2API_CONFIG_DIR / DEVIN2API_STATE_DIR；DEVIN2API_RUNTIME 是旧版单
# 目录变量的兼容别名，映射到 StateDir。
$ScriptDir = Split-Path -Parent $PSCommandPath
$RepoRoot = Split-Path -Parent $ScriptDir
$InRepo = Test-Path (Join-Path $RepoRoot 'go.mod')
if ($RuntimeDir -eq '') { $RuntimeDir = Join-Path $env:LOCALAPPDATA 'Programs\devin-2api' }
$ConfigDir = if ($env:DEVIN2API_CONFIG_DIR) { $env:DEVIN2API_CONFIG_DIR } else { Join-Path $env:APPDATA 'devin-2api' }
$StateDir = if ($env:DEVIN2API_STATE_DIR) { $env:DEVIN2API_STATE_DIR } elseif ($env:DEVIN2API_RUNTIME) { $env:DEVIN2API_RUNTIME } else { Join-Path $env:LOCALAPPDATA 'devin-2api' }
$RuntimeExe = Join-Path $RuntimeDir 'devin-2api.exe'
$RuntimeConfig = Join-Path $ConfigDir 'config.yaml'
$RepoConfig = Join-Path $RepoRoot 'config.yaml'
$UpstreamSlug = 'WncFht/devin2api'

if ($env:PROCESSOR_ARCHITECTURE -eq 'AMD64') { $AssetName = 'devin-2api-windows-amd64.zip' }
elseif ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { $AssetName = 'devin-2api-windows-arm64.zip' }
else { Die "不支持的架构: $env:PROCESSOR_ARCHITECTURE" }

# ---------- 小工具 ----------
# Invoke-NativeQuiet { & exe args }：PS5.1 在 EAP=Stop 下会把原生命令的
# stderr 写成 NativeCommandError 终止错误——包装内临时放宽为 Continue，
# stderr 照常显示但不打断脚本；返回 stdout。
function Invoke-NativeQuiet([scriptblock]$sb) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { & $sb }
    catch { }
    finally { $ErrorActionPreference = $prev }
}

function Get-GhToken {
    if ($env:GH_TOKEN) { return $env:GH_TOKEN }
    if (Get-Command gh -ErrorAction SilentlyContinue) {
        $t = Invoke-NativeQuiet { & gh auth token 2>$null }; if ($t) { return $t }
    }
    if (Get-Command git -ErrorAction SilentlyContinue) {
        $out = "protocol=https`nhost=github.com`n" | & cmd /c 'git credential fill 2>nul'
        foreach ($l in $out) { if ($l -match '^password=(.+)') { return $Matches[1] } }
    }
    return ""
}
$script:GhHeaders = @{}
$script:GhToken = Get-GhToken
if ($script:GhToken) { $script:GhHeaders['Authorization'] = "Bearer $script:GhToken" }

# release_slugs：origin（GitHub 地址）在前，上游兜底——fork 无资产时落到上游。
function Get-ReleaseSlugs {
    $slugs = @()
    if (Get-Command git -ErrorAction SilentlyContinue) {
        $origin = Invoke-NativeQuiet { & git -C $RepoRoot remote get-url origin 2>$null }
        if ($origin -match 'github\.com[:/]([^/]+/[^/.]+?)(\.git)?$') { $slugs += $Matches[1] }
    }
    if ($slugs -notcontains $UpstreamSlug) { $slugs += $UpstreamSlug }
    return $slugs
}

function Get-LatestTag([string]$slug) {
    # /releases/latest 的 302 解析不吃 REST API 匿名限流（60/h/IP）；
    # 失败再退到 REST API（有 token 时不受匿名额度影响）。
    try {
        $req = [System.Net.HttpWebRequest]::Create("https://github.com/$slug/releases/latest")
        $req.AllowAutoRedirect = $false
        $req.Timeout = 10000
        $req.UserAgent = 'devin-2api-deploy'
        $resp = $req.GetResponse()
        $loc = $resp.Headers['Location']
        $resp.Close()
        if ($loc -match '/releases/tag/([^/\s]+)') { return $Matches[1] }
    }
    catch { }
    try { return (Invoke-RestMethod -Uri "https://api.github.com/repos/$slug/releases/latest" -Headers $script:GhHeaders -TimeoutSec 10).tag_name }
    catch { return "" }
}

# yaml_scalar：取首个 "key: value"（去单/双引号、截断行内空格/注释）。
function Get-YamlScalar([string]$key, [string]$file) {
    if (-not (Test-Path $file)) { return "" }
    $pat = '^\s*' + [regex]::Escape($key) + '\s*:\s*[''"]?([^''"# ]*)'
    foreach ($line in Get-Content $file) {
        if ($line -match $pat) { return $Matches[1] }
    }
    return ""
}

# Set-YamlScalar：重写首个 key 行为 key: "<value>"（冒号后空格是 YAML 语法要求）。
function Set-YamlScalar([string]$key, [string]$value, [string]$file) {
    $pat = '^(\s*)' + [regex]::Escape($key) + ':'
    $lines = [System.IO.File]::ReadAllLines($file)
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match $pat) {
            $lines[$i] = $Matches[1] + $key + ': "' + ($value -replace '"', '\"') + '"'
            break
        }
    }
    [System.IO.File]::WriteAllLines($file, $lines)
}

function New-Secret { -join ((1..32) | ForEach-Object { '{0:x}' -f (Get-Random -Maximum 16) }) }

# ---------- 端口 / 进程 ----------
function Test-PortOccupied([int]$p) {
    try {
        $c = New-Object System.Net.Sockets.TcpClient
        $c.Connect('127.0.0.1', $p); $c.Close(); return $true
    }
    catch { return $false }
}
function Get-HealthzVersion([int]$p) {
    try { return (Invoke-RestMethod -Uri "http://localhost:$p/healthz" -TimeoutSec 2).version }
    catch { return "" }
}
function Find-FreePort([int]$from) {
    for ($p = $from; $p -lt $from + 50; $p++) { if (-not (Test-PortOccupied $p)) { return $p } }
    Die "在 $from 起 50 个端口内没找到空闲端口"
}
# 本目录实例 = 路径精确匹配；其它路径的 devin-2api 是 stray（会抢端口分流）。
function Get-DirProcess { @(Get-Process -Name 'devin-2api' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $RuntimeExe }) }
function Get-StrayProcess { @(Get-Process -Name 'devin-2api' -ErrorAction SilentlyContinue | Where-Object { $_.Path -ne $RuntimeExe }) }

function Stop-DirInstance {
    # 注意 @() 必须在调用侧包：函数输出枚举化后空结果是 $null，
    # StrictMode 下 $null.Count 会炸。
    $procs = @(Get-DirProcess)
    if ($procs.Count -eq 0) { return }
    if (-not $Force) {
        Die "实例正在运行（pid $($procs[0].Id)）：请到它的控制台窗口按 Ctrl+C 停掉后重跑；要由脚本强杀加 -Force（在途请求会断）"
    }
    Warn "强杀运行中的实例（pid $($procs[0].Id)）——在途请求会被掐断"
    $procs | Stop-Process -Force
    Start-Sleep -Seconds 1
}

function Read-ListenPort([string]$file) {
    $listen = Get-YamlScalar 'listen' $file
    if ($listen -match ':(\d{1,5})$') { return [int]$Matches[1] }
    return 0
}

# ---------- 配置引导 ----------
# token 发现链与 config.go 一致：配置值 → env → credentials.toml（Windows 双路径）。
function Get-TokenSource([string]$file) {
    if ((Get-YamlScalar 'token' $file) -ne '') { return 'config.yaml devin.token' }
    if ($env:DEVIN_TOKEN) { return 'env DEVIN_TOKEN' }
    if ($env:WINDSURF_API_KEY) { return 'env WINDSURF_API_KEY' }
    if (Test-Path (Join-Path $env:APPDATA 'devin\credentials.toml')) { return '%APPDATA%\devin\credentials.toml' }
    if (Test-Path (Join-Path $env:LOCALAPPDATA 'devin\credentials.toml')) { return '%LOCALAPPDATA%\devin\credentials.toml' }
    return ""
}

# Move-LegacyLayout：旧版把 config.yaml 与 logs\ 放在 exe 同目录——迁到
# ConfigDir/StateDir。logs 逐项并入：目标缺名直接搬，同名 .jsonl 属追加
# 日志把旧尾部接上，其余同名冲突不覆盖，残留目录留给用户确认。
function Move-LegacyLayout {
    $legacyConfig = Join-Path $RuntimeDir 'config.yaml'
    if ((Test-Path $legacyConfig) -and ($legacyConfig -ne $RuntimeConfig) -and -not (Test-Path $RuntimeConfig)) {
        New-Item -ItemType Directory -Force -Path $ConfigDir | Out-Null
        Move-Item $legacyConfig $RuntimeConfig
        Note "迁移 $legacyConfig → $ConfigDir\"
    }
    $legacyLogs = Join-Path $RuntimeDir 'logs'
    if (Test-Path $legacyLogs) {
        $targetLogs = Join-Path $StateDir 'logs'
        New-Item -ItemType Directory -Force -Path $targetLogs | Out-Null
        foreach ($item in Get-ChildItem $legacyLogs) {
            $dest = Join-Path $targetLogs $item.Name
            if (-not (Test-Path $dest)) {
                Move-Item $item.FullName $dest
            } elseif (-not $item.PSIsContainer -and $item.Name -like '*.jsonl') {
                Get-Content $item.FullName | Add-Content $dest
                Remove-Item $item.FullName
                Note "合并 $legacyLogs\$($item.Name) 尾部 → $targetLogs\"
            }
        }
        Remove-Item $legacyLogs -ErrorAction SilentlyContinue
    }
}

function Ensure-Config {
    New-Item -ItemType Directory -Force -Path $ConfigDir | Out-Null
    # 仓库模式：仓库 config.yaml 是权威副本，直接同步（与 bash 版语义一致）。
    if ($InRepo -and (Test-Path $RepoConfig)) {
        if ((Test-Path $RuntimeConfig) -and
            (Get-FileHash $RepoConfig).Hash -ne (Get-FileHash $RuntimeConfig).Hash) {
            Warn "config.yaml 与配置目录不一致，以仓库版本覆盖（权威副本在仓库）"
        }
        Copy-Item $RepoConfig $RuntimeConfig -Force
        return
    }
    if (Test-Path $RuntimeConfig) { return }

    $example = Join-Path $RepoRoot 'config.example.yaml'
    if (-not (Test-Path $example)) { $example = Join-Path $RuntimeDir 'config.example.yaml' }
    if (-not (Test-Path $example)) { Die "找不到 config.example.yaml（仓库根或安装目录下都没有）" }

    Note "first install: 从 config.example.yaml 生成 config.yaml"
    Copy-Item $example $RuntimeConfig
    Set-YamlScalar 'api_key' (New-Secret) $RuntimeConfig
    Set-YamlScalar 'password' (New-Secret) $RuntimeConfig

    # 示例 listen :8080 是全网卡绑定——首装改成 127.0.0.1 + 空闲端口，
    # 既不触发 Windows 防火墙弹窗也不裸暴露；要 LAN 访问改 config.yaml 即可。
    $port = Find-FreePort 8080
    Set-YamlScalar 'listen' "127.0.0.1:$port" $RuntimeConfig

    $token = ""
    try { $token = Read-Host "Devin session token（devin-session-token`$...，留空走自动发现）" } catch { }
    if ($token) { Set-YamlScalar 'token' $token $RuntimeConfig }
}

function Assert-Preflight {
    # 依赖：release 路径只要 PowerShell；源码构建另需 git+go。
    if ($Release -eq '') {
        if (-not (Get-Command git -ErrorAction SilentlyContinue)) { Die "源码构建需要 git；也可用 -Release latest 免构建" }
        if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Die "源码构建需要 go 工具链；也可用 -Release latest 免构建" }
    }
    Ensure-Config

    if (Select-String -Path $RuntimeConfig -Pattern 'devin-session-token\$mock-token' -Quiet) {
        Die "devin.token 仍是模板占位值（mock-token）：请编辑 $RuntimeConfig 填真实 token，或置空走自动发现"
    }
    $src = Get-TokenSource $RuntimeConfig
    if ($src -eq '') {
        Warn "未发现 token 来源（config/env/credentials.toml 均无）"
        Warn "空 token 启动的实例 /v1/* 不可用且不自愈——可执行 Windsurf 内置 CLI 生成凭证："
        Warn '  & "C:\Program Files\Windsurf\resources\app\extensions\windsurf\devin\bin\devin.exe" auth login'
        Warn "  （会弹浏览器登录 Devin 账号）然后把实例重启"
    }
    else { Note "token 来源: $src" }

    $listen = Get-YamlScalar 'listen' $RuntimeConfig
    if ($listen -notmatch '^(127\.|localhost:|\[::1\])' -and (Get-YamlScalar 'api_key' $RuntimeConfig) -eq '') {
        Warn "server.listen 非回环且 auth.api_key 为空——等于把配额开放给网络，请先配置 auth.api_key"
    }
}

# 端口占用判别：占用者能回 healthz 版本 → devin-2api；本目录实例归升级路径，
# 其它占用一律拦下（外来进程或 stray）。
function Assert-PortAvailable([int]$p) {
    if (-not (Test-PortOccupied $p)) { return }
    $v = Get-HealthzVersion $p
    if ($v -ne '') {
        if (@(Get-DirProcess).Count -gt 0) { return }
        Die "端口 $p 已被一个非本目录的 devin-2api 实例占用（$v）——先停掉它（见上方 stray 列表）"
    }
    Die "端口 $p 被非 devin-2api 进程占用——改 config.yaml 的 server.listen 或先释放端口"
}

# ---------- 安装 / 校验 ----------
function Install-Binary {
    New-Item -ItemType Directory -Force -Path $RuntimeDir, $ConfigDir, (Join-Path $StateDir 'logs') | Out-Null
    Stop-DirInstance   # Windows 锁运行中的 exe——覆盖前必须让位

    if ($Release -ne '') {
        $version = ''
        foreach ($slug in Get-ReleaseSlugs) {
            $tag = $Release
            if ($Release -eq 'latest') {
                $tag = Get-LatestTag $slug
                if ($tag -eq '') { continue }
            }
            $zip = Join-Path $env:TEMP "devin-2api-$tag.zip"
            try {
                Note "download $AssetName @ $tag ($slug)"
                # -UseBasicParsing：PS5.1 的 IWR 默认调 IE 引擎解析 HTML，
                # 无 IE 环境的机器直接报错；纯下载不需要 DOM。
                Invoke-WebRequest -Uri "https://github.com/$slug/releases/download/$tag/$AssetName" -OutFile $zip -Headers $script:GhHeaders -UseBasicParsing | Out-Null
                $raw = (Invoke-WebRequest -Uri "https://github.com/$slug/releases/download/$tag/checksums.txt" -Headers $script:GhHeaders -UseBasicParsing).Content
                # octet-stream 响应的 .Content 是 byte[] 而非 string——先解码。
                $sums = if ($raw -is [byte[]]) { [System.Text.Encoding]::UTF8.GetString($raw) } else { [string]$raw }
                $expected = (($sums -split "`n") | Where-Object { $_ -match "\s$([regex]::Escape($AssetName))\s*$" } | ForEach-Object { ($_ -split '\s+')[0] } | Select-Object -First 1)
                $actual = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
                if ($expected -and $expected -eq $actual) {
                    Expand-Archive $zip -DestinationPath $RuntimeDir -Force
                    $version = $tag; break
                }
                Warn "sha256 mismatch ($slug，expected $expected got $actual)"
            }
            catch { Warn "download failed ($slug)：$($_.Exception.Message)" }
            finally { Remove-Item $zip -Force -ErrorAction SilentlyContinue }
        }
        if ($version -eq '') { Die "release $Release 解析或下载失败（tag 不存在、无该平台资产或网络中断）" }
    }
    else {
        $version = Invoke-NativeQuiet { & git -C $RepoRoot describe --tags --always --dirty 2>$null }
        if (-not $version) { Die "git describe 失败——非 git 环境请用 -Release latest" }
        Note "build devin-2api $version"
        Push-Location $RepoRoot
        try { Invoke-NativeQuiet { & go build -ldflags "-X main.version=$version" -o (Join-Path $RuntimeDir 'devin-2api.exe') ./cmd/devin-2api } }
        finally { Pop-Location }
        if ($LASTEXITCODE -ne 0) { Die "go build 失败" }
    }

    $smoke = Invoke-NativeQuiet { & $RuntimeExe -version 2>$null }
    if ($smoke -ne $version) { Die "version mismatch in new binary（got '$smoke' want '$version'）" }
    Note "installed $RuntimeExe ($version)"
    return $version
}

# ---------- 冒烟 ----------
function Test-Upstream([int]$p) {
    $headers = @{}
    $key = Get-YamlScalar 'api_key' $RuntimeConfig
    if ($key) { $headers['X-Api-Key'] = $key }
    # 首调上游目录可能冷启动慢，超时重试一次再判失败。
    $code = ''
    foreach ($try in 1..2) {
        try { $code = [int](Invoke-WebRequest -Uri "http://localhost:$p/v1/models" -Headers $headers -TimeoutSec 30 -UseBasicParsing).StatusCode }
        catch {
            try { if ($_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode } } catch { }
        }
        if ($code -ne '') { break }
    }
    if ($code -eq 200) { Note "upstream auth verified (GET /v1/models 200)"; return $true }
    if ($code -eq 401 -or $code -eq 403) {
        Warn "服务已运行但 /v1/models 返回 HTTP $code——客户端 api_key 不匹配或上游 token 无效"
    }
    else { Warn "服务已运行但 /v1/models 返回 HTTP $(if ($code) { $code } else { '<timeout>' })——上游链路未通过" }
    $src = Get-TokenSource $RuntimeConfig
    if ($src -eq '') { Warn "未配置 token：见 README「提供 Devin token」；空 token 启动的实例配置后须重启" }
    else { Warn "token 来源 $src——可能已过期；排障看 logs\index.jsonl 与 /panel" }
    return $false
}

function Write-Summary([string]$version, [int]$p) {
    Write-Host @"
==> deployed $version
    exe      : $RuntimeExe
    配置     : $RuntimeConfig
    状态/日志: $StateDir\logs
    监听     : http://localhost:$p（面板 /panel，凭据见 config.yaml）
    运行方式 : 独立控制台窗口前台跑——停止在窗口里 Ctrl+C；关窗是强杀会掐断在途请求
    常驻     : Windows 不做服务化；要开机自起可用任务计划程序或 NSSM（见 docs/deployment.md）
"@
}

# ---------- 主流程 ----------
New-Item -ItemType Directory -Force -Path $RuntimeDir | Out-Null

$exeVersion = if (Test-Path $RuntimeExe) { Invoke-NativeQuiet { & $RuntimeExe -version 2>$null } } else { '<未安装>' }
$runningVersion = ""
$configPort = Read-ListenPort $RuntimeConfig
if ($configPort -le 0) { $configPort = Read-ListenPort (Join-Path $RuntimeDir 'config.yaml') }   # 旧布局兜底
$probePort = if ($configPort -gt 0) { $configPort } else { 8080 }
if (Test-PortOccupied $probePort) { $runningVersion = Get-HealthzVersion $probePort }

foreach ($s in @(Get-StrayProcess)) {
    Warn "非本目录的 devin-2api 实例（单实例约定，会抢端口分流请求）: pid $($s.Id) $($s.Path)"
}

if ($Check) {
    $latest = ''; foreach ($slug in Get-ReleaseSlugs) { $latest = Get-LatestTag $slug; if ($latest) { break } }
    if (-not $latest) { $latest = '<查询失败>' }
    Write-Host "installed: $exeVersion`nrunning:   $(if ($runningVersion) { $runningVersion } else { '<未运行>' })`nlatest:    $latest"
    exit ([int]($exeVersion -ne $latest))
}

if ($Uninstall) {
    Stop-DirInstance
    foreach ($f in 'devin-2api.exe', 'config.example.yaml', 'LICENSE') {
        $p = Join-Path $RuntimeDir $f
        if (Test-Path $p) { Remove-Item $p -Force; Note "removed $p" }
    }
    if ((Test-Path $RuntimeConfig) -or (Test-Path (Join-Path $StateDir 'logs'))) {
        Write-Host "    保留 $RuntimeConfig 与 $StateDir\logs\；彻底清理: Remove-Item -Recurse '$ConfigDir' '$StateDir' '$RuntimeDir'"
    }
    if ((Test-Path (Join-Path $RuntimeDir 'config.yaml')) -or (Test-Path (Join-Path $RuntimeDir 'logs'))) {
        Write-Host "    旧布局残留：$RuntimeDir 下仍有 config.yaml/logs\——下次部署会自动迁移"
    }
    exit 0
}

Move-LegacyLayout
Assert-Preflight
$port = Read-ListenPort $RuntimeConfig
if ($port -le 0) { Die "config.yaml 的 server.listen 解析不出端口" }
Assert-PortAvailable $port

$version = Install-Binary

if ($NoStart) { Write-Host "done (installed, not started)"; exit 0 }

# 独立控制台窗口启动——窗口归用户所有，Ctrl+C 走优雅排空。
Note "start: $RuntimeExe（新控制台窗口）"
Start-Process -FilePath $RuntimeExe -ArgumentList '-config', "`"$RuntimeConfig`"", '-state-dir', "`"$StateDir`"" -WorkingDirectory $StateDir

# 无服务管理器可委托，进程起没起来只能看 healthz；30s 足够覆盖慢启动。
$running = ''
foreach ($i in 1..60) {
    Start-Sleep -Milliseconds 500
    $running = Get-HealthzVersion $port
    if ($running -eq $version) { break }
}
if ($running -ne $version) {
    Warn "healthz 未出现版本 $version（last=$(if ($running) { $running } else { '<none>' })）"
    Warn "看新控制台窗口里的报错输出；常见原因：SmartScreen/杀毒拦了 exe、端口被抢、config.yaml 语法错"
    exit 1
}
Note "running: version=$running"

$ok = Test-Upstream $port
Write-Summary $running $port
exit ([int](-not $ok))
