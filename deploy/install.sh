#!/usr/bin/env bash
# Install or upgrade the playground on this host. Run as root from a directory
# containing: pgd vmlinux rootfs.squashfs firecracker jailer pgd.service pg-launcher.service pg-netns.service
# Idempotent: safe to re-run for every release.
set -euo pipefail
cd "$(dirname "$0")"
[ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
for f in pgd vmlinux rootfs.squashfs firecracker jailer pgd.service pg-launcher.service pg-netns.service; do
  [ -f "$f" ] || { echo "missing $f"; exit 1; }
done
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
inst 0755 firecracker /usr/local/bin/firecracker
inst 0755 jailer /usr/local/bin/jailer
inst 0755 pgd "$OPT/pgd"
inst 0644 vmlinux "$OPT/vmlinux"
inst 0644 rootfs.squashfs "$OPT/rootfs.squashfs"

if [ "$(stat -c %d "$OPT")" != "$(stat -c %d /srv/jailer)" ]; then
  echo "note: $OPT and /srv/jailer are on different filesystems; images will be copied per VM (slower)"
fi

install -m 0644 pg-netns.service pg-launcher.service pgd.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable pg-netns.service pg-launcher.service pgd.service >/dev/null
systemctl restart pg-netns.service
systemctl restart pg-launcher.service
systemctl restart pgd.service
sleep 1
systemctl --no-pager --lines=0 status pg-launcher.service pgd.service | grep -E "●|Active:"
echo "firecracker: $(/usr/local/bin/firecracker --version | head -1)"
curl -fsS http://127.0.0.1:8090/healthz && echo
