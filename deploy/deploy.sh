#!/usr/bin/env bash
# Deploy a playground release to a host, from your own machine.
#
#   deploy/deploy.sh <ssh-host> [<release-tag> | latest | --local]
#
#   latest (default)  newest GitHub release of pythonbpf/pythonbpf-playground
#   <release-tag>     a specific release, e.g. v2026.11.01-7
#   --local           what you built here (make kernel rootfs pgd)
#
# It also keeps Firecracker current: if the host's Firecracker is older than
# the newest 1.x release (or missing), that release's firecracker + jailer are
# shipped too. A new major version is reported but not installed automatically.
#
# DRY_RUN=1 does everything except uploading and installing, and shows the
# bundle that would be shipped.
#
# Needs locally: gh (logged in), ssh, rsync, curl, tar, zstd. On the host:
# sudo (you will be asked for your password if needed).
set -euo pipefail

REPO=pythonbpf/pythonbpf-playground
HOST="${1:?usage: deploy/deploy.sh <ssh-host> [<release-tag>|latest|--local]}"
WHAT="${2:-latest}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$HERE/.."
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/pythonbpf-playground"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }

ARCH=$(ssh "$HOST" uname -m)
[ "$ARCH" = x86_64 ] || { echo "$HOST is $ARCH; only x86_64 builds exist so far"; exit 1; }
ssh "$HOST" 'test -e /dev/kvm' || { echo "$HOST has no /dev/kvm: it cannot run Firecracker"; exit 1; }

# ---- 1. the playground bundle -------------------------------------------------
if [ "$WHAT" = --local ]; then
  say "using local build"
  for f in server/pgd guest/out/vmlinux; do [ -f "$ROOT/$f" ] || { echo "missing $f (run make kernel rootfs pgd)"; exit 1; }; done
  ls "$ROOT"/guest/out/images/*/rootfs.squashfs >/dev/null 2>&1 || { echo "no images in guest/out/images (run make rootfs)"; exit 1; }
  cp "$ROOT/server/pgd" "$ROOT/guest/out/vmlinux" "$STAGE/"
  cp "$ROOT"/deploy/{install.sh,pgd.service,pg-launcher.service,pg-netns.service} "$STAGE/"
  mkdir -p "$STAGE/images"
  for d in "$ROOT"/guest/out/images/*/; do
    [ -f "$d/rootfs.squashfs" ] && cp -r "$d" "$STAGE/images/"
  done
else
  [ "$WHAT" = latest ] && WHAT=$(gh release view -R "$REPO" --json tagName -q .tagName)
  say "release $WHAT"
  asset="pythonbpf-playground-$ARCH.tar.zst"
  file="$CACHE/$WHAT/$asset"
  mkdir -p "$CACHE/$WHAT"
  # GitHub publishes each asset's sha256: never trust a cached or fresh file
  # that does not match it (e.g. a download that was interrupted).
  want=$(gh release view "$WHAT" -R "$REPO" --json assets \
    -q ".assets[] | select(.name == \"$asset\") | .digest" | sed 's/^sha256://')
  [ -n "$want" ] || { echo "release $WHAT has no $asset"; exit 1; }
  have() { [ -f "$file" ] && [ "$(sha256sum "$file" | cut -d' ' -f1)" = "$want" ]; }
  if ! have; then
    rm -f "$file" "$file.part"
    gh release download "$WHAT" -R "$REPO" -p "$asset" -O "$file.part"
    mv "$file.part" "$file"
    have || { rm -f "$file"; echo "checksum mismatch for $asset"; exit 1; }
  fi
  tar -I zstd -xf "$file" -C "$STAGE"
fi
echo "versions: $(cd "$STAGE/images" && ls | paste -sd ' ')"

# ---- 2. Firecracker -----------------------------------------------------------
latest=$(curl -fsSL https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest \
  | python3 -c 'import json, sys; print(json.load(sys.stdin)["tag_name"])')
current=$(ssh "$HOST" '/usr/local/bin/firecracker --version 2>/dev/null | head -1 | grep -o "v[0-9.]*"' || true)
newest=$(printf '%s\n%s\n' "${current:-v0}" "$latest" | sort -V | tail -1)
if [ -z "$current" ] || { [ "$newest" = "$latest" ] && [ "$current" != "$latest" ] && [ "${current%%.*}" = "${latest%%.*}" ]; }; then
  say "Firecracker ${current:-not installed} -> $latest"
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/$latest/firecracker-$latest-$ARCH.tgz" \
    | tar -xz -C "$STAGE"
  mv "$STAGE/release-$latest-$ARCH/firecracker-$latest-$ARCH" "$STAGE/firecracker"
  mv "$STAGE/release-$latest-$ARCH/jailer-$latest-$ARCH" "$STAGE/jailer"
  rm -rf "$STAGE/release-$latest-$ARCH"
elif [ "$current" != "$latest" ]; then
  say "Firecracker $latest is out (host has $current): new major version, not upgrading automatically"
else
  say "Firecracker $current is current"
fi

# ---- 3. ship and install ------------------------------------------------------
if [ "${DRY_RUN:-0}" = 1 ]; then
  say "dry run: would ship this bundle to $HOST"
  (cd "$STAGE" && find . -type f -printf '%10s  %p\n' | sort -k2)
  for m in "$STAGE"/images/*/meta.json; do echo "$(basename "$(dirname "$m")"): $(tr -d '\n ' < "$m")"; done
  exit 0
fi
say "uploading to $HOST"
rsync -az --delete --info=progress2 "$STAGE/" "$HOST:pg-deploy/"
say "installing"
ssh -t "$HOST" 'cd ~/pg-deploy && sudo ./install.sh'
