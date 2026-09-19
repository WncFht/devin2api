# panel-verify — ccpanel 前端自包含验证套件

一个 `run.sh` 跑完整链路：每次现构建到临时目录（embed 资产防陈旧）→ 空闲端口起临时实例（临时 config + 独立 state dir）→ `checks/` 下的 playwright 检查 → SIGTERM 收尾。不依赖生产 config.yaml，临时配置用假上游凭据（面板检查不打上游，`devin.base_url`/`model` 仅为启动必填）。

```bash
cd scripts/panel-verify && ./run.sh          # 全量
node checks/nav.js                          # 单跑某条（需实例在跑，PV_BASE 指定地址）
./run.sh --port 3461 --keep                 # 固定端口 + 保留临时目录
./run.sh --base http://127.0.0.1:3461 --api-token testkey   # 打已在跑的实例
```

## --base 模式（对存量实例/生产跑冒烟）

`--base URL` 跳过构建与实例生命周期，`run.sh` 只 export `PV_*` 后跑 `checks/`——同一套断言直接打目标实例。`--admin-pw X`/`--api-token Y` 指定登录凭据（缺省仍是 testpw/testkey），密码面板必须传真实密码，否则所有 admin 上下文检查一起失败。

检查按目标实例形态降级：login.js 先用 `POST /login` + `GET /dashboard/session` 探令牌的有效角色——回 `api_token`（密码面板 + 受限令牌）才断言受限视图（4 项 nav、受限页重定向）；回 `admin`（开放面板，任何凭据都是 admin）改断言开放面板的正确结果（角色纠成 admin、满 8 项 nav）；令牌不被 `/login` 接受时整段 api_token 检查记 `skip` 而非 FAIL。nav/columns/mobile/console 四条与实例形态无关，照常跑。

注意：--base 模式只读页面、不写状态，但 columns.js 会点列显隐菜单（写浏览器 localStorage，不碰服务端），对生产无副作用。

## 前置条件

- node >= 20、go、bash、curl。
- 首次运行自动 `npm install` 与 `npx playwright install firefox`；离线或要控版本时先手动装好。
- 浏览器默认落 `~/.cache/ms-playwright`；`/tmp` 小或 archbox tmpfs 配额紧时设 `PLAYWRIGHT_BROWSERS_PATH=/somewhere/roomy`（已装好的也要重下到新路径），npm 缓存目录同理可用 `TMPDIR` 换。

## 检查清单

| 脚本                    | 断言                                                                                                                                                                   |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `checks/login.js`       | UI 表单 admin 登录落面板、8 项 nav；api_token 登录按探测到的有效角色断言——密码面板断受限视图（4 项 nav、受限页重定向），开放面板断角色纠成 admin（见 --base 模式）     |
| `checks/nav.js`         | 9 组「宽度×语言」下 nav 链接无裁切、文档无横向溢出（含 768/767 边界）                                                                                                  |
| `checks/columns.js`     | logs 列显隐：开合、隐藏生效、外部点击关闭、刷新持久化；`/dashboard/session` 拖慢 3s 的死窗内按钮已绑定可点开（`initLogsPageActions` 在模块顶层挂委托，不等 bootstrap） |
| `checks/mobile.js`      | 375/320 视口 logs 页：无横向溢出、列显隐按钮完整在视口内、可点开                                                                                                       |
| `checks/console.js`     | 8 页零 console error / pageerror / 4xx+ 响应                                                                                                                           |
| `checks/bucket-sec.js`  | `/dashboard/metrics` 秒级桶宽：X-Bucket-Sec 头回传与点数封顶（下限 10、缺省 600、上限 1d）；粒度下拉选 10 秒后请求带 `bucket_sec=10`、间隔片显示秒级                   |
| `checks/align-shots.js` | 7 页 × 1440/1024 两档视口 + 390 视口 3 页截图落 `shots/align/` 供人工目检，不断言                                                                                      |

约定：每条断言一行 `ok`/`FAIL`，截图只在失败时写 `shots/`（gitignore）；检查脚本进程退出码即结果，run.sh 汇总计数。所有检查经 `PV_BASE`/`PV_ADMIN_PW`/`PV_API_TOKEN`/`PV_SHOTS` 拿环境，由 run.sh 注入。

## 踩过的坑

- `require('playwright')` 按脚本所在目录向上找 node_modules，与 cwd 无关——在本目录装好依赖后，从仓库任何地方 `node scripts/panel-verify/checks/xx.js` 都能解析。
- 清理临时实例只 `kill $PID`：仓库约定里 `pkill -f <pattern>` 会匹配发起者自己的命令行，整条 shell 被杀。
- 开放面板（`dashboard.password: ""`）下任何凭据都是 admin；要测 api_token 受限视图必须给非空密码且令牌与密码不同值（本套件 testpw/testkey）。POST /login 的 role 字段在开放面板下也回 api_token，有效角色要看 `/dashboard/session`——login.js 的探测就按这个口径。
- `boundKey` 落在 `document.body.dataset`（`logsPageActionsBound === '1'`），不是 `window.*`——早期探测脚本查错位置会得到常 false。
- 面板渲染入口是 `window.initPageBootstrap`：`/dashboard/session` 先回角色才渲染 topnav，慢会话期间 nav 晚出现属预期；但 `data-page-filters` 容器与列显隐委托绑定都在模块顶层，不依赖 session。
