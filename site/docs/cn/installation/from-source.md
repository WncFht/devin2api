# 源码构建

生成的 proto 绑定已提交在 `outputs/devin-proto-go`，clone 后即可直接构建——只需要 Go 工具链，无需额外代码生成步骤。

```bash
git clone https://github.com/WncFht/devin2api && cd devin2api
go run ./cmd/devin-2api -config config.yaml
```

或产出二进制：

```bash
go build -o devin-2api ./cmd/devin-2api
./devin-2api -config config.yaml
```

[路径解析](/cn/installation/prebuilt-binary#路径解析)链相同。想要「自己构建 + 托管服务」的组合，跑[部署脚本](/cn/installation/deploy-scripts)时不带 `--release`——脚本会构建工作树并安装产物。
