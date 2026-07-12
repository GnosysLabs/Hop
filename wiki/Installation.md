# Installation

Packaged binaries are the recommended installation. They need Git 2.40 or
newer; they do not need a local Go toolchain.

## macOS and Linux

```bash
curl -fsSL https://githop.xyz/GnosysLabs/Hop/raw/branch/main/scripts/install.sh | sh
```

The installer:

1. detects macOS/Linux and `amd64`/`arm64`;
2. downloads the matching archive from the latest Gitea Release;
3. downloads `checksums.txt` and verifies SHA-256 before extraction;
4. installs the CLI to `~/.local/bin/hop`;
5. adds `~/.local/bin` to `.zprofile` or `.profile` when necessary; and
6. runs `hop skill install --force` to install the bundled agent integration.

The no-path skill command writes the same Hop-managed skill files to
`~/.agents/skills/hop`, `${CODEX_HOME:-~/.codex}/skills/hop`, and
`~/.claude/skills/hop`. The client-specific paths support Codex Desktop and
Claude Code; compatible clients can use the shared `.agents` path.

Review before execution:

```bash
curl -fsSLO https://githop.xyz/GnosysLabs/Hop/raw/branch/main/scripts/install.sh
less install.sh
sh install.sh
```

Installer options are environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `HOP_VERSION` | `latest` | Release tag such as `v0.1.0` |
| `HOP_INSTALL_DIR` | `~/.local/bin` | Binary destination |
| `HOP_INSTALL_SKILL` | `1` | Set to `0` for a CLI-only or custom integration |
| `HOP_MODIFY_PATH` | `1` | Set to `0` to leave shell profiles unchanged |
| `HOP_GITEA_URL` | `https://githop.xyz` | Gitea instance base URL |
| `HOP_REPOSITORY` | `GnosysLabs/Hop` | Alternate Gitea owner/repository |

Example:

```bash
HOP_VERSION=v0.1.0 HOP_INSTALL_DIR="$HOME/bin" sh install.sh
```

## Windows

In PowerShell as your normal user:

```powershell
irm https://githop.xyz/GnosysLabs/Hop/raw/branch/main/scripts/install.ps1 | iex
```

The script verifies the Windows archive, installs to
`%LOCALAPPDATA%\Programs\Hop`, updates the user PATH, and installs the shared,
Codex, and Claude Code skill bundles. To pin a version after downloading the
script:

```powershell
.\install.ps1 -Version v0.1.0
```

Use `-SkipSkill` for a CLI-only or separately managed integration. Use `-SkipPath`
when another tool manages your PATH.

## Go install

With Go 1.26 or newer:

```bash
go install githop.xyz/GnosysLabs/Hop/cmd/hop@latest
hop skill install --force
```

Put `$(go env GOPATH)/bin` on PATH if `hop` is not found. The second command
installs all default skill bundles; omit it for a CLI-only installation.

## Build from source

```bash
git clone https://githop.xyz/GnosysLabs/Hop.git
cd hop
go test ./...
go build -trimpath -o hop ./cmd/hop
mkdir -p "$HOME/.local/bin"
install -m 755 hop "$HOME/.local/bin/hop"
"$HOME/.local/bin/hop" skill install --force
```

Source builds are intended for contributors and as a pre-release fallback.

To install only one compatible runtime target, pass its parent skills directory:

```bash
hop skill install --path /path/to/agent/skills --force
```

## Verify

```bash
hop version
hop help
git --version
```

Restart any agent client that was open while its skill bundle changed. See
[Getting started](Getting-Started) next.
