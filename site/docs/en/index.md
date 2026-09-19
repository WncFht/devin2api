---
# https://vitepress.dev/reference/default-theme-home-page
layout: home

hero:
    name: devin-2api
    tagline: Devin models behind OpenAI- and Anthropic-compatible endpoints
    actions:
        - theme: brand
          text: What is devin-2api?
          link: /introduction/what-is-devin-2api
        - theme: alt
          text: Quick Start
          link: /introduction/quick-start
        - theme: alt
          text: GitHub
          link: https://github.com/WncFht/devin2api

features:
    - title: Three API surfaces
      details: POST /v1/responses (with a WebSocket transport for Codex-style clients), /v1/chat/completions, and /v1/messages on one upstream.
    - title: Reasoning and tools round-trip
      details: Thinking signatures replay across turns; custom tool calls and server-managed web_search work end to end.
    - title: Image, document, and video inputs
      details: Attachments decode on all three surfaces, gated by per-model capability flags.
    - title: Detached completion cache
      details: A client disconnect never kills a stream — an identical retry re-attaches, even across restarts.
    - title: Multi-account pool
      details: Per-lane credentials, quota tracking, session affinity, and failover across Devin accounts.
    - title: Admin panel
      details: Request browser, usage and quota tracking, token management, config hot reload, and one-click self-update.
---
