---
name: release-runbook
description: 通过 scripts/release/release.sh 端到端跑一次 devin-2api 发布——dry-run 复查、--publish 机制、发布后核验 workflow/资产/checksums/GHCR tag，以及发布损坏时的手工修复。当用户要求 'cut a release' 'publish a release'、检查发布是否正确落地，或 tag / release body 出错需要修复时使用。
---

# 发布 Runbook

`scripts/release/release.sh` 是唯一受支持的发布方式。它从 Conventional Commits 计算下一个版本号，要求被打 tag 的那个确切提交拥有全绿 CI，并推送一个注解 tag——tag 注解即 GitHub Release body（release.yml 提取 `%(contents)`——tag 注解是发布说明的唯一事实源）。

## 前置检查

- 工作树必须干净且 `HEAD` 必须等于 `origin/main`；否则脚本拒绝执行。先提交或 stash 所有改动。
- 改过 `release.sh` 本身之后跑 `bash scripts/check/release-selftest.sh`——它离线演练整个流程（bare origin + stub curl），能在真实发布踩坑前抓住 `##` 标题被剥掉这类回归。
- 确认版本升级档位正确：0.x 阶段 `feat`/破坏性变更升 minor、其余升 patch；`--version vX.Y.Z` 可覆盖计算值。

## 流程

```sh
scripts/release/release.sh             # dry-run：打印下一版本 + 分类 changelog
scripts/release/release.sh --publish   # VERSION bump 提交 -> 等 CI 绿 -> 打 tag -> 推送
```

`--publish` 机制按序执行：

1. 工作树脏或 `HEAD` 未推送时拒绝。
2. 把 `NEXT` 写进 `cmd/devin-2api/VERSION`，提交 `chore(release): bump VERSION to NEXT`，推送 `origin/main`。若文件已等于 `NEXT` 则跳过本步（失败重跑会复用之前的记录提交）。
3. 轮询 `workflow_runs?head_sha=<bump sha>` 中名为 `CI` 的 workflow 直到 `conclusion == success`（`CI_WAIT_SECONDS` 为截止时限，`CI_POLL_INTERVAL` 为轮询间隔）。`FAILED` 或超时即拒绝。
4. 重新 fetch 并复核 `HEAD == origin/main`——若等待期间有人推送，拒绝（TOCTOU 防护）。
5. `git tag -a NEXT -F notes --cleanup=verbatim`，然后 `git push origin NEXT`。

已知的可接受残留：若 CI 在第 2 步后失败，VERSION 记录提交会留在 main 上但没有 tag。无害——后续发布会检测到 `VERSION == NEXT` 并跳过重复提交。

## 发布后核验

- 该 tag 的 `release.yml` 运行必须全绿：它跑测试、构建 6 个矩阵资产（linux 用 readelf 断言静态链接）、从同一批产物发布 Docker 镜像，并创建 GitHub Release。
- Release 页面必须有 7 个资产：`devin-2api-{darwin,linux,windows}-{amd64,arm64}`（windows 为 `.zip`）加 `checksums.txt`。
- Release body 必须显示分类 changelog（`## Features` 等），而不是光秃秃的提交标题——若不是，说明 tag 注解丢失（见下）。
- GHCR tag：`ghcr.io/wncfht/devin2api:<version>`、`:<minor>`、`:<major>`、`:latest`（仅稳定版——预发布不移动 `latest`）。

## 修复规则（踩坑换来的）

- **永不重打已推送的 tag。**重推 tag 不会干净地重新触发 `release.yml`，还会改写别人可能已经 fetch 的公开历史。
- release body 错了但 tag 是对的：提取注解并原地修 body——`git for-each-ref refs/tags/vX.Y.Z --format='%(contents)' > notes.md`，然后 `gh release edit vX.Y.Z --notes-file notes.md`。
- tag 注解本身丢了 `##` 标题：原因是 `git tag -a -F` 默认 `cleanup=strip`（注释行被吃掉）。修法是 `--cleanup=verbatim`；body 按上述 `gh release edit` 修复。
- `release.yml` 产出的 release body 等于提交标题时：tag 推送触发的 checkout 可能留下轻量 tag ref；workflow 在读 `%(contents)` 前会强取 `refs/tags/X`。不要删掉那次 fetch。
