---
name: extract-embedded-protos
description: 从编译后的 Go 二进制中恢复全部内嵌 FileDescriptorProto，保留原始 descriptor set，并把每个恢复的 protobuf 包扁平化成一个可搜索、可用 protoc 编译的 .proto 文件。检查 Devin、Windsurf、Codeium 或其他编译后 Go 应用的 protobuf/gRPC schema，定位 RPC 请求、消息、系统提示词、工具定义、字段号或服务流式契约，或用 descriptor 支撑的定义替换猜测出来的逆向 struct 时使用。
---

# 提取内嵌 proto

用随仓的确定性 Go 提取器从二进制恢复 protobuf schema。原始 descriptor set 与扁平化产物是两个不同的证据面，分开对待。

## 跑提取器

1. 确定要检查的确切编译产物，必须是普通文件。
2. 选一个专用输出目录。提取器每次运行前会彻底删除该目录；绝不能指向工作区根目录或混放其他文件的目录。
3. 在项目根目录运行 Go 命令，带且仅带两个位置参数：

```sh
go run ./cmd/protoextract \
  /absolute/path/to/source-binary \
  /absolute/path/to/output-directory
```

首次构建需要 Go，可能需要网络拉取 module 依赖。不要给提取器 CLI 加 flag 或兼容模式。

## 理解输出

全新输出目录里应当恰好有这些文件：

- `descriptors.pb`：无损恢复的 `FileDescriptorSet`。它是原始文件名、包名、service 路径、语法、option、字段名与字段号的权威来源。
- `all-protos.proto`：全部恢复声明合并进一个可编译包。用于搜索、阅读和代码生成实验。
- `manifest.json`：提取计数、缺失依赖、注释覆盖率、注意事项，以及原始名到扁平名的符号映射。

扁平化文件在存在时保留 `exa.api_server_pb`。它给非根符号加前缀、重写全部 message/enum/service/extension 引用、重命名冲突的枚举值，并用 proto2 语法承载 proto2/proto3 混合定义。它保留已知字段的 wire 结构，包括 `required`、map、oneof、extension range、枚举号、字段号和 packed 编码。

不要把扁平化名当成原始协议名。非根的生成类型名与 service RPC 路径会变化。proto3 presence 与开放枚举 API 无法在 proto2 视图中精确表示。

## 每次提取都要验证

读 `manifest.json` 并报告：

- descriptor、candidate 与重复计数；
- 缺失依赖；
- 注释位置与含注释的文件；
- 选定的扁平化包名与语法。

有 `protoc` 时编译扁平化文件，不留检查产物：

```sh
protoc \
  --proto_path=/absolute/path/to/output-directory \
  --descriptor_set_out=/dev/null \
  /absolute/path/to/output-directory/all-protos.proto
```

修改提取器行为前先跑项目 Go 测试：

```sh
go test -race ./cmd/protoextract
go vet ./...
```

## 分析协议

按确切的 service、method、message、字段名搜索 `all-protos.proto`。对 RPC 路径下结论前，先拿 `descriptors.pb` 或 `manifest.json` 的映射确认原始身份。

涉及状态、消息、系统提示词或工具的请求：

1. 记录确切的请求与响应字段号和类型。
2. 把提示词字符串、结构化消息历史、ID 和工具定义分开看，不要从猜出来的名字推断语义。
3. 相关 HTTP/SSE 调用单独追踪；schema 本身证明不了服务端是否保存会话状态。
4. 说明提取边界：只有内嵌在这个二进制里的 descriptor 能恢复。被 strip 的、动态下载的或单独分发的 schema 不在其中。
5. 绝不编造注释。原始注释只在 `SourceCodeInfo` 编译后幸存时存在；拿 manifest 计数当证据。

依赖缺失或扁平化文件编译失败时，保留 `descriptors.pb`，报告确切的未解析名，并在改动提取算法前检查相邻二进制或应用资源。
