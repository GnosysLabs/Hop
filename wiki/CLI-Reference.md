# CLI reference

Add `--json` anywhere in a command for machine-readable output.

## Project and prompt lifecycle

| Command | Purpose |
|---|---|
| `hop init [path]` | Initialize Hop without moving the Git branch or index |
| `hop begin ...` | Desktop entry point: initialize if needed, capture prompt, continue session |
| `hop prompt ...` | Controller-managed prompt or follow-up capture |
| `hop checkpoint STATE` | Freeze current attempt progress |
| `hop check STATE -- COMMAND...` | Validate an immutable checkpoint |
| `hop propose [--summary TEXT] STATE` | Freeze a candidate proposal |
| `hop land PROPOSAL [-- COMMAND...]` | Accept and synchronize the visible root |
| `hop refresh PROPOSAL` | Explicitly prepare/reuse conflict reconciliation |

`hop start` aliases `hop prompt`; `hop reconcile` aliases `hop refresh`.

## Controller and synchronization commands

| Command | Purpose |
|---|---|
| `hop accept PROPOSAL [-- COMMAND...]` | Accept internally without changing visible files |
| `hop sync` | Materialize the current accepted tree from a safe accepted ancestor |
| `hop undo` | Create a forward-only acceptance that restores the previous accepted tree |
| `hop doctor [--repair]` | Validate database/object/ref consistency |

## Inspection

| Command | Purpose |
|---|---|
| `hop status` | Accepted head, attempts, and visible-root status |
| `hop graph` | State graph |
| `hop state STATE` | One state and its provenance |
| `hop env STATE` | Shell exports for an attempt |
| `hop diff STATE` | Diff represented by a state |
| `hop history` | Accepted lineage |
| `hop version` | Installed version |

## Skill distribution

```bash
hop skill install [--path SKILLS_DIR] [--force]
hop skill print
```

Without `--path`, the skill installs under
`${CODEX_HOME:-~/.codex}/skills/hop`.

## Exit codes

| Code | Meaning |
|---:|---|
| `0` | Success |
| `1` | Git, SQLite, filesystem, or internal failure |
| `2` | Invalid usage |
| `20` | Merge conflict; reconciliation workspace was prepared |
| `21` | Attempt or accepted head changed during compare-and-swap |
| `22` | Validation command failed |
| `23` | Visible-root divergence or overwrite collision |

Exit `20` is a continuation signal for an agent, not a request for the user to
manually merge files.
