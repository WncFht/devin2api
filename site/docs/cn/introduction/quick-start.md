# 快速开始

四步从零到可用的 `/v1` 端点。这条路径覆盖 Linux 与 macOS 的托管安装；Windows 请走[部署脚本](/cn/installation/deploy-scripts)或[预编译二进制](/cn/installation/prebuilt-binary)。

## 前置条件

- 一个你获准这样使用的 Devin 账号（[app.devin.ai](https://app.devin.ai/)）。
- Linux 或 macOS。一键脚本免 root、免 clone。

## 1. 拿一个凭据

每个上游账号需要一个凭据。最省心的选择是 durable Devin 平台 key：

- **`api_key`（推荐）**——在 [app.devin.ai](https://app.devin.ai/) → Settings → API keys 签发的 `cog_...` key。它没有内嵌寿命：上游报 `unauthenticated` 时 lane 会用它现场铸新的 session token，`api_key` 单源账号可以无限自愈。

其它来源——字面量 `devin-session-token$...` 或 Devin CLI 的 `credentials.toml` 文件——见[账号池](/cn/configuration/account-pool)。

## 2. 安装

```bash
curl -sSL https://raw.githubusercontent.com/WncFht/devin2api/main/scripts/install.sh | bash
```

一条命令把二进制装进 `~/.local/bin`、配置与状态装进平台目录，并在 `systemd --user`（Linux）或 launchd（macOS）下拉起服务。首跑自动生成 `config.yaml`（含随机 `dashboard.password`）并提示粘贴凭据——粘贴 `cog_...` key，或直接回车以空池起跑、事后在面板加号。

细节与子命令（`upgrade`、`rollback`、`status`、`uninstall`）见[一键脚本](/cn/installation/install-script)。

## 3. 创建下游令牌

客户端凭面板创建的令牌访问 `/v1`——配置里没有数据面凭据。打开 `http://localhost:8080/web/tokens.html`，用生成的 `dashboard.password` 登录（安装时打印过，也在 `config.yaml` 里），创建令牌。明文只出示一次，仓内只存哈希。

如果刚才跳过了凭据提示，到 `/web/accounts.html` 添加 Devin 账号——不用重启。

## 4. 指向你的客户端

Claude Code 示例（`~/.claude/settings.json`）：

```json
{
    "env": {
        "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080",
        "ANTHROPIC_AUTH_TOKEN": "<下游令牌>",
        "ANTHROPIC_MODEL": "swe-2-max",
        "ANTHROPIC_SMALL_FAST_MODEL": "swe-2-max",
        "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "262000",
        "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "230000"
    }
}
```

各客户端页面：[Claude Code](/cn/clients/claude-code)、[Codex](/cn/clients/codex)、[其他客户端](/cn/clients/other-clients)。

## 验证

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"...","uptime_seconds":12,"debug_logging":false}

curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer <下游令牌>" \
  -H "Content-Type: application/json" \
  -d '{"model": "swe-2-max", "input": "Hello"}'
```

出问题的话，每个 `/v1` 响应都带一个 `X-Request-Id` 指向它的调试记录——见[排障](/cn/troubleshooting)。
