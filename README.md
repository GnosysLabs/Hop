# Hop

Hop is prompt-native version control for coding agents.

Every instruction becomes an immutable state before project effects. Agent work produces checkpoint and proposal states beneath it. Landing a proposal creates a new accepted state and safely materializes it in the visible project folder without moving the user’s Git branch or real index. Controller integrations may capture the stronger pre-delivery boundary as well.

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
- Codex Desktop first-action prompt capture and session-aware follow-ups
- Controller-grade pre-delivery prompt capture
- Isolated detached worktrees per attempt
- Immutable checkpoints and proposals
- Checks bound to exact source trees
- Real three-way content merging, including compatible same-file edits
- Agent-ready reconciliation workspaces for genuine merge conflicts
- Optional validation on the final integrated tree
- Compare-and-swap acceptance
- Safe visible-root materialization on Desktop landing
- Controller-only internal acceptance plus explicit root synchronization
- Forward-only undo of the latest accepted transition
- Git-compatible accepted commits under `refs/hop/accepted`
- SQLite WAL state graph and machine-readable JSON output
- Pre-persistence credential redaction across prompts, summaries, and check evidence
- Embedded, installable vendor-neutral agent skill with implicit invocation enabled

It does not yet include a trusted raw-prompt hook, project knowledge, claims, remote synchronization, a GUI, or semantic merging.

