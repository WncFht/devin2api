# 编译问题

## 模块问题

```bash
go clean -modcache      # 清模块缓存
go mod download         # 重新下载依赖
go mod verify           # 校验依赖
go mod tidy             # 整理依赖
go mod why <package>    # 这个依赖为什么在这？
```

## CGO 问题

```bash
go env CGO_ENABLED                         # 查 CGO 是否启用
export CGO_CFLAGS="-I/usr/local/include"   # 设 CGO CFLAGS
# macOS: brew install pkg-config
# Ubuntu: apt install pkg-config
```

## 版本不匹配

```bash
go version              # 查 Go 版本
go mod edit -go=1.21    # 设最低要求版本
```
