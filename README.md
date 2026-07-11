# Hop

Hop is an experimental prompt-native version-control kernel for coding agents.

Every instruction becomes an immutable state before it is delivered. Agent work produces checkpoint and proposal states beneath it. Landing a proposal creates a new accepted state without moving the user’s Git branch or checkout.

```text
A0 accepted
├─ P1 prompt
│  └─ C1 checkpoint
│     └─ R1 proposal
└─ P2 prompt
   └─ R2 proposal

A0 + R1 → A1 accepted
```

Git provides source-tree storage, diffs, worktrees, and interoperability. Hop’s prompt-state graph, evidence, and acceptance history live separately in `.hop/hop.db`.

## Current status

This repository contains the first local alpha kernel. It supports:

- Existing and unborn Git repositories
- Pre-delivery prompt states
- Isolated detached worktrees per attempt
- Immutable checkpoints and proposals
- Checks bound to exact source trees
- Conservative path-level conflict detection
- Three-tree composition of disjoint proposals
- Optional validation on the final integrated tree
- Compare-and-swap acceptance
- Forward-only undo of the latest accepted transition
- Git-compatible accepted commits under `refs/hop/accepted`
- SQLite WAL state graph and machine-readable JSON output
- Embedded, installable vendor-neutral agent skill

It does not yet include an agent-process adapter, project knowledge, claims, remote synchronization, a GUI, or semantic merging.

See [the product blueprint](docs/product-blueprint.md) for the complete model,
design principles, and phased roadmap.

## How the pieces fit

```text
Human/controller
  │  records exact prompt
  ▼
hop start ──> prompt state + isolated workspace
                              │
                              ▼
                       agent + Hop skill
                       edit → check → propose
                              │
                              ▼
Human/controller       review → land → accepted state
```

The controller owns prompt delivery and acceptance. The skill teaches the agent how to behave inside the assigned state. The Hop kernel computes and stores trees, evidence, ancestry, conflicts, and accepted history. A skill alone cannot guarantee that an initial prompt was saved before the agent saw it; `hop start` or a future process adapter must create that boundary first.

## Build

Requires Go 1.26+ and Git.

```bash
go build -o hop ./cmd/hop
./hop help
```

## Quick start

Install the embedded skill for Codex. By default this writes to `${CODEX_HOME:-~/.codex}/skills/hop`:

```bash
hop skill install
```

Export it to another agent’s skills directory with:

```bash
hop skill install --path /path/to/agent/skills
```

Initialize Hop without changing the current Git branch, working tree, or index:

```bash
hop init
```

Create a prompt state and isolated workspace. The string passed to `hop start` must be the same instruction delivered to the agent:

```bash
PROMPT='Use $hop. Add password reset emails'
hop start --agent codex "$PROMPT"
```

Pass the prompt as one quoted argument; Hop preserves its bytes, including leading/trailing whitespace and newlines. Hop prints the prompt-state ID, task/attempt IDs, workspace, and environment. The prompt is durable before the command returns, so it is then safe to deliver to an agent.

Load the environment and enter the isolated workspace using the returned prompt-state ID:

```bash
eval "$(hop env P_...)"
```

Now launch Codex, Claude Code, or another harness from that workspace and deliver `$PROMPT`. Explicitly invoking `$hop` is the most reliable way to load the skill. The skill verifies the Hop state, confines edits to the workspace, records exact-tree checks, and freezes the result as a proposal.

After the agent edits the printed workspace:

```bash
hop check P_... -- go test ./...
hop propose --summary "Added password reset emails" P_...
hop land R_... -- go test ./...
```

The command supplied to `land` runs in a temporary worktree containing the exact final tree that would become accepted. If it fails, `refs/hop/accepted` and the SQLite accepted head do not move.

Inspect the project:

```bash
hop status
hop graph
hop state P_...
hop diff R_...
hop history
hop doctor
```

Inspect or hand the skill to a harness without installing it:

```bash
hop skill print
```

Create a follow-up prompt in an existing attempt:

```bash
hop prompt --from P_... "Use Resend instead of SendGrid"
```

Hop first captures the workspace as a checkpoint, then writes the follow-up prompt as its child before returning.

Load the returned prompt state with `eval "$(hop env P_...)"` before delivering that exact follow-up to the agent.

Undo the latest accepted transition without rewriting history:

```bash
hop undo
```

## Parallel work

Two prompts started from the same accepted state receive independent worktrees:

```bash
hop start --agent codex "Add a health endpoint"
hop start --agent claude "Add an account empty state"
```

If their changed paths are disjoint, both proposals can land in either order. The second proposal is composed onto the latest accepted tree and may be validated there.

If both proposals changed the same path since their shared base, Hop blocks the stale proposal even when Git could textually merge it. The proposal remains addressable for review or a future reconciliation prompt.

## State model

| Kind | Prefix | Meaning |
|---|---:|---|
| Accepted | `A_` | Canonical project revision |
| Prompt | `P_` | Exact instruction and pre-effect context |
| Checkpoint | `C_` | Immutable workspace progress |
| Proposal | `R_` | Frozen candidate result |
| Failed | `F_` | Terminal failed execution state |
| Cancelled | `X_` | Terminal cancelled execution state |

Each Hop state references an immutable Git tree and synthetic commit. Multiple prompt states can reference the same tree while remaining distinct occurrences.

Source objects are pinned beneath `refs/hop/states/*`, preventing Git garbage collection from deleting states referenced by SQLite. Accepted source history is mirrored at `refs/hop/accepted` and can be exported as ordinary Git commits later.

## Storage

```text
.hop/
├── hop.db                 SQLite state graph and audit log
├── workspaces/            isolated attempt worktrees
├── integration/           temporary final-state validation worktrees
└── accept.lock            short-lived acceptance serialization lock
```

The repository’s `.git/info/exclude` receives `.hop/`; the public `.gitignore`, current branch, and real Git index are left alone.

Initialization refuses to proceed if `.hop` is already tracked as user-owned project source.

## JSON protocol

Add `--json` anywhere:

```bash
hop --json status
hop start --agent codex --json "Add password reset"
```

Successful output follows:

```json
{
  "ok": true,
  "data": {}
}
```

The JSON shape is an alpha contract and may evolve before the first tagged release.

## Safety boundary

Hop currently treats any shared changed path as a conflict. Disjoint files can still conflict behaviorally, so manual acceptance remains the default and final-tree validation is strongly recommended.

Agent-reported scope and test claims are never used as the source of truth. Hop computes source trees and changed paths itself.

Prompt text and check output are currently stored locally in SQLite without encryption or redaction. Do not put credentials or sensitive secrets into this alpha ledger.