Start with [Installation](#installation) and [Getting started](#getting-started),
or browse the [documentation wiki](wiki/Home.md). See
[the product blueprint](docs/product-blueprint.md) for the complete model,
design principles, and phased roadmap.

## How the pieces fit

```text
Prompt in Codex Desktop
  │
  ▼
Hop skill's first action ──> hop begin ──> prompt state + workspace
                                                │
                                                ▼
                                      edit → check → propose
                                                │
                                                ▼
Agent                           validate → auto-land → accepted state
                                                   → visible project root
```

The user continues typing into Codex Desktop normally. The skill invokes `hop begin` before inspecting or changing the project. `hop begin` initializes Hop when needed, uses `CODEX_THREAD_ID` to recognize follow-ups, checkpoints prior effects, and returns the isolated workspace. The skill confines work to that workspace, validates it, and automatically lands a successful proposal. `hop land` advances accepted state and then projects that exact tree into the selected folder. It proceeds only when the visible source still matches an accepted Hop state, so user edits are never overwritten. The original task prompt is the authorization; no extra landing prompt is required. Say `review first` or `proposal only` to opt into manual approval. This is a pre-project-effect boundary: the prompt is stored after Codex receives it but before the agent performs project work. A controller or future trusted prompt hook can provide strict pre-delivery capture.

## Installation

Packaged releases require Git 2.40 or newer. Go is not required unless you use
the developer installation path.

### macOS and Linux — recommended

The installer detects your operating system and CPU, downloads the matching
release archive, verifies its SHA-256 checksum, installs `hop` to
`~/.local/bin`, adds that directory to your shell PATH when necessary, and
installs the Codex skill:

```bash
curl -fsSL https://githop.xyz/hop/hop/raw/branch/main/scripts/install.sh | sh
```

To inspect the installer before running it:

```bash
curl -fsSLO https://githop.xyz/hop/hop/raw/branch/main/scripts/install.sh
less install.sh
sh install.sh
```

Pin a version or choose another destination with environment variables:

```bash
HOP_VERSION=v0.1.0 HOP_INSTALL_DIR="$HOME/bin" sh install.sh
```

### Windows PowerShell

Run PowerShell as your normal user. The installer verifies the release,
installs `hop.exe` under `%LOCALAPPDATA%\Programs\Hop`, adds it to your user
PATH, and installs the Codex skill:

```powershell
irm https://githop.xyz/hop/hop/raw/branch/main/scripts/install.ps1 | iex
```

### Go install — developer path

If Go 1.26 or newer is already installed:

```bash
go install githop.xyz/hop/hop/cmd/hop@latest
hop skill install --force
```

Ensure `$(go env GOPATH)/bin` is on PATH. Tagged module builds report the tag
through `hop version`.

### Build from source

Cloning is supported, but it is the contributor/fallback path rather than the
normal product installation:

```bash
git clone https://githop.xyz/hop/hop.git
cd hop
go test ./...
go build -trimpath -o hop ./cmd/hop
mkdir -p "$HOME/.local/bin"
install -m 755 hop "$HOME/.local/bin/hop"
"$HOME/.local/bin/hop" skill install --force
```

### Verify the installation

```bash
hop version
hop help
```

The packaged installers install two things:

- the `hop` CLI; and
- the embedded Hop agent skill at `${CODEX_HOME:-~/.codex}/skills/hop`.

No project is modified during installation. A project receives its local
`.hop/` state only when Hop first runs inside that project.

Release installer URLs become available after the corresponding Gitea Release
is published. Until the first public release exists, use the source build.

## Getting started

1. Install Hop using one of the methods above.
2. Restart Codex Desktop if it was open during installation.
3. Open any existing Git project in Codex Desktop.
4. Ask Codex to change the project normally. You do **not** need to run
   `hop init`, create a branch, or route prompts through another application.
5. After the first task, run `hop status` in the project to inspect its accepted
   state and confirm the visible root is synchronized.

The skill is configured for implicit use on every local repository turn. If an
agent ever fails to activate it automatically, mention `$hop` in that task as a
deterministic fallback.

The agent's first project action is equivalent to:

```bash
hop begin --agent codex --heredoc <<'HOP_PROMPT_EOF'
<the current user message>
HOP_PROMPT_EOF
```

The agent—not the user—runs this command. It initializes Hop without changing
the current Git branch, working tree, or index, stores the prompt, and returns
the state IDs and isolated workspace. Follow-up messages in the same Codex task
automatically continue the same Hop attempt.

For another agent runtime, export the embedded skill to that runtime's skills
directory:

```bash
hop skill install --path /path/to/agent/skills --force
```

After editing the printed workspace, the agent runs this lifecycle automatically:

```bash
hop check P_... -- go test ./...
hop propose --summary "Added password reset emails" P_...
hop land R_... -- go test ./...
```

The command supplied to `land` runs in a temporary worktree containing the exact final tree that would become accepted. If it fails, `refs/hop/accepted` and the SQLite accepted head do not move. After acceptance, Hop updates only the visible working files through a disposable Git index; HEAD, the active branch, and the real index do not move. Compatible edits to the same file are merged automatically. A genuine merge conflict becomes a fresh agent reconciliation workspace, which the skill resolves, checks, and lands without asking the user to manage source-control mechanics. The agent pauses only for an explicit review-first request, failed validation, visible-root divergence, unresolved product intent, or newly required destructive/external scope.

`land` and `accept` are intentionally different:

```bash
hop land R_... -- go test ./...   # Desktop: accept and synchronize the selected folder
hop accept R_... -- go test ./... # Controller: accept internally, leave the folder untouched
hop sync                          # Catch a stale visible folder up to accepted state
```

`hop sync` is also the upgrade path for projects accepted by an older Hop build. It synchronizes only when the folder still matches an accepted ancestor.

Inspect the project:

```bash
hop status
hop graph
hop state P_...
hop diff R_...
hop history
hop sync
hop doctor
```

Inspect or hand the skill to a harness without installing it:

```bash
hop skill print
```

For a harness or controller that can capture prompts before delivery, initialize and start explicitly:

```bash
hop init
hop start --agent codex --heredoc <<'HOP_PROMPT_EOF'
Add password reset emails
HOP_PROMPT_EOF
eval "$(hop env P_...)"
```

In this mode, deliver the same message only after `hop start` succeeds. Use `hop prompt --from P_... --heredoc` for controller-managed follow-ups.

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

If their changed paths are disjoint, both proposals can land in either order. The second proposal is composed onto the latest accepted tree and validated there.

If both proposals changed the same file, Hop performs a real three-way merge.
Independent hunks and identical changes land automatically. Only genuine merge
conflicts pause acceptance; `hop land` prepares a fresh reconciliation
workspace and the agent resolves, retests, reproposes, and lands it
automatically. Text conflicts use diff3 markers; structural and binary
conflicts may not. The visible project root remains at the last accepted state
until that resolution passes.

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

The repository’s `.git/info/exclude` receives `.hop/`; the public `.gitignore`, current branch, and real Git index are left alone. `hop land`, `hop sync`, and `hop undo` update visible source files only after confirming that the folder matches accepted Hop history.

Initialization refuses to proceed if `.hop` is already tracked as user-owned project source.

## JSON protocol

Add `--json` anywhere:

```bash
hop --json status
hop begin --agent codex --heredoc --json
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

Hop uses Git's three-way content merge rather than treating shared paths as
conflicts. Textual cleanliness still cannot prove behavioral compatibility, so
automatic acceptance reruns the strongest relevant validation on the exact
final tree. Manual acceptance remains available as an explicit review-first
mode.

Desktop landing fails closed when nonignored visible source does not exactly match an accepted Hop state or when an ignored/untracked path would be overwritten. It never uses `reset --hard`, moves HEAD, or rewrites the user's real index. The lower-level `hop accept` command deliberately retains controller-only behavior and does not update the visible root.

Agent-reported scope and test claims are never used as the source of truth. Hop computes source trees and changed paths itself.

Non-secret prompt text and check output are stored locally in SQLite without encryption. Before persistence, Hop redacts high-confidence provider tokens and contextual credentials, private keys, authorization headers, and credential-bearing URLs. The same boundary sanitizes proposal summaries plus recorded validation commands and output.

Detection cannot recognize every private or future token format. Prefer environment variables or a secret manager, and rotate any real credential pasted into any agent prompt even when Hop reports that it redacted the value.

Skill-driven Desktop capture stores the agent's verbatim transcription of the visible message and attachment references. It cannot prove byte-for-byte fidelity with Codex's raw submission. A trusted `UserPromptSubmit` hook is the future deterministic capture boundary; the skill remains the no-UI-change alpha workflow.
