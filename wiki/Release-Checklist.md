# Release checklist

Hop releases are built on a trusted maintainer machine and uploaded by
GoReleaser as draft Gitea Releases. Gitea Actions and act runners are not
required; the server only stores source, tags, release metadata, and assets.

The canonical repository is `githop.xyz/GnosysLabs/Hop`.

## One-time Gitea setup

- Create the `GnosysLabs/Hop` repository and set its default branch to `main`.
- Configure `origin` as `https://githop.xyz/GnosysLabs/Hop.git`.
- Keep Gitea Actions disabled when the instance does not have dedicated runner
  capacity; Hop's release process does not depend on it.
- Create a narrowly scoped maintainer access token that can write releases.
  Export it as `GITEA_TOKEN` only for the local publish command, then unset it.
- When upgrading GoReleaser, update its pinned version and four archive
  checksums in `scripts/release-local.sh` from the official checksum file.
- Permit release attachment MIME types for `.tar.gz`, `.zip`, and `.txt` in
  Gitea's `[attachment] ALLOWED_TYPES` configuration.
- Enable the repository wiki, then push the files in `wiki/` to its wiki Git
  repository.

## Public-launch gates

- Choose and add a `LICENSE`. The local publishing script intentionally fails
  without one; this is a product/legal decision, not a build default.
- Add `SECURITY.md` with a monitored private disclosure address.
- Create an offline-controlled release-signing key, publish its public key, and
  add detached signing for `checksums.txt` before general availability.
- Confirm the `githop.xyz/GnosysLabs/Hop` Go import path serves valid `go-import`
  metadata.
- Back up the Gitea database, repositories, and release attachments.

## Validate before tagging

```bash
scripts/release-local.sh --snapshot
```

Inspect `dist/` and test at least one archive on each operating system family.
Confirm `hop version` reports the snapshot/tag-injected version and
`hop skill install --force` installs usable skill files.

## Create a release

1. Update release notes. The signed Git tag is the version source and is
   injected into the binaries automatically.
2. Run `scripts/release-local.sh --snapshot` and inspect the artifacts.
3. Create a signed semantic-version tag such as `v0.1.0-alpha.1` and push it.
4. Export a locally stored, scoped token: `export GITEA_TOKEN=...`.
5. Run `scripts/release-local.sh --publish`. It reruns race tests, vet, installer
   checks, builds six platform archives, generates `checksums.txt`, and uploads
   a draft without executing build work on the Gitea server.
6. Immediately run `unset GITEA_TOKEN`.
7. Download the draft assets and independently verify checksums, version output,
   skill installation, and a disposable Hop project.
8. Publish the Gitea draft only after those checks pass.
9. Test both one-command installers against the now-published release.

## Expected assets

```text
hop_darwin_amd64.tar.gz
hop_darwin_arm64.tar.gz
hop_linux_amd64.tar.gz
hop_linux_arm64.tar.gz
hop_windows_amd64.zip
hop_windows_arm64.zip
checksums.txt
```

## After the first release

- Create a Gitea-hosted Homebrew tap/cask fed by immutable release URLs and
  checksums; do not publish placeholder hashes.
- Add Windows package-manager metadata only after the Windows artifact has been
  tested on a real signed build.
- Establish release retention, package cleanup, rollback, and incident-response
  procedures for the custom Gitea instance.
