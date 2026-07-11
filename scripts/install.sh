#!/bin/sh
set -eu

gitea_url=${HOP_GITEA_URL:-https://githop.xyz}
gitea_url=${gitea_url%/}
repository=${HOP_REPOSITORY:-GnosysLabs/Hop}
requested_version=${HOP_VERSION:-latest}
install_dir=${HOP_INSTALL_DIR:-"$HOME/.local/bin"}
install_skill=${HOP_INSTALL_SKILL:-1}
modify_path=${HOP_MODIFY_PATH:-1}

fail() {
  printf 'hop installer: %s\n' "$*" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported operating system: $(uname -s)" ;;
esac

case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

asset="hop_${os}_${arch}.tar.gz"
if [ "$requested_version" = latest ]; then
  # Gitea's /releases/latest endpoint omits prereleases. Hop is distributed as
  # an alpha today, so select the newest published release from the list API.
  latest_json=$(curl -fL --retry 3 --proto '=https' --tlsv1.2 \
    "$gitea_url/api/v1/repos/$repository/releases?draft=false&page=1&limit=1")
  tag=$(printf '%s' "$latest_json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
  [ -n "$tag" ] || fail "could not determine the latest published release"
else
  case "$requested_version" in
    v*) tag=$requested_version ;;
    *) tag="v${requested_version}" ;;
  esac
fi
case "$tag" in
  *[!A-Za-z0-9._-]*) fail "release API returned an unsafe tag: $tag" ;;
esac
release_url="$gitea_url/$repository/releases/download/${tag}"

tmp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t hop-install)
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

printf 'Downloading %s...\n' "$asset"
curl -fL --retry 3 --proto '=https' --tlsv1.2 \
  -o "$tmp_dir/$asset" "$release_url/$asset"
curl -fL --retry 3 --proto '=https' --tlsv1.2 \
  -o "$tmp_dir/checksums.txt" "$release_url/checksums.txt"

expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1; exit }' "$tmp_dir/checksums.txt")
[ -n "$expected" ] || fail "checksums.txt does not contain $asset"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp_dir/$asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp_dir/$asset" | awk '{print $1}')
else
  fail "sha256sum or shasum is required to verify the download"
fi
[ "$actual" = "$expected" ] || fail "checksum verification failed for $asset"

tar -xzf "$tmp_dir/$asset" -C "$tmp_dir"
[ -f "$tmp_dir/hop" ] || fail "release archive does not contain hop"
mkdir -p "$install_dir"
cp "$tmp_dir/hop" "$install_dir/hop"
chmod 0755 "$install_dir/hop"

case ":${PATH:-}:" in
  *":$install_dir:"*) ;;
  *)
    if [ "$modify_path" = 1 ] && [ "$install_dir" = "$HOME/.local/bin" ]; then
      case ${SHELL:-} in
        */zsh) profile="$HOME/.zprofile" ;;
        *) profile="$HOME/.profile" ;;
      esac
      path_line='export PATH="$HOME/.local/bin:$PATH"'
      if ! grep -F "$path_line" "$profile" >/dev/null 2>&1; then
        printf '\n%s\n' "$path_line" >>"$profile"
        printf 'Added ~/.local/bin to PATH in %s.\n' "$profile"
      fi
    else
      printf 'Add %s to PATH before opening a new terminal.\n' "$install_dir" >&2
    fi
    ;;
esac

if [ "$install_skill" = 1 ]; then
  "$install_dir/hop" skill install --force
fi

printf 'Installed %s\n' "$("$install_dir/hop" version)"
printf 'Binary: %s\n' "$install_dir/hop"
if [ "$install_skill" = 1 ]; then
  printf 'Restart any open agent application, then use it normally in any Git repository.\n'
fi
