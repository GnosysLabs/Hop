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
| `HOP_AGENT` | Optional runtime name used by `hop begin` when `--agent` is omitted |

Interactive agents may begin without these variables. `hop begin` returns the
equivalent IDs and workspace. Integrations should identify themselves with
`HOP_AGENT` or `--agent` and pass a stable `--session` value when available.
That session binds later messages to unfinished work; without it, each
invocation begins independent work. The Codex adapter uses `CODEX_THREAD_ID` as
the default session and `codex` as the default runtime name. Follow-ups before
acceptance continue the attempt; the first prompt after acceptance starts a
fresh task and attempt at the latest accepted state.

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

POSIX shell:

```bash
hop begin --heredoc <<'HOP_PROMPT_EOF'
<exact current user message>
HOP_PROMPT_EOF
```

PowerShell:

```powershell
$hopPrompt = @'
<exact current user message>
'@
$hopPrompt | hop begin --heredoc
```

Then continue in the returned workspace:

```bash
hop state "$HOP_STATE_ID" --json
hop status --json
hop check "$HOP_STATE_ID" -- <command>
hop propose --summary "<summary>" "$HOP_STATE_ID"
hop land <proposal-state> -- <final validation command>
hop complete --summary "<summary>" --heredoc "$HOP_STATE_ID"
hop gc
hop refresh <proposal-state>
hop push
```

An adapter may set `HOP_AGENT=<runtime>` or pass `--agent <runtime>`. If it has
a stable conversation/run identifier, it should also pass `--session <id>` on
every `hop begin`. Codex normally needs neither explicit flag because
`CODEX_THREAD_ID` supplies its session adapter automatically.

`hop check` snapshots the attempt and runs the command in a detached worktree materialized from that exact checkpoint. Edits made concurrently in the live workspace do not change the tested tree.

`hop propose` freezes the current nonignored workspace tree. Later workspace edits cannot change the proposal.

`hop complete` records the concise summary and exact user-visible final response
for the current prompt state. Completion is deliberately separate from the Git
state graph: read-only diagnostics, deployments, and other external operations
can finish without manufacturing a proposal. The command accepts the final
response through `--stdin` or `--heredoc`, persists it before delivery, and
immediately attempts private prompt sync. It also closes source-clean read-only
attempts and reclaims source-clean terminal worktrees. Begin, completion, and
`hop gc` park other attempts inactive for 24 hours by checkpointing their exact
tree and removing only the checkout. A later message in the original session
rehydrates that checkpoint automatically. `hop gc --all` parks every other
unfinished attempt immediately and archives dirty terminal worktrees. The
calling workspace and immutable state history are never deleted.
Agents call completion as their final tool action, then send the identical text
to the user without intervening work.

The initial task prompt authorizes the agent to run `hop land` after successful
validation; a second user approval is not required. Manual review is an opt-in
mode: stop at the proposal only when the user explicitly asks to review or
approve before acceptance. Validation failure, protected staged/index state or
ignored-root collisions, unresolved product ambiguity, or newly required
destructive/external scope stops automatic acceptance. Path overlap, ordinary
nonignored root edits, and a stale accepted head do not: Hop captures, merges,
retries, or prepares agent reconciliation.

`hop land` is the interactive working-root operation. It performs a real Git
three-way content merge, so compatible edits in the same file and identical
changes compose automatically. It validates and advances accepted state, then
safely materializes that tree into the selected visible project root. Ordinary
nonignored root edits are first captured as an explicit accepted transition,
then merged against the proposal; genuine overlaps enter the same agent
reconciliation flow. Staged/index state and ignored destination collisions
remain protected. Materialization uses a disposable index and never moves HEAD,
the active branch, or the user's real index.

This means raw `git status` is not an authoritative description of user work in
the visible root. When the accepted tree is projected over an older branch or
index, Git reports the projection as dirtiness. Use `hop status --json`: its
`git.projection_only_changes`, `git.user_worktree_changed`, and
`git.user_index_changed` fields distinguish the projection from genuine edits.

When either the accepted-state merge or the fetched upstream-branch merge has
genuine unresolved conflicts, `hop land` returns exit `20` and automatically
prepares a reconciliation prompt in the original task but a fresh isolated
attempt/workspace. Its JSON includes `reconciliation.prompt`, `workspace`,
`conflicts`, the proposal/current accepted states, and `remote_tip` when the
upstream branch is an input. The agent adopts that prompt/workspace, resolves
both intents, checks, proposes, and lands again. Hop records the reconciled
remote tip and re-fetches before acceptance, so later compatible remote
advancement is composed without reopening the resolved conflict.
Structural, binary, delete/rename, mode, and symlink conflicts may have no text
markers, so the agent must inspect both returned input states. Hop requires a
successful `hop check` on the resolved tree before reproposal. The user is not
asked to coordinate ordinary code conflicts. `hop refresh` is the idempotent
explicit form of the same preparation step.

