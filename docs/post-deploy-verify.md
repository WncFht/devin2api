# 部署后验证 SOP

`deploy-linux.sh` 推生产实例后按本清单走一遍，确认版本切换完成且面板无回归。命令在生产机本机执行；生产实例监听 `:3033`，本机用 `http://127.0.0.1:3033`、其它机器用 tailscale 地址 `http://<tailnet-ip>:3033`（旧地址 `<old-tailnet-ip>:3003` 由旧 Mac 的 forwarder shim 转发，同样可达）。

## 步骤

1. 部署：`scripts/deploy-linux.sh`（构建工作树现状并部署）。
2. 确认版本：`curl -s http://127.0.0.1:3033/healthz`，`version` 字段等于刚部署的版本才算切换完成——排空期旧进程仍以旧版本应答，脚本退出或首个 200 都不代表切完。同一信息也可看 `/public/version`。
3. 跑面板验证：`scripts/panel-verify/run.sh --base http://127.0.0.1:3033 --admin-pw "$PW"`，`$PW` 取法见下节。`--base` 模式跳过构建与实例生命周期，同一套 playwright 断言直接打目标实例。
4. 判定：退出码 0 且输出 `PASS: all checks green` 即通过；失败按 `FAIL` 行定位，截图在 `scripts/panel-verify/shots/`（gitignore）。单跑某条：`PV_BASE=http://127.0.0.1:3033 node scripts/panel-verify/checks/<name>.js`。

## 凭据与安全性

生产实例是密码面板（2026-09-18 起 `dashboard.password` 非空，实例绑 `*:3033` 对 tailnet/LAN 开放，必须收口）：`--admin-pw` 必须传真实密码，否则 admin 上下文检查整段失败。密码从 live config 取：

```bash
PW=$(awk '/^dashboard:/{f=1;next} /^[^ ]/{f=0} f&&/password:/{print $2}' ~/.config/devin-2api/config.yaml | tr -d "\"'")
```

若目标换回开放面板，`--admin-pw`/`--api-token` 传不传都行，`login.js` 自动按开放面板形态断言。套件对实例只读（columns 检查只写浏览器 localStorage），可直接打生产。

## 覆盖范围

| 检查            | 断言                                                                                                                        |
| --------------- | --------------------------------------------------------------------------------------------------------------------------- |
| `login.js`      | UI 表单 admin 登录落面板、满 8 项 nav；api_token 登录按 `/dashboard/session` 探测的有效角色自适应断言                       |
| `nav.js`        | 9 组宽度×语言下 nav 链接无裁切、文档无横向溢出                                                                              |
| `columns.js`    | logs 列显隐开合/生效/外部点击关闭/刷新持久化；session 拖慢 3s 死窗内按钮已绑定可点开                                        |
| `mobile.js`     | 375/320 视口无横向溢出、列显隐按钮完整在视口内可点开                                                                        |
| `console.js`    | 8 页零 console error / pageerror / 4xx+ 响应                                                                                |
| `bucket-sec.js` | /dashboard/metrics 秒级桶宽：断言 `X-Bucket-Sec` 头回传与夹取、UI 粒度选 10 秒后请求带 `bucket_sec=10`、间隔片显示「10 秒」 |

`align-shots.js` 也随套件跑但不做断言：多视口截图留档 `shots/align/` 供人工目检对齐与工具栏行数。

套件细节与踩坑见 `scripts/panel-verify/README.md`。
