#!/usr/bin/env bash
# Build the guest root filesystem image: guest/out/rootfs.squashfs
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GUEST="$HERE/.."
OUT="$GUEST/out"
PYBPF_REPO="${PYBPF_REPO:-$GUEST/../../python-bpf}"
PYTHONBPF_VERSION="${PYTHONBPF_VERSION:-0.2.0}"

test -f "$OUT/vmlinux" || { echo "build the kernel first (guest/kernel/build-kernel.sh)"; exit 1; }

# vmlinux-gen.py from the same release as the pinned pythonbpf.
git -C "$PYBPF_REPO" show "v${PYTHONBPF_VERSION}:tools/vmlinux-gen.py" > "$HERE/vmlinux-gen.py"

docker build -q -t pythonbpf-playground-rootfs \
  --build-arg PYTHONBPF_VERSION="$PYTHONBPF_VERSION" \
  -f "$HERE/Dockerfile" "$GUEST/.."

cid=$(docker create pythonbpf-playground-rootfs)
trap 'docker rm -f "$cid" >/dev/null' EXIT
rm -f "$OUT/rootfs.squashfs"
docker export "$cid" | docker run --rm -i -v "$OUT:/out" ubuntu:24.04 bash -euo pipefail -c '
  apt-get update -qq && apt-get install -y -qq squashfs-tools >/dev/null
  mkdir /r && tar -x -C /r --exclude=.dockerenv
  rm -rf /r/dev/* /r/proc/* /r/sys/*
  mksquashfs /r /out/rootfs.squashfs -comp zstd -noappend -quiet
  chown '"$(id -u):$(id -g)"' /out/rootfs.squashfs'
ls -la "$OUT/rootfs.squashfs"
