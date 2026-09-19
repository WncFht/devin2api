# 基础配置

devin-2api 启动时读一份 YAML 文件。未知字段会被拒绝——拼错的 key 在启动时响亮失败，而不是静默不生效。

## 文件在哪

托管安装（一键脚本 / 部署脚本）把 `config.yaml` 放在平台配置目录并显式传 `-config`——见[布局表](/cn/installation/deploy-scripts#平台布局)。手动跑的二进制按 `-config` → `DEVIN2API_CONFIG` → `./config.yaml` → 平台默认解析。

## 最小文件

```yaml
server:
    listen: ":8080"

devin:
    base_url: "https://server.codeium.com"
    accounts:
        - name: "main"
          api_key: "cog_..."
    model: "swe-2-max"

debug:
    enabled: true

dashboard:
    password: "" # /web 登录；留空 = 开放
```

再少也行：`devin.accounts` 可以是空列表——空池起跑，事后在 `/web/accounts.html` 加号。

仓库里的 `config.example.yaml` 就是这份骨架，所有可选键都以注释形式列出并写明默认值——复制后按需取消注释，别凭记忆写 key。

## 改动生效

大多数字段热重载：改完文件后 `POST /admin/config/reload`（admin Bearer）。例外是 `server.listen`——要重启才生效。校验失败时旧配置继续服役：坏的新文件永不顶替最近一次的好配置。

面板改的设置（debug 开关、保留策略、账号编辑）以覆盖行存在 `devin-2api.db` 里，对文件值恒赢——每次 reload 后都会重放压回。

## 凭据卫生

下游 `/v1` 令牌永不入配置——只在面板管理。`config.yaml` 确实装上游凭据（`token`、`api_key`），别提交进 git；仓库已 gitignore 该文件，pre-commit 还跑 gitleaks 兜底。
