# Release checklist

Hop releases are built on a trusted maintainer machine and uploaded as draft
GitHub Releases. GitHub Actions is not required.

The canonical repository is `github.com/GnosysLabs/Hop`.

## One-time setup

- Configure `origin` as `https://github.com/GnosysLabs/Hop.git` or its SSH
  equivalent and set the default branch to `main`.
- Install `gh`, run `gh auth login`, and keep its credential in the OS keychain.
- Agents and release scripts must never create, rotate, list, or revoke account
  tokens.
- When upgrading GoReleaser, update its pinned version and archive checksums in
  `scripts/release-local.sh` from the official checksum file.

## Validate before tagging

```bash
scripts/release-local.sh --snapshot
```

Inspect `dist/` and test representative archives. Confirm `hop version` reports
the injected version and `hop skill install --force` refreshes the embedded
skill bundles.

## Create a release

1. Update release notes.
2. Run the snapshot command and inspect the artifacts.
3. Create and verify a signed semantic-version tag, then publish it without
   force using `hop push-tag TAG`.
4. Run `scripts/release-local.sh --publish`. It validates locally, builds the
   platform archives, generates `checksums.txt`, and uploads a GitHub draft.
5. Verify the draft assets, checksums, version output, and a disposable install.
6. Publish the draft and test both one-command installers.

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

## Migration bridge

The first GitHub-native release must also be published to the former Gitea
release feed. Versions through 1.0.10 query that feed for updates; after they
install the bridge release, `hop update` uses GitHub. The old forge can be
retired only after this bridge is public and verified.
