# Architecture

Hop uses Git under the hood, but it is not merely a directory of metadata files
inside `.git`.

## Git responsibilities

Git provides:

- content-addressed blobs and trees;
- synthetic commits for immutable snapshots;
- detached worktrees for attempt isolation;
- three-way merge behavior;
- diffs and path inspection; and
- interoperability with existing repositories.

Hop never needs to move the user's active branch or rewrite the real index.
Private objects are pinned beneath `refs/hop/states/*`; accepted history is
mirrored at `refs/hop/accepted`.

## Hop responsibilities

`.hop/hop.db` is a SQLite WAL database containing:

- prompt/task/attempt identity;
- typed state edges and accepted lineage;
- evidence tied to exact source trees;
- session heads for interactive-agent follow-ups;
- materialized-root state; and
- immutable audit events.

## Project layout

```text
.hop/
├── hop.db
├── workspaces/
├── checks/
├── integration/
└── *.lock
```

`.hop/` is added to `.git/info/exclude`, not the public `.gitignore`. Hop refuses
initialization when `.hop` is already tracked as user-owned project content.

## Acceptance consistency

Acceptance is serialized and compare-and-swapped. SQLite is authoritative;
derived Git refs can be repaired by `hop doctor --repair`. Visible-root landing
also tracks which accepted state is physically visible, allowing safe catch-up
with `hop sync` without treating a divergent folder as disposable.

After that local transaction succeeds, Hop attempts a non-forced push of the
accepted commit to the inferred upstream branch. Remote publication is derived
and retryable: its failure cannot roll back or corrupt the durable local
acceptance.

For the full product direction, read the
[product blueprint](https://githop.xyz/GnosysLabs/Hop/src/branch/main/docs/product-blueprint.md).
