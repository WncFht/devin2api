# 闸门与配额账本：口径、陷阱与查询配方

devin-2api 对「流量、配额、换号、存储」的测量分散在若干持久表与内存计数器里，各记各的单位。本文登记每个账本记什么、单位之间已验证的陷阱、以及对应的 `sqlite3` 查询配方——回答「这个数字该去哪查、查出来是什么单位」一类问题。相邻文档的分工：准入语义（fg/bg 怎么判）在 `gate-classes.md`，上游计费模型（额度怎么烧）在 `quota-billing.md`，上游限流模型与闩在 `upstream-rate-limit.md`，选号与 failover 语义在 `devin-accounts.md`；本篇只管本地怎么记账。

## 账本分层

| 层         | 载体                                                                                                                                             | 生命周期                                                                                       |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------- |
| 内存瞬态   | `gate.wait` 样本环（每 lane 256 条）、`rejects` 事件环（256 条）、`GateStats` 计数器                                                             | 进程重启清零；`/admin/runtime-metrics` 的 `.data.gate` / `.data.accounts.<lane>.gate` 段实时读 |
| 持久明细账 | `gate_windows`（分钟窗×lane）、`lane_attempt_causes`（日×lane×cause）、`quota_samples`（采样快照）、`logs` 与 `log_cells`/`log_err_cells` rollup | SQLite，按各自保留期删除                                                                       |
| 派生口径   | `sends_per_row`、`attempt_causes`（`/admin/usage` 快照段）、`/admin/quota` 耗尽外推                                                              | 查询时计算，不落表                                                                             |

## 单位陷阱

读任何「拒绝率」「换号率」「放大系数」之前先过这节——每个坑都在生产数据上实测踩过。

**闸门拒绝与 logs 行不是一个单位。** 闸门拒绝按每次 `gate.wait` 评估计数——号池 failover、同 lane 续试、托管搜索各自独立过闸各记一次，而 `logs` 每请求一行。实测比值约 14:1（评估：日志行），用 logs 行数当分母或当计数都会低估一个数量级。

**闸门拒绝可以对 logs 终结口径 100% 隐形。** 号池下每次闸门快败都可能被 failover 救回：post-deploy 逐窗对账 18 次 `reject_*` 对应 18 条「首 lane error.json + 兄弟 lane 完成」的吸收行，终结性 `rate_gate` 行 0 条——该类拒绝在终结口径完全消失。对账公式：`reject_* ≈ 被 failover 吸收的完成行 + 终结性 rate_gate 行`。评估拒绝量必须走 `gate_windows`，不能走 `logs`。

**`sends_per_row` 不等于 1。** 分子是 `gate_windows` 的 `used_fg+used_bg−used_bg_ping`——每次放行对应一次真实上游发送，含 `logs` 不可见的同请求内层 connect 重试与闩内 drip 探针；保温 ping 走 adapter 内部路径不产生 logs 行，留在分子里会把「每请求一发」的基线垫高 ~5-6%（实测 ~289 ping/h），故单列剔除——注意迁移 0014 前的历史行 `used_bg_ping` 恒 0 无法回补，那些日的分子仍含 ping。分母是当日非 rejected `logs` 行。基线约 1.0，漂移 = 重试/换号放大信号。行内 `retry_admits` 是放行中同 lane 续试重发的日合计（reopen/续轮/凭据自愈/瞬时重试），占 sends 份额即重试贡献；`coverage` 是当日实有 `(lane,window_start)` 槽 ÷ 预期槽数（`lanes × 当日已流逝分钟数`——过去日整日长、今日按已流逝计），`sends_missing` 是预期−实有缺额：部署截断日、未装表日、丢窗日的低比值据此读成低覆盖而非低倍率，丢窗与零触碰空窗不可分辨故只记缺额不插值。注意 `quota<=0`（不限速）时窗行照记、`quota` 列存非正值；纯探针日 `rows=0` 时 ratio 按定义缺省。

