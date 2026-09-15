---
name: protocol-drift
description: Detect upstream Devin Connect protocol drift and adapter field-coverage gaps by reconciling extracted protobuf descriptors against logged wire traffic. Use when the Devin CLI was upgraded, when error_stage patterns shift in logs/index.jsonl, before cutting a release, when proxied behavior diverges from the official client, or when deciding whether to re-run proto extraction and binding generation.
---

# Protocol Drift

The proxy maintains two evidence surfaces about the upstream Devin Connect
protocol. `cmd/protocensus` reconciles them; this skill covers reading its
report and deciding what to do about it.

## Evidence model and its limits

- `outputs/devin-proto/descriptors.pb` — the extracted FileDescriptorSet,
  the static contract: what the client _could_ send. Authority for original
  names, packages, and field numbers.
- `logs/<dir>/03-devin-request.json` — the protojson request the adapter
  actually sent upstream (what we _do_ send).
- `logs/<dir>/04-devin-response.jsonl` — the protojson frames upstream
  returned (what it _does_ return).

Hard limit on what logs can prove: `03`/`04` are re-encoded through the
generated bindings, and `protojson.Marshal` drops fields the generated code
does not know. **A brand-new upstream field is invisible in old logs.** What
still surfaces:

- New enum members appear as bare numbers where a name is expected.
- Field-level drift on known fields (populated or not) remains visible.
- New fields/messages/services require re-extracting descriptors from a
  newer upstream binary and diffing (below).

## Run the census

Requires generated bindings (`outputs/devin-proto-go/`); if missing, run
`task generate` first.

```sh
go run ./cmd/protocensus census                # all request dirs
go run ./cmd/protocensus census -max-dirs 500  # newest N dirs only
```

(`task census [MAX_DIRS=N]` wraps the same command when go-task is
installed.) Reads `logs/` by default; the repo `logs/` is a symlink into the
platform state dir's `logs/`, so this inspects production traffic.

## Read the report

The JSON report has `request` and `response` sections, each with:

- `messages` — per message type: `occurrences` and per-field hit counts.
  This is the coverage baseline.
- `fields_never_seen` — fields defined in the schema but zero hits **within
  message types that did occur**. Interpretation needs judgment:
  legitimately-optional fields (e.g. `images`, `num_tokens`,
  `custom_tool_grammar`) are noise; a field the official client sends but
  our adapter never populates is a real gap. Cross-check suspects against
  `all-protos.proto` comments and `internal/adapter/devin` population code.
- `unknown_keys` — JSON keys the descriptor cannot resolve. Should be
  empty; a non-empty entry means the logging path changed or a hand-written
  projection leaked — investigate as an adapter bug, not upstream drift.
- `enum_anomalies` — enum values outside the known member set. `number:N`
  means upstream returned a member our schema does not know: **treat as
  confirmed drift**. A string name not in the member list indicates stale
  or mismatched bindings.

Each anomaly carries up to 3 `examples` (request dir names); open
`logs/<dir>/meta.json` and the cited stage file to inspect context.

## Detect field-level drift after a CLI upgrade

The definitive check for added/removed/changed schema:

```sh
go run ./cmd/protoextract <new-upstream-binary> outputs/devin-proto-new
go run ./cmd/protocensus diff outputs/devin-proto/descriptors.pb outputs/devin-proto-new/descriptors.pb
```

`diff` reports `added` / `removed` / `changed` across types, fields
(number+type+label), enum values, and RPC methods. Only promote the new
set into `outputs/devin-proto/` after reviewing the diff; then
`task generate` and re-run `task census`.

Notes on reading a diff:

- Renames appear as `- old_field` + `+ new_field` — check the field number;
  same number means rename, different number means a real change.
- `type_name` differences that only swap `exa.codeium_common_pb.X` for
  `exa.api_server_pb.ExaCodeiumCommonPb_X` indicate comparing an
  original-name extraction against a flattened one — meaningless; diff two
  `descriptors.pb` files (both preserve original names).

## Live behavioral check

`go run ./cmd/probe rerun -file logs/<dir>/03-devin-request.json -n 8`
replays a captured request against the live upstream and prints stop
reason, tool-call count, and text tail per run. Use it after a CLI upgrade
or when `index.jsonl` shows a new `error_stage` cluster: divergent
responses across replays indicate behavioral drift even when the schema is
unchanged.

## Decision rules

- `enum_anomalies` non-empty, or `diff` shows added/removed `rpc`/`type` on
  `GetChatMessage*` → escalate: re-extract, regenerate bindings, audit
  `internal/adapter/devin` for the changed symbols.
- `fields_never_seen` entries that map to user-visible features (images,
  caching, custom tools) → likely adapter coverage gap; verify the adapter
  never populates them, then file as a feature gap rather than drift.
- Schema clean but replays diverge → behavioral drift upstream; document
  the delta and adjust the adapter's interpretation, not the schema.
