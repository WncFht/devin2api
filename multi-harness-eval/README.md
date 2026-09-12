# multi-harness-eval:同一模型 × 多 harness 评测

目标：把 **swe-2-max**（经 devin-2api / ccload 链路暴露，见 `docs/harness-verification.md`）灌进多个 coding-agent harness，在固定 benchmark 上对比。被测模型恒定，只换 harness。基建用 **Harbor**(Terminal-Bench 团队的通用评测框架，TB 2.x 官方 harness)[^harbor]。

## 1. 端点与协议

链路里有两个入口，不同 harness 各吃一个：

| 入口                                  | 协议                               | 服务对象                                     |
| ------------------------------------- | ---------------------------------- | -------------------------------------------- |
| `http://<ccload>:49173`               | Anthropic Messages(`/v1/messages`) | claude-code、kimi-code、pi                   |
| `http://<devin-2api>:3003`            | OpenAI Responses(`/v1/responses`)  | codex、pi(备选 `model_api=openai-responses`) |
| `api.devin.ai` / `server.codeium.com` | Devin 原生 API                     | devin-cli(不走本地链路，直连官服)            |

**注意：agent 跑在 Docker 容器里，`localhost` 指容器自己。** 宿主机上的服务要写 `http://host.docker.internal:<port>`(Docker Desktop) 或局域网 IP；如果任务的 `[agent]` 网络策略拦了 egress，还要加 `--allow-agent-host=<host>` 或在 task.toml 里放开。

**被测模型恒定 swe-2-max。** 网关侧统一改写模型名，所以各 harness 的 `model_name` 只是过客户端 picker 校验的前台名，不参与实际路由 —— claude-code 要求名字含 claude 家族词，kimi-code 沿用 `kimi-k3` 最稳，pi 和 codex 可以直接写 `swe-2-max`。

## 2. Harness 接入矩阵

四个第三方 harness 全部内置（`harbor agent list` / `harbor agent schema <name>` 可查）[^harbor-agents];`devin-cli` 没有内置，用自定义 agent 接入（见下节）。通用参数：`--ak version=X.Y.Z` 钉版本、`--agent-env KEY=VAL` 注入容器内 agent 进程环境、`--ak`/`--agent-kwarg` 传 agent 级选项。

| Agent                        | 模型名（`-m`)                                                                         | endpoint 接法                                                                                                                                                   | 备注                                                                                                                                                   |
| ---------------------------- | ------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `claude-code`                | `anthropic/claude-sonnet-4-5`（名字须含 claude/opus/sonnet/haiku 家族词，网关侧改写） | `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`                                                                                                                   | 可选 kwargs:`reasoning_effort`、`max_turns`、`append_system_prompt`、`allowed_tools`/`disallowed_tools`、`memory_dir`                                  |
| `kimi-code`                  | `kimi-k3`                                                                             | `KIMI_MODEL_BASE_URL` + `KIMI_MODEL_API_KEY`；可选 `KIMI_MODEL_MAX_CONTEXT_SIZE`、`KIMI_MODEL_CAPABILITIES=image_in,thinking`、`KIMI_MODEL_THINKING_EFFORT=max` | 自带 CC 请求封套伪装，走 Anthropic 端点                                                                                                                |
| `pi`                         | `anthropic/swe-2-max`                                                                 | `ANTHROPIC_BASE_URL` + `ANTHROPIC_API_KEY` + `--ak model_api=anthropic-messages`                                                                                | 自定义端点会自动写 `models.json`;`--ak thinking=<档>`                                                                                                  |
| `codex`                      | `openai/swe-2-max`                                                                    | `OPENAI_BASE_URL=<devin-2api>/v1`(Harbor 写入容器 `config.toml` 的 `openai_base_url`)                                                                           | 原生说 Responses，直连 devin-2api;`--ak reasoning_effort=...`、`web_search=disabled`；复杂配置用 `--ak config=./codex.toml`                            |
| `mini-swe-agent`（可选基线） | `openai/swe-2-max`                                                                    | litellm `api_base` + `OPENAI_API_KEY`                                                                                                                           | 极简 bash-loop，作「harness 下限」参照，Epoch AI 用它做跨模型标准 scaffold[^epoch]；需要 chat-completions 格式，端点只有 responses 时经 LiteLLM 转一道 |
| `devin`                      | `devin/swe-2-max`                                                                     | `DEVIN_API_KEY` env → Harbor 写容器内 `~/.local/share/devin/credentials.toml`（不进 agent 进程 env)                                                             | **swe-2-max 的原生 harness**,Harbor 内置，见下一节                                                                                                     |

