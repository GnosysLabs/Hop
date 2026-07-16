# Troubleshooting

## `hop: command not found`

Open a new terminal after installation. Confirm the install directory is on
PATH:

```bash
command -v hop
printf '%s\n' "$PATH"
```

The default Unix location is `~/.local/bin`. On Windows it is
`%LOCALAPPDATA%\Programs\Hop`.

## An agent integration does not activate Hop

```bash
hop skill install --force
```

The command refreshes all default skill bundles. Restart the agent client. For
Codex Desktop, mention `$hop` in a task as a deterministic activation fallback.
Claude Code reads `~/.claude/skills/hop`; if the top-level skills directory was
created after Claude Code started, restart it. For another compatible runtime,
confirm that it reads `~/.agents/skills` or run `hop skill install --path
/path/to/agent/skills --force`.

## A lock path points outside the selected repository

Upgrade Hop. Older builds could discover an ancestor `.hop` across a nested Git
repository boundary or place the first-use bootstrap lock in the user cache,
outside the selected project. Current Hop treats each nested Git repository as
an independent project and keeps repository bootstrap locks inside that
project's private `.hop` directory. Do not grant the agent broader filesystem
permissions as a workaround.

## `.hop/workspaces` is using too much disk space

Current Hop automatically parks attempts after 24 hours without activity. It
checkpoints unfinished files, removes the checkout, and rehydrates the same
attempt when its agent session resumes. Reclaim every non-current workspace
immediately with:

```sh
hop gc --all
```

This preserves immutable prompt and source history. Run `hop status` afterward;
parked attempts remain resumable even though their workspace directory is gone.

## The installer cannot find a release

Only published Gitea Releases appear through the public releases API. Drafts
and an instance that is not live will return an error; published prereleases
are supported. Pin an existing tag with `HOP_VERSION`, or use the source build
until the first release is published.

## Git is too old

Hop requires Git 2.40 or newer for structured, explicit-base `merge-tree`
behavior:

```bash
git --version
```

Upgrade Git through the operating system package manager before retrying.

## Exit code 20: merge conflict

The agent should adopt the returned reconciliation prompt and workspace,
resolve both intents, run `hop check`, propose, and land again. Users should not
need to perform ordinary source merges.

## Exit code 22: validation failed

The accepted head did not advance. Inspect the recorded output, fix the attempt
workspace, and rerun the check.

## Exit code 23 or `Root: diverged`

Hop found protected staged/index state, ignored content, a conflict with a
newer accepted projection, or a materialization race. Ordinary nonignored root
edits are captured automatically by `hop land`, so code `23` now means Hop
cannot safely infer a worktree-only intent. Preserve the reported paths and
resolve that protected state intentionally. Do not bypass this with `hop
accept` in interactive workflows.

## Internal ref or object warning

```bash
hop doctor
```

If SQLite is healthy and the report specifically identifies derived refs:

```bash
hop doctor --repair
```

Do not repair while a final validation command is running.

## Automatic push failed

The accepted state remains safe locally. Hop never force-pushes. Retry a
transient network or authentication failure with:

```bash
hop push
```

If the remote branch moved independently, preserve both histories and resolve
the divergence intentionally; do not replace this with a force-push.

## A secret was pasted

Rotate it. Hop redaction reduces durable exposure but cannot prove that every
credential format, attachment, agent log, or external system omitted the value.
