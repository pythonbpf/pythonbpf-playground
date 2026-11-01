#!/usr/bin/env bash
# Build the playground guest kernel (uncompressed vmlinux for Firecracker) in Docker.
# Output: guest/out/vmlinux (+ guest/out/vmlinux.config)
set -euo pipefail

KVER="${KVER:-6.1.191}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/../out"
mkdir -p "$OUT"

docker run --rm -i \
  -v "$HERE:/cfg:ro" -v "$OUT:/out" \
  -e KVER="$KVER" -e JOBS="$(nproc)" -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
  ubuntu:24.04 bash -euo pipefail <<'EOF'
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq build-essential flex bison bc libelf-dev libssl-dev dwarves \
  python3 curl xz-utils cpio >/dev/null
cd /tmp
curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KVER}.tar.xz" | tar xJ
cd "linux-${KVER}"
cp /cfg/microvm-6.1.config .config
scripts/kconfig/merge_config.sh -m .config /cfg/bpf.config >/dev/null
make olddefconfig >/dev/null

# Fail loudly if any required option got dropped by Kconfig dependencies.
missing=0
while read -r line; do
  case "$line" in CONFIG_*=y)
    grep -qx "$line" .config || { echo "MISSING after olddefconfig: $line"; missing=1; } ;;
  esac
done < /cfg/bpf.config
[ "$missing" = 0 ] || exit 1

make -j"${JOBS}" vmlinux 2>&1 | grep -E "^(  LD|  BTF|.*error)" || true
test -f vmlinux
strip --strip-debug -o /out/vmlinux vmlinux   # keeps .BTF, drops DWARF
cp .config /out/vmlinux.config
chown "${HOST_UID}:${HOST_GID}" /out/vmlinux /out/vmlinux.config
EOF

ls -la "$OUT/vmlinux"