### devin 接入

Harbor **内置** `devin` agent(`agents/installed/devin.py`)，不用写自定义适配器：

- 装法：容器里 `curl -fsSL https://cli.devin.ai/install.sh | bash`,`--ak version=<ver>` 钉版本（install.sh 支持 `bash -s -- <version>`)
- 认证：`DEVIN_API_KEY` = 本机 `~/.local/share/devin/credentials.toml` 里的 `windsurf_api_key`（即 `devin-session-token$...`)。Harbor 直接写容器内 credentials.toml,token 不进 agent 进程环境，不落日志
- 跑法：`devin --model swe-2-max --permission-mode yolo --print --respect-workspace-trust false -- <instruction>`；轨迹从 CLI 的 `sessions.db` 解析成 ATIF(`SUPPORTS_ATIF`)，顺带算 ACU 成本（`USD_PER_ACU = 2.0` 估算）
- 语义注意：它是 swe-2-max 的**原厂 harness**，对照组意义大——可以分离「模型能力」和「我们的 devin-2api/ccload 转发链 + 第三方 harness」两层变量。计费走真实 Devin 账户；容器要能出网到 `api.devin.ai`/`server.codeium.com`
- CLI 二进制里有 `DEVIN_HARNESS_LEAD_ONLY`/`SIDEKICK_ONLY` 环境变量（lead/sidekick 多 agent 结构开关），冒烟时值得探一下能否关 sidekick 做单 agent 纯净对比
- 备选 env:`DEVIN_API_SERVER_URL` 可把 CLI 指到别的 API server（官配内部测试用；我们的 devin-2api 说 Responses 协议，不适用）

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

## 6. 采样与超参数

**`n_attempts` 是每题独立 trial 数，不是 agent 内部步数。** 每题跑 $N$ 次，题级得分是 $N$ 次 0/1 的均值，总分是题级得分再对题目取均值（即 avg@N / mean pass@1)。

参照系：

- DeepSeek-V4.1 报告：Terminal-Bench 2.1 用 $N=3$,DeepSWE v1.1 用 $N=8$;temperature 1.0、top_p 0.95、max_steps 500、1M 上下文、TB 断网[^v41]
- Terminal-Bench 2.1 官方榜单提交要求每题至少 **5 次** trial[^tb]

$N=3$ 是论文里的成本选择，不是统计上够用的选择：题级得分只能取 $\{0, 1/3, 2/3, 1\}$ 四个值，粒度粗；而且 V4.1 报告里 harness 之间的差距本来就只有几个百分点（TB 2.1 上 84.1–90.6),$N=3$ 下这个差距在噪声范围内。二项分布下，真值 $p=0.85$ 的题 $N=3$ 的题级标准差约 0.21,90 题均值的 95% 置信区间约 ±4.5pp —— 够用但勉强；$N=5$ 收窄到 ±3.5pp,$N=8$ 到 ±2.8pp。

分档建议（TB 2.1 约 90 题，跑 1 次 = 90 trial):

| 用途          | n_attempts | 说明                                   |
| ------------- | ---------- | -------------------------------------- |
| smoke         | 1          | 只验证链路通                           |
| 主力对比      | **3**      | 对齐 V4.1 TB2.1 协议；差距 <5pp 不解读 |
| 加强结论/对外 | 5–10       | TB 官方下限 5,DeepSWE 档 8             |

当前 job 用 $N=3$：和 V4.1 报告同协议，数值可直接对表。要知道它的代价——CI 约 ±4.5pp,harness 间小差距区分不开；需要更硬的结论时升到 5（官方下限）或 8。

其它维度：

