#!/bin/sh
set -eu

fail() {
  printf 'hop release: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: scripts/release-local.sh --snapshot
       scripts/release-local.sh --publish

--snapshot  Test and build all release archives locally without uploading.
--publish   Test, build, and upload a draft release to GitHub.
            Requires a clean signed tag, LICENSE, and `gh auth login`.
EOF
}

[ "$#" -eq 1 ] || { usage >&2; exit 2; }
mode=$1
case "$mode" in
  --snapshot | --publish) ;;
  *) usage >&2; exit 2 ;;
esac

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

for command in curl git go tar; do
  command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done

if [ "$mode" = --publish ]; then
  [ -f LICENSE ] || fail "LICENSE is required before publishing"
  command -v gh >/dev/null 2>&1 || fail "gh is required for GitHub release publication"
  gh auth status --hostname github.com >/dev/null 2>&1 || fail "authenticate with gh auth login"
  [ -z "$(git status --porcelain)" ] || fail "the Git worktree must be clean"
  tag=$(git describe --tags --exact-match HEAD 2>/dev/null) ||
    fail "HEAD must have an exact release tag"
  case "$tag" in
    v[0-9]*) ;;
    *) fail "release tag must start with v followed by a number" ;;
  esac
  [ "$(git cat-file -t "$tag")" = tag ] || fail "$tag must be an annotated tag"
  git verify-tag "$tag" >/dev/null 2>&1 || fail "$tag must have a valid signature"
  git ls-remote --exit-code origin "refs/tags/$tag" >/dev/null 2>&1 ||
    fail "$tag must be pushed to origin before publishing"
fi

printf 'Running local release validation...\n'
go test -race ./...
go vet ./...
sh -n scripts/install.sh
sh scripts/test-install.sh
git diff --check

goreleaser_version=2.17.0
case "$(uname -s)/$(uname -m)" in
  Darwin/arm64)
    archive=goreleaser_Darwin_arm64.tar.gz
    expected=58912a80159199c0fd5c8484e4c868bf87414129655d6d87cd1cd84ee645736c
    ;;
  Darwin/x86_64)
    archive=goreleaser_Darwin_x86_64.tar.gz
    expected=f37e89fb844ddfd23cffb97e30d91f972c42da68232a676bfba2beacea300543
    ;;
  Linux/arm64 | Linux/aarch64)
    archive=goreleaser_Linux_arm64.tar.gz
    expected=75f93fc0e25d10d8535ffd0e4abcf39d6784a2467ba453d479ae513729a9ebbf
    ;;
  Linux/x86_64 | Linux/amd64)
    archive=goreleaser_Linux_x86_64.tar.gz
    expected=dde10e2d5a13cef969c0eec00c74f359c0ac306d702b1bd291ad9337b4e54c1d
    ;;
  *) fail "unsupported release host: $(uname -s)/$(uname -m)" ;;
esac

tmp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t hop-release)
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

printf 'Downloading verified GoReleaser %s...\n' "$goreleaser_version"
curl -fsSL --retry 3 --proto '=https' --tlsv1.2 \
  -o "$tmp_dir/$archive" \
  "https://github.com/goreleaser/goreleaser/releases/download/v${goreleaser_version}/${archive}"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp_dir/$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp_dir/$archive" | awk '{print $1}')
else
  fail "sha256sum or shasum is required"
fi
[ "$actual" = "$expected" ] || fail "GoReleaser checksum verification failed"
tar -xzf "$tmp_dir/$archive" -C "$tmp_dir" goreleaser

"$tmp_dir/goreleaser" check
if [ "$mode" = --snapshot ]; then
  "$tmp_dir/goreleaser" release --snapshot --clean
  printf 'Local snapshot complete: %s/dist\n' "$root"
else
  "$tmp_dir/goreleaser" release --clean --skip=publish
  gh release create "$tag" dist/*.tar.gz dist/*.zip dist/checksums.txt \
    --repo GnosysLabs/Hop --draft --verify-tag --generate-notes --title "$tag"
  printf 'Draft release uploaded to https://github.com/GnosysLabs/Hop/releases\n'
fi
