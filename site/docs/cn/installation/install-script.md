# 一键脚本

Linux 与 macOS 上的推荐路线：一条命令，免 clone、免 root。

```bash
curl -sSL https://raw.githubusercontent.com/WncFht/devin2api/main/scripts/install.sh | bash
```

`install.sh` 是薄引导层：按目标版本拉取部署管线后交给平台 deploy 脚本。装出来的是托管服务——二进制落在 `~/.local/bin`，配置与状态落在平台目录（布局表见[部署脚本](/cn/installation/deploy-scripts)），进程托管在 `systemd --user`（Linux）或 launchd（macOS）下，每次重启走零停机 REUSEPORT 交接。

## 子命令

| 命令              | 作用                                   |
| ----------------- | -------------------------------------- |
| `install`（缺省） | 安装或重装；`-v <tag>` 钉版本          |
| `upgrade`         | 升到最新 release                       |
| `rollback <tag>`  | 装回指定旧版本                         |
| `status`          | 显示已安装 / 运行中 / 最新版本         |
| `list-versions`   | 列出可用 release tag                   |
| `uninstall`       | 移除服务与二进制（保留 config 与日志） |

把脚本存下来后跑 `bash install.sh <子命令>`；或者在 `curl | bash` 后面直接跟子命令：`curl -sSL .../install.sh | bash -s upgrade`。

## 首跑

首跑自动从 `config.example.yaml` 生成 `config.yaml`（写入随机 `dashboard.password`）并提示粘贴 Devin 凭据——粘贴，或留空以空池起跑、事后在面板 `/web/accounts.html` 加号。下游 `/v1` 令牌在面板 `/web/tokens.html` 创建，不入配置。想提前定制，先把 `config.example.yaml` 复制成脚本 config 位置上的 `config.yaml` 再编辑——或装完改生成的那份。

## 未登录也常驻（Linux）

`systemd --user` 服务默认随最后一个登录会话退出。要未登录也常驻：

```bash
loginctl enable-linger $USER
```

## Windows

`install.sh` 只覆盖 Linux 与 macOS。Windows 请走 [PowerShell 部署脚本](/cn/installation/deploy-scripts#windows)或[预编译二进制](/cn/installation/prebuilt-binary)。
