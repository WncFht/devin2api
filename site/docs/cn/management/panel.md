# 管理面板

`/web` 面板是唯一管理面——请求浏览、用量、配额、令牌、号池 lane、模型注册表与设置都集中在这里，也是下游 `/v1` 令牌唯一存在的地方。

## 登录

`config.yaml` 的 `dashboard.password` 是管理密码。托管安装首跑生成随机密码——安装时打印，也写在 `config.yaml` 里。留空则无需登录：绑回环自用没问题，一旦 `server.listen` 绑到真实网卡就必须设。

全程 Bearer 无 cookie：`Authorization: Bearer <password>` 可直接访问 `/admin/*` 与 `/dashboard/*` 端点；`POST /login` 给面板 UI 发 token。下游令牌也能登录，身份是只读 `api_token`，数据范围收敛到自己名下的行。

## 页面

| 页面                 | 作用                                                                |
| -------------------- | ------------------------------------------------------------------- |
| `/web/index.html`    | 请求浏览——逐请求状态、延迟、token、错误阶段；可钻取调试 payload     |
| `/web/tokens.html`   | 下游令牌管理——创建/吊销，逐令牌 RPM/并发/费用窗口限额，`fg`/`bg` 类 |
| `/web/accounts.html` | 号池 lane——增删改账号、逐号健康与配额、failover 归因                |
| `/web/settings.html` | debug 开关、保留策略，以及托管安装的版本更新卡                      |

## 下游令牌

客户端凭这里创建的令牌访问 `/v1`。明文创建时一次性出示，仓内只存哈希。令牌可带逐项限额（RPM、并发、费用窗口、模型白名单）与 `class`——`fg`（默认）给交互流量，`bg` 给能忍受更长闸内排队的无人值守批跑。

::: warning 空仓 = 开放访问
令牌仓没有行时 `/v1` 放行一切请求。`server.listen` 绑到非回环地址前请先建至少一个令牌——否则等于把你的 Devin 配额发给整个网络。
:::

## 模型注册表

注册表控制 `/v1/models` 与请求路由对每个名字的行为：停用某模型（请求得 `model_disabled`），或把一个名字重定向到另一个（先于 `devin.aliases` 解析）。

## 自更新

托管安装在 `/web/settings.html` 有版本更新卡：检查新版本 → 下载 → sha256 校验 → 换二进制 → 跨零停机交接重启，进度轮询无缝跨过进程切换。见[升级](/cn/installation/upgrading)。

## 脚本化访问

`/admin/*` 端点认 admin Bearer 供自动化：`/admin/logs`（可过滤的请求行 + 导出）、`/admin/runtime-metrics`（闸门/号池/进程状态）、`/admin/config`（脱敏生效配置）与 `/admin/config/reload`、`/admin/update*`、`/admin/debug-logs/{id}`（逐请求 payload）。`GET /admin/api` 返回端点目录。
