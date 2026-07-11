# Hop agent protocol

## State graph

```text
A accepted
├─ P prompt, persisted before effects
│  └─ C checkpoint
│     └─ R proposal
└─ P independent prompt

A + R ──land──> A next accepted state
```

State prefixes:

| Prefix | Kind | Meaning |
|---|---|---|
| `A_` | accepted | Canonical project revision |
| `P_` | prompt | Exact instruction and pre-effect context |
| `C_` | checkpoint | Immutable workspace progress |
| `R_` | proposal | Frozen candidate result |
| `F_` | failed | Durable failed execution or validation state |
| `X_` | cancelled | Durable cancelled state |

Prompt, checkpoint, and proposal states may reference identical Git trees while remaining distinct causal occurrences.

## Environment contract

| Variable | Purpose |
|---|---|
| `HOP_ROOT` | Canonical project root containing `.hop/hop.db` |
| `HOP_STATE_ID` | Prompt state authorizing the current instruction |
| `HOP_TASK_ID` | Logical task grouping related prompts and attempts |
| `HOP_ATTEMPT_ID` | Current agent approach/run |
| `HOP_WORKSPACE` | Only directory the agent may modify |

Treat missing variables as an invalid agent launch. Do not infer an attempt from a nearby worktree when causality matters.

## Command contract

### Human or controller

```bash
hop init
hop start --agent <name> "<exact initial prompt>"
hop env <prompt-state>
hop prompt --from <state> "<exact follow-up prompt>"
hop land <proposal> -- <final validation command>
hop undo
```

`hop start` creates the task, attempt, prompt state, and detached workspace before returning. The controller may deliver the prompt only after exit `0`.

`hop prompt` captures a checkpoint of current workspace effects before creating the follow-up prompt state.

### Agent

```bash
hop state "$HOP_STATE_ID" --json
hop status --json
hop check "$HOP_STATE_ID" -- <command>
hop propose --summary "<summary>" "$HOP_STATE_ID"
```

`hop check` snapshots the attempt and runs the command in a detached worktree materialized from that exact checkpoint. Edits made concurrently in the live workspace do not change the tested tree.

`hop propose` freezes the current nonignored workspace tree. Later workspace edits cannot change the proposal.

`hop land` compares paths changed by the proposal with paths accepted since its base. Any shared changed path blocks landing. Disjoint proposals are composed with Git three-tree plumbing and may then be validated on the final tree.

## Exit codes

| Code | Meaning |
|---:|---|
| `0` | Success |
| `1` | Git, SQLite, filesystem, or internal error |
| `2` | Invalid CLI usage |
| `20` | Overlap or conservative conflict block |
| `21` | Accepted or attempt head changed during compare-and-swap |
| `22` | Validation command failed |

A failed `hop check` or final landing check persists its evidence. A blocked or failed landing does not advance accepted state.

## Human launch sequence

```bash
hop init
hop start --agent codex "Add password reset emails"
```

Use the returned workspace and environment to launch the agent. For example, conceptually:

```bash
eval "$(hop env P_...)"
<agent-command> "<the same exact prompt>"
```

The exact agent command is harness-specific. Until a Hop process adapter intercepts prompts automatically, follow-up prompts must also pass through `hop prompt` before the agent acts.

## Failure handling

- **Missing Hop environment:** stop before editing and request a Hop-controlled launch.
- **Check failure:** fix the live workspace, checkpoint/check again, then create a new proposal.
- **Frozen proposal needs changes:** record a follow-up prompt; never mutate the stored proposal.
- **Overlap on landing:** retain both lineages and reconcile through a new prompt against current accepted state.
- **Ref inconsistency:** run `hop doctor`; use `hop doctor --repair` only outside final validation.
- **Secrets:** prompt text and check output are stored locally without encryption in the alpha. Never place credentials in them.
