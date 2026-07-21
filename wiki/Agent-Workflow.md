# Agent integrations and workflow

## Skill-based integration

A compatible agent skill makes prompt capture the agent's first repository
action. The integration supplies a stable agent and session identity. In a
POSIX shell:

```bash
hop begin --agent my-agent --session stable-session-id --heredoc <<'HOP_PROMPT_EOF'
<exact visible user message>
HOP_PROMPT_EOF
```

In PowerShell:

```powershell
$hopPrompt = @'
<exact visible user message>
'@
$hopPrompt | hop begin --agent my-agent --session stable-session-id --heredoc
```

The agent adopts the returned `HOP_STATE_ID`, `HOP_TASK_ID`,
`HOP_ATTEMPT_ID`, and `HOP_WORKSPACE`, then confines reads, commands, edits, and
tests to that workspace.

The normal lifecycle is:

```bash
hop check P_... -- go test ./...
hop propose --summary "Implemented the requested behavior" P_...
hop land R_... -- go test ./...
```

One `hop land` call also handles a normal clean commit made by another tool or
agent after the proposal was frozen. Hop adopts that exact strict-fast-forward
commit as the concurrent baseline and composes the proposal onto it. Agents do
not reset, copy, re-propose, or manually reconcile a clean committed advance.
If only final validation fails, they inspect the evidence and retry the same
proposal with the corrected command or environment; a new proposal is needed
only after source changes.

Immediately before the agent sends its final response, it records both the
concise summary and the exact response text:

```sh
hop complete --summary "Implemented the requested behavior" --heredoc P_... <<'HOP_FINAL_EOF'
Implemented the requested behavior and verified the test suite.
HOP_FINAL_EOF
```

The response sent to the user must exactly match the heredoc body. Completion
also applies to read-only diagnostics and external operations that do not
produce a proposal.

No second landing authorization is requested unless the user explicitly asks
for review-first behavior. After acceptance, Hop automatically pushes the
accepted commit when the repository has an unambiguous upstream. The agent does
not ask the user to run `git push`.

Skill-based capture stores the agent's verbatim transcription of the visible
message and its attachment references. Because the skill runs after the client
receives the message, it cannot prove byte-for-byte fidelity with the raw
submission. A trusted prompt-submission hook or controller is the deterministic
capture boundary.

### Codex Desktop example

The bundled Codex integration uses `CODEX_THREAD_ID` as its stable session key,
defaults the agent name to `codex`, and lets the user type normally. Its bundle
is installed at `${CODEX_HOME:-~/.codex}/skills/hop`; the same files are also
installed at `~/.agents/skills/hop` for compatible clients and at
`~/.claude/skills/hop` for Claude Code.

## Follow-up messages

A later `hop begin` with the same integration session checkpoints existing
workspace effects, appends a new prompt state, and continues the same attempt
while that work remains unfinished. If Hop prepares reconciliation, the session
follows its fresh workspace. After the result lands, the next prompt starts a
new task and attempt rooted at the latest accepted state. Completed workspaces
are never reopened, and the user does not carry state IDs between messages.
If an unfinished session was parked after 24 hours of inactivity, the same
`hop begin` rehydrates its checkpointed workspace before appending the follow-up.

## Controller-grade capture

A harness that can persist before delivering a prompt to the model can use:

```bash
hop init
hop start --agent my-agent --heredoc <<'HOP_PROMPT_EOF'
Add password reset emails
HOP_PROMPT_EOF
eval "$(hop env P_...)"
```

Only deliver the prompt after `hop start` exits successfully. Controller-managed
follow-ups use:

```bash
hop prompt --from P_... --heredoc
```

This provides a stronger pre-delivery boundary than an agent-side skill, which
can only guarantee capture before project effects.

## Agent rules

- Never edit the canonical project root directly.
- Never mutate a frozen proposal.
- Inspect landing warnings. If automatic push failed transiently, retry once
  with `hop push`; never force-push a diverged remote.
- Do not bypass `hop land` with Git reset, checkout, worktree, or manual copying.
- Run validation against immutable checkpoints and the final integrated tree.
- Let Hop merge compatible concurrent work.
- Resolve genuine reconciliation workspaces without asking the user to perform
  source-control mechanics, unless the underlying product intents are ambiguous.