**「换号」不等于「发给上游」。** `lane_attempt_causes` 实测首日约 82% 的放弃 lane 尝试成因是 `local_gate:*`——本地闸门快败的幻影换号，该 lane 零上游发送、零上游成本。只有 connect code 行才是真发过包的 failover。

**02/03 残差随编码档位变化。** 残差口径 `LENGTH(content)/usize` 只在 `SpeedBetterCompression` 档贴近语料预期（02≈0.6%、03≈2.75%）；此前 `SpeedFastest` 档的 dict 编码器是 32K 槽单探针表，字典超 ~300KB 后碰撞饱和，残差随字典尺寸单调爬坡（实测 0.6%@150K → 41.6%@750K）。读历史库存的残差先确认部署档位——修复见 `compress.go` `EncodePayloadDelta`。等价关系要记准：zstd CLI `-3` ≈ klauspost `SpeedBetterCompression`，不是 `SpeedFastest`。

## gate_windows：闸门分钟明细账

每 lane 每个「被观察关闭」的对齐分钟窗口一行——闸门在该窗内被流量/面板/保温触碰过才有关窗行，整窗未触碰的空窗期是缺口而非零行。`(lane, window_start)` 唯一索引；reuseport 交接期新旧两进程并发观察同一窗口时后写者被 `INSERT OR IGNORE` 丢弃。

| 列                                                                     | 含义                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| ---------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `window_start`                                                         | unix 秒，对齐分钟桶界                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `quota`                                                                | 关窗时生效的窗口配额（`<=0` 表示不限速）                                                                                                                                                                                                                                                                                                                                                                                                        |
| `used_fg` / `used_bg`                                                  | 本窗 fg/bg 放行数（按请求自身 class 分列，bg 含保温 ping；闩内滴灌探针同样按自身 class 入账，由 `drip` 列单列归因）                                                                                                                                                                                                                                                                                                                             |
| `used_bg_ping`                                                         | 本窗经 `tryAdmit` 的保温 ping 放行数（`used_bg` 的子集——`used_bg - used_bg_ping` 即真实 bg 需求；迁移 0014 起，历史行 0 不可回补）                                                                                                                                                                                                                                                                                                              |
| `drip`                                                                 | 闩内滴灌探针放行数（`used_*` 的子集，单列供配额归因）                                                                                                                                                                                                                                                                                                                                                                                           |
| `retry_admits`                                                         | 同 lane 续试重发的放行数（`used_*` 的子集——reopen/续轮/凭据自愈/瞬时重试；不含号池 failover 后新 lane 首发）                                                                                                                                                                                                                                                                                                                                    |
| `reserve_peak` / `waiters_peak`                                        | 本窗 bg 预留量峰值 / 闸内排队数峰值（fg+bg）                                                                                                                                                                                                                                                                                                                                                                                                    |
| `reject_quota` / `reject_bg_reserve` / `reject_latch` / `reject_yield` | 按成因分列的快败数：排队预算耗尽（桶满/死区睡到下一窗口将超预算；脆弱期窗内滴灌挡住的 fg 也归此列）、bg 让路（预留/爬坡/脆弱期滴灌——bg 的滴灌拒绝按既有 `reserveBlocked` 口径落本列）、闩内快败、让位（预计排队超长且兄弟 lane 能更快放行时提前拒，交给 failover；迁移 0012 起）。脆弱期是限流判决/解闩后 `gate_fragile_seconds`（默认 600）内 fg/bg 同束的窗内线性放行约束，2026-09-23 起；`reject_hold` 列已退役（hold 词并入 quota），恒为 0 |
| `fg_rate`                                                              | 关窗折叠后的 fg 准入速率 EMA（条/窗），bg 动态预留的输入                                                                                                                                                                                                                                                                                                                                                                                        |

