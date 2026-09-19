# HTML 报告格式

架构评审渲成 OS 临时目录里的单个自包含 HTML 文件。Tailwind 与 Mermaid 都从 CDN 引入。Mermaid 可靠地处理图状图示；手搭 div 与内联 SVG 处理更编辑感的视觉（体量图、剖面图）。两者混用：别什么都靠 Mermaid，会开始显得千篇一律。

## 骨架

```html
<!doctype html>
<html lang="en">
    <head>
        <meta charset="utf-8" />
        <title>Architecture review for {{repo name}}</title>
        <script src="https://cdn.tailwindcss.com"></script>
        <script type="module">
            import mermaid from "https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.esm.min.mjs";
            mermaid.initialize({
                startOnLoad: true,
                theme: "neutral",
                securityLevel: "loose",
            });
        </script>
        <style>
            /* Tailwind 覆盖不干净的小自定义层：
               虚线 seam、手绘感箭头尖等 */
            .seam {
                stroke-dasharray: 4 4;
            }
            .leak {
                stroke: #dc2626;
            }
            .deep {
                background: linear-gradient(135deg, #0f172a, #1e293b);
            }
        </style>
    </head>
    <body class="bg-stone-50 text-slate-900 font-sans">
        <main class="max-w-5xl mx-auto px-6 py-12 space-y-12">
            <header>...</header>
            <section id="candidates" class="space-y-10">...</section>
            <section id="top-recommendation">...</section>
        </main>
    </body>
</html>
```

## 头部

仓库名、日期、紧凑图例：实心框 = module、虚线 = seam、红箭头 = 泄漏、深色粗框 = 深 module。不要引言段落，直接进候选。

## 候选卡片

图承担主要表达。文字稀疏、平实，不带仪式地使用术语表词（来自 `/codebase-design` skill）。

每个候选一个 `<article>`：

- **标题**：短，点名这次深化（如「Collapse the Order intake pipeline」）。
- **徽章行**：推荐强度（`Strong` = emerald、`Worth exploring` = amber、`Speculative` = slate），加依赖类别标签（`in-process`、`local-substitutable`、`ports & adapters`、`mock`）。
- **Files**：等宽列表，`font-mono text-sm`。
- **Before / After 图**：核心。两列并排。模式见下。
- **Problem**：一句话。哪里疼。
- **Solution**：一句话。变什么。
- **Wins**：要点列，每条 ≤6 词。如 "Tests hit one interface"、"Pricing logic stops leaking"、"Delete 4 shallow wrappers"。
- **ADR 提示框**（如适用）：琥珀色底框里一行。

不要成段解释。图若需要一段话才能看懂，重画图。

## 图示模式

挑贴合候选的模式。混着用。别让每张图长一样——多样性本身是目的。

### Mermaid 图（依赖 / 调用流的主力）

要点是「X 调 Y 调 Z，看这团乱」时用 Mermaid `flowchart` 或 `graph`。包在 Tailwind 卡片里免得显得空降。用 classDef 把泄漏边染红、深 module 染深色。时序图适合「before：6 次往返；after：1 次」。

```html
<div class="rounded-lg border border-slate-200 bg-white p-4">
    <pre class="mermaid">
    flowchart LR
      A[OrderHandler] --> B[OrderValidator]
      B --> C[OrderRepo]
      C -.leak.-> D[PricingClient]
      classDef leak stroke:#dc2626,stroke-width:2px;
      class C,D leak
  </pre>
</div>
```

### 手搭方块箭头（Mermaid 布局跟你作对时）

module 用带边框和标签的 `<div>`。箭头用内联 SVG `<line>` 或 `<path>`，绝对定位在 relative 容器上。想让 after 图呈现「一个粗框深 module、内部灰掉」的分量时用这招——Mermaid 渲不出那种重量感。

### 剖面图（适合分层浅薄）

横带堆叠（`h-12 border-l-4`）展示一次调用穿过的层。Before：6 条薄带各无所事事。After：1 条厚带，标上合并后的职责。

### 体量图（适合「interface 与 implementation 一样宽」）

每个 module 两个矩形：一个 interface 表面积，一个 implementation。Before：interface 矩形几乎与 implementation 矩形等高（浅）。After：interface 矩形矮、implementation 矩形高（深）。

### 调用图坍缩

Before：函数调用树渲成嵌套方块。After：同一棵树坍缩进一个方块，已变内部的调用在里面淡显。

## 样式指引

- 走编辑感，不走企业仪表盘感。留白慷慨。标题可用衬线（`font-serif` 配 stone/slate 效果好）。
- 用色克制：一个强调色（emerald 或 indigo）加红色表泄漏、琥珀色表警告。
- 图高约 320px，before/after 并排不滚动也能看舒服。
- 图内 module 标签用 `text-xs uppercase tracking-wider`，读起来是示意图不是 UI。
- 脚本只有 Tailwind CDN 与 Mermaid ESM import 两个。报告其余部分纯静态：无应用代码，除 Mermaid 自身渲染外无交互。

## Top recommendation 一节

一张更大的卡片。候选名、一句为什么、指向其卡片的锚链接。就这些。

## 语气

平实、简洁，但架构名词动词严格取自 `/codebase-design` skill。简洁不是漂移的借口。

**严格使用：** module、interface、implementation、depth、deep、shallow、seam、adapter、leverage、locality。

**永不替换：** component、service、unit（代 module）· API、signature（代 interface）· boundary（代 seam）· layer、wrapper（你想说 module 时）。

**合风格的措辞：**

- "Order intake module is shallow: interface nearly matches the implementation."
- "Pricing leaks across the seam."
- "Deepen: one interface, one place to test."
- "Two adapters justify the seam: HTTP in prod, in-memory in tests."

**Wins 要点**用术语表词点名收益：_"locality: bugs concentrate in one module"_、_"leverage: one interface, N call sites"_、_"interface shrinks; implementation absorbs the wrappers"_。别写 _"easier to maintain"_、_"cleaner code"_——这些词不在术语表里，挣不到位置。

不闪烁其词、不垫场、不写 "it's worth noting that…"。句子能变要点就变要点，要点能删就删。术语表里没有的词，先用表里的，别造新的。
