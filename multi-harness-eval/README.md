# multi-harness-eval:同一模型 × 多 harness 评测

目标：把 **swe-2-max**（经 devin-2api / ccload 链路暴露，见 `docs/harness-verification.md`）灌进多个 coding-agent harness，在固定 benchmark 上对比。被测模型恒定，只换 harness。基建用 **Harbor**(Terminal-Bench 团队的通用评测框架，TB 2.x 官方 harness)[^harbor]。

## 1. 端点与协议

链路里有两个入口，不同 harness 各吃一个：

| 入口                       | 协议                               | 服务对象                                     |
| -------------------------- | ---------------------------------- | -------------------------------------------- |
| `http://<ccload>:49173`    | Anthropic Messages(`/v1/messages`) | claude-code、kimi-code、pi                   |
| `http://<devin-2api>:3003` | OpenAI Responses(`/v1/responses`)  | codex、pi(备选 `model_api=openai-responses`) |

**注意：agent 跑在 Docker 容器里，`localhost` 指容器自己。** 宿主机上的服务要写 `http://host.docker.internal:<port>`(Docker Desktop) 或局域网 IP；如果任务的 `[agent]` 网络策略拦了 egress，还要加 `--allow-agent-host=<host>` 或在 task.toml 里放开。

**被测模型恒定 swe-2-max。** 网关侧统一改写模型名，所以各 harness 的 `model_name` 只是过客户端 picker 校验的前台名，不参与实际路由 —— claude-code 要求名字含 claude 家族词，kimi-code 沿用 `kimi-k3` 最稳，pi 和 codex 可以直接写 `swe-2-max`。

## 2. Harness 接入矩阵

四个目标 harness 全部内置（`harbor agent list` / `harbor agent schema <name>` 可查）[^harbor-agents]。通用参数：`--ak version=X.Y.Z` 钉版本、`--agent-env KEY=VAL` 注入容器内 agent 进程环境、`--ak`/`--agent-kwarg` 传 agent 级选项。

| Agent                        | 模型名（`-m`)                                                                         | endpoint 接法                                                                                                                                                   | 备注                                                                                                                                                   |
| ---------------------------- | ------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `claude-code`                | `anthropic/claude-sonnet-4-5`（名字须含 claude/opus/sonnet/haiku 家族词，网关侧改写） | `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`                                                                                                                   | 可选 kwargs:`reasoning_effort`、`max_turns`、`append_system_prompt`、`allowed_tools`/`disallowed_tools`、`memory_dir`                                  |
| `kimi-code`                  | `kimi-k3`                                                                             | `KIMI_MODEL_BASE_URL` + `KIMI_MODEL_API_KEY`；可选 `KIMI_MODEL_MAX_CONTEXT_SIZE`、`KIMI_MODEL_CAPABILITIES=image_in,thinking`、`KIMI_MODEL_THINKING_EFFORT=max` | 自带 CC 请求封套伪装，走 Anthropic 端点                                                                                                                |
| `pi`                         | `anthropic/swe-2-max`                                                                 | `ANTHROPIC_BASE_URL` + `ANTHROPIC_API_KEY` + `--ak model_api=anthropic-messages`                                                                                | 自定义端点会自动写 `models.json`;`--ak thinking=<档>`                                                                                                  |
| `codex`                      | `openai/swe-2-max`                                                                    | `OPENAI_BASE_URL=<devin-2api>/v1`(Harbor 写入容器 `config.toml` 的 `openai_base_url`)                                                                           | 原生说 Responses，直连 devin-2api;`--ak reasoning_effort=...`、`web_search=disabled`；复杂配置用 `--ak config=./codex.toml`                            |
| `mini-swe-agent`（可选基线） | `openai/swe-2-max`                                                                    | litellm `api_base` + `OPENAI_API_KEY`                                                                                                                           | 极简 bash-loop，作「harness 下限」参照，Epoch AI 用它做跨模型标准 scaffold[^epoch]；需要 chat-completions 格式，端点只有 responses 时经 LiteLLM 转一道 |

## 3. 测哪些 bench

Harbor 已适配 ~90 个 benchmark（仓库 `adapters/` 目录 + registry)。四个 harness 全是终端/编程类 agent，选 bench 也在这个域内：

**阶段 0 —— 冒烟**：`harbor/hello-world` 或 `examples/tasks/hello-world`。每 harness 先跑通鉴权、协议、轨迹落盘，再谈正事。

**阶段 1 —— 主力**：`terminal-bench/terminal-bench-2-1`。容器化终端任务，DeepSeek V4.1、Kimi 等厂商的 code-agent 主评测场[^v41]。选 2.1 而非 2.0，因为 2.0 有已知环境问题；3.0/4.0 偏科学/专家级、难度陡增，想拉开区分度再加。