`hop accept` is the controller/kernel operation. It advances SQLite and
`refs/hop/accepted` but intentionally leaves the visible root untouched.
`hop sync` safely catches a stale accepted-ancestor root up to the current
accepted state, including projects created with older Hop builds.

After every successful `hop land` or `hop accept`, Hop automatically performs a
non-forced push of the accepted commit when the active branch has an
unambiguous upstream. No remote is a normal local-only mode. Push failure does
not undo acceptance. Hop durably records the target, attempt time, sanitized
failure category, retryability, and last authoritative remote tip. `hop push`
retries the current accepted commit and clears the warning after success.
Agents must never replace a non-fast-forward rejection with a force-push.

`hop begin` is the interactive-agent entry point. It initializes Hop when
necessary and captures the current message before the agent performs project
work. Runtime adapters identify themselves through `HOP_AGENT` or `--agent`
and use `--session` to supply a stable conversation/run key. A later
`hop begin` with the same session checkpoints the prior workspace before
appending a follow-up while that work remains unfinished. Reconciliation
transfers the session to its fresh attempt. After a proposal is accepted, the
next `hop begin` starts from the latest accepted state and never reopens the
completed workspace. Codex Desktop supplies `CODEX_THREAD_ID` as its default
session key, so its adapter does not need to add `--session` explicitly.

Pass the original message to `hop begin` without model-side redaction. Hop's
sanitizer replaces detected credential values before any durable write and
returns only typed redaction counts. Do not place the value in any later
command, summary, output, or source file.

## Account credential boundary

Hop never creates, rotates, lists, or revokes provider access tokens. Agents
must not use account token-management APIs or settings pages as a shortcut for
publishing work.

Run `hop host` to discover the repository's provider. Core Hop publication uses
normal Git remotes and credentials and therefore works with GitHub, GitLab,
Gitea, generic Git servers, and local bare repositories. Preserve the configured
remote unless the user explicitly asks to replace it.

Host-aware collaboration commands delegate to `gh` on GitHub, `glab` on GitLab,
and the embedded compatibility adapter on Gitea. Use an existing provider CLI
or OS-keychain credential. Do not request, create, rotate, enumerate, revoke,
print, or persist an access token. If authentication is absent, ask the user to
complete the provider's normal login flow; never mint a task-named token.

Gitea's legacy `hop auth`, `hop forge api`, and embedded Tea-compatible commands
remain available as a compatibility adapter. They are not part of the core Git
protocol and must not be required for repositories hosted elsewhere.

## Exit codes

| Code | Meaning |
|---:|---|
| `0` | Success |
| `1` | Git, SQLite, filesystem, or internal error |
| `2` | Invalid CLI usage |
| `20` | Genuine three-way merge conflict; reconciliation workspace prepared |
| `21` | Accepted or attempt head changed during compare-and-swap |
| `22` | Validation command failed |
| `23` | Visible project root diverged or contains an overwrite collision |

A failed `hop check` or final landing check persists its evidence. A blocked or failed landing does not advance accepted state.

## Capture modes

### Interactive agent skill

The user types normally in their agent interface. The Hop skill makes
`hop begin` its first project action and then directs every operation into the
returned workspace. This is a pre-project-effect boundary: the runtime has
already received the prompt, but no repository inspection, command, or
modification may precede the durable prompt state. Codex Desktop is one such
adapter; it provides session continuity through `CODEX_THREAD_ID`.

### Controller-grade pre-delivery capture

```bash
hop init
hop start --agent <runtime> "Add password reset emails"
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
- **Capture failure:** retry and diagnose Hop without inspecting repository
  contents. Read-only repository inspection is still a project effect and is
  not a fallback for failed prompt capture.
- **Completion sync warning:** send the already-recorded response, then let a later `hop sync` retry the durable local completion.
- **Check failure:** fix the live workspace, checkpoint/check again, then create a new proposal.
- **Review-only request:** preserve and report the proposal without landing it.
- **Frozen proposal needs changes:** record a follow-up prompt; never mutate the stored proposal.
- **Merge conflict on landing:** continue automatically in the returned
  reconciliation prompt/workspace; inspect both inputs, resolve textual and
  structural conflicts, validate, propose, and land again. Stop only for
  product ambiguity, not ordinary textual overlap.
- **Visible-root edits:** `hop land` captures ordinary nonignored edits and merges them automatically. Continue through any returned reconciliation. For protected staged/index state or ignored collisions, preserve the proposal and user files; never substitute controller-only `hop accept`.
- **Controller-accepted root is stale:** run `hop sync`; it succeeds only from an accepted ancestor and never overwrites divergence.
- **Automatic push warning:** retry once with `hop push`; preserve a diverged remote and never force-push it.
- **Ref inconsistency:** run `hop doctor`; use `hop doctor --repair` only outside final validation.
- **Secrets:** Hop redacts high-confidence provider keys plus contextual tokens,
  passwords, private keys, authorization headers, and credential-bearing URLs
  before durable storage. It also sanitizes recorded check commands/output and
  proposal summaries. Detection is defense in depth, not a substitute for
  environment variables or a secret manager. Never repeat a detected secret.
