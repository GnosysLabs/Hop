# Release checklist

Hop releases are built on `githop.xyz` by Gitea Actions and uploaded by
GoReleaser as draft Gitea Releases.

The examples assume the canonical repository is `githop.xyz/hop/hop`.

## One-time Gitea setup

- Create the `hop/hop` repository and set its default branch to `main`.
- Configure `origin` as `https://githop.xyz/hop/hop.git`.
- Run a currently patched Gitea 1.26.x or newer, enable Actions, and enable
  Actions for the repository. Hop's least-privilege workflow uses the 1.26 job
  token permission model.
- Register trusted, isolated `ubuntu-latest` and `windows-latest` act runners.
- Allow the repository job token `code: read` and `releases: write`.
- Ensure `${{ secrets.GITEA_TOKEN }}` can create releases in this repository.
- Keep third-party Actions pinned to reviewed commit SHAs. When upgrading
  GoReleaser, update its version and both hard-coded Linux archive checksums in
  `.gitea/workflows/release.yml` from the official release checksum file.
- Permit release attachment MIME types for `.tar.gz`, `.zip`, and `.txt` in
  Gitea's `[attachment] ALLOWED_TYPES` configuration.
- Enable the repository wiki, then push the files in `wiki/` to its wiki Git
  repository.

## Public-launch gates

- Choose and add a `LICENSE`. The release workflow intentionally fails without
  one; this is a product/legal decision, not a build default.
- Add `SECURITY.md` with a monitored private disclosure address.
- Create an offline-controlled release-signing key, publish its public key, and
  add detached signing for `checksums.txt` before general availability.
- Confirm the `githop.xyz/hop/hop` Go import path serves valid `go-import`
  metadata.
- Back up the Gitea database, repositories, release attachments, and Actions
  secrets.

## Validate before tagging

```bash
go test -race ./...
go vet ./...
sh -n scripts/install.sh
goreleaser check
goreleaser release --snapshot --clean
```

Inspect `dist/` and test at least one archive on each operating system family.
Confirm `hop version` reports the snapshot/tag-injected version and
`hop skill install --force` installs usable skill files.

## Create a release

1. Update release notes and the expected version.
2. Create a signed semantic-version tag such as `v0.1.0-alpha.1`.
3. Push the tag to `githop.xyz`.
4. The `.gitea/workflows/release.yml` workflow runs race tests and vet, builds
   six platform archives, generates `checksums.txt`, and uploads a draft.
5. Download the draft assets and independently verify checksums, version output,
   skill installation, and a disposable Hop project.
6. Publish the Gitea draft only after those checks pass.
7. Test both one-command installers against the now-published release.

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
