# Deepening

给定一簇浅 module 的依赖情况，如何安全地深化它。假定你已读过 [SKILL.md](SKILL.md) 的词汇：**module**、**interface**、**seam**、**adapter**。

## 依赖分类

评估一个深化候选时，先给它的依赖分类。类别决定深化后的 module 隔着 seam 怎么测。

### 1. 进程内（In-process）

纯计算、内存状态、无 I/O。永远可深化：合并这些 module，直接通过新 interface 测。不需要 adapter。

### 2. 本地可替代（Local-substitutable）

有本地测试替身的依赖（Postgres 用 PGLite、文件系统用内存版）。替身存在即可深化。深化后的 module 在测试套件里带着替身跑。seam 是内部的；module 的外部 interface 上没有 port。

### 3. 远程但自有（Ports & Adapters）

跨网络边界的自有服务（微服务、内部 API）。在 seam 上定义 **port**（interface）。深 module 持有逻辑；传输层作为 **adapter** 注入。测试用内存 adapter，生产用 HTTP/gRPC/队列 adapter。

建议措辞：_「在 seam 上定义一个 port，生产实现 HTTP adapter、测试实现内存 adapter，这样逻辑坐在一个深 module 里，哪怕它部署跨了网络。」_

### 4. 真外部（Mock）

你控制不了的第三方服务（Stripe、Twilio 等）。深化后的 module 把外部依赖当注入的 port 接收；测试提供 mock adapter。

## Seam 纪律

- **一个 adapter 是假想 seam，两个 adapter 才是真 seam。**除非至少有两个 adapter 站得住脚（典型是生产 + 测试），否则别引入 port。单 adapter 的 seam 只是多一层间接。
- **内部 seam vs 外部 seam。**深 module 可以有内部 seam（implementation 私有、给自己的测试用），也有 interface 处的外部 seam。别因为测试在用内部 seam 就把它从 interface 暴露出去。

## 测试策略：替换，不叠加

- 浅 module 上的旧单元测试，在深化后 module 的 interface 测试就位后就成了废物——删掉。
- 新测试写在深化后 module 的 interface 上。**Interface 就是测试面。**
- 测试断言的是透过 interface 可观察的结果，不是内部状态。
- 测试应该扛得住内部重构，因为它描述的是行为不是实现。实现一改测试就得跟着改的测试，是在隔着 interface 测里面的东西。
