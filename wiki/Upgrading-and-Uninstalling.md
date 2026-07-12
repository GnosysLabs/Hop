# Upgrading and uninstalling

## Upgrade packaged installations

Rerun the installer. It replaces the binary and refreshes all default skill
bundles:

```bash
curl -fsSL https://githop.xyz/GnosysLabs/Hop/raw/branch/main/scripts/install.sh | sh
```

Pin a release when required:

```bash
curl -fsSL https://githop.xyz/GnosysLabs/Hop/raw/branch/main/scripts/install.sh | \
  HOP_VERSION=v0.1.0 sh
```

Windows:

```powershell
irm https://githop.xyz/GnosysLabs/Hop/raw/branch/main/scripts/install.ps1 -OutFile install.ps1
.\install.ps1 -Version v0.1.0
Remove-Item install.ps1
```

After upgrading:

```bash
hop version
hop skill install --force
hop doctor
```

Restart any agent client whose installed skill changed.

## Upgrade Go installations

```bash
go install githop.xyz/GnosysLabs/Hop/cmd/hop@latest
hop skill install --force
```

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
