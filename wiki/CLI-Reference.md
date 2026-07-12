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
| `hop land PROPOSAL [-- COMMAND...]` | Accept and synchronize the visible root |
| `hop refresh PROPOSAL` | Explicitly prepare/reuse conflict reconciliation |

`hop start` aliases `hop prompt`; `hop reconcile` aliases `hop refresh`.

## Controller and synchronization commands

| Command | Purpose |
|---|---|
| `hop accept PROPOSAL [-- COMMAND...]` | Accept internally without changing visible files |
| `hop sync` | Materialize the accepted tree and retry authenticated private prompt sync |
| `hop export [--output PATH]` | Write a private local prompt export to ignored `.hop/records/prompts/` |
| `hop push` | Retry publishing the current accepted commit to its inferred upstream |
| `hop undo` | Create a forward-only acceptance that restores the previous accepted tree |
| `hop doctor [--repair]` | Validate database/object/ref consistency |

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
| `hop status` | Accepted head, attempts, and visible-root status |
| `hop graph` | State graph |
| `hop state STATE` | One state and its provenance |
| `hop env STATE` | Shell exports for an attempt |
| `hop diff STATE` | Diff represented by a state |
| `hop history` | Accepted lineage |
| `hop version` | Installed version |

## Agent integration bundle

```bash
hop skill install [--path SKILLS_DIR] [--force]
hop skill print
```

Without `--path`, Hop installs the same Hop-managed skill files at
`~/.agents/skills/hop`, `${CODEX_HOME:-~/.codex}/skills/hop`, and
`~/.claude/skills/hop`. With `--path`, it installs only to
`SKILLS_DIR/hop`.

## Authenticated forge operations

```bash
hop repo create [--private | --public] [--remote NAME] [--replace-remote] OWNER/NAME
hop forge api [--method METHOD] [--data JSON|@-] API_PATH
hop auth exec [--env NAME] -- COMMAND [ARG...]
```

All three commands use the current `hop auth login` OAuth grant. `repo create`
creates a user or organization repository and configures the selected Git
remote. Existing remotes require `--replace-remote`, which should be used only
when the user requested a publishing-destination change. `forge api` accepts
only relative `/api/v1/` paths on the authenticated forge. `auth exec` provides
the current token to one child process through `GITEA_TOKEN` by default and
redacts it from captured stdout and stderr.

## Native Gitea command families

The same Hop binary embeds the established Gitea collaboration command engine
behind Hop-native command names:

```text
clone       whoami       issues       pulls         labels
milestones  releases     times        organizations repos
branches    actions      wiki         webhooks      comments
open        notifications ssh-keys    admin         api       man
```

Examples:

```bash
hop clone OWNER/REPOSITORY
hop issues list --repo OWNER/REPOSITORY --output json
hop pulls checkout NUMBER
hop releases create TAG --asset ./dist/archive.tar.gz
```

These commands automatically receive the current refreshed Hop OAuth session.
They do not read or create a Tea login. `hop login` and `hop logout` are aliases
for Hop OAuth authentication, while `hop auth login` and `hop auth logout`
remain the explicit canonical forms.

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
