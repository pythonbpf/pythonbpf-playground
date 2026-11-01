# kprobe + kretprobe on the kernel function that deletes files.
import os
import tempfile

from ctypes import c_int64, c_void_p
from pythonbpf import BPF, bpf, bpfglobal, section, trace_fields


@bpf
@section("kprobe/do_unlinkat")
def unlink_enter(ctx: c_void_p) -> c_int64:
    print("do_unlinkat: enter")
    return c_int64(0)


@bpf
@section("kretprobe/do_unlinkat")
def unlink_exit(ctx: c_void_p) -> c_int64:
    print("do_unlinkat: return")
    return c_int64(0)


@bpf
@bpfglobal
def LICENSE() -> str:
    return "GPL"


b = BPF()
b.load()
b.attach_all()

fd, path = tempfile.mkstemp()
os.close(fd)
os.unlink(path)  # -> unlinkat() -> do_unlinkat()
print(f"deleted {path}")

for _ in range(2):
    print(trace_fields()[-1])
