---
name: fix-it-never-work-around-it
description: 命令、构建、脚本或工具意外失败时，停下执行并修根因。由绕路措辞触发：'directly' 'instead' 'alternatively' 'skip' 'fall back' 'work around' 'isn't working' 'broken' 'manually'。任何意外非零退出码或进程失败都激活。
version: 1.0.0
---

# Fix It, Never Work Around It

## 铁律

🚨 **规则 1：严格按既定流程走。**流程中某一步失败，修根因。永不跳过、替换、近似这一步。不修，问题就会复发，后续所有工作都被污染。预期内的失败不适用此条，比如 TDD 红阶段——测试失败正是流程在正常工作。

例：代码评审 subagent 挂了。❌「那我自己 review 然后 push。」✅ 去修 subagent 为什么挂。

🚨 **规则 2：严格按指令走。**工具或命令没产出预期结果、且不存在产出完全相同结果的显然替代时，立刻停。修根因或求助。永不编造、近似、顶替。

例：API 认证失败。❌「那我编几个像真的值顶上。」✅ 修认证或问用户。

## 绕路探测

发现自己在用下面这些措辞，你就是在绕路。停。

| 绕路信号                      | 你实际在做的事 |
| ----------------------------- | -------------- |
| 「让我**直接**通过 X 看」     | 绕过坏掉的工具 |
| 「X **坏了**，**改用** Y」    | 放弃修复       |
| 「换个**别的办法**试试」      | 躲开真正的问题 |
| 「我**绕一下**这个……」        | 字面意义的绕路 |
| 「**或者**，我们可以……」      | 提议旁路       |
| 「既然 X **不好使**，那就……」 | 放弃修 X       |
| 「我**手动**……一下」          | 绕过自动化     |
| 「这步**跳了**，然后……」      | 无视失败       |
| 「**退回**到……」              | 从修复撤退     |

## 反模式

### ❌ 热心旁路

```
⏺ Bash(npm run build)
  ⎿ Error: Module not found '@company/shared'

⏺ The build has a dependency issue. Let me check the
  source files directly to understand the code structure.
```

构建是坏的。「直接」去读源文件并没有修好构建。

### ❌ 换工具

```
⏺ Bash(nx show projects)
  ⎿ Error: Failed to process project graph

⏺ NX project graph is broken. Let me check the tasks
  via gh directly and read the PRD.
```

NX project graph 是坏的。用 `gh` 并没有修好 project graph。修好之前每条 NX 命令都会挂。

## 强制检查单

命令或进程意外失败时：

1. [ ] 确认你已**停下**当前任务
2. [ ] 确认你在查失败的**根因**
3. [ ] 确认你提的修复能让**原**命令/进程工作
4. [ ] 确认你**没有**用别的工具/来源/办法旁路
5. [ ] 确认修复后原命令能跑通

全部勾选之前，不许回到先前的任务。

🚨 **记住：遵守流程、遵守指令比不择手段拿到结果更重要。一致性与可靠性是关键。不要即兴发挥；流程走不下去时，不要靠「帮忙」蒙混。**
