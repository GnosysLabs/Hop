# Hop agent protocol

## State graph

```text
A accepted
├─ P prompt, persisted before project effects
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

Interactive agents may begin without these variables. `hop begin` returns the
equivalent IDs and workspace, while `CODEX_THREAD_ID` binds later messages in
the same Codex task to the existing attempt.

## Command contract

### Human or controller

```bash
hop init
hop start --agent <name> "<exact initial prompt>"
hop env <prompt-state>
hop prompt --from <state> "<exact follow-up prompt>"
hop accept <proposal> -- <final validation command>
hop sync
hop undo
```

`hop start` creates the task, attempt, prompt state, and detached workspace before returning. The controller may deliver the prompt only after exit `0`.

`hop prompt` captures a checkpoint of current workspace effects before creating the follow-up prompt state.

### Agent

```bash
hop begin --agent codex --heredoc <<'HOP_PROMPT_EOF'
<exact current user message>
HOP_PROMPT_EOF
hop state "$HOP_STATE_ID" --json
hop status --json
hop check "$HOP_STATE_ID" -- <command>
hop propose --summary "<summary>" "$HOP_STATE_ID"
hop land <proposal-state> -- <final validation command>
```

`hop check` snapshots the attempt and runs the command in a detached worktree materialized from that exact checkpoint. Edits made concurrently in the live workspace do not change the tested tree.

`hop propose` freezes the current nonignored workspace tree. Later workspace edits cannot change the proposal.

The initial task prompt authorizes the agent to run `hop land` after successful
validation; a second user approval is not required. Manual review is an opt-in
mode: stop at the proposal only when the user explicitly asks to review or
approve before acceptance. Validation failure, overlap, a stale accepted head,
or newly required destructive/external scope also stops automatic acceptance.

`hop land` is the Desktop operation. It compares paths changed by the proposal with paths accepted since its base, validates and advances accepted state, then safely materializes that tree into the selected visible project root. The root must still match an accepted Hop ancestor, and ignored or untracked destination collisions block before acceptance. Materialization uses a disposable index and never moves HEAD, the active branch, or the user's real index.

`hop accept` is the controller/kernel operation. It advances SQLite and
`refs/hop/accepted` but intentionally leaves the visible root untouched.
`hop sync` safely catches a stale accepted-ancestor root up to the current
accepted state, including projects created with older Hop builds.

`hop begin` is the Codex Desktop entry point. It initializes Hop when necessary,
captures the current message before the agent performs project work, and uses
`CODEX_THREAD_ID` as the default session key. A later `hop begin` in the same
Codex task checkpoints the prior workspace before appending the follow-up
prompt state.

Pass the original message to `hop begin` without model-side redaction. Hop's
sanitizer replaces detected credential values before any durable write and
returns only typed redaction counts. Do not place the value in any later
command, summary, output, or source file.

## Exit codes

| Code | Meaning |
|---:|---|
| `0` | Success |
| `1` | Git, SQLite, filesystem, or internal error |
| `2` | Invalid CLI usage |
| `20` | Overlap or conservative conflict block |
| `21` | Accepted or attempt head changed during compare-and-swap |
| `22` | Validation command failed |
| `23` | Visible project root diverged or contains an overwrite collision |

A failed `hop check` or final landing check persists its evidence. A blocked or failed landing does not advance accepted state.

## Capture modes

### Codex Desktop skill

The user types normally in Codex Desktop. The Hop skill makes `hop begin` its
first project action and then directs every operation into the returned
workspace. This is a pre-project-effect boundary: Codex has already received
the prompt, but no repository inspection, command, or modification may precede
the durable prompt state.

### Controller-grade pre-delivery capture

```bash
hop init
hop start --agent codex "Add password reset emails"
```

Use the returned workspace and environment to launch the agent. For example, conceptually:

```bash
eval "$(hop env P_...)"
<agent-command> "<the same exact prompt>"
```

The exact agent command is harness-specific. This stronger mode stores the
prompt before the model receives it. A future trusted prompt-submission hook can
provide the same boundary inside compatible agent clients.

## Failure handling

- **Missing Hop environment:** run `hop begin` before project work and use the returned state and workspace.
- **Check failure:** fix the live workspace, checkpoint/check again, then create a new proposal.
- **Review-only request:** preserve and report the proposal without landing it.
- **Frozen proposal needs changes:** record a follow-up prompt; never mutate the stored proposal.
- **Overlap on landing:** retain both lineages and reconcile through a new prompt against current accepted state.
- **Visible-root conflict:** preserve the proposal and the user's files. Do not substitute controller-only `hop accept`; resolve or capture the visible changes, then land again.
- **Controller-accepted root is stale:** run `hop sync`; it succeeds only from an accepted ancestor and never overwrites divergence.
- **Ref inconsistency:** run `hop doctor`; use `hop doctor --repair` only outside final validation.
- **Secrets:** Hop redacts high-confidence provider keys plus contextual tokens,
  passwords, private keys, authorization headers, and credential-bearing URLs
  before durable storage. It also sanitizes recorded check commands/output and
  proposal summaries. Detection is defense in depth, not a substitute for
  environment variables or a secret manager. Never repeat a detected secret.