**阶段 2 —— SWE 类**：`swebench` adapter(SWE-bench Verified,500 题）。真实 GitHub issue 修复，信号最强但环境重；用 `task_names`/`n_tasks` 抽 50–100 题分层子集。想看非 Python 仓库加 `swebench_multilingual`。

**阶段 3 —— 补充（可选）**:

- `aider_polyglot`:225 道小题，便宜快速，当回归冒烟
- `programbench`:V4.1 在用的难题集（Almost@1 指标），模型强才有区分度
- `cybergym`：关心安全方向再跑

**不跑**:`gaia`/`osworld`/`theagentcompany` 要浏览器/GUI，终端 agent 缺工具会失真；`tau3-bench` 是领域 API 调用，测的不是一回事。

## 4. 运行方式

CLI 一把跑：

```bash
harbor run -d terminal-bench/terminal-bench-2-1 \
  -a kimi-code -m kimi-k3 \
  --agent-env KIMI_MODEL_BASE_URL=http://host.docker.internal:49173 \
  --agent-env KIMI_MODEL_API_KEY=<key> \
  --ak version=0.42.0 \
  -k 3 -n 4
```

推荐写成 job yaml（可复现、入 git)，见 `jobs/smoke.yaml`、`jobs/tb21.yaml`:

```bash
harbor run --config jobs/tb21.yaml
```

关键字段：`n_attempts` 每题采样数（agentic 方差大，至少 3；算 pass@1 均值）、`datasets[].n_tasks`/`task_names` 抽子集、`agents[].env`/`kwargs`/`skills` 逐项配 agent。

## 5. Harbor 能控制什么（system prompt / skills / tools)

结论：**Harbor 不改写 harness 的原生 system prompt** —— 这是刻意的，评测对象就是「harness 作为部署系统」的完整表现[^scaffold-effect]。它提供的控制面分四层：

**指令层（user turn，等价控制）**

- `--ak prompt_template_path=<file>`：所有 installed agent 通用，Jinja2 模板必须含 `{{ instruction }}`，把任务指令包一层统一的前后缀
- job 级 `extra_instructions` / `extra_instruction_paths`：给所有 agent 追加统一指令
- 任务仓库里放 `AGENTS.md`/`CLAUDE.md`:harness 原生的 repo 指令文件机制，会读的 harness 自然会读 —— 保留各家差异，做 harness 对比时这是正确姿势

**系统提示词（各家自己管，只能追加不能替换）**

- `claude-code`:`--ak append_system_prompt=<text>` → `--append-system-prompt`
- `codex`:`--ak config=...` 原生 config.toml 全量透传
- 其它 harness 没有系统提示词注入口，要改只能改 harness 本身（比如 pi 的 extension)

**Skills**

- job yaml `agents[].skills`：列表，支持本地目录、git URL、`org/name[@ref]` 缩写；Harbor 把内容合进任务环境，每个 agent 再拷到自己 harness 认的位置 —— claude-code 拷到 `$CLAUDE_CONFIG_DIR/skills/`,pi 拷到 `~/.agents/skills/`。没有 skills 概念的 harness 自动忽略
- `claude-code` 另有 `memory_dir`(host 目录 → Claude memory)

**Tools**

- 没有统一 tool 层，各家原生 toolset。可调项：claude-code 的 `allowed_tools`/`disallowed_tools`/`permission_mode`(Harbor 默认 `bypassPermissions`);codex 的 `web_search=disabled|cached|live`;kimi-code 的 `KIMI_MODEL_CAPABILITIES`
- task.toml 可声明 MCP servers,Harbor 会合并进 agent 的原生配置（codex → config.toml,cc → .mcp.json)
- 任务侧 `[agent]` 段有 `network_mode`/`allowed_hosts` 网络策略 —— 防作弊用（断网、防查答案），coding bench 建议开

**其它**：轨迹统一存 ATIF 格式，turns/tokens/耗时跨 harness 可比；`resume_trajectory`/`load_trajectory` 支持多步任务和会话续跑。

## 6. 实验纪律

- 版本钉死：`--ak version=X.Y.Z` 或 job yaml 里写明，结果里记录每个 harness 的精确版本（V4.1 报告里 Claude Code 四个小版本分数都不同[^v41])
- 采样：`n_attempts: 3` 起步；50 题规模下 ±8pp 内不显著，差异解读看置信区间
- 别只看 pass rate:token/解题、wall time、空转轮次一起报 —— 同模型跨 harness 的 token 消耗实测能差 40 倍[^scaffold-effect]
- decoding 显式传（`--ak temperature=...`)，不吃 harness 默认值

### 参考文献

[^harbor]: harbor-framework. Harbor: framework for evaluating and optimizing agents. [github.com/harbor-framework/harbor](https://github.com/harbor-framework/harbor)

[^harbor-agents]: Harbor Docs. Agents. [harborframework.com/docs/agents](https://www.harborframework.com/docs/agents)

[^epoch]: Epoch AI. SWE-bench Verified methodology. [epoch.ai/benchmarks/swe-bench-verified](https://epoch.ai/benchmarks/swe-bench-verified)

[^scaffold-effect]: Vats & Golev. The Scaffold Effect in Coding Agents: Harness Choice as a Hidden Variable in Coding-Agent Evaluation. KDD Agentic AI Eval Workshop 2026. [paper](https://kdd-eval-workshop.github.io/agenticai-evaluation-kdd2026/assets/papers/74_The_Scaffold_Effect_in_Codi.pdf)

[^v41]: DeepSeek-AI. DeepSeek-V4.1-Flash: Pushing the Limits of KV Cache Compression. 2026.