- **decoding**：五个 harness 在 Harbor 0.22.0 里都**没有** temperature/top_p kwarg —— 但这不是问题：客户端不发这两个字段时，devin-2api 网关用默认值 `temperature=1, top_p=0.95`（见 `internal/adapter/devin/devin.go`)，恰好等于 V4.1 报告值，且对四个第三方 harness 一致生效。devin CLI 直连 Devin 后端，decoding 不可控 —— 记为「原生默认」，这正是把它当参照系的意义
- **推理强度**:SWE-2 的 effort 编码在 model UID 里 —— `devin models list` 实测有 `swe-2-medium` / `swe-2-high` / `swe-2-max` 三档，**`swe-2-max` 本身已是最高档**,devin 侧无需再调。客户端侧的 effort 旋钮（cc `reasoning_effort`、codex `reasoning_effort`、kimi `KIMI_MODEL_THINKING_EFFORT`、pi `thinking`）只影响 harness 本地行为，不透传到上游 —— job 里统一顶格（`max`/`xhigh`)，但注意这只是客户端设置，真正决定推理量的是模型 UID。**effort 扫档 = 换 model_name 到 `swe-2-high`/`swe-2-medium`**（第三方 harness 需网关支持对应 UID 改写），这是后续一组独立实验维度
- **步数预算**:V4.1 的 max_steps=500 是模型生成轮数，不是 tool call 数也不是 wall time。cc 有 `max_turns` kwarg 可显式对齐；其它家没有对应口，靠 `override_timeout_sec` 兜底。比较时从 ATIF 轨迹里读实际 turns，谁提前触顶要标出来
- **并发不是实验变量**：每个 trial 独立容器，并发只影响墙钟和上游限流；但限流触发重试会污染结果，所以 devin 单独限 `n_concurrent: 2`（真实计费），其余按配额给。跑完把实际并发记进结果元数据
- **配对比较**：所有 harness 跑同一套题，差异分析按题配对（per-task 差值的 bootstrap 区间）比两个独立 pass rate 的差更省样本 —— 题目难度这项方差被配对消掉了

报告时除 pass rate 外一起给：题级 $N$ 次结果明细、bootstrap 95% CI、prompt/cached/completion tokens、wall time、turns、触顶/超时次数。

## 7. 实验纪律

- 版本钉死：`--ak version=X.Y.Z` 或 job yaml 里写明，结果里记录每个 harness 的精确版本（V4.1 报告里 Claude Code 四个小版本分数都不同[^v41])
- 采样对齐 V4.1:`n_attempts: 3`(TB 2.1)；要更硬结论升 5/8，见 §6 分档
- 别只看 pass rate:token/解题、wall time、空转轮次一起报 —— 同模型跨 harness 的 token 消耗实测能差 40 倍[^scaffold-effect]
- decoding 由 devin-2api 网关默认（temp 1 / top_p 0.95，对齐 V4.1);harness 侧无统一 kwarg，见 §6

### 参考文献

[^harbor]: harbor-framework. Harbor: framework for evaluating and optimizing agents. [github.com/harbor-framework/harbor](https://github.com/harbor-framework/harbor)

[^harbor-agents]: Harbor Docs. Agents. [harborframework.com/docs/agents](https://www.harborframework.com/docs/agents)

[^epoch]: Epoch AI. SWE-bench Verified methodology. [epoch.ai/benchmarks/swe-bench-verified](https://epoch.ai/benchmarks/swe-bench-verified)

[^scaffold-effect]: Vats & Golev. The Scaffold Effect in Coding Agents: Harness Choice as a Hidden Variable in Coding-Agent Evaluation. KDD Agentic AI Eval Workshop 2026. [paper](https://kdd-eval-workshop.github.io/agenticai-evaluation-kdd2026/assets/papers/74_The_Scaffold_Effect_in_Codi.pdf)

[^v41]: DeepSeek-AI. DeepSeek-V4.1-Flash: Pushing the Limits of KV Cache Compression. 2026.

[^tb]: Terminal-Bench. terminal-bench-2.1 submission guide. [github.com/laude-institute/terminal-bench](https://github.com/laude-institute/terminal-bench)
