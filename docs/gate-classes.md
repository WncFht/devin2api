# 闸门分级准入（fg/bg 请求类）

速率闸门（`internal/adapter/devin/rategate.go`，模型见 `upstream-rate-limit.md`）把发往上游的消息流整形到自然分钟桶：可发区间内 `bucketUsed < quota` 即放行，否则睡到下一窗口或快败。请求类（class）在这条准入判定上叠加一层类别语义——`fg`（前台，交互会话，人在等）与 `bg`（后台，无人值守批跑），目标是「fg 永远有保底余量、bg 把 fg 不用的配额吃干净」两件事同时成立。

## 设计动机

此前 texlate 的 bench 批跑靠客户端旁挂调度器轮询 `/admin/accounts` 自我节流。客户端方案有三个结构性上限：两个礼貌客户端各自估算对方需求，窗口边界同时放行即撞车，而被拒也计上游额度；桶尾配额不用即废，客户端必须留安全边际，网关自己知道精确 `bucketUsed`/`waiters` 才能把尾槽放出去；请求无类别，fg 与 bg 排队同权。把调度下沉进闸门，本质是把预测从多个分布式估计器挪到唯一持真值的写者手里——`bucketUsed`、`waiters_fg`、fg 实际准入速率在同一把 `gate.mu` 下都是精确值。

## class 判定与传播

分类绑定在下游令牌上，不绑路径、模型或请求头。`auth_tokens` 表 `class` 列取值 `fg`（默认）| `bg`；`POST /admin/auth-tokens` 与 `PUT /admin/auth-tokens/{id}` 接受 `class` 参数，令牌列表回显。存量行、匿名通道、未标 class 的令牌一律 fg——fg 准入规则与旧行为字节级一致，bg 的全部差异是新增的前置约束，不是对原逻辑的改写。

`createCompletion` 解析出令牌后把 class 随 `*adapter.GateContext` 挂进请求 ctx（`internal/adapter`），chat 主路径、托管 websearch、reopen/extend 续试、WS 轮次内层请求共用同一 ctx，一次注入全链生效。class 同时落进 `RequestMeta`（`meta.json` 的 client 块与 `/admin/active-requests` 的 `class` 字段）。

## 准入规则

fg：`sendable && bucketUsed < quota`——与未分级时完全一致。

bg：在 fg 规则上叠加两层约束——

- 动态预留：`bucketUsed + 1 <= quota − reserve`，最后 reserve 个槽对 bg 数学上不可达。
- 窗口内爬坡：`bucketUsedBg + 1 <= ceil((quota − reserve) × 已过秒数 / 可发区间秒数)`——bg 放行额度从窗口开放起按经过时间线性放出（首槽开放后立即可用，末尾恰好收敛到 quota − reserve），压住 :02 齐射，fg 在窗口前段到达看到的是半空的桶；bg 吞吐不变，只是被摊匀到整个可发区间。

其中

```
reserve = fg_rate × 可发区间剩余秒数 + waiters_fg + margin
```

- `fg_rate` 是本 lane 的 fg 准入速率估计：每窗口 fg 准入数的指数滑动平均（EMA，半衰期约 3 个窗口）折算成条/秒。它预测「本桶剩余时间里 fg 还会来多少」。
- `waiters_fg` 是当前睡在闸内的 fg 请求数——上一桶没挤上的 fg 在新窗口的既得需求，显式计入。
- `margin` 是固定安全边际（`gate_bg_reserve_margin`，默认 4），吸收 EMA 滞后与 fg 小并发突发。

动态预留同时给出两条性质。保证性：最后 `reserve` 个槽对 bg 数学上不可达，fg 任何时刻到达都有保底余量，且窗口开放瞬间 `reserve ≥ waiters_fg + margin`，睡过一桶的 fg waiter 人人有槽——类别保证靠容量预留实现，不靠唤醒顺序（本闸门的等待本就不是有序队列，睡醒者与新到者同刻竞争）。工作保守：`t_left → 0` 时 `reserve → waiters_fg + margin`，fg 没来的预测需求自动归零，bg 在桶尾吃掉剩余槽，配额不死在静态余量里。

