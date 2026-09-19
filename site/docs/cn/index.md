---
# https://vitepress.dev/reference/default-theme-home-page
layout: home

hero:
    name: devin-2api
    tagline: 把 Devin 模型挂到 OpenAI 与 Anthropic 兼容端点之后
    actions:
        - theme: brand
          text: devin-2api 是什么？
          link: /cn/introduction/what-is-devin-2api
        - theme: alt
          text: 快速开始
          link: /cn/introduction/quick-start
        - theme: alt
          text: GitHub
          link: https://github.com/WncFht/devin2api

features:
    - title: 三个 API 面
      details: 同一个上游同时服务 /v1/responses（含 Codex 式客户端的 WebSocket 传输）、/v1/chat/completions 与 /v1/messages。
    - title: 推理与工具调用完整往返
      details: thinking 签名跨轮次保留重放；自定义工具调用与服务端托管 web_search 端到端可用。
    - title: 图片、文档、视频输入
      details: 三个协议面都能解码附件，由按模型能力位在本地把关。
    - title: 脱钩完成缓存
      details: 客户端断连不会掐断上游流——语义相同的重试直接续接，跨重启也有效。
    - title: 多账号池
      details: 每 lane 独立凭据、配额追踪、会话亲和与跨号 failover。
    - title: 管理面板
      details: 请求浏览、用量与配额追踪、令牌管理、配置热重载、一键自更新。
---