两个读法要点：`reject_bg_reserve` 名义是快败，实际形态可以是「骑满排队预算再拒」——bg 被预留/爬坡挡住走 `gateBgRecheck` 短睡重查，直到预计等待超剩余预算才拒，实测被吸收行首 lane 停泊 p50=118.7s（紧贴 `bgMaxHold` 默认 120s），每吸收行平均多付 ~130s 首个上游字节。`waiters_peak=0` 而 transform 段长等，是闸前 stall（AssignModel/目录拉取不过闸）的嗅探信号——gate 账本对它完全不可见。

写路径是每翻页一次性异步协程（30s 上限，脱离 `gate.mu`）：批内首个 INSERT 失败即停手，未写行挂进每 lane 深度 60 的重放缓冲随下窗翻页重放（同一唯一索引使重放幂等），溢出丢最老行。落库健康账透在 `/admin/runtime-metrics`：`persist_failures` 记失败批次数（与 stderr `gate window persist failed` WARN 一一对应），`persist_dropped` 记缓冲溢出被永久丢弃的行数，`pending_windows` 是重放缓冲当前深度（未落库挂账行数，与 quota 组 `pending_samples` 同口径）——顶层 `gate` 段是首 lane 后兼容视图，逐 lane 在 `accounts.<lane>.gate` 下。

保留期与 `logs` 摘要行共用面板 settings 覆盖键 `log_row_retention_days`（默认 90 天）。

## gate.wait 样本环：每次评估的结局

每 lane 256 条容量的内存环（80 RPM 配额下约覆盖最近三个窗口），`gate.wait` 各出口（放行/拒绝/ctx 取消）由 defer 统一记账，等待时长取墙钟与 `X-Gate-Wait-Ms` 同口径。三结局词表：`admit` 放行、`reject` 闸门拒绝（`reason` 记 `gateReason*`）、`cancel` 客户端断连/打断——`logs` 的 transform 段只反映放行幸存者，估计器校准靠这个全结局样本。

`/admin/runtime-metrics` `.data.gate.wait`（首 lane 后兼容视图）与 `.data.accounts.<lane>.gate.wait` 的形状：`samples` 环内样本数、`evals` 进程启动以来评估总数（实例生命周期累计）、`since` 环覆盖期起点、`rejects{reason}` 环内拒绝分账、`all`/`fg`/`bg` 各给 `{count, mean_ms, p50_ms, p90_ms, max_ms, rejects, cancels}`——有估计的放行样本另附 `err_count`/`err_mean_ms`/`err_p10_ms`/`err_p50_ms`/`err_p90_ms` 估计误差校准面（est−realized，`err_count` 为零时整组缺省）。`totals.fg`/`totals.bg` 是进程期单调累计账 `{evals, waits, wait_total_ms}`：`evals` 评估总数、`waits` 真排过队（占过 waiters 名额）的评估数——即时放行与闩内/配额/让位快败都不计、`wait_total_ms` 实测墙钟等待累计。环覆盖期只有最近 256 条评估（约几分钟流量），分类等待率 `waits/evals` 与平均等待 `wait_total_ms/waits` 要靠 totals 快照差分取任意区间。

## quota_samples：上游配额快照

后台协程每 `debug.quota_interval_minutes`（默认 5 分钟）对每 account 打一条快照；`(account, at)` 唯一索引兜底导入重跑去重；全局 20,000 行帽，超帽截到最新。关键字段：`daily_remaining`/`weekly_remaining` 可空 REAL——`NULL` 是「上游没报」，`0` 是真到 0，二者必须区分；`daily_reset_at`/`weekly_reset_at` 重置点；`prompt/flow/flex_credits` 与对应 `used_*`；`acu_consumed`/`acu_limit`；`grace_period_*`、`top_up_*`；`overage_balance_micros`（迁移 0007 起）。配额字段是 int32 百分比向下取整，反推计费只能吃翻转点——方法与日/周额度大小见 `quota-billing.md`。`/admin/quota` 直接给日/周曲线与按燃烧速率外推的耗尽时刻，不必手算。

