# Design It Twice

当用户想为选定的深化候选探索备选 interface 时，用这个并行 subagent 模式。出自 "Design It Twice"（Ousterhout）：你的第一个想法几乎不可能是最优。

使用 [SKILL.md](SKILL.md) 的词汇：**module**、**interface**、**seam**、**adapter**、**leverage**。

## 流程

### 1. 框定问题空间

派 subagent 之前，为选定的候选写一段面向用户的问题空间说明：

- 任何新 interface 必须满足的约束
- 它会依赖什么、各属哪一类（见 [DEEPENING.md](DEEPENING.md)）
- 一个粗略的示意代码草图，用来把约束落到实处——不是提案，只是把约束变具体的手段

把这段给用户看，然后立刻进第 2 步。subagent 并行干活时，用户在读在想。

### 2. 派 subagent

并行派 3 个以上 subagent。每个都必须为深化后的 module 产出一个**根本不同**的 interface。

每个 subagent 各给一份独立技术简报（文件路径、耦合细节、[DEEPENING.md](DEEPENING.md) 里的依赖类别、seam 后面坐着什么）。简报与第 1 步面向用户的问题空间说明相互独立。给每个 agent 一个不同的设计约束：

- Agent 1：「最小化 interface：目标至多 1–3 个入口。最大化每入口的 leverage。」
- Agent 2：「最大化灵活性：支持尽量多的用例与扩展。」
- Agent 3：「为最高频调用方优化：让默认情形 trivial。」
- Agent 4（如适用）：「围绕 ports & adapters 设计跨 seam 依赖。」

简报里同时带上 [SKILL.md](SKILL.md) 词汇与 CONTEXT.md 词汇，让每个 subagent 命名时既贴合架构语言也贴合项目领域语言。

每个 subagent 输出：

1. Interface（类型、方法、参数，外加不变量、顺序、错误模式）
2. 展示调用方怎么用的使用示例
3. seam 后面藏了什么实现
4. 依赖策略与 adapter（见 [DEEPENING.md](DEEPENING.md)）
5. 取舍：哪里 leverage 高，哪里薄

### 3. 呈现并比较

逐个呈现设计让用户消化，再用文字比较。按 **depth**（interface 处的 leverage）、**locality**（变更集中到哪）、**seam 位置**三个维度对照。

比较完给出你自己的推荐：你认为哪个设计最强、为什么。不同设计的元素若能合得好，提一个混合方案。要有主见：用户要的是强判断，不是菜单。
