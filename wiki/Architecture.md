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

Private objects are pinned beneath `refs/hop/states/*`; accepted history is
mirrored at `refs/hop/accepted`. Hop does not use the checked-out branch or real
index to create agent states or materialize files. After a proven accepted tree
is already visible, it may separately fast-forward the intended attached local
branch and atomically replace a projection-only index so ordinary Git presents
the same truthful state.

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

Every new checkpoint and proposal stores a versioned authorization proof: its
canonical base tree, exact candidate tree, immutable inputs, and a manifest of
every changed path with before/after object IDs and modes. The manifest covers
deletions, renames, executable bits, symlinks, and submodule gitlinks and is
bound into the state's digest.

Before any accepted-head compare-and-swap, an independent verifier recomputes
the manifest from Git objects and proves that every candidate path is present
in an authorized proposal, reconciliation, remote, visible-root, or undo input.
Anything outside that set must retain the canonical parent's exact object ID
and mode. The database rejects non-initial accepted states without this proof,
so a service call cannot bypass the verifier accidentally.

Acceptance is serialized and compare-and-swapped. SQLite is authoritative;
derived Git refs can be repaired by `hop doctor --repair`. Visible-root landing
also tracks which accepted state is physically visible, allowing safe catch-up
with `hop sync` without treating a divergent folder as disposable.

Branch/index synchronization is a second, fail-closed projection transaction.
Under Hop's acceptance lock it verifies the accepted commit/tree and durable
materialized marker, exact visible-tree manifest, real index tree, recorded
target branch, attached HEAD, strict fast-forward ancestry, absence of active
Git operations and locks, and current accepted/materialized heads. It prepares
the accepted index privately, obtains `.git/index.lock`, revalidates every
condition, updates the branch ref by compare-and-swap against the exact old
object ID, and atomically installs the prepared index. It never rewrites visible
files, force-updates a ref, or uses `reset --hard`. A concurrent ref or state
change leaves that newer state intact; every unprovable condition returns a
specific reason and recovery action. `hop sync-git` exposes this transaction,
while normal landing, `hop sync`, and `hop begin` invoke it automatically.

After that local transaction succeeds, Hop attempts a non-forced push of the
accepted commit to the inferred upstream branch. Remote publication is derived
and retryable: its failure cannot roll back or corrupt the durable local
acceptance. A failed or pending publication does not justify leaving local Git
dirty: the local branch/index may still be synchronized, with ordinary Git then
showing the accurate ahead/behind relationship to the upstream.

An existing branch whose tree is older than the claimed visible Hop projection
creates an information-theoretic ambiguity: the filesystem alone cannot prove
whether a large diff is deliberate work or a stale checkout plus a few edits.
Hop therefore refuses implicit visible-root capture in that condition. It does
not advance accepted state or infer deletions. Agent proposals remain unaffected
because their base and workspace are immutable and explicit.

For the full product direction, read the
[product blueprint](../docs/product-blueprint.md).