写路径同步落库（在采样协程/手动刷新的调用 ctx 内）：批内首个 INSERT 失败即停手，未写点挂进全局深度 4 的重放缓冲随下一次落库重放（同一唯一索引使重放幂等），溢出丢最老点。落库健康账透在 `/admin/runtime-metrics` 的 `quota` 段：`persist_failures` 记失败批次数（与 stderr `quota sample persist failed` WARN 一一对应），`persist_dropped` 记缓冲溢出被永久丢弃的点数，`persist_replayed` 记挂账点经后续落库救回的点数，`pending_samples` 是缓冲当前深度。缓冲按全局一份计（不分 lane）——采样协程只有一个，挂账点自带 account 列。

## lane_attempt_causes：被放弃 lane 尝试的日账

主键 `(day, lane, cause)`，`n` 为当日该 lane 该成因计数。写方是 `logs` 行同事务对 `meta.json` `upstream_attempts` 的展开——payload 目录淘汰后「为什么换号」只剩这里的口径。cause 词表：`local_gate[:reason]` 本地闸门快败的幻影换号（零上游发送）、connect code（`deadline_exceeded`/`unavailable` 等）真实 failover 发送、`nocode` 无 code 传输断裂。读侧 `/admin/usage` 快照 `attempt_causes` 段直读 31 天窗口；保留期同 `logs` 行（90 天按龄删）。表自迁移 0008 起累计，之前的历史不可回填——部署前的换号归因只能回 `meta.json` 时代。

## 派生口径

`/admin/usage` 快照里两个非 `logs` 源段（本地日粒度）：`sends_per_row` 是逐日 `gate_windows` 放行数（已剔保温 ping）÷ 当日 `logs` 行，是内层 connect 重试漂移的唯一活指标，行内 `coverage`/`sends_missing` 标注分子对分母日界的覆盖（`gate_windows` 全期无行时整段省略）；`attempt_causes` 即上节表的 31 天直读。两段都随快照计算，进程重启不丢口径。

## CAS 存储账（debug_blobs / debug_chunk_refs）

自迁移 0009 起，01 阶段 payload 可存成 CAS manifest：内容按 rolling-hash 切块进 `debug_blobs`（`hash` = 明文 sha256 截 16B 主键，`content` 是 `EncodePayload` 编码块，`usize` 块明文尺寸，`created_at` 给 reaper 插入宽限），`debug_chunk_refs` 记「哪个文件行引用了哪些块」——引用即行，随文件行同一 WHERE 同生死，是 mark-sweep GC 的事实源；块序由 manifest 位置表承载，refs 不记序号。manifest 行魔数 `00 43 41 53 31`（`\x00CAS1`），写在 `debug_files.content` 原位——回滚期旧二进制读 01 manifest 为乱码，属预期。保活不变式「blob 活 ⟺ ∃ ref 行」由同事务写入 + `ReapOrphanBlobs` 的 `NOT EXISTS` 反连接兜底维持。dangling refs 常规路径随文件行删除自清，`ReapOrphanBlobs` 的首趟 dangling-ref sweep 兜底回滚期/异常路径的漏网。

## 查询配方

全部直查 `devin-2api.db`（WAL，习惯加 `-readonly` 防误写）；`window_start` 是 unix 秒，`logs.started_at` 是 RFC3339 TEXT。lane 名/表存在性以本机部署为准——`lane_attempt_causes` 与 CAS 两表需迁移 0008/0009 落地后才有行。

