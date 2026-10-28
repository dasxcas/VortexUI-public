#!/usr/bin/env bash
# VortexUI — One-line installer
# Usage: bash <(curl -sL https://raw.githubusercontent.com/dasxcas/VortexUI-public/main/install.sh)
#
# Puts the tree in /opt/vortexui (or $VORTEX_DIR) and hands over to
# scripts/bootstrap.sh, which installs Docker if needed, writes the mTLS chain
# and .env, brings the stack up and creates the first admin. Re-running is safe:
# existing secrets, certificates and the admin are kept.

set -euo pipefail

REPO="${VORTEX_REPO:-https://github.com/dasxcas/VortexUI-public.git}"
BRANCH="${VORTEX_BRANCH:-main}"
DIR="${VORTEX_DIR:-/opt/vortexui}"

say() { printf '\033[34m==>\033[0m %s\n' "$1"; }
die() { printf '\033[31merror\033[0m %s\n' "$1" >&2; exit 1; }

[[ "${EUID}" -eq 0 ]] || die "run as root: sudo bash install.sh"
command -v git >/dev/null 2>&1 || die "git is required"

if [[ -d "$DIR/.git" ]]; then
  say "updating $DIR"
  git -C "$DIR" fetch --depth 1 origin "$BRANCH"
  git -C "$DIR" reset --hard "origin/$BRANCH"
else
  say "cloning into $DIR"
  mkdir -p "$(dirname "$DIR")"
  git clone --depth 1 --branch "$BRANCH" "$REPO" "$DIR"
fi

exec bash "$DIR/scripts/bootstrap.sh" "$@"
