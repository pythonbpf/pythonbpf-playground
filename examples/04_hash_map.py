# Count clone() calls per PID in a BPF hash map, then read the map from Python.
import os

from ctypes import c_int32, c_int64, c_uint64, c_void_p
from pythonbpf import BPF, bpf, bpfglobal, map, section
from pythonbpf.helper import pid
from pythonbpf.maps import HashMap


@bpf
@map
def counts() -> HashMap:
    return HashMap(key=c_int32, value=c_uint64, max_entries=1024)


@bpf
@section("tracepoint/syscalls/sys_enter_clone")
def on_clone(ctx: c_void_p) -> c_int64:
    process_id = pid()
    prev = counts.lookup(process_id)
    if prev:
        counts.update(process_id, prev + 1)
    else:
        counts.update(process_id, 1)
    return c_int64(0)


@bpf
@bpfglobal
def LICENSE() -> str:
    return "GPL"


b = BPF()
b.load()
b.attach_all()

for _ in range(5):  # fork() -> clone()
    child = os.fork()
    if child == 0:
        os._exit(0)
    os.waitpid(child, 0)

print(f"this process is pid {os.getpid()}")
counts = b["counts"].items()  # a plain dict: {pid: count}
for key, value in counts.items():
    print(f"pid {key}: {value} clone() calls")
