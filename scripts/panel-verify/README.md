# panel-verify — ccpanel 前端自包含验证套件

一个 `run.sh` 跑完整链路：构建 `./devin-2api`（缺失时）→ 空闲端口起临时实例（临时 config + 独立 state dir）→ `checks/` 下的 playwright 检查 → SIGTERM 收尾。不依赖生产 config.yaml，临时配置用假上游凭据（面板检查不打上游，`devin.base_url`/`model` 仅为启动必填）。

```bash
cd scripts/panel-verify && ./run.sh          # 全量
node checks/nav.js                          # 单跑某条（需实例在跑，PV_BASE 指定地址）
./run.sh --port 3461 --keep                 # 固定端口 + 保留临时目录
```

## 前置条件

- node >= 20、go、bash、curl。
- 首次运行自动 `npm install` 与 `npx playwright install firefox`；离线或要控版本时先手动装好。
- 浏览器默认落 `~/.cache/ms-playwright`；`/tmp` 小或 archbox tmpfs 配额紧时设 `PLAYWRIGHT_BROWSERS_PATH=/somewhere/roomy`（已装好的也要重下到新路径），npm 缓存目录同理可用 `TMPDIR` 换。

## 检查清单

| 脚本                | 断言                                                                                                                                                                   |
| ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `checks/login.js`   | UI 表单 admin 登录落面板、8 项 nav；api_token 登录得受限角色（4 项 nav）、受限页重定向                                                                                 |
| `checks/nav.js`     | 9 组「宽度×语言」下 nav 链接无裁切、文档无横向溢出（含 768/767 边界）                                                                                                  |
| `checks/columns.js` | logs 列显隐：开合、隐藏生效、外部点击关闭、刷新持久化；`/dashboard/session` 拖慢 3s 的死窗内按钮已绑定可点开（`initLogsPageActions` 在模块顶层挂委托，不等 bootstrap） |
| `checks/mobile.js`  | 375/320 视口 logs 页：无横向溢出、列显隐按钮完整在视口内、可点开                                                                                                       |
| `checks/console.js` | 8 页零 console error / pageerror / 4xx+ 响应                                                                                                                           |

约定：每条断言一行 `ok`/`FAIL`，截图只在失败时写 `shots/`（gitignore）；检查脚本进程退出码即结果，run.sh 汇总计数。所有检查经 `PV_BASE`/`PV_ADMIN_PW`/`PV_API_TOKEN`/`PV_SHOTS` 拿环境，由 run.sh 注入。

## 踩过的坑

- `require('playwright')` 按脚本所在目录向上找 node_modules，与 cwd 无关——在本目录装好依赖后，从仓库任何地方 `node scripts/panel-verify/checks/xx.js` 都能解析。
- 清理临时实例只 `kill $PID`：仓库约定里 `pkill -f <pattern>` 会匹配发起者自己的命令行，整条 shell 被杀。
- 开放面板（`dashboard.password: ""`）下任何凭据都是 admin；要测 api_token 受限视图必须给非空密码且令牌与密码不同值（本套件 testpw/testkey）。
- `boundKey` 落在 `document.body.dataset`（`logsPageActionsBound === '1'`），不是 `window.*`——早期探测脚本查错位置会得到常 false。
- 面板渲染入口是 `window.initPageBootstrap`：`/dashboard/session` 先回角色才渲染 topnav，慢会话期间 nav 晚出现属预期；但 `data-page-filters` 容器与列显隐委托绑定都在模块顶层，不依赖 session。
