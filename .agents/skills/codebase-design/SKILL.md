---
name: codebase-design
description: 设计深模块（deep module）的共享词汇。当用户想设计或改进模块的接口、找深化（deepening）机会、决定 seam 放哪、让代码更可测或更宜 AI 导航，或别的 skill 需要这套深模块词汇时使用。
---

# Codebase Design

设计**深模块（deep module）**：大量行为藏在小接口后面，落在干净的 seam 上，能通过那个接口测试。凡是在设计或重构代码的地方，都用这套语言和这些原则。目标是给调用方杠杆、给维护者局部性、给所有人可测性。

## 术语表

严格使用下列术语：不要换成「组件」「服务」「API」「边界」。语言一致本身就是目的。

**Module**：任何有 interface 和 implementation 的东西。刻意与规模无关：函数、类、包、跨层切片都算。_避免_：unit、component、service。

**Interface**：调用方要正确使用 module 所必须知道的一切：类型签名，还有不变量、顺序约束、错误模式、必需配置、性能特征。_避免_：API、signature——太窄，它们只指类型层面。

**Implementation**：module 内部的东西，它的代码本体。与 **Adapter** 相区别：一个东西可以是小 adapter 大 implementation（一个 Postgres 仓库），也可以是大 adapter 小 implementation（一个内存假实现）。谈的是 seam 时用「adapter」，否则用「implementation」。

**Depth**：interface 处的杠杆。调用方（或测试）每学一单位 interface 能驱动多少行为。大量行为坐在小 interface 后面时 module 是**深（deep）**的；interface 复杂度逼近 implementation 时是**浅（shallow）**的。

**Seam**（Michael Feathers）：不改该处代码就能改变行为的地方；module 的 interface 所在的_位置_。seam 放哪里本身是独立的设计决策，与它后面放什么分开。_避免_：boundary（与 DDD 的 bounded context 撞车）。

**Adapter**：在 seam 处满足 interface 的具体物。描述的是_角色_（填哪个槽），不是实质（里面装了什么）。

**Leverage**：调用方从 depth 拿到的东西。每学一单位 interface 换来更多能力。一个 implementation 回报给 N 个调用点和 M 个测试。

**Locality**：维护者从 depth 拿到的东西。变更、bug、知识、验证集中在一处，而不是摊到各调用方。修一次，处处修好。

## 深 vs 浅

**深 module** = 小 interface + 大量 implementation：

```
┌─────────────────────┐
│   Small Interface   │  ← 方法少、参数简单
├─────────────────────┤
│                     │
│  Deep Implementation│  ← 复杂逻辑藏在里面
│                     │
└─────────────────────┘
```

**浅 module** = 大 interface + 一点 implementation（要避免）：

```
┌─────────────────────────────────┐
│       Large Interface           │  ← 方法多、参数复杂
├─────────────────────────────────┤
│  Thin Implementation            │  ← 只是转发
└─────────────────────────────────┘
```

设计 interface 时问：

- 方法数能不能再减？
- 参数能不能再简化？
- 能不能把更多复杂性藏进内部？

## 原则

- **Depth 是 interface 的属性，不是 implementation 的。**深 module 内部可以由小的、可 mock、可替换的部件组成——它们只是不进 interface。module 除了 interface 处的**外部 seam**，还可以有**内部 seam**（implementation 私有，给自己的测试用）。
- **删除测试。**想象把这个 module 删掉。复杂性若随之消失，它就是个透传；复杂性若在 N 个调用方那里重新冒出来，它就挣到了自己的存在。
- **Interface 就是测试面。**调用方和测试过的是同一个 seam。如果你想_绕过_ interface 测里面的东西，这个 module 的形状多半是错的。
- **一个 adapter 是假想 seam，两个 adapter 才是真 seam。**除非真有东西在 seam 两侧变化，否则别引入 seam。

## 为可测性设计

好 interface 让测试自然发生：

1. **接受依赖，不要自建依赖。**

    ```typescript
    // 可测
    function processOrder(order, paymentGateway) {}

    // 难测
    function processOrder(order) {
        const gateway = new StripeGateway();
    }
    ```

2. **返回结果，不要产生副作用。**

    ```typescript
    // 可测
    function calculateDiscount(cart): Discount {}

    // 难测
    function applyDiscount(cart): void {
        cart.total -= discount;
    }
    ```

3. **小表面积。**方法越少 = 要写的测试越少。参数越少 = 测试搭建越简单。

## 概念关系

- 一个 **Module** 恰有一个 **Interface**（它呈现给调用方与测试的面）。
- **Depth** 是 **Module** 的属性，对着它的 **Interface** 度量。
- **Seam** 是 **Module** 的 **Interface** 所在之处。
- **Adapter** 坐在 **Seam** 上，满足 **Interface**。
- **Depth** 给调用方产出 **Leverage**，给维护者产出 **Locality**。

## 被否掉的框架

- **把 depth 当 implementation 行数对 interface 行数的比率**（Ousterhout）：会奖励给 implementation 注水。我们用「depth 即杠杆」。
- **把 "interface" 当 TypeScript 的 `interface` 关键字或类的公开方法集**：太窄——这里的 interface 包括调用方必须知道的每一个事实。
- **"Boundary"**：与 DDD 的 bounded context 撞车。说 **seam** 或 **interface**。

## 继续深入

- **给定依赖如何深化一簇模块**，见 [DEEPENING.md](DEEPENING.md)：依赖分类、seam 纪律、replace-don't-layer 测试法。
- **探索备选 interface**，见 [DESIGN-IT-TWICE.md](DESIGN-IT-TWICE.md)：并行派 subagent 把 interface 按几种根本不同的方式各设计一遍，再按 depth、locality、seam 位置比较。