bg 被预留或爬坡挡住（桶未满、非死区）时不睡到下一窗口，而是按短间隔（4s）睡醒重查——预留随时间衰减、爬坡额度随经过时间释放，中段让出的槽 bg 能及时吃到。bg 因桶满/死区被拒与 fg 同形（睡到下一窗口）；排队预算用 `gate_bg_max_hold_seconds`（默认 120，fg 仍 `gate_max_hold_seconds`，默认 30）——无人值守负载等得起。bg 快败时 `Retry-After` 给到下一窗口开放秒数，闩内被拒照旧给闩剩余。

前缀保温 ping（`tryAdmit`）视同可 dip 入预留的最低优先级流量：准入条件为 `sendable && bucketUsed < quota`，不再要求 `waiters == 0`——bg 常驻排队不该饿死保温（缓存冷的是 fg），ping 占用预留槽的规模被 ping 节拍（默认 180s/谱系）天然限制在 margin 吸收范围内。

## 响应头

每次真实发送通过闸门后，响应携带 `X-Gate-*` 遥测（首字节写出前 stamp，流式与错误路径同生效）：

| 头                                           | 含义                                                                          |
| -------------------------------------------- | ----------------------------------------------------------------------------- |
| `X-Gate-Lane`                                | 实际服务的 lane 名（单 lane 部署可空）                                        |
| `X-Gate-Class`                               | 回显分类 `fg`/`bg`（含未过闸门的准入拒绝响应）                                |
| `X-Gate-Window-Used` / `X-Gate-Window-Quota` | 准入时该 lane 桶用量/配额                                                     |
| `X-Gate-Window-Reset`                        | 到下一窗口开放的秒数                                                          |
| `X-Gate-Wait-Ms`                             | 本次在闸内排队耗时（多次发送累计）                                            |
| `X-Gate-Reason`                              | 仅 429：`latch`（冷却闩）\| `quota`（桶满/预留不足）\| `hold`（排队预算耗尽） |

bg 客户端拿到 `quota` 型 429 直接睡满 `Retry-After`；成功响应的 used/quota/reset 可用于校准自己的余量模型，不再需要轮询 `/admin/accounts`。

## 观测

`GateStats`（`/admin/accounts` 的 `gate` 段、`/admin/runtime-metrics` 的 `gate`/`accounts` 段）新增：`window_used_fg`/`window_used_bg`（桶用量按类分列，验证 bg 未吃 fg 预留）、`waiters_fg`/`waiters_bg`（`waiters` 仍为两者之和）、`reject_bg_reserve_count`（因预留/爬坡让路被拒掉的 bg 数，礼让强度直接指标）、`reserve`/`fg_rate`（当前预留量与 fg 速率估计，预留行为的可解释性来源）、`pace_allowance`（爬坡此刻为 bg 释放的额度上限，死区/零配额为 0）。

## 配置

```yaml
gate_bg_max_hold_seconds: 120 # bg waiter 最长闸内排队（fg 仍走 gate_max_hold_seconds，默认 30）
gate_bg_reserve_margin: 4 # reserve 公式中的固定安全边际
```

## 失败语义与边界

bg 的全部新失败模式都只发生在本地闸门内，不触达上游——fg 持续占满某 lane 时 bg 在该 lane 饿死（本地快败循环），不产生上游 `resource_exhausted`，不连累 lane 上闩。fg 的最坏情形是实际到达超过 EMA 预测 + margin，超出部分睡一窗口——与未分级时桶满的行为完全一致，失败模式只向下退化到现状。EMA 估高让 bg 少吃几个槽（fg 无损），估低收缩成静态 `margin` 预留（fg 仍有保底）。`waiters_fg` 与 `fg_rate` 对同一批 fg 有意的轻微双计是保守方向（过度预留），不是 bug。
