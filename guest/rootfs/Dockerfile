# Playground guest root filesystem. Built by build-rootfs.sh, exported to squashfs.
# Build context: repository root (needs guest/out/vmlinux from guest/kernel/build-kernel.sh)

ARG UBUNTU=ubuntu:24.04
# One image per playground version (see guest/versions.json, build-rootfs.sh):
# pip requirement strings, either "pythonbpf==0.2.0" or
# "pythonbpf @ git+https://github.com/pythonbpf/Python-BPF@<sha>", and the
# Python-BPF git ref whose tools/vmlinux-gen.py matches that pythonbpf.
ARG PYTHONBPF_PIP="pythonbpf==0.2.0"
ARG PYLIBBPF_PIP="pylibbpf==0.0.7"
ARG PYTHONBPF_TOOLS_REF=v0.2.0
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
ARG PYTHONBPF_TOOLS_REF
RUN DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
      clang libclang-dev python3 python3-pip ca-certificates curl >/dev/null \
 && pip install -q --break-system-packages ctypeslib2 clang==16.0.6
ENV LD_LIBRARY_PATH=/usr/lib/llvm-18/lib
COPY guest/out/vmlinux /build/vmlinux
RUN curl -fsSL -o /build/vmlinux-gen.py \
      "https://raw.githubusercontent.com/pythonbpf/Python-BPF/${PYTHONBPF_TOOLS_REF}/tools/vmlinux-gen.py"
RUN cd /build && python3 vmlinux-gen.py --btf-source /build/vmlinux -o /build/vmlinux.py \
 && test -s /build/vmlinux.py

# ---------- wheels: pythonbpf + pylibbpf (+ deps), compiled here if needed ----------
# pylibbpf from git is a C++/pybind11 build (with its libbpf submodule); keep
# the toolchain out of the VM image by building wheels in this stage.
FROM ${UBUNTU} AS wheels
ARG PYTHONBPF_PIP
ARG PYLIBBPF_PIP
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
      python3-pip python3-dev build-essential cmake git ca-certificates pkg-config \
      libelf-dev zlib1g-dev >/dev/null
RUN pip wheel -q --wheel-dir /wheels "${PYTHONBPF_PIP}" "${PYLIBBPF_PIP}"

# ---------- final image ----------
FROM ${UBUNTU}
ARG LLVM_VERSION
ENV DEBIAN_FRONTEND=noninteractive
# apt.llvm.org can be slow or flaky: retry, and fetch the key to a file first
# so a failed download fails loudly instead of feeding gpg an empty pipe.
RUN echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-retries \
 && apt-get update -qq && apt-get install -y -qq --no-install-recommends ca-certificates curl gnupg >/dev/null \
 && curl -fsSL --retry 5 --retry-all-errors -o /tmp/llvm.key https://apt.llvm.org/llvm-snapshot.gpg.key \
 && gpg --dearmor -o /usr/share/keyrings/llvm.gpg /tmp/llvm.key && rm /tmp/llvm.key \
 && echo "deb [signed-by=/usr/share/keyrings/llvm.gpg] http://apt.llvm.org/noble/ llvm-toolchain-noble-${LLVM_VERSION} main" \
      > /etc/apt/sources.list.d/llvm.list \
 && apt-get update -qq && apt-get install -y -qq --no-install-recommends \
      llvm-${LLVM_VERSION} python3 python3-pip tini iproute2 procps psmisc less nano vim-tiny \
      file strace libelf1t64 libcap2 zlib1g >/dev/null \
 && ln -s /usr/lib/llvm-${LLVM_VERSION}/bin/llc /usr/local/bin/llc \
 && ln -s /usr/lib/llvm-${LLVM_VERSION}/bin/opt /usr/local/bin/opt \
 && apt-get purge -y -qq python3-pip curl gnupg >/dev/null && apt-get autoremove -y -qq >/dev/null \
 && rm -rf /var/lib/apt/lists/* /usr/share/doc /usr/share/man /usr/share/locale /root/.cache

RUN --mount=type=bind,from=wheels,source=/wheels,target=/wheels \
    apt-get update -qq && apt-get install -y -qq --no-install-recommends python3-pip >/dev/null \
 && pip install -q --no-cache-dir --break-system-packages --no-index --find-links /wheels pythonbpf pylibbpf \
 && apt-get purge -y -qq python3-pip >/dev/null && apt-get autoremove -y -qq >/dev/null \
 && rm -rf /var/lib/apt/lists/* /root/.cache

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

# Links the image on ghcr.io to the repository (permissions + README).
LABEL org.opencontainers.image.source="https://github.com/pythonbpf/pythonbpf-playground" \
      org.opencontainers.image.description="PythonBPF playground guest root filesystem (pythonbpf, pylibbpf, LLVM, bpftool)" \
      org.opencontainers.image.licenses="Apache-2.0"
