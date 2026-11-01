# Playground guest root filesystem. Built by build-rootfs.sh, exported to squashfs.
# Build context: repository root (needs guest/out/vmlinux from guest/kernel/build-kernel.sh)

ARG UBUNTU=ubuntu:24.04
ARG PYTHONBPF_VERSION=0.2.0
ARG PYLIBBPF_VERSION=0.0.7
ARG BPFTOOL_VERSION=v7.5.0
# Same LLVM generation as upstream PythonBPF CI: llvmlite's IR needs llc/opt >= 21.
ARG LLVM_VERSION=22

# ---------- bpftool from source ----------
FROM ${UBUNTU} AS bpftool
ARG BPFTOOL_VERSION
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
      build-essential git libelf-dev zlib1g-dev libcap-dev pkg-config >/dev/null
RUN git clone -q --depth 1 --recurse-submodules -b ${BPFTOOL_VERSION} \
      https://github.com/libbpf/bpftool /src \
 && make -s -C /src/src -j"$(nproc)" >/dev/null \
 && install -m 0755 /src/src/bpftool /usr/local/sbin/bpftool

# ---------- vmlinux.py for the guest kernel ----------
FROM bpftool AS vmlinuxpy
ARG PYTHONBPF_VERSION
RUN DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
      clang libclang-dev python3 python3-pip >/dev/null \
 && pip install -q --break-system-packages ctypeslib2 clang==16.0.6
ENV LD_LIBRARY_PATH=/usr/lib/llvm-18/lib
COPY guest/out/vmlinux /build/vmlinux
COPY guest/rootfs/vmlinux-gen.py /build/vmlinux-gen.py
RUN cd /build && python3 vmlinux-gen.py --btf-source /build/vmlinux -o /build/vmlinux.py \
 && test -s /build/vmlinux.py

# ---------- final image ----------
FROM ${UBUNTU}
ARG PYTHONBPF_VERSION
ARG PYLIBBPF_VERSION
ARG LLVM_VERSION
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends ca-certificates curl gnupg >/dev/null \
 && curl -fsSL https://apt.llvm.org/llvm-snapshot.gpg.key | gpg --dearmor -o /usr/share/keyrings/llvm.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/llvm.gpg] http://apt.llvm.org/noble/ llvm-toolchain-noble-${LLVM_VERSION} main" \
      > /etc/apt/sources.list.d/llvm.list \
 && apt-get update -qq && apt-get install -y -qq --no-install-recommends \
      llvm-${LLVM_VERSION} python3 python3-pip tini iproute2 procps psmisc less nano vim-tiny \
      file strace libelf1t64 libcap2 zlib1g >/dev/null \
 && ln -s /usr/lib/llvm-${LLVM_VERSION}/bin/llc /usr/local/bin/llc \
 && ln -s /usr/lib/llvm-${LLVM_VERSION}/bin/opt /usr/local/bin/opt \
 && pip install -q --no-cache-dir --break-system-packages \
      pythonbpf==${PYTHONBPF_VERSION} pylibbpf==${PYLIBBPF_VERSION} \
 && apt-get purge -y -qq python3-pip curl gnupg >/dev/null && apt-get autoremove -y -qq >/dev/null \
 && rm -rf /var/lib/apt/lists/* /usr/share/doc /usr/share/man /usr/share/locale /root/.cache

COPY --from=bpftool /usr/local/sbin/bpftool /usr/local/sbin/bpftool
COPY --from=vmlinuxpy /build/vmlinux.py /usr/local/lib/python3.12/dist-packages/vmlinux.py
COPY guest/rootfs/pg_ir_capture.py guest/rootfs/pg_ir_capture.pth /usr/local/lib/python3.12/dist-packages/
COPY guest/rootfs/agent.py /opt/pg/agent.py
COPY guest/rootfs/pg-init /sbin/pg-init
COPY examples/ /opt/pg/examples/

# The root filesystem is read-only at runtime, so byte-compile everything now
# (vmlinux.py especially: it is huge and slow to compile on a nested-virt CPU).
RUN chmod 0755 /sbin/pg-init \
 && mkdir -p /work /sys/kernel/tracing /sys/kernel/debug /sys/fs/bpf \
 && python3 -m compileall -q -j0 /usr/lib/python3 /usr/lib/python3.12 \
      /usr/local/lib/python3.12/dist-packages /opt/pg/agent.py >/dev/null || true \
 && printf 'Welcome to the PythonBPF playground VM.\nYour last run is in /work/main.py. Examples: /opt/pg/examples\n' > /etc/motd
