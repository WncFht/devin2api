# Brief template

Paste-ready skeleton for an `Agent` dispatch. Fill the slots; delete lines that don't apply. The `PROTOCOL` line is what makes the tree recursive — every node that may spawn carries it forward into its own briefs.

```text
You are <label>, a worker node in an agent tree.
PROTOCOL: invoke the Skill `orchestrating-agents` if it is listed for you;
otherwise Read <abs path to this skill>/SKILL.md and follow it. Apply the same
rules — brief, budget, report contract — to any children you dispatch.

TASK: <one-sentence objective>

CONTEXT YOU NEED:
<files, pasted errors, prior findings — everything; the worker sees nothing else>

IN SCOPE: <what to touch / investigate>
OUT OF SCOPE: <explicit do-NOT list — files, approaches, side effects>

EVIDENCE RULES: every claim cites the command you ran and its output. Check
primary sources — docs, code, a real run; never answer API/flag/version
questions from memory. Budget ≈<n> tool calls; spend them on verification,
not hedging.

SPAWNING: SUBTREE_BUDGET=<k>  DEPTH=<d>  MAX_DEPTH=<usually 2>
<if k>1 and d<MAX_DEPTH:> You may dispatch children whose SUBTREE_BUDGETs sum
to ≤<k-1>; each carries DEPTH=<d+1> and this PROTOCOL block.
<if k=1 or d=MAX_DEPTH:> Do not spawn subagents.

ARTIFACTS: write large outputs to <run dir>/<label>-*.md; report paths,
don't paste bulk.

REPORT back exactly one status:
- DONE — result + the evidence behind each claim
- DONE_WITH_CONCERNS — result + what to double-check
- NEEDS_CONTEXT — exactly what you're missing
- BLOCKED — why, and what you already tried
```

Notes on filling it:

- **CONTEXT YOU NEED is the make-or-break slot.** A vague brief produces agents that duplicate each other or answer the wrong question. Paste the actual error text, the actual file paths, the constraints you already know.
- **Tool-call budget** comes from the effort table in SKILL.md — 3–10 for a fact check, 10–15 for a comparison. It is a floor as much as a ceiling: a worker that stops at 2 calls on a 15-call budget didn't look hard enough.
- **OUT OF SCOPE earns its keep** even when it feels obvious — "do NOT modify production code", "do NOT just raise the timeout; find the real cause", "do NOT touch files outside pkg/x".
