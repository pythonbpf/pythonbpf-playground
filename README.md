# PythonBPF Playground

Write eBPF programs in Python and run them in a real Linux kernel from your browser.
Each visitor gets a private [Firecracker](https://firecracker-microvm.github.io/) microVM with an editor,
live output, the generated LLVM IR, and a root terminal.

Live at **https://pythonbpf.xeon.me**.

```
browser ──HTTPS──► Cloudflare ──tunnel──► cloudflared ──► 127.0.0.1:8090  pgd serve  (user "playground")
                                                             │ static UI · /ws · FIFO queue · sessions
                                                             │ unix socket: "start N" / "stop N" only
                                                             ▼
                                                      pgd launcher (root)
                                                             │ jailer → firecracker: chroot, uid pg-fcN,
                                                             │ empty netns, seccomp, cgroup cpu/mem/pids
                                                             ▼
                                                      microVM (no NIC, read-only squashfs root)
                                                        agent.py on vsock :5000 (runs) and :5001 (pty)
```

## Layout

| Path | What |
|---|---|
| `server/` | `pgd`, a single static Go binary. `serve` runs the web UI, WebSocket API and queue; `launcher` is the root helper that jails Firecracker |
| `server/static/` | The UI: plain HTML/CSS/JS with vendored CodeMirror 5 and xterm.js. No build step |
| `guest/kernel/` | Guest kernel: Linux 6.1 LTS using Firecracker's microVM config plus `bpf.config` (BPF, BTF, kprobes, tracepoints, vsock, balloon) |
| `guest/rootfs/` | Guest root filesystem: Ubuntu 24.04, LLVM 22 (`llc`/`opt`), pythonbpf, pylibbpf, bpftool and a `vmlinux.py` generated for the guest kernel |
| `examples/` | Examples shown in the UI and shipped in the VM at `/opt/pg/examples` |
| `deploy/` | systemd units and `install.sh` |

## Sessions and the queue

- A **session** is one microVM leased to one browser tab. The VM boots on the first Run or when the Terminal tab opens, and it is destroyed when the session ends.
- When every slot is busy, sessions wait in a **FIFO queue** (at most 20). Each waiting tab sees its position and an estimated wait. Closing the tab gives up its place.
- A session ends after **2 minutes idle**. The **10-minute cap** applies only while someone is waiting, and always with at least 2 minutes' notice.
- Each client IP gets one session at a time.
- Each run is limited to **20 s** and **256 KB** of output. If the VM hangs or the guest kernel panics, it is restarted in the same slot, up to 3 times per session.

## Build

Requirements: Docker, Go ≥ 1.23, and `/dev/kvm` to run VMs locally.

```sh
make kernel     # guest/out/vmlinux
make rootfs     # guest/out/rootfs.squashfs
make test       # unit tests (queue / session logic)
make dev        # http://127.0.0.1:8090, Firecracker without the jailer (dev only)
```

## Deploy

`deploy/install.sh` installs or upgrades everything on a host, and it is idempotent. Run it as root from a directory that contains
`pgd vmlinux rootfs.squashfs firecracker jailer pgd.service pg-launcher.service`.
