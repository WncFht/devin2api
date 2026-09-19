# 账号池

`devin.accounts` 声明上游账号池——每条 lane 对应一个 Devin 账号，自带凭据、速率闸门与配额追踪。单账号就是一条 lane 的池；空池也合法（先起服务，事后在面板加号——没有账号时 `/v1` 返回 `unavailable`）。

## 声明一条 lane

```yaml
devin:
    accounts:
        - name: "main"
          api_key: "cog_..."
          priority: 10
        - name: "backup"
          token: "devin-session-token$..."
          credentials_file: "/home/user/.local/share/devin/credentials.toml"
```

`name` 是 lane 在日志、闸门状态与面板里的身份：字母/数字/连字符/下划线 1–32 字符、池内唯一。可选逐号字段：`priority` 决定新会话选号次序（大的优先，默认 0）；`max_rpm` 覆盖该号自己的分钟窗口配额（0 继承 `devin.max_rpm`）。

两条目引用同一有效 token、同一 `credentials_file` 或同一 `api_key` 按配置错误拒绝。

## 凭据三来源

每条 lane 至少给一种，可叠加：

- **`api_key`（推荐）**——Devin 平台 durable key（`cog_...`），在 [app.devin.ai](https://app.devin.ai/) → Settings → API keys 手工签发。无内嵌寿命：上游判 `unauthenticated` 时 lane 用它现场铸新 session token，`api_key` 单源 lane 无限自愈。
- **`token`**——字面量 session token（`devin-session-token$...`）。直接可用，但寿命由服务端管——死了之后 lane 只能靠其它来源捞回。获取途径：`devin auth login` 后 `credentials.toml` 里的 `windsurf_api_key`（见下），或从 Devin 桌面应用本地状态提取。
- **`credentials_file`**——指向 Devin CLI 凭据文件（macOS/Linux 在 `~/.local/share/devin/credentials.toml`，Windows 在 `%APPDATA%\devin\credentials.toml`），里面是 CLI 自己续期的 session token——重读文件即自动跟随续期。`devin` CLI 随 [Devin 桌面应用](https://devin.ai/download)发布，在 `resources/app/extensions/windsurf/devin/bin/` 下——跑 `devin auth login` 完成浏览器登录即生成该文件。支持 `~/` 展开；相对路径锚定 config 文件所在目录，不是进程 CWD。

同 lane 多来源组合没问题——比如 `api_key` + `token`：字面量 token 先服役，死了再由 durable key 铸新。

上游报 `unauthenticated` 时 lane 按序重解析凭据——`credentials_file` 重读（跟随 CLI 续期）→ 行内/配置 token 若有变化 → `api_key` 铸新——除裸字面 token 外每种来源都能让代理免重启自愈。

## 请求怎么选 lane

- **会话亲和**——一段对话钉在同一条 lane 上，上游 prompt cache 才保得住。亲和键先取请求头（`X-Claude-Code-Session-Id`、`X-Session-ID`、`X-Session-Affinity`、`X-Conversation-Id`、`X-Thread-Id`），再取 body 字段（`metadata.user_id`、`prompt_cache_key`、`user`）。绑定走滑动 TTL（`devin.session_affinity_ttl_seconds`，默认 1 小时）。
- **Failover**——钉选的 lane 上游失败时换到更健康的兄弟号重试，客户端只看到一次请求。
- **配额降权**——lane 周配额跌破 `devin.quota_low_threshold_percent`（默认 15%）时，新会话把它排在健康 lane 之后；已绑定会话不动。

## 面板实时管理

`/web/accounts.html` 免重启增删改 lane——除文件路径外还可以 `credentials_content` 直接粘贴凭据内容，另有 `notes` 备注。面板编辑 config 声明过的 lane 会生成覆盖行（面板值恒赢）；面板删除 config 里的名字会留墓碑压住声明。
