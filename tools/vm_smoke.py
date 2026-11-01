"""Smoke test: boot the guest in Firecracker (no jailer), run every example through
the agent, and poke the terminal. Usage: python3 tools/vm_smoke.py [version-id]
Exits non-zero if the VM does not come up or any example fails."""
import json, os, socket, subprocess, sys, time, glob

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
IMAGE = sys.argv[1] if len(sys.argv) > 1 else "master"
FC = os.environ.get("FIRECRACKER", os.path.join(ROOT, "dist/firecracker"))
import tempfile
WORK = tempfile.mkdtemp(prefix="pg-smoke-")
os.makedirs(WORK, exist_ok=True)
sock = os.path.join(WORK, "v.sock")
if os.path.exists(sock):
    os.unlink(sock)
cfg = {
    "boot-source": {"kernel_image_path": f"{ROOT}/guest/out/vmlinux",
                    "boot_args": "console=ttyS0 quiet loglevel=3 reboot=k panic=1 pci=off root=/dev/vda ro rootfstype=squashfs init=/sbin/pg-init"},
    "drives": [{"drive_id": "rootfs", "path_on_host": f"{ROOT}/guest/out/images/{IMAGE}/rootfs.squashfs", "is_root_device": True, "is_read_only": True}],
    "machine-config": {"vcpu_count": 1, "mem_size_mib": 256},
    "vsock": {"guest_cid": 3, "uds_path": sock},
    "balloon": {"amount_mib": 0, "deflate_on_oom": True, "free_page_reporting": True},
}
open(f"{WORK}/vm.json", "w").write(json.dumps(cfg))
log = open(f"{WORK}/console.log", "wb")
t0 = time.time()
fc = subprocess.Popen([FC, "--no-api", "--config-file", f"{WORK}/vm.json", "--level", "Warning"],
                      stdin=subprocess.DEVNULL, stdout=log, stderr=log)


def connect(port):
    s = socket.socket(socket.AF_UNIX)
    s.connect(sock)
    s.sendall(f"CONNECT {port}\n".encode())
    line = b""
    while not line.endswith(b"\n"):
        c = s.recv(1)
        if not c:
            raise ConnectionError("closed")
        line += c
    assert line.startswith(b"OK"), line
    return s


failures = ["boot"]
try:
    while True:
        try:
            ctrl = connect(5000)
            f = ctrl.makefile("rb")
            hello = json.loads(f.readline())
            break
        except (ConnectionError, FileNotFoundError, ConnectionRefusedError, json.JSONDecodeError, AssertionError):
            if fc.poll() is not None or time.time() - t0 > 30:
                raise SystemExit("VM did not come up; console:\n" + open(f"{WORK}/console.log").read()[-3000:])
            time.sleep(0.1)
    print(f"agent up in {time.time() - t0:.2f}s: {hello}")

    failures = []
    examples = sorted(glob.glob(f"{ROOT}/examples/*.py"))
    for i, path in enumerate(examples, 1):
        code = open(path).read()
        t1 = time.time()
        ctrl.sendall((json.dumps({"op": "run", "id": i, "code": code, "timeout": 20}) + "\n").encode())
        out, ir = "", None
        while True:
            m = json.loads(f.readline())
            if m.get("id") != i:
                continue
            if m["t"] == "out":
                out += m["data"]
            elif m["t"] == "ir":
                ir = m["data"]
            elif m["t"] == "exit":
                break
        print(f"\n=== {os.path.basename(path)}  exit={m['code']} reason={m['reason']} {time.time() - t1:.2f}s  IR={'%d lines' % ir.count(chr(10)) if ir else None}")
        print(out.rstrip()[-1500:])
        if m["code"] != 0 or not ir:
            failures.append(os.path.basename(path))

    # terminal smoke test
    t = connect(5001)
    def frame(typ, payload):
        return bytes([typ]) + len(payload).to_bytes(2, "big") + payload
    t.sendall(frame(1, (100).to_bytes(2, "big") + (30).to_bytes(2, "big")))
    t.sendall(frame(0, b"echo TERM-$((6*7)); uname -r; ip -o link | cut -d: -f2; stty size; free -m | head -2\n"))
    time.sleep(1.5)
    t.settimeout(0.5)
    buf = b""
    try:
        while True:
            d = t.recv(65536)
            if not d:
                break
            buf += d
    except socket.timeout:
        pass
    text = buf.decode(errors="replace")
    print("\n=== terminal\n" + text[-800:])
    if "TERM-42" not in text:
        failures.append("terminal")
    print("\nFAILED: " + ", ".join(failures) if failures else "\nALL OK")
finally:
    fc.kill()
    fc.wait()
sys.exit(1 if failures else 0)
