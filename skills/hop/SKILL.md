---
name: hop
description: Capture local repository prompts as Hop states and perform agent work in isolated Hop workspaces. Use at the start of every Codex Desktop or CLI repository turn and follow-up, before inspecting files, running project commands, editing, reviewing, delegating, landing, or undoing—even when the user does not mention Hop. Also use whenever HOP_STATE_ID, HOP_TASK_ID, HOP_ATTEMPT_ID, CODEX_THREAD_ID, or .hop/hop.db is present.
---

# Hop

Make prompt capture the first project action, then keep all effects inside the
returned Hop workspace.

## Capture the current prompt first

Do not inspect repository files, plan from repository contents, run project
commands, edit, or delegate before capture. Run this from the selected project
directory:

```bash
hop begin --agent codex --heredoc <<'HOP_PROMPT_EOF'
<copy the current user message verbatim>
HOP_PROMPT_EOF
```

Choose a different quoted delimiter if that exact delimiter appears in the
message. Include visible attachment paths and references. Do not paraphrase,
pre-redact, or omit a suspected credential in this one capture stream; Hop must
see it to replace it deterministically before persistence. `--heredoc` removes
only the shell-added final newline. Never copy the credential anywhere else.

`hop begin` performs the Desktop bootstrap:

- Initialize Hop automatically when the project has not used it before.
- Use `CODEX_THREAD_ID` to bind this Codex task to one Hop attempt.
- Create a prompt state and isolated workspace on the first turn.
- Checkpoint prior workspace effects and append a prompt state on follow-ups.
- Redact detected API keys, tokens, passwords, private keys, authorization
  headers, and credential-bearing connection strings before persistence.

Read the returned `HOP_STATE_ID`, `HOP_TASK_ID`, `HOP_ATTEMPT_ID`, and workspace.
If capture fails or `hop` is unavailable, stop without project effects and
report the error.

If Hop reports redactions, never repeat the credential in output, summaries,
commands recorded as evidence, or proposal text. Refer to its environment
variable or secret-manager name instead.

## Enforce the workspace boundary

- Direct every shell command to the returned workspace.
- Use absolute paths beneath that workspace for file reads and edits.
- Never edit the selected canonical project root.
- Do not run `git commit`, `git checkout`, `git switch`, `git branch`,
  `git rebase`, `git reset`, `git stash`, or `git worktree`.
- Do not stage files. Hop captures every nonignored workspace change.
- Give a subagent project-changing work only after creating a distinct Hop
  prompt/attempt for that delegation.
- Never silently merge overlapping proposals.

Verify the captured state before making changes:

```bash
hop state <HOP_STATE_ID> --json
hop status --json
```

## Execute and auto-accept

1. Inspect and modify only the Hop workspace.
2. Keep the change scoped to the captured prompt.
3. Bind validation evidence to an immutable checkpoint:

   ```bash
   hop check <HOP_STATE_ID> -- <test-command> [args...]
   ```

4. Fix failures in the live Hop workspace and rerun checks.
5. Freeze project changes as a proposal:

   ```bash
   hop propose --summary "<behavioral summary>" <HOP_STATE_ID>
   ```

6. Unless the user explicitly requested review-only mode, immediately accept
   the proposal and validate the exact final tree:

   ```bash
   hop land <proposal-state> -- <final-test-command> [args...]
   ```

7. Report the accepted result, validation, and remaining risks. Keep internal
   state and evidence IDs out of the normal response unless they help explain a
   failure or the user asks for them. Confirm that `hop land` reported the
   selected visible project root as synchronized.

For a read-only or informational turn, the prompt state is sufficient; do not
invent a proposal when the workspace tree is unchanged.

Do not edit a frozen proposal. A user follow-up triggers this skill again;
run `hop begin` again before acting. Session binding selects the existing
attempt automatically, so the user never needs to carry state IDs.

## Auto-accept by default

The captured task prompt authorizes accepting the local project changes needed
to complete that task. Do not ask for separate landing permission and do not
capture a second prompt merely to land. After checks pass and the proposal is
frozen, run `hop land` as part of the same turn.

Use the strongest relevant final validation command. If the task truly has no
runnable validation, `hop land <proposal-state>` is allowed and the final
response must say that acceptance was not validated by a command.

Stop before acceptance only when:

- the user explicitly says `review first`, `proposal only`, `do not land`, or
  otherwise asks to approve the result before it is accepted;
- validation fails;
- Hop reports overlap, a changed accepted head, or visible-root divergence; or
- acceptance would require a destructive, external, or out-of-scope action not
  authorized by the captured task.

On overlap or validation failure, preserve the proposal and report the block.
If visible-root synchronization is blocked, do not bypass it with `hop accept`,
force checkout, reset, or file copying. Preserve the proposal and identify the
user-owned paths that must be resolved. `hop accept` is reserved for an
explicitly controller-only workflow; Desktop work always uses `hop land`.
Use `hop undo` only after a separately captured, explicit user request.

Read [references/protocol.md](references/protocol.md) for state semantics, exit
codes, recovery, and controller-grade pre-delivery capture. Skill-driven
Desktop capture is a pre-project-effect boundary; it does not claim the prompt
was stored before Codex received it.
