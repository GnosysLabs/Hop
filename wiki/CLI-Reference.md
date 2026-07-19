# CLI reference

Add `--json` anywhere in a command for machine-readable output. Successful
responses use this envelope:

```json
{
  "ok": true,
  "data": {}
}
```

The JSON shape is an alpha contract and may evolve before the first tagged
release.

## Project and prompt lifecycle

| Command | Purpose |
|---|---|
| `hop init [path]` | Initialize Hop without moving the Git branch or index |
| `hop begin ...` | Interactive-agent entry point: initialize if needed, capture prompt, continue session |
| `hop prompt ...` | Controller-managed prompt or follow-up capture |
| `hop checkpoint STATE` | Freeze current attempt progress |
| `hop check STATE -- COMMAND...` | Validate an immutable checkpoint |
| `hop propose [--summary TEXT] STATE` | Freeze a candidate proposal |
| `hop complete --summary TEXT (--stdin \| --heredoc) PROMPT` | Record the summary and exact final response for a prompt |
| `hop land PROPOSAL [-- COMMAND...]` | Accept and synchronize the visible root |
| `hop refresh PROPOSAL` | Explicitly prepare/reuse conflict reconciliation |

`hop start` aliases `hop prompt`; `hop reconcile` aliases `hop refresh`.

## Controller and synchronization commands

| Command | Purpose |
|---|---|
| `hop accept PROPOSAL [-- COMMAND...]` | Accept internally without changing visible files |
| `hop sync` | Materialize the accepted tree and retry authenticated private prompt sync |
| `hop export [--output PATH]` | Write a private local prompt export to ignored `.hop/records/prompts/` |
| `hop push` | Safely retry a pending/failed accepted-state publication; never force-push |
| `hop undo` | Create a forward-only acceptance that restores the previous accepted tree |
| `hop doctor [--repair]` | Validate database/object/ref consistency |
| `hop gc [--older-than DURATION \| --all]` | Remove terminal worktrees and park inactive attempts while preserving resumable state |

## Forge authentication

| Command | Purpose |
|---|---|
| `hop auth login FORGE_URL` | Pair this device through browser OAuth + PKCE |
| `hop auth status` | Verify the signed-in forge account, refreshing the session if needed |
| `hop auth logout` | Delete the local device credential from the OS keychain |

Authentication is global to the device, not stored in a repository. The
selected forge is discovered through `FORGE_URL/hop/api/v1/auth/config` and must
advertise Gitea's `all` scope. Prompt uploads are
matched to the repository's configured Git remote; the CLI never submits a
user ID supplied by local project data. The same OAuth grant authenticates
Hop's private same-forge Git fetch and push operations.

## Inspection

| Command | Purpose |
|---|---|
| `hop status` | Accepted/root/branch/upstream relationships, real user changes, attempts, and durable publication status |
| `hop graph` | State graph |
| `hop state STATE` | One state and its provenance |
| `hop env STATE` | Shell exports for an attempt |
| `hop diff STATE` | Diff represented by a state |
| `hop history` | Accepted lineage |
| `hop update [--check] [--version VERSION] [--force]` | Check for or install a verified Hop release and refresh the embedded skill |
| `hop version` | Installed version |

`hop status` is read-only with respect to refs, HEAD, the real index, and the
working tree. Its JSON separates the accepted tree projected into the visible
root from the active branch and index. `git.projection_only_changes=true` means
raw Git dirtiness is expected projection output, not uncommitted user work.
`git.user_worktree_paths` and `git.user_index_paths` contain filesystem/index
differences, while `accepted_provenance` reports whether the accepted
transition's exact-tree authorization proof is `verified`,
`legacy_unverified`, or `invalid`.

Publication is `not_configured`, `pending`, `current`, `failed`, or `unknown`
for a pre-migration accepted state. Failures retain a sanitized error category,
timestamp, retryability, target remote/ref, and any authoritative remote tip.
`hop push` performs a fresh remote comparison, rejects divergence without
force, and changes the durable state to `current` after success.

## CLI updates

```bash
hop update
hop update --check
hop update --version v1.0.10
```

Hop downloads the matching release archive and `checksums.txt`, verifies its
SHA-256 checksum and embedded version, atomically replaces a standalone install,
and refreshes all default skill bundles. A downgrade or same-version reinstall
requires `--force`. On Windows, a verified helper finishes replacement after the
running process exits. `HOP_RELEASE_API_URL`, `HOP_RELEASE_URL`, and
`HOP_REPOSITORY` select another release source; the equivalent CLI flags are
`--api-url`, `--download-url`, and `--repository`. `--base-url` remains a
deprecated Gitea bridge for older private distributions.

Package-manager installations remain owned by that package manager and should
use its update command instead of replacing files inside its package directory.

## Agent integration bundle

```bash
hop skill install [--path SKILLS_DIR] [--force]
hop skill print
```

Without `--path`, Hop installs the same Hop-managed skill files at
`~/.agents/skills/hop`, `${CODEX_HOME:-~/.codex}/skills/hop`, and
`~/.claude/skills/hop`. With `--path`, it installs only to
`SKILLS_DIR/hop`.

## Git host and collaboration operations

```bash
hop host
hop repo create [--host PROVIDER] [--private | --public] [--remote NAME] [--replace-remote] OWNER/NAME
hop issues
hop pulls
hop releases
```

`hop host` detects the provider from the push remote. Core Hop commands use Git
directly and work without a provider API. Collaboration commands use `gh` for
GitHub, `glab` for GitLab, and the embedded adapter for Gitea:

```text
clone  whoami  issues  pulls  labels  releases  repos  actions
open   notifications  ssh-keys  api
```

Examples:

```bash
hop issues list
hop pulls create --fill
hop releases view v1.1.0
```

Unsupported provider capabilities fail clearly without affecting core Hop or
Git workflows. The legacy Gitea OAuth/API commands remain available for
existing installations.

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
