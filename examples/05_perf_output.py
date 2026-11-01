# Send structs from the kernel to Python through a perf event buffer.
import os

from ctypes import c_int64, c_void_p
from pythonbpf import BPF, bpf, bpfglobal, map, section, struct
from pythonbpf.helper import comm, ktime, pid
from pythonbpf.maps import PerfEventArray


@bpf
@struct
class data_t:
    pid: c_int64
    ts: c_int64
    comm: str(16)  # type: ignore [valid-type]


@bpf
@map
def events() -> PerfEventArray:
    return PerfEventArray(key_size=c_int64, value_size=c_int64)


@bpf
@section("tracepoint/syscalls/sys_enter_clone")
def hello(ctx: c_void_p) -> c_int64:
    dataobj = data_t()
    dataobj.pid, dataobj.ts = pid(), ktime()
    comm(dataobj.comm)
    events.output(dataobj)
    return 0  # type: ignore [return-value]


@bpf
@bpfglobal
def LICENSE() -> str:
    return "GPL"


b = BPF()
b.load()
b.attach_all()

start = 0


def callback(cpu, event):
    global start
    start = start or event.ts
    print(f"[CPU {cpu}] pid={event.pid} comm={event.comm.decode()} +{(event.ts - start) / 1e6:.3f} ms")


b["events"].open_perf_buffer(callback, struct_name="data_t")

for _ in range(3):
    child = os.fork()
    if child == 0:
        os._exit(0)
    os.waitpid(child, 0)

b["events"].poll(1000)
