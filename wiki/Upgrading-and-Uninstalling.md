# Upgrading and uninstalling

## Upgrade Hop

For a standalone installation made by Hop's macOS, Linux, or Windows installer:

```bash
hop update
```

This selects the latest published release for the current operating system and
architecture, verifies its checksum and reported version, replaces the CLI, and
refreshes all default skill bundles. Check without changing anything:

```bash
hop update --check
```

Pin a release with `hop update --version v1.0.10`. Hop refuses implicit
downgrades and same-version reinstalls; add `--force` only when that replacement
is intentional.

Rerunning the installer remains the no-Node bootstrap and recovery path:

```bash
curl -fsSL https://raw.githubusercontent.com/GnosysLabs/Hop/main/scripts/install.sh | sh
```

Pin a release when required:

```bash
curl -fsSL https://raw.githubusercontent.com/GnosysLabs/Hop/main/scripts/install.sh | \
  HOP_VERSION=v0.1.0 sh
```

Windows:

```powershell
irm https://raw.githubusercontent.com/GnosysLabs/Hop/main/scripts/install.ps1 -OutFile install.ps1
.\install.ps1 -Version v0.1.0
Remove-Item install.ps1
```

After upgrading, verify the installation:

```bash
hop version
hop doctor
```

Restart any agent client whose installed skill changed.

## Upgrade Go installations

```bash
go install github.com/GnosysLabs/Hop/cmd/hop@latest
hop skill install --force
```

Package-manager installations should be upgraded through the package manager
that owns their executable. This avoids modifying files inside a package store
behind that manager's back.

## Project migrations

Hop opens and migrates older supported SQLite schemas automatically. Back up
important repositories before alpha upgrades and read the release notes for any
one-way schema change.

## Uninstall the CLI and skill bundles

Unix default:

```bash
rm -f "$HOME/.local/bin/hop"
rm -rf "$HOME/.agents/skills/hop"
rm -rf "${CODEX_HOME:-$HOME/.codex}/skills/hop"
rm -rf "$HOME/.claude/skills/hop"
```

Windows PowerShell:

```powershell
Remove-Item -Force "$env:LOCALAPPDATA\Programs\Hop\hop.exe"
Remove-Item -Recurse -Force "$HOME\.agents\skills\hop"
Remove-Item -Recurse -Force "$HOME\.codex\skills\hop"
Remove-Item -Recurse -Force "$HOME\.claude\skills\hop"
```

If `CODEX_HOME` points somewhere else, remove its `skills\hop` directory instead
of `$HOME\.codex\skills\hop`. Remove any explicit `--path` target manually.
Remove the Hop install directory from PATH if it is no longer used.

Uninstalling the program does not delete project-local `.hop/` histories. That
is intentional, so reinstalling restores access. Deleting a project's `.hop/`
directory permanently removes its prompt graph, evidence, workspaces, and
accepted Hop history; make a backup and treat that as destructive data removal.
