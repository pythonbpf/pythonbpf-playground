#!/usr/bin/env python3
"""PythonBPF playground guest agent (stdlib only).

Listens on two vsock ports inside the microVM:

  5000  control: JSON lines.
        host -> guest  {"op": "run", "id": N, "code": "...", "timeout": 20}
                       {"op": "kill"} | {"op": "ping"}
        guest -> host  {"t": "hello", "kernel": ..., "pythonbpf": ...}
                       {"t": "start", "id": N}
                       {"t": "out", "id": N, "data": "..."}
                       {"t": "ir", "id": N, "data": "..."}
                       {"t": "exit", "id": N, "code": rc, "reason": ..., "ms": ...}
                       {"t": "pong"}

  5001  terminal: one bash on a pty per connection.
        guest -> host  raw pty bytes
        host -> guest  frames: type(1) len(2, big endian) payload
                       type 0 = keyboard bytes, type 1 = resize (cols u16, rows u16)
"""

import codecs
import fcntl
import json
import os
import pty
import select
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time

CTRL_PORT = 5000
PTY_PORT = 5001
WORK = "/work"
MAX_TIMEOUT = 60.0
OUT_CAP = 256 * 1024
IR_CAP = 512 * 1024

BASE_ENV = {
    "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "HOME": "/root",
    "LANG": "C.UTF-8",
    "USER": "root",
}


def pkg_version(name):
    try:
        from importlib.metadata import version

        return version(name)
    except Exception:
        return "unknown"


def kill_group(pid, sig=signal.SIGKILL):
    try:
        os.killpg(pid, sig)
    except (ProcessLookupError, PermissionError):
        pass


class Control:
    def __init__(self, conn):
        self.conn = conn
        self.send_lock = threading.Lock()
        self.proc = None

    def send(self, obj):
        data = (json.dumps(obj) + "\n").encode()
        with self.send_lock:
            self.conn.sendall(data)

    def kill(self):
        proc = self.proc
        if proc is not None and proc.poll() is None:
            kill_group(proc.pid)

    def serve(self):
        self.send(
            {
                "t": "hello",
                "kernel": os.uname().release,
                "pythonbpf": pkg_version("pythonbpf"),
                "pylibbpf": pkg_version("pylibbpf"),
                "python": sys.version.split()[0],
            }
        )
        try:
            for line in self.conn.makefile("rb"):
                try:
                    msg = json.loads(line)
                except ValueError:
                    continue
                op = msg.get("op")
                if op == "run":
                    self.kill()
                    threading.Thread(target=self.run, args=(msg,), daemon=True).start()
                elif op == "kill":
                    self.kill()
                elif op == "ping":
                    self.send({"t": "pong"})
        except OSError:
            pass
        finally:
            self.kill()

    def run(self, msg):
        rid = msg.get("id", 0)
        code = str(msg.get("code", ""))
        timeout = min(float(msg.get("timeout", 20)), MAX_TIMEOUT)
        src = os.path.join(WORK, "main.py")
        ll = os.path.join(WORK, "main.ll")
        t0 = time.monotonic()

        try:
            os.makedirs(WORK, exist_ok=True)
            with open(src, "w") as f:
                f.write(code)
            for p in (ll, ll + ".tmp"):
                if os.path.exists(p):
                    os.unlink(p)
            # Drop bpf_printk lines left over from a previous run.
            with open("/sys/kernel/tracing/trace", "w"):
                pass
        except OSError as e:
            self.send({"t": "out", "id": rid, "data": f"[playground] setup failed: {e}\n"})

        env = dict(BASE_ENV, PYTHONUNBUFFERED="1", PG_IR_OUT=ll, TERM="dumb")
        self.send({"t": "start", "id": rid})
        proc = subprocess.Popen(
            [sys.executable, "-u", src],
            cwd=WORK,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            start_new_session=True,
            env=env,
        )
        self.proc = proc
        fd = proc.stdout.fileno()
        os.set_blocking(fd, False)
        dec = codecs.getincrementaldecoder("utf-8")(errors="replace")
        deadline = t0 + timeout
        sent = 0
        ir_sent = False
        reason = "exited"
        exited_at = None

        def try_send_ir():
            nonlocal ir_sent
            if ir_sent or not os.path.exists(ll):
                return
            ir_sent = True
            with open(ll, "rb") as f:
                data = f.read(IR_CAP + 1)
            text = data[:IR_CAP].decode("utf-8", "replace")
            if len(data) > IR_CAP:
                text += "\n; ... truncated ...\n"
            self.send({"t": "ir", "id": rid, "data": text})

        try:
            while True:
                r, _, _ = select.select([fd], [], [], 0.2)
                if r:
                    try:
                        chunk = os.read(fd, 16384)
                    except BlockingIOError:
                        chunk = None
                    if chunk == b"":
                        break  # every writer closed stdout
                    if chunk:
                        if sent + len(chunk) > OUT_CAP:
                            chunk = chunk[: OUT_CAP - sent]
                            reason = "output limit (256 KB) reached"
                        sent += len(chunk)
                        text = dec.decode(chunk)
                        if text:
                            self.send({"t": "out", "id": rid, "data": text})
                        if reason != "exited":
                            break
                try_send_ir()
                now = time.monotonic()
                if now > deadline:
                    reason = f"time limit ({int(timeout)}s) reached"
                    break
                if proc.poll() is not None:
                    # Main process is gone; give children a moment to flush, then stop.
                    exited_at = exited_at or now
                    if now - exited_at > 0.5:
                        break
        except OSError:
            reason = "host disconnected"
        finally:
            kill_group(proc.pid)
            try:
                rc = proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                rc = None
            proc.stdout.close()

        try:
            tail = dec.decode(b"", final=True)
            if tail:
                self.send({"t": "out", "id": rid, "data": tail})
            try_send_ir()
            self.send(
                {
                    "t": "exit",
                    "id": rid,
                    "code": rc,
                    "reason": reason,
                    "ms": int((time.monotonic() - t0) * 1000),
                }
            )
        except OSError:
            pass


