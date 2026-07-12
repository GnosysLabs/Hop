#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
tmp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t hop-installer-test)
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) printf 'installer smoke test does not support %s\n' "$(uname -s)" >&2; exit 1 ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) printf 'installer smoke test does not support %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

asset="hop_${os}_${arch}.tar.gz"
mkdir -p "$tmp_dir/payload" "$tmp_dir/fixtures" "$tmp_dir/mock-bin" "$tmp_dir/home"
go build -trimpath \
  -ldflags '-X githop.xyz/GnosysLabs/Hop/internal/hop.Version=9.9.9-installer-test' \
  -o "$tmp_dir/payload/hop" "$root/cmd/hop"
tar -czf "$tmp_dir/fixtures/$asset" -C "$tmp_dir/payload" hop
if command -v sha256sum >/dev/null 2>&1; then
  hash=$(sha256sum "$tmp_dir/fixtures/$asset" | awk '{print $1}')
else
  hash=$(shasum -a 256 "$tmp_dir/fixtures/$asset" | awk '{print $1}')
fi
printf '%s  %s\n' "$hash" "$asset" >"$tmp_dir/fixtures/checksums.txt"

cat >"$tmp_dir/mock-bin/curl" <<'MOCK_CURL'
#!/bin/sh
set -eu
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; output=$1 ;;
    http://* | https://*) url=$1 ;;
  esac
  shift
done
case "$url" in
  */api/v1/repos/*/releases\?draft=false\&page=1\&limit=1)
    printf '[{"tag_name":"v9.9.9-installer-test","prerelease":true,"draft":false}]'
    ;;
  */checksums.txt)
    cp "$HOP_TEST_FIXTURES/checksums.txt" "$output"
    ;;
  */hop_*.tar.gz)
    cp "$HOP_TEST_FIXTURES/${url##*/}" "$output"
    ;;
  *)
    printf 'unexpected installer URL: %s\n' "$url" >&2
    exit 1
    ;;
esac
MOCK_CURL
chmod 0755 "$tmp_dir/mock-bin/curl"

HOME="$tmp_dir/home" \
CODEX_HOME="$tmp_dir/home/.codex" \
PATH="$tmp_dir/mock-bin:$PATH" \
HOP_TEST_FIXTURES="$tmp_dir/fixtures" \
HOP_GITEA_URL="https://gitea.test" \
HOP_REPOSITORY="GnosysLabs/Hop" \
HOP_INSTALL_DIR="$tmp_dir/home/bin" \
HOP_MODIFY_PATH=0 \
sh "$root/scripts/install.sh"

version=$($tmp_dir/home/bin/hop version)
[ "$version" = "hop 9.9.9-installer-test" ] || {
  printf 'unexpected installed version: %s\n' "$version" >&2
  exit 1
}
shared_bundle="$tmp_dir/home/.agents/skills/hop"
codex_bundle="$tmp_dir/home/.codex/skills/hop"
claude_bundle="$tmp_dir/home/.claude/skills/hop"
[ -s "$shared_bundle/SKILL.md" ] || {
  printf 'installer did not install the shared Hop skill\n' >&2
  exit 1
}
[ -s "$codex_bundle/SKILL.md" ] || {
  printf 'installer did not install the Codex Hop skill\n' >&2
  exit 1
}
[ -s "$claude_bundle/SKILL.md" ] || {
  printf 'installer did not install the Claude Code Hop skill\n' >&2
  exit 1
}
if ! diff -r "$shared_bundle" "$codex_bundle" >/dev/null ||
   ! diff -r "$shared_bundle" "$claude_bundle" >/dev/null; then
  printf 'installed Hop skill bundles differ\n' >&2
  exit 1
fi
