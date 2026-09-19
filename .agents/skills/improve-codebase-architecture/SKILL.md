---
name: improve-codebase-architecture
description: 扫描代码库找深化（deepening）机会，以可视化 HTML 报告呈现，再对你选中的那个进入 grilling 追问。
disable-model-invocation: true
---

# Improve Codebase Architecture

暴露架构摩擦并提出**深化机会**：把浅 module 变深的重构。目标是可测性与 AI 可导航性。

本命令由项目的领域模型_喂养_，建在一套共享设计词汇上：

- 用 Skill 工具调 "codebase-design" 取架构词汇（**module**、**interface**、**depth**、**seam**、**adapter**、**leverage**、**locality**）与它的原则（删除测试、「interface 就是测试面」、「一个 adapter = 假想 seam，两个 = 真 seam」）。每条建议里严格使用这些术语，别漂到「组件」「服务」「API」「边界」去。
- `CONTEXT.md` 的领域语言给好 seam 提供了名字；`docs/adr/` 里的 ADR 记着本命令不该重新翻炒的决策。

## 流程

### 1. 探索

**扫描前先定范围：YAGNI。**深化 module 的回报是让未来的变更更容易，所以给近期改动过的部分额外加权。看之前先决定_看哪_：

- 用户点了方向（某个模块、子系统、痛点）就接过来，跳过下面的推断。
- 否则回翻一段足够长的提交历史（`git log --oneline`）找代码库的热点——反复出现的文件与区域——让这些路径先拉住你的注意力。改动若分散无热点，把网撒宽。

先读项目的领域词汇表（`CONTEXT.md`）和你动手区域的 ADR。

然后派一个 subagent 走代码库。不要套死板启发式；有机地探索，记下你感到摩擦的地方：

- 哪里理解一个概念要在许多小 module 之间反复横跳？
- 哪些 module 是**浅**的——interface 复杂度逼近 implementation？
- 哪些纯函数只为可测性被抽出来，而真正的 bug 藏在它们的调用方式里（没有 **locality**）？
- 哪些紧耦合 module 从自己的 seam 往外漏？
- 代码库哪些部分没测试，或透过现有 interface 难以测试？

对任何疑似浅的东西施加**删除测试**：删掉它，复杂性会集中起来还是只是挪窝？「会集中」就是你要的信号。

### 2. 把候选做成 HTML 报告

写一个自包含 HTML 文件到 OS 临时目录，不让任何东西落进仓库。临时目录取 `$TMPDIR`，退回 `/tmp`（Windows 上 `%TEMP%`），写到 `<tmpdir>/architecture-review-<timestamp>.html`，每次跑都是新文件。替用户打开它（Linux `xdg-open <path>`、macOS `open <path>`、Windows `start <path>`）并告诉他们绝对路径。

报告用 **CDN 引入的 Tailwind** 做布局样式，用 **CDN 引入的 Mermaid** 画那些图/流/时序确实能讲清结构的图。Mermaid 与手写 CSS/SVG 视觉混用：关系是图状时（调用图、依赖、时序）用 Mermaid，想要更编辑感的东西（体量图、剖面图、坍缩动画）用手搭 div/SVG。每个候选配一张 **before/after 可视化**。要可视化。

每个候选渲一张卡片：

- **Files**：涉及哪些文件/模块
- **Problem**：当前架构为什么造成摩擦
- **Solution**：会变什么的平实描述
- **Benefits**：用 locality 与 leverage 解释，以及测试会怎样变好
- **Before / After 图**：并排、定制绘制，呈现浅与深化
- **Recommendation strength**：`Strong`、`Worth exploring`、`Speculative` 之一，渲成徽章

报告以 **Top recommendation** 一节收尾：你会先动哪个候选、为什么。

**领域用 CONTEXT.md 词汇，架构用 `/codebase-design` 词汇。**若 `CONTEXT.md` 定义了「Order」，就说「Order intake module」，而不是「FooBarHandler」，也不是「Order service」。

**ADR 冲突**：候选与现有 ADR 冲突时，只有摩擦真实到值得重开 ADR 才提出来。在卡片里清楚标记（比如琥珀色警示框：_「与 ADR-0007 冲突，但值得重开，因为……」_）。别把 ADR 禁止的每个理论重构都列一遍。

完整 HTML 骨架、图示模式与样式指引见 [HTML-REPORT.md](HTML-REPORT.md)。

先**不要**提 interface 方案。文件写完后问用户：「你想深入哪一个？」

### 3. Grilling 回路

用户选定候选后，用 Skill 工具调 "grilling" 陪用户走决策树：约束、依赖、深化后 module 的形状、seam 后面坐什么、哪些测试活下来。

副作用随决策成形就地发生；用 Skill 工具调 "domain-modeling" 让领域模型保持最新：

- **给深化后的 module 起了 `CONTEXT.md` 里没有的概念名？**把该词补进 `CONTEXT.md`。文件不存在就惰性创建。
- **对话中把模糊的词磨清了？**当场更新 `CONTEXT.md`。
- **用户因承重理由否掉候选？**提议记一条 ADR，措辞：_「要不要我把这个记成 ADR，省得以后的架构评审再提一遍？」_只有当理由真是未来探索者避免重提所需要时才提议；ephemeral 的理由（「现在不值当」）与不证自明的跳过。
- **想为深化后的 module 探索备选 interface？**用 Skill 工具调 "codebase-design"，走它的 design-it-twice 并行 subagent 模式。