```sql
-- 最近的闸门拒绝（逐 lane 逐窗，单位=每次 gate.wait 评估）
SELECT lane, datetime(window_start,'unixepoch','localtime') w,
       reject_quota, reject_bg_reserve, reject_latch, reject_yield
FROM gate_windows
WHERE reject_quota+reject_bg_reserve+reject_latch+reject_yield > 0
ORDER BY window_start DESC LIMIT 20;

-- 逐日 sends_per_row：分子 gate_windows 放行数（剔保温 ping）
SELECT date(window_start,'unixepoch','localtime') d, SUM(used_fg+used_bg-used_bg_ping) sends
FROM gate_windows GROUP BY d ORDER BY d DESC;
-- 覆盖率实有侧：lane-slot 数 ÷ (lane 数 × 当日已流逝分钟数)
SELECT date(window_start,'unixepoch','localtime') d, COUNT(*) slots
FROM gate_windows GROUP BY d ORDER BY d DESC;
SELECT COUNT(DISTINCT lane) FROM gate_windows;
-- 分母：当日非 rejected logs 行
SELECT date(started_at) d, COUNT(*) rows
FROM logs WHERE log_source IS NOT 'rejected' GROUP BY d ORDER BY d DESC;

-- 非对称饱和现场（双 lane 本机示例）：一边 >=72(90%×80)、兄弟 <=55
SELECT datetime(a.window_start,'unixepoch','localtime') w, a.lane,
       a.used_fg+a.used_bg sat, b.used_fg+b.used_bg sib
FROM gate_windows a JOIN gate_windows b
  ON b.window_start=a.window_start AND b.lane!=a.lane
WHERE a.used_fg+a.used_bg>=72 AND b.used_fg+b.used_bg<=55
ORDER BY a.window_start DESC;

-- 换号成因（需迁移 0008）：local_gate:* 幻影 vs connect code 真实发送
SELECT day, lane, cause, n FROM lane_attempt_causes
WHERE day>=date('now','-7 day') ORDER BY day, lane, n DESC;

-- 配额燃烧速率（近 24h 两次快照差；NULL=上游未报需剔除）
SELECT account, datetime(at,'unixepoch','localtime'),
       daily_remaining, weekly_remaining
FROM quota_samples WHERE at>=strftime('%s','now','-1 day')
ORDER BY account, at DESC;

-- CAS 覆盖（需迁移 0009）：manifest 化行数与块共享规模
SELECT COUNT(*) manifests FROM debug_files
WHERE substr(content,1,5)=X'0043415331';
SELECT COUNT(*) blobs, ROUND(SUM(usize)/1048576.0,1) logical_mb
FROM debug_blobs;

-- 02/03 残差（usize=解压尺寸；读数先确认编码档位，见「单位陷阱」）
SELECT name, COUNT(*) n, ROUND(AVG(1.0*LENGTH(content)/usize),4) residual
FROM debug_files WHERE name GLOB '0[23]*' AND usize>0 GROUP BY name;
```

## 已验证的读数锚点

以下数字来自生产实测（2026-09-18 观测窗），用于校准「什么样的值算正常」，不是恒定承诺：

- `sends_per_row` 基线 ~1.0（剔 ping 口径；迁移 0014 前的日行分子仍含 ping 系统性偏高 ~5-6%）；显著上漂 = 内层重试/幻影换号放大；日 coverage 明显 <1 = 分子覆盖缺口（部署截断/丢窗），读比值前先校覆盖。
- 闸门评估 : logs 行 ≈ 14 : 1。
- `local_gate:*` 幻影换号占放弃尝试 ~82%（部署首日）。
- 非对称饱和（`used≥72` & 兄弟 `≤55`）占 lane-分钟 ~17%；其中饱和 lane 流量 ~88% 是 bound 绑定强制，bound 请求人均多付 ~8.6s（8.8s vs 兄弟侧 0.24s）。
- 被吸收 failover 行首 lane 停泊 p50 118.7s ≈ `bgMaxHold` 骑满；每吸收行 +~130s TTFB。
- `quota<=0` 不限速部署下 `gate_windows` 仍记窗行（`quota` 列存非正值）；`sends_per_row` 段仅在全期无窗行时省略——这不是故障。
