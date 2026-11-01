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
| `guest/versions.json` | The selectable playground versions: pairs of pythonbpf and pylibbpf |
| `examples/` | Examples shown in the UI and shipped in the VM at `/opt/pg/examples` |
| `tools/vm_smoke.py` | Boots a version in Firecracker and runs every example plus a terminal check |
| `deploy/` | `deploy.sh` (run from your machine), systemd units, `install.sh` |

## Sessions and the queue

- A **session** is one microVM leased to one browser tab. The VM boots on the first Run or when the Terminal tab opens, and it is destroyed when the session ends.
- When every slot is busy, sessions wait in a **FIFO queue** (at most 20). Each waiting tab sees its position and an estimated wait. Closing the tab gives up its place.
- A session ends after **2 minutes idle**. The **10-minute cap** applies only while someone is waiting, and always with at least 2 minutes' notice.
- Each client IP gets one session at a time.
- Each run is limited to **20 s** and **256 KB** of output. If the VM hangs or the guest kernel panics, it is restarted in the same slot, up to 3 times per session.

## Versions

Like the Go and Rust playgrounds, the UI has a version picker. Each entry in `guest/versions.json` pairs a pythonbpf with a pylibbpf, because the two are developed together:

```json
{ "id": "master", "pythonbpf": "git:master", "pylibbpf": "git:master", "default": true }
{ "id": "0.2.0",  "pythonbpf": "pypi:0.2.0", "pylibbpf": "pypi:0.0.7" }
```

`git:<ref>` is resolved to a commit at build time; `pypi:<version>` is a release. Each version becomes its own rootfs image (`guest/out/images/<id>/`), with a `meta.json` that records exactly what went in. The default is **master of both**. Switching versions during a session reboots the VM in the same slot, without re-queuing.

## Build

Requirements: Docker, Go ≥ 1.23, and `/dev/kvm` to run VMs locally.

```sh
make kernel     # guest/out/vmlinux
make rootfs     # one image per version in guest/out/images/ (V=master for just one)
make test       # unit tests (queue, sessions, versions, HTTP)
python3 tools/vm_smoke.py master   # run every example in a real VM
make dev        # http://127.0.0.1:8090, Firecracker without the jailer (dev only)
```

## CI and releases

GitHub Actions runs the tests on every push and pull request. The full build is started by hand from the Actions tab ("Run workflow"):
1. It builds the guest kernel (cached), every version's rootfs and `pgd`.
2. It runs `tools/vm_smoke.py` for every version in a real Firecracker VM, on KVM.
3. It uploads `pythonbpf-playground-x86_64.tar.zst`, and publishes it as a GitHub release if **release** is ticked.

## Deploy

From your own machine:

```sh
deploy/deploy.sh himari            # newest release
deploy/deploy.sh himari v2026.11.01-7
deploy/deploy.sh himari --local    # what you built locally
```

The script downloads the release, checks Firecracker on the host and ships a newer 1.x release if there is one, uploads with rsync and runs `install.sh` with sudo. `install.sh` is idempotent: it creates the users, installs the binaries, images and systemd units, removes versions that are no longer in the bundle and restarts the services. Running VMs keep their old files (hard links) until they exit.

The services run directly on the host under systemd rather than in Docker. The jailer needs root, cgroups and network namespaces, so a container would have to be `--privileged`, which adds complexity and no isolation.

One-time on a new host, to publish through an existing Cloudflare tunnel: `deploy/cloudflared-route.sh`.
