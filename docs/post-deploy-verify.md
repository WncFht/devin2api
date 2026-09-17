# 部署后验证 SOP

`deploy-remote.sh` 推生产实例后按本清单走一遍，确认版本切换完成且面板无回归。命令在开发机执行；示例地址 `http://100.105.212.52:3003` 是生产实例的 tailscale 地址（本机示例）——开发机上的 `localhost:3003` 到不了生产实例，一律用这个地址。

## 步骤

1. 部署：`scripts/deploy-remote.sh --ref origin/main`。worktree 默认模式（部署含未提交改动的工作树）省略 `--ref`。
2. 确认版本：`curl -s http://100.105.212.52:3003/healthz`，`version` 字段等于刚部署的版本才算切换完成——排空期旧进程仍以旧版本应答，脚本退出或首个 200 都不代表切完。同一信息也可看 `/admin/status`。
3. 跑面板验证：`scripts/panel-verify/run.sh --base http://100.105.212.52:3003`。`--base` 模式跳过构建与实例生命周期，同一套 playwright 断言直接打目标实例。
4. 判定：退出码 0 且输出 `PASS: all checks green` 即通过；失败按 `FAIL` 行定位，截图在 `scripts/panel-verify/shots/`（gitignore）。单跑某条：`PV_BASE=http://100.105.212.52:3003 node scripts/panel-verify/checks/<name>.js`。

## 凭据与安全性

生产实例是开放面板（`dashboard.password` 为空）：任何凭据经 `POST /login` + `GET /dashboard/session` 探测后有效角色都解为 admin，`--admin-pw`/`--api-token` 传不传都行，`login.js` 自动按开放面板形态断言。若目标换成密码面板，`--admin-pw` 必须传真实密码，否则 admin 上下文检查整段失败。套件对实例只读（columns 检查只写浏览器 localStorage），可直接打生产。

## 覆盖范围

| 检查         | 断言                                                                                                  |
| ------------ | ----------------------------------------------------------------------------------------------------- |
| `login.js`   | UI 表单 admin 登录落面板、满 8 项 nav；api_token 登录按 `/dashboard/session` 探测的有效角色自适应断言 |
| `nav.js`     | 9 组宽度×语言下 nav 链接无裁切、文档无横向溢出                                                        |
| `columns.js` | logs 列显隐开合/生效/外部点击关闭/刷新持久化；session 拖慢 3s 死窗内按钮已绑定可点开                  |
| `mobile.js`  | 375/320 视口无横向溢出、列显隐按钮完整在视口内可点开                                                  |
| `console.js` | 8 页零 console error / pageerror / 4xx+ 响应                                                          |

套件细节与踩坑见 `scripts/panel-verify/README.md`。
