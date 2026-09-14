---
name: orchestrating-agents
description: Run large or uncertain work as a budgeted tree of subagents with evidence gates. Use when a task exceeds one context, spans independent sub-problems, needs real research or experimentation rather than a guessed answer, or when the user asks to fan out / delegate / orchestrate.
---

# Orchestrating Agents

Big tasks fail two ways. **Flat**: one context does everything serially — reading, exploring, implementing, verifying all compete for the same window, so each stays shallow. **Lazy**: conclusions drawn from memory, or from a subagent's own report, instead of from evidence.

This skill prevents both. You run the work as a **tree**: scout the problem yourself, decompose it into independent problems, dispatch one subagent per problem with a written **brief** and a **budget**, steer them mid-flight, and verify their output before integrating. You are the tech lead of a small team: the team does the digging, you do the judging.

## The gate

Fan-out is a tool for scale and isolation, not a default. Work alone when:

- One command or one file answers the question.
- The problems share a root cause, or would edit the same files.
- You are still exploring what the problem even is — scout first, fan out once the shape is known.
- The value doesn't justify the cost: a busy tree burns roughly an order of magnitude more tokens than a single thread.

Delegation is also a place to hide laziness. Two rules bind the orchestrator itself:

- **Verify what you rely on.** Any evidence your global judgment stands on, you check personally — read the diff, re-run the command. A worker's report is a claim, not a fact.
- **The buck stops in the tree.** Before declaring the whole task done, some node — usually you — must have exercised the result end to end: ran the app, re-ran the original failing symptom, read the merged diff.

## Scout, then split

Decomposition quality is set before the first dispatch. Spend a bounded amount of your own effort — a handful of reads, one real run — to learn the terrain: where the seams are, what's already known, what a worker will trip on. Then carve along the boundaries you actually saw.

An orchestrator that splits blind produces overlapping agents and vague briefs. If you can't yet write a brief with real file names and real error text, you haven't scouted enough.

## Budget

Every tree has a **total cap** — default 20 agents per task, adjust to the task. You cannot count agents globally (nodes can't see each other), so the cap is enforced by **arithmetic, not vigilance**:

- You start holding the whole budget.
- Every brief you dispatch carries `SUBTREE_BUDGET=k`: the maximum number of agents that subtree may contain, counting itself.
- You hand out disjoint shares summing to ≤ your remaining budget. A node may spend at most `SUBTREE_BUDGET - 1` on children, and each child's own `SUBTREE_BUDGET` bounds its subtree. Total spawned stays under the cap by construction.
- `SUBTREE_BUDGET=1` forbids spawning outright. `DEPTH=d` records how far below you the node sits; `MAX_DEPTH` in its brief forbids spawning at that depth even with budget left — otherwise one budget of 5 can degenerate into a chain five agents deep.

Keep the tree bushy, not deep: root → workers → leaf helpers covers almost everything. Dispatch in **waves of 3–5** — multiple `Agent` calls in one response run in parallel; across responses they serialize. Waves beat both the serial drip and the 15-wide fan-out: reviews land as results arrive, and conflicting edits surface while the tree is still small.

Effort scales with the task:

| Task shape                           | Agents        | Per-agent tool calls |
| ------------------------------------ | ------------- | -------------------- |
| Fact check, single question          | 0–1           | 3–10                 |
| Comparison, a few independent angles | 2–4           | 10–15 each           |
| Many independent domains             | up to the cap | divided by domain    |

For a hard cap the model can't reason its way around, register `scripts/agent-budget-gate.sh` as a `PreToolUse` hook on the `Agent` tool. It counts spawns in the session and denies past the cap, feeding the reason back so the run winds down instead of dying.

## The brief

Workers get a fresh context: they cannot see your conversation. The brief is all they get — it must be **self-contained**: objective, scope boundaries (an explicit "do NOT" list), the evidence you already hold (paste the error, name the files), constraints, tool-call budget, `SUBTREE_BUDGET`/`DEPTH`, and the report format.

Every brief orders **evidence over opinion**: the worker shows the command and output behind every claim — "tests pass" means the run's tail, not its word. Point workers at primary sources — the docs, the code, a real run — and forbid answering from memory on anything that drifts: APIs, flags, versions, prices.

Give every dispatch a distinct label; you will steer by name later. Template: `references/brief-template.md`.

## The report contract

Every worker reports exactly one status, plus evidence:

- **DONE** — the result and the evidence for each claim (command outputs, diff paths, artifact files).
- **DONE_WITH_CONCERNS** — done, with a list of what to double-check.
- **NEEDS_CONTEXT** — blocked on information only you hold; states exactly what's missing.
- **BLOCKED** — cannot proceed; states why and what was already tried.

Long artifacts go to files in the run's workspace; the report returns paths, not bulk.

## Steering

You can talk to a live or finished agent by name — `SendMessage` resumes it with its full transcript intact. Steering mid-flight is cheaper than restarting, and the agent keeps everything it learned.

- **NEEDS_CONTEXT** → answer it via `SendMessage`; the worker resumes where it stopped.
- **Off-course** → send a correction ("drop approach B, the constraint was X"), not a new dispatch.
- **Fix rounds**: rounds 1–3, send the findings back to the same agent. Round 4+, open a fresh agent — a new reader sees what a stale context has rationalized away — and consider a stronger model. Cap fix rounds at 5, then **adjudicate**: fix what's load-bearing yourself, or record a ruling that consciously defers it.
- Never force a stuck agent to retry an unchanged approach. Same input, same wall.

Use `ListAgents` to reconcile who's still running against the ledger.

## Review

The implementer never reviews its own work. For each DONE: re-run the cheap claims, read the diff behind the load-bearing ones, check for collisions with other agents' edits, and look for systematic errors — an agent repeats a wrong assumption faithfully across every file it touches.

Judge the artifact, not the story. An agent that wandered but produced verified output succeeded; a clean narrative with unverified claims did not.

## Ledger

For runs beyond a few agents, keep a file — `work/orch-<slug>/ledger.md` (`work/` is gitignored here; elsewhere use any scratch dir). One line per dispatch: label, brief path, budget share, status, rulings. It survives context compression and makes the budget auditable. Keep briefs and large artifacts under the same directory.

## Completion checklist

- [ ] Every dispatched agent resolved: done, ruled, or reported as a gap.
- [ ] Budget accounting clean: spent ≤ cap, no unaccounted children.
- [ ] Load-bearing claims verified against evidence you saw yourself.
- [ ] Cross-agent edits checked for conflicts; the full suite / whole-program run passed after integration.
- [ ] Rulings listed in the final report: what was deferred, and why.
