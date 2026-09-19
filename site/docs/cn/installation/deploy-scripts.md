# 部署脚本

与[一键脚本](/cn/installation/install-script)相同的托管服务，改从仓库检出驱动——适合想把仓库留在盘上的场景（脚本以仓库为家：同步 `config.yaml` 进平台配置目录、在仓库内维护指向状态目录的 `logs` 符号链接）。

```bash
git clone https://github.com/WncFht/devin2api && cd devin2api
bash scripts/deploy/deploy-linux.sh --release latest    # macOS 用 scripts/deploy/deploy.sh
```

`--release latest` 下载经 sha256 校验的预编译二进制，装完轮询 `/healthz` 确认新版本接管，再打一发 `GET /v1/models` 验证上游鉴权真的通了。

## 参数

| 参数              | 作用                                                    |
| ----------------- | ------------------------------------------------------- |
| `--release <tag>` | 装指定 release（或 `latest`）；不带则构建工作树源码安装 |
| `--check`         | 对比已安装 / 运行中 / 最新版本                          |
| `--uninstall`     | 移除服务与二进制（保留 config 与日志）                  |
| `--no-restart`    | 只换二进制不重启                                        |

首跑配置生成与一键脚本相同：生成 `config.yaml`（随机 `dashboard.password`）并提示粘贴凭据。

## 平台布局

| 平台    | 托管方式                                 | 布局                                                                                                      | 脚本                                |
| ------- | ---------------------------------------- | --------------------------------------------------------------------------------------------------------- | ----------------------------------- |
| macOS   | launchd 代理                             | 二进制 `~/.local/bin` · 配置 + 状态 `~/Library/Application Support/devin-2api`                            | `scripts/deploy/deploy.sh`          |
| Linux   | `systemd --user`                         | 二进制 `~/.local/bin` · 配置 `~/.config/devin-2api` · 状态 `~/.local/state/devin-2api`                    | `scripts/deploy/deploy-linux.sh`    |
| Windows | 无——控制台运行，或用 NSSM / 任务计划程序 | exe `%LOCALAPPDATA%\Programs\devin-2api` · 配置 `%APPDATA%\devin-2api` · 状态 `%LOCALAPPDATA%\devin-2api` | `scripts/deploy/deploy-windows.ps1` |

## Windows

```powershell
.\scripts\deploy\deploy-windows.ps1 -Release latest
```

脚本生成的 `config.yaml` 绑回环地址加空闲端口（避开防火墙弹窗与裸暴露），实例在独立控制台窗口起跑。

::: warning
请在本机交互会话里执行，别走 SSH——会话结束 job object 会回收实例。
:::

Windows 没有开箱的服务托管器：exe 在控制台前台跑，Ctrl+C 触发与其它平台相同的优雅排空；关窗或 `taskkill /F` 都是强杀。要无人值守运行，用 NSSM 或任务计划程序注册。
