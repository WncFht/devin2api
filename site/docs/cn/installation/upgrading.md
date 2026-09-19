# 升级

升级方式取决于进程归谁托管。

## 托管安装（一键脚本 / 部署脚本）

由 `install.sh` 或部署脚本装出的服务可以从面板自更新：`/web/settings.html` 的版本更新卡——检查 → 下载 → sha256 校验 → 换二进制 → 跨同一套 REUSEPORT 交接重启，全程零断连，面板轮询进度无缝跨过进程切换。

脚本侧等价端点：

| 端点                          | 作用                                                  |
| ----------------------------- | ----------------------------------------------------- |
| `POST /admin/update`          | 升到最新（或 body `{"tag": "vX.Y.Z"}` 指定版本）      |
| `POST /admin/update/check`    | 对比已安装 / 运行中 / 最新版本                        |
| `GET /admin/update/status`    | 轮询在途更新进度                                      |
| `POST /admin/update/rollback` | 对上次更新留下的 `.backup` 二进制做对称换回——无需下载 |

均认 admin Bearer（`dashboard.password`）。错误码：在途冲突 `409`、无 `.backup` `404`、坏 tag `400`。

命令行下也可以重跑安装路线——`install.sh upgrade`（或 `rollback <tag>`），或再跑一次 `deploy-*.sh --release latest`。

## 非托管形态

手动起的二进制、Windows 控制台、Docker——更新端点回 `501`。按你的安装路线重跑一遍：

| 路线         | 升级动作                                          |
| ------------ | ------------------------------------------------- |
| 预编译二进制 | 下载新资产、替换、重启进程                        |
| Docker       | `docker pull ghcr.io/wncfht/devin2api` 后重建容器 |
| 源码         | `git pull` 后重新构建 / 重启                      |

状态目录（`devin-2api.db`）在任何升级路径下都存活——令牌、日志、配额历史与脱钩完成缓存条目全部保留。
