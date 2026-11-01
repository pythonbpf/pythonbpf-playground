#!/usr/bin/env bash
# Install or upgrade the playground on this host. Run as root from a directory
# containing: pgd vmlinux *.service images/<version>/{rootfs.squashfs,meta.json}
# and, when Firecracker should be installed or upgraded, firecracker + jailer.
# Idempotent: safe to re-run for every release.
set -euo pipefail
cd "$(dirname "$0")"
[ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
for f in pgd vmlinux pgd.service pg-launcher.service pg-netns.service; do
  [ -f "$f" ] || { echo "missing $f"; exit 1; }
done
ls images/*/rootfs.squashfs >/dev/null 2>&1 || { echo "missing images/<version>/rootfs.squashfs"; exit 1; }
[ -e /dev/kvm ] || { echo "/dev/kvm missing: this host cannot run Firecracker"; exit 1; }

OPT=/opt/pythonbpf-playground
SLOTS="${SLOTS:-2}"

# Users: pgd runs as 'playground'; each VM slot gets its own unprivileged uid.
id playground >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin playground
for i in $(seq 0 $((SLOTS - 1))); do
  id "pg-fc$i" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "pg-fc$i"
done

install -d -m 0755 "$OPT" /srv/jailer
# Atomic replace: VMs that are running keep their (hard-linked) old files.
inst() { install -m "$1" "$2" "$3.new" && mv -f "$3.new" "$3"; }
if [ -f firecracker ] && [ -f jailer ]; then
  inst 0755 firecracker /usr/local/bin/firecracker
  inst 0755 jailer /usr/local/bin/jailer
fi
[ -x /usr/local/bin/firecracker ] && [ -x /usr/local/bin/jailer ] || { echo "firecracker/jailer missing: include them in the bundle"; exit 1; }
inst 0755 pgd "$OPT/pgd"
inst 0644 vmlinux "$OPT/vmlinux"

# Playground versions: exactly the images in this bundle.
install -d -m 0755 "$OPT/images"
for d in images/*/; do
  id=$(basename "$d")
  install -d -m 0755 "$OPT/images/$id"
  inst 0644 "$d/rootfs.squashfs" "$OPT/images/$id/rootfs.squashfs"
  [ -f "$d/meta.json" ] && inst 0644 "$d/meta.json" "$OPT/images/$id/meta.json"
done
for d in "$OPT"/images/*/; do
  [ -d "images/$(basename "$d")" ] || { echo "removing old version $(basename "$d")"; rm -rf "$d"; }
done
rm -f "$OPT/rootfs.squashfs"   # pre-versions layout

if [ "$(stat -c %d "$OPT")" != "$(stat -c %d /srv/jailer)" ]; then
  echo "note: $OPT and /srv/jailer are on different filesystems; images will be copied per VM (slower)"
fi

install -m 0644 pg-netns.service pg-launcher.service pgd.service /etc/systemd/system/
echo "versions: $(ls "$OPT/images" | paste -sd " ")"
systemctl daemon-reload
systemctl enable pg-netns.service pg-launcher.service pgd.service >/dev/null
systemctl restart pg-netns.service
systemctl restart pg-launcher.service
systemctl restart pgd.service
sleep 1
systemctl --no-pager --lines=0 status pg-launcher.service pgd.service | grep -E "●|Active:"
echo "firecracker: $(/usr/local/bin/firecracker --version | head -1)"
curl -fsS http://127.0.0.1:8090/healthz && echo
