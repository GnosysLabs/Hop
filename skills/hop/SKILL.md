---
name: hop
description: Capture local repository prompts as Hop states and perform agent work in isolated Hop workspaces. Use at the start of every interactive coding-agent repository turn and follow-up, before inspecting files, running project commands, editing, reviewing, delegating, landing, or undoing—even when the user does not mention Hop. Also use whenever HOP_STATE_ID, HOP_TASK_ID, HOP_ATTEMPT_ID, HOP_AGENT, CODEX_THREAD_ID, or .hop/hop.db is present.
---

# Hop

Make prompt capture the first project action, then keep all effects inside the
returned Hop workspace.

## Capture the current prompt first

Do not inspect repository files, plan from repository contents, run project
commands, edit, or delegate before capture. Run the form for the current shell
from the selected project directory.

POSIX shell:

```bash
hop begin --heredoc <<'HOP_PROMPT_EOF'
<copy the current user message verbatim>
HOP_PROMPT_EOF
```

PowerShell:

```powershell
$hopPrompt = @'
<copy the current user message verbatim>
'@
$hopPrompt | hop begin --heredoc
```

Choose a different non-interpolating stdin construction if the applicable
terminator appears in the message. Include visible attachment paths and
references. Do not paraphrase, pre-redact, or omit a suspected credential in
this one capture stream; Hop must see it to replace it deterministically before
persistence. `--heredoc` removes only the shell-added final newline. Never copy
the credential anywhere else.

An integration may identify its runtime through `HOP_AGENT` or `--agent`, and
should pass a stable `--session` value when it has one. A stable session lets
Hop connect unfinished follow-ups without making the user carry state IDs.
Codex is one adapter example: when `CODEX_THREAD_ID` is present, `hop begin`
uses it as the default session and identifies the runtime as `codex` unless
`HOP_AGENT` or `--agent` overrides that name.

`hop begin` performs the interactive-agent bootstrap:

- Initialize Hop automatically when the project has not used it before.
- Use the integration's stable session identity to bind later messages to
  unfinished Hop work. Without one, each invocation begins independent work.
- Create a prompt state and isolated workspace on the first turn.
- Checkpoint prior workspace effects and append follow-ups until that work lands.
- Follow a reconciliation into its fresh attempt, then start the first prompt
  after landing from the latest accepted state instead of reopening old work.
- Redact detected API keys, tokens, passwords, private keys, authorization
  headers, and credential-bearing connection strings before persistence.

Read the returned `HOP_STATE_ID`, `HOP_TASK_ID`, `HOP_ATTEMPT_ID`, and workspace.
Allow at least 120 seconds for capture. If it times out or fails transiently,
retry once with the same message, agent, and session; Hop session binding makes
the retry idempotent for the task. If the retry fails, inspect Hop's error and
locks, run `hop doctor`, and repair a safe local operational problem when
possible. Stop without project effects only when Hop remains unavailable or
unsafe after recovery; do not abandon the user's request after the first
timeout.

If Hop reports redactions, never repeat the credential in output, summaries,
commands recorded as evidence, or proposal text. Refer to its environment
variable or secret-manager name instead.

## Enforce the workspace boundary

- Direct every shell command to the returned workspace.
- Use absolute paths beneath that workspace for file reads and edits.
- Never edit the selected canonical project root.
- Do not run `git commit`, `git checkout`, `git switch`, `git branch`,
  `git rebase`, `git reset`, `git stash`, `git worktree`, or `git push`.
- Do not stage files. Hop captures every nonignored workspace change.
- Never create, rotate, enumerate, revoke, or paste account access tokens
  through a provider website or API. For release or publishing work, use only a
  credential the user has already provisioned in an OS secret store or supplied
  through the runtime's secret mechanism. If it is missing, stop and ask the
  user to provision it; do not call a token-management endpoint.
- Give a subagent project-changing work only after creating a distinct Hop
  prompt/attempt for that delegation.
- Never discard either side of concurrent work. Let Hop perform its three-way
  merge, then resolve only the genuine conflict hunks in the reconciliation
  workspace it returns.

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

6. Unless the user explicitly requested review-only mode, immediately land the
   proposal and validate the exact final tree:

   ```bash
   hop land <proposal-state> -- <final-test-command> [args...]
   ```

   Same-file edits with compatible hunks merge automatically.

7. If `hop land` reports a prepared reconciliation prompt/workspace, continue
   immediately in that returned workspace. Do not stop or ask the user to
   coordinate an ordinary code conflict:

   - adopt every returned `HOP_*` value and the fresh reconciliation workspace;
   - inspect every conflict candidate plus both returned proposal/current
     accepted states; compare their commits when a delete/rename, binary, mode,
     symlink, or directory conflict has no text markers;
   - resolve every conflict intelligently, preserving both compatible intents;
   - remove all merge markers;
   - run `hop check` with the returned prompt state (Hop requires checked
     reconciliation evidence before it will accept a new proposal);
   - create a new proposal and run `hop land` again; and
   - repeat if accepted state raced forward again.

8. Report the accepted result, validation, and remaining risks. Keep internal
   state and evidence IDs out of the normal response unless they help explain a
   failure or the user asks for them. Confirm that `hop land` reported the
   selected visible project root as synchronized. When it reports an automatic
   push warning, retry once with `hop push`; never force-push or ask the user to
   perform routine source-control mechanics.

For a read-only or informational turn, the prompt state is sufficient; do not
invent a proposal when the workspace tree is unchanged.

Do not edit a frozen proposal. A user follow-up triggers this skill again;
run `hop begin` again before acting. Session binding selects unfinished work
automatically and rolls completed work onto the latest accepted state, so the
user never needs to carry state IDs.

## Auto-accept by default

The captured task prompt authorizes accepting the local project changes needed
to complete that task. Do not ask for separate landing permission and do not
capture a second prompt merely to land. After checks pass and the proposal is
frozen, run `hop land` as part of the same turn.

An existing unambiguous Git upstream is standing project configuration for
non-forced publication of accepted states. Hop pushes accepted commits
automatically after landing; prompts, checkpoints, proposals, and `.hop/` state
remain local. Do not run raw `git push`.

Use the strongest relevant final validation command. If the task truly has no
runnable validation, `hop land <proposal-state>` is allowed and the final
response must say that acceptance was not validated by a command.

Stop before acceptance only when:

- the user explicitly says `review first`, `proposal only`, `do not land`, or
  otherwise asks to approve the result before it is accepted;
- validation fails;
- Hop reports visible-root divergence; a conflict has genuine product ambiguity
  that cannot be resolved from both recorded intents; or
- acceptance would require a destructive, external, or out-of-scope action not
  authorized by the captured task.

Ordinary textual overlap is not a reason to stop. Hop first performs a real
three-way content merge; genuine unresolved hunks enter the automatic
reconciliation loop above. Preserve and report a block only when the intents
are product-level incompatible, required validation cannot be repaired, or
safe continuation needs new user authority.
If visible-root synchronization is blocked, do not bypass it with `hop accept`,
force checkout, reset, or file copying. Preserve the proposal and identify the
user-owned paths that must be resolved. `hop accept` is reserved for an
explicitly controller-only workflow; interactive agent work uses `hop land`.
Use `hop undo` only after a separately captured, explicit user request.

Read [references/protocol.md](references/protocol.md) for state semantics, exit
codes, recovery, and controller-grade pre-delivery capture. Skill-driven
interactive capture is a pre-project-effect boundary; it does not claim the
prompt was stored before the runtime received it. On Codex Desktop, for
example, Codex has already received the prompt before this skill can run.
