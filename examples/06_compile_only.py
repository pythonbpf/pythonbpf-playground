# Compile without loading: look at the LLVM IR tab to see what PythonBPF generates.
from ctypes import c_int64, c_void_p
from pythonbpf import bpf, bpfglobal, compile, map, section
from pythonbpf.helper import ktime
from pythonbpf.maps import HashMap


@bpf
@map
def last() -> HashMap:
    return HashMap(key=c_int64, value=c_int64, max_entries=1)


@bpf
@section("tracepoint/syscalls/sys_enter_sync")
def do_trace(ctx: c_void_p) -> c_int64:
    key = 0
    tsp = last.lookup(key)
    if tsp:
        delta = ktime() - tsp
        if delta < 1000000000:
            print(f"{delta} ns since last sync")
        last.delete(key)
    else:
        last.update(key, ktime())
    return c_int64(0)


@bpf
@bpfglobal
def LICENSE() -> str:
    return "GPL"


compile()
print("Compiled to main.o; see the LLVM IR tab.")
