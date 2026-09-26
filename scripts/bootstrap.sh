#!/usr/bin/env bash
set -Eeuo pipefail

[[ "$EUID" -eq 0 ]] || { echo 'Run through sudo: curl ... | sudo bash -s -- --domain ... --email ...' >&2; exit 1; }
[[ -r /etc/os-release ]] || { echo 'Ubuntu is required' >&2; exit 1; }
# shellcheck disable=SC1091
. /etc/os-release
[[ "${ID:-}" == ubuntu ]] || { echo 'Ubuntu is required' >&2; exit 1; }

install_dir=/opt/linkbox-send
repo_url=https://github.com/Swebersmith/linkbox-send.git
[[ ! -L "$install_dir" ]] || { echo "$install_dir must not be a symlink" >&2; exit 1; }

if [[ ! -e "$install_dir" ]]; then
  if ! command -v git >/dev/null; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates git
  fi
  git clone --depth 1 "$repo_url" "$install_dir"
fi

[[ -d "$install_dir" && ! -L "$install_dir" ]] || {
  echo "$install_dir must be a regular directory" >&2
  exit 1
}
[[ -f "$install_dir/scripts/install.sh" && -f "$install_dir/docker-compose.yml" ]] || {
  echo "$install_dir exists but is not a complete LinkBox Send checkout; inspect it before retrying" >&2
  exit 1
}
[[ "$(git -C "$install_dir" remote get-url origin)" == "$repo_url" ]] || {
  echo "$install_dir has an unexpected Git origin; inspect it before retrying" >&2
  exit 1
}

exec bash "$install_dir/scripts/install.sh" "$@"
