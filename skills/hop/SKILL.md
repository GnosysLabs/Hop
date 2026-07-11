---
name: hop
description: Work safely in Hop prompt-native version-control projects. Use whenever HOP_STATE_ID, HOP_TASK_ID, or HOP_ATTEMPT_ID is set; when a repository contains .hop/hop.db; or when the user asks to use Hop to isolate, checkpoint, validate, propose, land, inspect, continue, or undo coding-agent work instead of directly managing Git branches or commits.
---

# Hop

Use Hop as the change-control boundary between agent work and accepted project state.

## Enforce the boundary

- Require a durable Hop prompt state before making repository changes.
- Work only in the assigned `HOP_WORKSPACE`; never edit the canonical project root.
- Do not run `git commit`, `git checkout`, `git switch`, `git branch`, `git rebase`, `git reset`, `git stash`, or `git worktree`. Hop owns snapshots and worktrees.
- Do not stage files. Hop captures every nonignored workspace change.
- Do not land your own proposal unless the user explicitly requests it. Default to stopping after proposal creation.
- Never resolve an overlap by silently merging. Preserve the proposal and request or create a reconciliation prompt.

## Verify the launch context

Before planning or editing:

```bash
command -v hop
test -n "$HOP_ROOT"
test -n "$HOP_STATE_ID"
test -n "$HOP_TASK_ID"
test -n "$HOP_ATTEMPT_ID"
test -n "$HOP_WORKSPACE"
hop state "$HOP_STATE_ID" --json
hop status --json
```

Confirm the current working directory is `HOP_WORKSPACE` or direct every filesystem operation there.

If the Hop variables are missing, stop before editing. Explain that the controller must first run:

```bash
hop start --agent <agent-name> "<exact prompt>"
```

Then relaunch or redirect the agent into the printed workspace with the printed environment. A skill loaded after prompt delivery cannot retroactively guarantee pre-delivery recording.

## Execute the task

1. Read the prompt state and current Hop status.
2. Inspect and modify only the assigned workspace.
3. Keep the change scoped to the recorded instruction.
4. Run relevant validation through Hop so evidence is bound to an immutable checkpoint:

```bash
hop check "$HOP_STATE_ID" -- <test-command> [args...]
```

5. Fix failures in the workspace and rerun the check as needed.
6. Freeze the result as a proposal:

```bash
hop propose --summary "<behavioral summary>" "$HOP_STATE_ID"
```

7. Report the proposal ID, checks run, remaining risks, and any follow-up needed. Do not continue editing the frozen proposal; later changes require another prompt and proposal.

## Handle follow-up instructions

Every follow-up instruction needs a new prompt state before effects.

- If the controller supplies a new `HOP_STATE_ID`, inspect it and continue.
- If no new state was supplied, stop before acting and ask the controller to record the exact follow-up:

```bash
hop prompt --from <current-state> "<exact follow-up>"
```

The command first checkpoints prior effects and then creates the follow-up prompt state. Continue only from the returned prompt state.

## Land only with explicit authority

When the user explicitly asks to land a proposal, validate the exact final composed tree:

```bash
hop land <proposal-state> -- <final-test-command> [args...]
```

- On success, report the accepted-state ID.
- On overlap, do not mutate or discard the proposal. Report the conflicting paths and request a reconciliation prompt based on the latest accepted state.
- On final validation failure, preserve the failed state and evidence, then request a corrective follow-up.
- If no final test command is available, state clearly that landing will be manual and unvalidated.

## Inspect and recover

Use these commands as needed:

```bash
hop status
hop graph
hop state <state-id>
hop diff <state-id>
hop history
hop doctor
```

Use `hop undo` only when the user explicitly asks to undo the latest accepted transition. It creates a new forward state; it does not erase history.

Read [references/protocol.md](references/protocol.md) when command semantics, state kinds, exit codes, or troubleshooting details are needed.

