# Parallel agents and conflict resolution

Each independent prompt starts from an accepted state and receives its own
worktree. Multiple agents can therefore read, edit, and test the same codebase
without sharing a mutable working directory.

## Compatible work

Hop uses Git's real three-way content merge:

- disjoint files compose automatically;
- independent hunks in the same file compose automatically;
- identical same-file changes coalesce; and
- compatible rename/content and mode/content changes can compose.

Before acceptance, Hop validates the exact integrated tree rather than trusting
tests run only against an agent's stale starting point.

## Genuine conflicts

When Git cannot compose the proposal with either the accepted state or the
fetched upstream branch, `hop land` exits with code `20` and creates a fresh
reconciliation attempt. The response includes:

- a reconciliation prompt state;
- a new isolated workspace;
- current accepted and proposed inputs;
- the remote input commit when upstream conflicts; and
- conflict candidate paths.

The agent switches to that workspace, preserves both compatible intents,
resolves the conflict, runs `hop check`, creates a new proposal, and lands again.
Hop requires successful checked evidence before a reconciliation proposal can
be frozen. Remote reconciliation records the upstream tip that the agent
resolved and re-fetches it during landing, preserving any later compatible
remote commits without force-pushing.

Text conflicts usually contain diff3 markers. Delete/rename, binary, symlink,
mode, and directory conflicts may not, so the agent must inspect both input
states rather than assuming the provisional tree is resolved.

## What still needs a person

The agent should stop only when the source conflict exposes a real product
decision—for example, two prompts require mutually exclusive API behavior and
neither recorded intent determines the right result. Ordinary code overlap is
not user work.

## Visible-root safety

The selected project directory remains at the last accepted state while
reconciliation is underway. If it has ordinary nonignored edits when landing
resumes, Hop captures them as a labeled accepted transition and merges them
with the proposal. Protected staged/index state, ignored collisions, and
materialization races still exit with code `23` rather than being overwritten.
