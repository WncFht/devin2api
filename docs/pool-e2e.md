# 多账号池端到端冒烟（devin-pool-smoke.sh）

`scripts/devin-pool-smoke.sh` 用「一好一坏」两条 lane 起临时实例，对真实上游验证号池的钉选、换号与归因——它是对单测的补充：只有真上游才能暴露「凭据失效到底在哪个阶段冒头」这类行为。

## 验证点

临时配置写 `devin.accounts`：`good`（真实 token）与 `bad`（固定无效 token `devin-session-token$invalid.badtoken.for-smoke`，`DEVIN_TOKEN_BAD` 可覆盖）。

钉选判定在脚本内用 sha256sum 复刻：`affinity = sha256(user)[0:16] hex`，rendezvous 分 `sha256(affinity|name)` 升序——与 `SessionAffinityKey`/`rankLanes` 同种子同序（完整排序是绑定命中 → 健康桶 → priority 降序 → rendezvous 分，脚本两号同 priority 且未绑定时即退化为分数序），因此脚本能预知每个会话键钉到哪条 lane，并从候选键里各挑 3 个钉到 bad / good。亲和键先取请求头链（`X-Claude-Code-Session-Id` → `X-Session-ID` → `X-Session-Affinity` → `X-Conversation-Id` → `X-Thread-Id`），脚本不传头时回落 body 的 `user` 字段。

断言围绕四条行为：rendezvous 钉选（同键恒落同 lane）、failover 换号（钉到 bad 的首击经 unauthenticated 失败转投 good，logs 行 `account_switches>=1`）、凭据冷却降级（bad 标记冷却后同键直发 good、零换号）、归因字段（logs 行 `account`、meta `upstream_account`/`upstream_attempts`、runtime-metrics `accounts` 段、per-lane `runtime_state` 的 `gate:<name>` 键）。

## 用法

```bash
DEVIN_TOKEN_GOOD=<tok> scripts/devin-pool-smoke.sh [--port 3199]
scripts/devin-pool-smoke.sh --config config.yaml   # 取首个 devin.accounts 凭据/model/base_url
```

构建临时二进制 → 空闲端口起独立实例（`-state-dir` 指向 mktemp 目录，不污染真实状态）→ 两轮 12 个 `POST /v1/chat/completions`（`user` 字段做亲和键、`X-Client-Request-Id` 做关联、`max_tokens:8` 压成本）→ 查 `logs` 表与 `/admin/runtime-metrics` 断言。

成本：约 15 次真实上游调用，均为「回复一个词」级小请求；坏号的失败尝试不消耗配额。

失败时工作目录保留（输出里给路径），可直接查 `state/devin-2api.db` 的 `logs` 与 `debug_*` 表。

## 双真实账号部署的人工核对清单

冒烟只覆盖「坏号」这一失败形态；双活号上线后按下面核：

1. 配置两条 `devin.accounts`（A 号字面 token、B 号 `credentials_file`），`deploy-linux.sh` 上线。
2. 跑一段时间后 `sqlite3 <状态目录>/devin-2api.db "SELECT account, COUNT(*) FROM logs WHERE account!='' GROUP BY account"`——rendezvous 对均匀随机会话键近似均分，分布应大致一半一半。
3. `quota_samples` 表每账号一条快照行（`account` 列区分），两号配额各自累计。
4. `/admin/runtime-metrics` 的 `accounts` 段两号各自有 `gate`/`warm` 簿记；`runtime_state` 表按号分 `gate:<name>` 键。
5. 让某号自然触发限流（quota 耗尽或上游 429）后观察：`account_switches` 非零的行应出现在「钉到受限号」的会话上，且该号闩期内会话改由另一号服务。
