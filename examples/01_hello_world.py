# Hello, World! from the kernel.
# A tracepoint fires on every execve(); bpf_printk output lands in the trace pipe.
import subprocess

from ctypes import c_int64, c_void_p
from pythonbpf import BPF, bpf, bpfglobal, section, trace_fields


@bpf
@section("tracepoint/syscalls/sys_enter_execve")
def hello_world(ctx: c_void_p) -> c_int64:
    print("Hello, World!")
    return c_int64(0)


@bpf
@bpfglobal
def LICENSE() -> str:
    return "GPL"


b = BPF()
b.load()
b.attach_all()
print("Attached. Running `true` three times to trigger execve()...\n")

for _ in range(3):
    subprocess.run(["true"])

for _ in range(3):
    task, pid, cpu, flags, ts, msg = trace_fields()
    print(f"{ts:.6f}  {task:<12} pid={pid:<5} {msg}")
