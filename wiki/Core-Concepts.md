# Core concepts

Hop versions intent and source together. Git remains the content store; Hop adds
the causal state graph that explains which instruction produced which result.

## States

| Prefix | State | Meaning |
|---|---|---|
| `A_` | Accepted | Canonical Hop project revision |
| `P_` | Prompt | Durable instruction and pre-effect context |
| `C_` | Checkpoint | Immutable snapshot of attempt progress |
| `R_` | Proposal | Frozen candidate result |
| `F_` | Failed | Durable failed execution or validation result |
| `X_` | Cancelled | Terminal cancelled result |

Two states can reference the same Git tree and still be distinct occurrences.
For example, a prompt and checkpoint may contain identical files but represent
different moments and causal roles.

## Task

A task groups the prompts and attempts pursuing one user outcome. Follow-up
messages pursuing unfinished work stay connected automatically through
a stable session ID supplied by the integration. Codex Desktop supplies
`CODEX_THREAD_ID` automatically. Once that outcome is accepted, the next message
starts a new Hop task at the latest accepted state even when the client
conversation stays open.

## Attempt and workspace

An attempt is one agent approach. Each attempt has a detached Git worktree under
`.hop/workspaces/`. Agents edit there instead of racing in the visible project
root. `hop complete` removes source-clean accepted/completed worktrees while
leaving their immutable Git and SQLite history intact. Active workspaces and
terminal workspaces with unrecorded source changes are preserved; `hop gc`
retries the same safe cleanup explicitly.

## Evidence

`hop check` snapshots the workspace and runs validation against that immutable
tree. Evidence stores the command, redacted output, exit code, and exact tree
hash.

## Proposal

`hop propose` freezes a candidate tree. Later workspace edits cannot mutate the
proposal.

## Landing

`hop land` composes the proposal onto the current accepted state, runs optional
final-tree validation, advances accepted history with compare-and-swap, and
safely materializes the result into the visible project directory.

`hop accept` is lower-level controller behavior: it advances internal accepted
state but intentionally leaves the visible folder unchanged.

## Visible root

The visible root is the project directory selected in an agent client or passed
as the controller's working directory. During `hop land`, Hop captures ordinary
nonignored edits there as a labeled accepted transition, then merges them with
the proposal through the normal three-way reconciliation flow. The real Git
index, ignored content, and changes racing with materialization remain
fail-closed so Hop never silently changes staging intent or private files.

## Automatic upstream push

Every successful accepted transition is automatically pushed to the active
branch's configured Git upstream. If no upstream is set, Hop uses `origin`, or a
single unambiguous remote, with the active branch name. It pushes only accepted
commits—not prompts, checkpoints, proposals, SQLite history, or workspaces—and
never force-pushes. A network, authentication, or non-fast-forward failure
leaves the accepted local state intact and is returned as a warning for the
agent to handle.
