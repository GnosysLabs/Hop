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
timeout. Repository reads are project effects under this protocol: never
continue with a "read-only inspection" after prompt capture failed.

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
  through a provider website or API. Follow the forge authentication rules
  below instead.
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

Treat `hop status --json` as authoritative for the selected visible root. Hop
intentionally projects the accepted tree without moving the active branch or
real index, so raw `git status` may show a large dirty tree that is entirely
projection-only. Never describe those paths as user edits unless Hop reports
`git.user_worktree_changed` or `git.user_index_changed`.

## Authenticate to githop.xyz with Hop OAuth

For repositories hosted on `githop.xyz`, the intended authentication method is:

```bash
hop auth login https://githop.xyz
```

Check `hop auth status` first. If the account is not authenticated, run the
login command and let the user approve the browser authorization. Treat this
device-global OAuth grant as the user's authorization for Hop to access their
Gitea account. Hop stores it in the OS keychain and refreshes it automatically.
When status succeeds for the matching forge, do not open a standalone Gitea
login page or ask the user to sign in again.

Use this OAuth grant by default for every in-scope operation on the matching
forge: prompt sync; Git fetch, push, and tags; repository creation and settings;
issues, comments, pull requests, releases, and other Gitea API work. Prefer a
typed Hop command when one exists. This applies to both public and private
repositories. For example, create a private repository and configure it as the
publishing destination with:

```bash
hop repo create --private --replace-remote OWNER/REPOSITORY
```

Hop also ships native, OAuth-authenticated Gitea command families. Use these
directly; do not invoke or install Tea:

```text
hop clone             hop whoami           hop issues / issue / i
hop pulls / pull / pr hop labels            hop milestones
hop releases          hop times             hop organizations / orgs
hop repos             hop branches          hop actions
hop wiki              hop webhooks          hop comments
hop open              hop notifications     hop ssh-keys / ssh-key
hop admin             hop api               hop man
```

Each family retains its established subcommands and flags, such as
`hop issues list`, `hop comments add`, `hop pulls checkout`,
`hop releases create`, and `hop repos create`. Run `hop COMMAND --help` for the
complete surface. `hop login` and `hop logout` are convenient aliases for Hop's
OAuth login and logout; they never create a separate Tea credential.

Use `--replace-remote` only when the user asked to change an existing remote.
After landing, Hop's normal authenticated push publishes the accepted code. For
Gitea operations without a typed command, call the same-forge API without
handling the token yourself:

```bash
hop forge api --method PATCH --data '{"state":"closed"}' \
  /api/v1/repos/OWNER/REPOSITORY/issues/NUMBER
```

When an established forge tool requires an environment token, run it through
`hop auth exec --env GITEA_TOKEN -- COMMAND [ARG...]`. Never print that variable
or write it to a file. Hop redacts the token from the child process's captured
stdout and stderr. Preserve the user's configured remote, including SSH-form
remotes, unless their request explicitly changes the publishing destination;
Hop applies HTTPS OAuth only for each Git operation.

Do not ask for or create a personal access token, embed a token in a URL or Git
configuration, or fall back to a server-wide credential merely because a
repository is private. If the OAuth grant is expired or revoked, repeat
`hop auth login https://githop.xyz`. For other forges, use only credentials the
user already provisioned through an OS secret store or the runtime's secret
mechanism; never call a token-management endpoint.

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
   selected visible project root as synchronized. Inspect its durable
   publication state. If it is `failed` and retryable, retry once with
   `hop push`; never force-push or ask the user to perform routine
   source-control mechanics. A `diverged` failure requires Hop reconciliation,
   not repeated pushes.

9. Before sending the final response, compose its exact text and durably record
   it against the current prompt state:

   ```bash
   hop complete --summary "<concise outcome>" --heredoc <HOP_STATE_ID> <<'HOP_FINAL_EOF'
   <exact final response that will be sent to the user>
   HOP_FINAL_EOF
   ```

   Use the current turn's prompt `HOP_STATE_ID`, not a proposal or accepted
   state. The summary and final response are both private prompt-history data
   and are sanitized before persistence. `hop complete` closes source-clean
   read-only attempts, removes source-clean terminal workspaces, and immediately
   attempts authenticated private sync. It also parks other attempts inactive
   for 24 hours: Hop checkpoints their exact tree, removes only the checkout,
   and rehydrates it automatically if that session resumes. The current attempt
   is never parked. `hop gc --all` immediately parks every other unfinished
   attempt and archives dirty terminal workspaces without deleting state.

10. Send exactly the same response in the final channel immediately after the
    completion command. Do not run another tool or send commentary between
    `hop complete` and the final response. This last-step ordering ensures the
    user-visible response and prompt history cannot silently diverge.

For a read-only, informational, or external-operation turn, do not invent a
proposal when the workspace tree is unchanged. Still run `hop complete` so the
prompt receives its summary and exact final response.

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
- Hop reports unsafe staged/index state or an ignored destination collision; a
  conflict has genuine product ambiguity that cannot be resolved from both
  recorded intents; or
- acceptance would require a destructive, external, or out-of-scope action not
  authorized by the captured task.

Ordinary textual overlap is not a reason to stop. Hop first performs a real
three-way content merge; genuine unresolved hunks enter the automatic
reconciliation loop above. Preserve and report a block only when the intents
are product-level incompatible, required validation cannot be repaired, or
safe continuation needs new user authority.
Ordinary nonignored visible-root edits are captured by `hop land` as an
explicit accepted transition and then merged like any other concurrent work.
Continue through any returned reconciliation workspace. If visible-root
synchronization is instead blocked by staged/index state, ignored content, or
a race, do not bypass it with `hop accept`, force checkout, reset, or file
copying. Preserve the proposal and identify the protected paths. `hop accept`
is reserved for an explicitly controller-only workflow; interactive agent work
uses `hop land`.
Use `hop undo` only after a separately captured, explicit user request.

Read [references/protocol.md](references/protocol.md) for state semantics, exit
codes, recovery, and controller-grade pre-delivery capture. Skill-driven
interactive capture is a pre-project-effect boundary; it does not claim the
prompt was stored before the runtime received it. On Codex Desktop, for
example, Codex has already received the prompt before this skill can run.
