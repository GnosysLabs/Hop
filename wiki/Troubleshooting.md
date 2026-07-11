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

## Codex does not activate Hop

```bash
hop skill install --force
```

Restart Codex Desktop. Mention `$hop` in a task as a deterministic activation
fallback.

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

Hop found visible files or index state it will not overwrite. Preserve those
changes. Capture them as a new Hop task or resolve them intentionally, then
retry `hop land`. Do not bypass this with `hop accept` in Desktop workflows.

## Internal ref or object warning

```bash
hop doctor
```

If SQLite is healthy and the report specifically identifies derived refs:

```bash
hop doctor --repair
```

Do not repair while a final validation command is running.

## A secret was pasted

Rotate it. Hop redaction reduces durable exposure but cannot prove that every
credential format, attachment, agent log, or external system omitted the value.
