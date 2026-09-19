---
name: code-review
description: 沿两根轴评审自某个固定点（commit、branch、tag 或 merge-base）以来的变更——规范轴看代码是否遵循本仓已文档化的编码规范，Spec 轴看代码是否实现原始 issue/spec 所要求的行为。两路评审在并行 subagent 中运行并并列汇报。当用户想评审分支、PR、进行中的改动，或说 'review since X' 'code review' 'review this branch' 时使用。
---

# Code Review

对 `HEAD` 与用户给出的固定点之间的 diff 做两轴评审：

- **规范**：代码是否符合本仓文档化的编码规范？
- **Spec**：代码是否忠实实现了原始 issue / spec？

两根轴各跑一个**并行 subagent**，互不污染上下文，再由本 skill 汇总两边发现。

issue tracker 应该已经提供给你了。如果 `docs/agents/issue-tracker.md` 缺失，让用户去跑 `/setup-matt-pocock-skills`。

## 流程

### 1. 钉住固定点

用户说什么，什么就是固定点（commit SHA、分支名、tag、`main`、`HEAD~5` 等）。没说就问。

diff 命令记一次就够：`git diff <fixed-point>...HEAD`（三点写法，比较对象是 merge-base）。同时用 `git log <fixed-point>..HEAD --oneline` 记下提交清单。

继续之前，先确认固定点能解析（`git rev-parse <fixed-point>`）且 diff 非空。坏 ref 或空 diff 要在这里就失败，而不是进了两个并行 subagent 才炸。

### 2. 找 spec 来源

按这个顺序找原始 spec：

1. 提交信息里的 issue 引用（`#123`、`Closes #45`、GitLab `!67` 等），按 `docs/agents/issue-tracker.md` 的流程取回。
2. 用户当参数传进来的路径。
3. `docs/`、`specs/`、`.scratch/` 下与分支名或特性对得上的 spec 文件。
4. 都没有就问用户 spec 在哪。回答说没有，**Spec** subagent 就跳过，报「无 spec 可用」。

### 3. 找规范来源

仓里任何写明「代码该怎么写」的文档，比如 `CODING_STANDARDS.md`、`CONTRIBUTING.md`。

在仓库文档之上，规范轴永远背着下面的**异味基线**：一组固定的 Fowler 代码异味（《重构》第 3 章），仓库什么都没写时也适用。两条规则管着它：

- **仓库说了算。**文档化的仓库规范永远赢；它认可的东西基线要标时，压掉这个异味。
- **永远是判断题。**每个异味都是贴了标签的启发式（「疑似 Feature Envy」），不是硬违规。和这里其他规范一样，工具已经管的事跳过。

每个异味按「是什么 → 怎么修」读，拿去对 diff：

- **Mysterious Name**：函数、变量或类型的名字看不出它做什么、装什么。→ 改名；起不出诚实的名字，说明设计本身是糊的。
- **Duplicated Code**：同一逻辑形状出现在变更的多个 hunk 或多个文件里。→ 抽出共享形状，两处都调它。
- **Feature Envy**：方法伸手摸别的对象的数据多过用自己的。→ 把方法挪到它羡慕的那份数据上。
- **Data Clumps**：同样几个字段或参数总是结伴出行（一个想出生的类型）。→ 捆成一个类型，传它。
- **Primitive Obsession**：拿 primitive 或字符串顶替该有自己类型的领域概念。→ 给这个概念一个小类型。
- **Repeated Switches**：同一类型上的同一 `switch`/`if` 级联在变更里反复出现。→ 换多态，或者两处共享一张 map。
- **Shotgun Surgery**：一个逻辑变更逼得 diff 里一堆文件散开改。→ 把要一起变的收进一个模块。
- **Divergent Change**：一个文件或模块因为好几个不相干的原因被改。→ 拆开，让每个模块只为一个原因而变。
- **Speculative Generality**：为 spec 没有的需求加的抽象、参数、钩子。→ 删；内联回去，等真实需求现身。
- **Message Chains**：调用方不该依赖的长 `a.b().c().d()` 导航。→ 把这段路藏进第一个对象的一个方法里。
- **Middle Man**：基本只往下转手的类或函数。→ 砍掉，直接调真目标。
- **Refused Bequest**：把继承来的东西大部分忽略或覆写掉的子类/实现者。→ 别继承了，用组合。

### 4. 并行派两个 subagent

**规范 subagent 的 prompt** 要含：

- 完整 diff 命令与提交清单。
- 第 3 步找到的规范来源文件清单，**外加第 3 步异味基线全文**原样粘贴（subagent 没有别的途径拿到它）。
- 任务书：「按文件/hunk 报告（相关处）：(a) diff 违反文档化规范的每一处——引用规范（文件 + 条目）；(b) 你发现的每个基线异味——点名异味并引用 hunk。区分硬违规与判断题：文档化规范的违反可以是硬违规，但基线异味永远是判断题，且文档化的仓库规范压过基线。工具已管的跳过。400 词以内。」

**Spec subagent 的 prompt** 要含：

- diff 命令与提交清单。
- spec 的路径或取回的内容。
- 任务书：「报告：(a) spec 要求但缺失或只做了一半的需求；(b) diff 里没人要求的行为（scope creep）；(c) 看似实现了但实现看起来不对的需求。每条发现引用 spec 原句。400 词以内。」

spec 缺失就跳过 Spec subagent，并在最终报告里注明。

### 5. 汇总

两份报告分别放在 `## Standards` 和 `## Spec` 标题下，原样或轻度清理。**不要**合并或重排发现，因为两根轴刻意分开（见「为什么是两根轴」）。

末尾一行总结：每轴发现总数，以及_各轴内部_最重的那个问题（如果有）。别跨轴选唯一冠军——那正是分开汇报要防的重排。

## 为什么是两根轴

一个变更可能过一轴挂一轴：

- 条条规范都守但实现错了东西的代码 → **规范过，Spec 挂。**
- 分毫不差做了 issue 要的但破坏项目约定的代码 → **Spec 过，规范挂。**

分开汇报，一轴才不会盖住另一轴。
