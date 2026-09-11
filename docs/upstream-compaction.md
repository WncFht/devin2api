# Devin CLI 自动压缩(Compaction)逆向报告

> 来源:对 `/opt/homebrew/Caskroom/devin-cli/3000.10.21/bin/devin`(v3000.10.21, ~159MB Rust 二进制)做 strings 提取 + `outputs/devin-proto/all-protos.proto` 协议分析。**全部为客户端行为**,与 swe-2 模型本身无关——`GetChatMessage` RPC 没有任何压缩相关字段。

## 结论先行

`GetChatMessageRequest` 是无状态补全调用(全量历史重放),上游**不做**自动压缩。压缩义务在调用方:devin CLI 在 `agent-ext/src/compactor/` 实现了完整的压缩器;Claude Code 也有自己的压缩器。对 devin-2api 来说,接没有自带压缩的客户端(kimi-cli、pi 等)时,长会话会直接顶爆窗口,届时才需要代理侧压缩。

## 触发:三阈值异步管线

CLI 暴露的配置面:

| 入口                                                                        | 形式                                        |
| --------------------------------------------------------------------------- | ------------------------------------------- |
| `--compaction-thresholds SPAWN,[APPLY,]HARD`                                | 绝对 token 数,例 `183500,183500,262144`     |
| `DEVIN_COMPACTION_THRESHOLDS`                                               | 同上,环境变量                               |
| `--sidekick-compaction-thresholds` / `DEVIN_SIDEKICK_COMPACTION_THRESHOLDS` | 子代理(sidekick)链独立阈值,缺省回落到主链   |
| `agent.compaction_threshold_tokens`                                         | 用户/项目配置文件项,合法值是 1 到上限的整数 |

语义(按二进制内 help 字符串原话):

- **spawn**:达到该 token 数时**启动摘要任务**——后台异步流式调模型,不阻塞当前轮
- **apply**:摘要就绪后**应用**——把历史换成 summary + 保留尾部;`spawn,hard` 两参数形式则只在硬顶才做
- **hard**:上下文窗口硬顶,**阻塞推理**直到压缩完成

上下文窗口取自 `ModelInfo`(由 `GetCliModelConfigs` 下发);找不到时 `using default context tokens for force compact`。二进制内出现过的组合 `183500,183500,262144` 对应 256k 窗口、spawn 点约 70%。

日志/事件面:`Compacting conversation history with N messages and M tokens`、`AgentEvent::Compacted`、ACP 通知 `cognition.ai/compaction`、UI 命令 "Force conversation compaction" / "Show context window usage" / "Context compacted"。

## 摘要生成(summarize.rs)

压缩是一次**普通模型调用**(流式、带重试)。提取到的完整提示词:

```text
You are a Summarizer that summarizes conversation history.
You will be shown a conversation between the user and the assistant, a coding
agent. You should summarize the conversation + work done for future work to be
continued by the coding agent.
Structure your summary as follows:
<summary>
## Overview
A high-level summary of what was being worked on and the overall goal (1-2 sentences).
## Key Details & Breadcrumbs
Important details that may be needed later (key findings, decisions,
constraints, error messages, progress or modified files, etc).
For each item, note:
- What it is and why it matters
- If it would be helpful to look at the original source, include citations to
  message ids or search terms to find the details in the history file
## Current State
What the agent was actively working on when this summary was created:
- The immediate task or step in progress
- Any pending actions or next steps that were planned
- Blockers or questions that need resolution
</summary>
IMPORTANT: Do NOT reproduce or recite any rules, instructions, or guidelines
that were included verbatim in the conversation (e.g., content inside <rules>
or <rule> tags). Rules will be re-discovered and re-injected as needed when the
agent accesses relevant files. Focus on summarizing the work done and decisions
made, not the instructions themselves.
Note, the full conversation will be saved to a history file (<path>). The full
path will be provided alongside the summary you create.
Be concise but ensure someone could resume work using only your summary plus
the reference file.
Now summarize the conversation above as per the format given. Remember, do NOT
take any actions. Just provide the summary in <summary> tags.
```

用户侧输入以 `Conversation to summarize:` 开头。模型可用 override 指定,失败时回落:`Compaction with override model failed, falling back to default model` / `AsyncFileCompactor: override model failed, retrying with fallback`;流建立失败会重试(`Compaction stream creation failed (attempt N), retrying`)。

## 压缩后的上下文形状

全文历史先落盘(`file_compactor.rs` / `async_file_compactor.rs`),再拼装新上下文:

- **历史文件**:`~/.local/share/devin/cli/summaries/history_<id>.md`,内含 `#Full conversation history saved at <path>` 和 `Summary:` 段——与 CLI 会话恢复摘要文件格式完全一致
- **恢复包装**:`You are continuing work from a previous conversation thread. Below is a summary of the previous conversation thread:` + `<summary>` 内容 + `Here are some files that you edited` + `<last_todo_list>` 等段
- **逐字保留**(`apply_summary`):
    - 编辑过的文件路径清单(`compact/edited_files`,`apply_summary: preserving N edited file path(s)`)
    - todo list(`compact/todo_list`,`apply_summary: preserving todo list (...)`)
    - handoff 消息尾部(`Summarize: preserving N handoff message(s) verbatim across compaction`,过老的会被截断)
    - `preserve_on_compaction` 标记的 rules(常规 rules 不保留——按提示词约定会被重新注入)
- **provenance**:消息节点(`MessageNode`)带 `summarized_from`、`num_tokens_preceding` 元数据,记录被压缩前的来源

## 失败与边界

- `No nodes to summarize` / `too few messages to summarize` / `Nothing to compact.`
- `System prefix exceeds compaction threshold`——光 system 前缀就超阈值
- `Cannot compact: no active model set.` / `Force compaction failed`
- `Compaction cancelled by revert` / `cancelled by user` / `interrupted by user` / `task disconnected`
- `Fatal compaction error` / `cannot determine context window limits`

## 对 devin-2api 的含义

1. **当前链路不需要我们压缩**:Claude Code 自带压缩器;它提示词里 "The system will automatically compress prior messages" 一句已在上游指纹库中,由 `sanitize.go` 改写。
2. **无压缩客户端**(kimi-cli、pi 等若不带)长会话会顶爆 swe-2 窗口,表现为上游 `prompt_too_long` 类失败。需要时可照本文实现代理侧压缩:token 估算到 spawn 阈值 → 用同模型跑上面的 summarizer 提示词 → 历史替换为 `<summary>` + 逐字保留段 + 尾部 N 条。
3. **工具调用配对不变**:压缩替换的是消息列表,`devin.go` 的 call→result 配对约束照样适用——保留尾部必须从**完整的 user 轮边界**切开,不能切在 call/result 对中间,否则复现 `invalid_argument`。
4. **缓存**:压缩后历史前缀改变,前缀缓存整体失效;逐字保留段(文件清单/todo)若放在 system 前缀内可保住 system 部分的缓存。

## 附:服务端压缩(未走 GetChatMessage 的)

proto 里另有 Cascade 轨迹流的压缩设施(`ExaCortexPb_CascadeSummarizerConfig`、`truncation_threshold_tokens`、`UpdateCascadeTrajectorySummaries` RPC、`truncate_trajectory_to_judge_steps` 等),属 IDE agent 路径,与本代理使用的 `GetChatMessage` 无关,仅记录备查。
