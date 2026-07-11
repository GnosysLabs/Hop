# Codex Desktop and agent workflow

## Codex Desktop

Users type into Codex normally. The installed skill makes prompt capture the
agent's first repository action:

```bash
hop begin --agent codex --heredoc <<'HOP_PROMPT_EOF'
<exact visible user message>
HOP_PROMPT_EOF
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

No second landing authorization is requested unless the user explicitly asks
for review-first behavior.

Desktop capture stores the agent's verbatim transcription of the visible
message and its attachment references. Because the skill runs after Codex
receives the message, it cannot prove byte-for-byte fidelity with the raw
submission. A trusted prompt-submission hook or controller is the deterministic
capture boundary.

## Follow-up messages

A later `hop begin` with the same Codex task session checkpoints existing
workspace effects, appends a new prompt state, and continues the same attempt
while that work remains unfinished. If Hop prepares reconciliation, the session
follows its fresh workspace. After the result lands, the next prompt starts a
new task and attempt rooted at the latest accepted state. Completed workspaces
are never reopened, and the user does not carry state IDs between messages.

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

This provides a stronger pre-delivery boundary than a Desktop skill, which can
only guarantee capture before project effects.

## Agent rules

- Never edit the canonical project root directly.
- Never mutate a frozen proposal.
- Do not bypass `hop land` with Git reset, checkout, worktree, or manual copying.
- Run validation against immutable checkpoints and the final integrated tree.
- Let Hop merge compatible concurrent work.
- Resolve genuine reconciliation workspaces without asking the user to perform
  source-control mechanics, unless the underlying product intents are ambiguous.