def write_all(fd, data):
    while data:
        n = os.write(fd, data)
        data = data[n:]


def terminal(conn):
    pid, master = pty.fork()
    if pid == 0:
        os.chdir(WORK)
        env = dict(BASE_ENV, TERM="xterm-256color", PS1=r"\[\e[1;32m\]root@playground\[\e[0m\]:\[\e[1;34m\]\w\[\e[0m\]# ")
        os.execve("/bin/bash", ["bash", "--norc", "-i"], env)
    buf = b""
    try:
        while True:
            r, _, _ = select.select([conn, master], [], [])
            if master in r:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    break
                if not data:
                    break
                conn.sendall(data)
            if conn in r:
                data = conn.recv(65536)
                if not data:
                    break
                buf += data
                while len(buf) >= 3:
                    typ = buf[0]
                    (n,) = struct.unpack(">H", buf[1:3])
                    if len(buf) < 3 + n:
                        break
                    payload, buf = buf[3 : 3 + n], buf[3 + n :]
                    if typ == 0:
                        write_all(master, payload)
                    elif typ == 1 and n == 4:
                        cols, rows = struct.unpack(">HH", payload)
                        fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    except OSError:
        pass
    finally:
        kill_group(pid, signal.SIGHUP)
        time.sleep(0.1)
        kill_group(pid)
        try:
            os.waitpid(pid, 0)
        except ChildProcessError:
            pass
        os.close(master)
        conn.close()


def listen(port, handler):
    srv = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((socket.VMADDR_CID_ANY, port))
    srv.listen(4)
    while True:
        conn, _ = srv.accept()
        threading.Thread(target=handler, args=(conn,), daemon=True).start()


# Compiling a tiny program right after boot pulls Python, pythonbpf, opt and llc
# into the guest page cache (read from the compressed rootfs once), so the
# user's first Run is as fast as later ones. Low priority: a real run wins.
WARMUP = """
from ctypes import c_int64, c_void_p
from pythonbpf import bpf, bpfglobal, compile, section
import pylibbpf


@bpf
@section("tracepoint/syscalls/sys_enter_sync")
def warm(ctx: c_void_p) -> c_int64:
    return c_int64(0)


@bpf
@bpfglobal
def LICENSE() -> str:
    return "GPL"


compile()
"""


def warm_cache():
    d = tempfile.mkdtemp(prefix="warm-")
    try:
        path = os.path.join(d, "warm.py")
        with open(path, "w") as f:
            f.write(WARMUP)
        subprocess.run(
            [sys.executable, path], cwd=d, env=BASE_ENV, timeout=120,
            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            preexec_fn=lambda: os.nice(10),
        )
    except Exception:
        pass
    finally:
        shutil.rmtree(d, ignore_errors=True)


def main():
    os.makedirs(WORK, exist_ok=True)
    threading.Thread(target=listen, args=(PTY_PORT, terminal), daemon=True).start()
    threading.Thread(target=warm_cache, daemon=True).start()
    listen(CTRL_PORT, lambda c: Control(c).serve())


if __name__ == "__main__":
    main()
