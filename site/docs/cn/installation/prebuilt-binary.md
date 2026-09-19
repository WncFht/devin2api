# 预编译二进制

下载 release 资产直接跑——不经任何服务托管器。

[Releases](https://github.com/WncFht/devin2api/releases) 资产命名 `devin-2api-{darwin,linux}-{amd64,arm64}`；Windows 为 `devin-2api-windows-{amd64,arm64}.zip` 包（内含 exe + `config.example.yaml` + LICENSE），附 `checksums.txt` 可校验。

## Linux / macOS

```bash
# Linux 示例；macOS 换成 devin-2api-darwin-arm64 或 -darwin-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/devin-2api-linux-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing   # 期望输出：devin-2api-linux-amd64: OK
chmod +x devin-2api-linux-amd64
./devin-2api-linux-amd64 -config config.yaml
```

## Windows

解压 zip 后编辑 `config.yaml`（账号可以只给 `credentials_file`——Windsurf 内嵌 `devin.exe` 产出凭证文件的方法见[账号池](/cn/configuration/account-pool)），在控制台运行：

```powershell
.\devin-2api.exe -config config.yaml
```

Ctrl+C 触发与其它平台相同的优雅排空；关窗或 `taskkill /F` 不走排空——Windows 对控制台进程只有强杀路径。

## 路径解析

二进制按平台惯例解析路径：

- **配置文件**——`-config` flag → `DEVIN2API_CONFIG` → `./config.yaml`（存在才选）→ [布局表](/cn/installation/deploy-scripts#平台布局)中的平台默认
- **状态目录**——`-state-dir` flag → `DEVIN2API_STATE_DIR` → 平台默认

所以 `解压 && ./devin-2api.exe` 会自动选中 `./config.yaml`，而托管安装总是显式传两个 flag。
