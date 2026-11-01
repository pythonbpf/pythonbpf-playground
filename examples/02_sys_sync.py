# Trace the sync() syscall, like BCC's sys_sync example.
import os

from ctypes import c_int64, c_void_p
from pythonbpf import BPF, bpf, bpfglobal, section, trace_fields


@bpf
@section("tracepoint/syscalls/sys_enter_sync")
def on_sync(ctx: c_void_p) -> c_int64:
    print("sys_sync() called")
    return c_int64(0)


@bpf
@bpfglobal
def LICENSE() -> str:
    return "GPL"


b = BPF()
b.load()
b.attach_all()

os.sync()
os.sync()

for _ in range(2):
    task, pid, cpu, flags, ts, msg = trace_fields()
    print(f"{task} (pid {pid}) on CPU {cpu}: {msg}")
