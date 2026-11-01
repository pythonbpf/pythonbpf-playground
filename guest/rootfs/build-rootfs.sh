#!/usr/bin/env bash
# Build guest root filesystems, one per playground version in guest/versions.json:
#   guest/out/images/<id>/rootfs.squashfs + meta.json
# Usage: build-rootfs.sh [id]        (default: every version)
#
# Version specs: "git:<ref>" (resolved to a commit now, so the image is
# reproducible and its label says exactly what it contains) or "pypi:<version>".
#
# Layers are reused from the ghcr.io build cache when available (the slow part
# is downloading LLVM). CI sets ROOTFS_PUSH=1 to push the cache and the image
# (<registry>:<id>) back.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GUEST="$HERE/.."
ROOT="$GUEST/.."
OUT="$GUEST/out"
ONLY="${1:-}"
PYBPF_GIT=https://github.com/pythonbpf/Python-BPF
PYLIB_GIT=https://github.com/pythonbpf/pylibbpf
REGISTRY="${ROOTFS_REGISTRY:-ghcr.io/pythonbpf/pythonbpf-playground-rootfs}"
PUSH="${ROOTFS_PUSH:-0}"

test -f "$OUT/vmlinux" || { echo "build the kernel first (guest/kernel/build-kernel.sh)"; exit 1; }

# resolve <spec> <git url> <pip name> -> PIP (requirement), REF (git ref/tag), SHOW (label)
resolve() {
  case "$1" in
    git:*)
      local ref="${1#git:}" sha
      sha=$(git ls-remote "$2" "$ref" | awk 'NR == 1 { print $1 }')
      [ -n "$sha" ] || sha="$ref" # already a commit id
      PIP="$3 @ git+$2@$sha"; REF="$sha"; SHOW="$ref@${sha:0:7}" ;;
    pypi:*)
      PIP="$3==${1#pypi:}"; REF="v${1#pypi:}"; SHOW="${1#pypi:}" ;;
    *) echo "bad version spec: $1"; exit 1 ;;
  esac
}

# Tiny helper image with mksquashfs (cached after the first build).
docker buildx build --load -q -t pythonbpf-playground-squashfs - >/dev/null <<'EOF'
FROM ubuntu:24.04
RUN apt-get update -qq && apt-get install -y -qq squashfs-tools >/dev/null && rm -rf /var/lib/apt/lists/*
EOF

built=0
while IFS='|' read -r id pybpf pylib; do
  [ -z "$ONLY" ] || [ "$ONLY" = "$id" ] || continue
  resolve "$pybpf" "$PYBPF_GIT" pythonbpf; PYBPF_PIP="$PIP"; TOOLS_REF="$REF"; PYBPF_SHOW="$SHOW"
  resolve "$pylib" "$PYLIB_GIT" pylibbpf; PYLIB_PIP="$PIP"; PYLIB_SHOW="$SHOW"
  echo "==> $id: pythonbpf $PYBPF_SHOW, pylibbpf $PYLIB_SHOW"

  build=(docker buildx build
    --build-arg PYTHONBPF_PIP="$PYBPF_PIP"
    --build-arg PYLIBBPF_PIP="$PYLIB_PIP"
    --build-arg PYTHONBPF_TOOLS_REF="$TOOLS_REF"
    --cache-from "type=registry,ref=$REGISTRY:buildcache-$id"
    -f "$HERE/Dockerfile")
  [ "$PUSH" = 1 ] && build+=(--cache-to "type=registry,ref=$REGISTRY:buildcache-$id,mode=max")

  # The image's filesystem goes straight from BuildKit into mksquashfs.
  dest="$OUT/images/$id"
  mkdir -p "$dest"
  "${build[@]}" --output type=tar,dest=- "$ROOT" \
  | docker run --rm -i -v "$dest:/out" pythonbpf-playground-squashfs bash -euo pipefail -c '
    mkdir /r && tar -x -C /r
    rm -rf /r/dev/* /r/proc/* /r/sys/*
    mksquashfs /r /out/rootfs.squashfs.new -comp zstd -noappend -quiet >/dev/null
    mv /out/rootfs.squashfs.new /out/rootfs.squashfs
    chown '"$(id -u):$(id -g)"' /out/rootfs.squashfs'

  if [ "$PUSH" = 1 ]; then # same build, fully cached: just tag + push
    "${build[@]}" --tag "$REGISTRY:$id" --push "$ROOT"
  fi

  python3 - "$GUEST/versions.json" "$id" "$PYBPF_SHOW" "$PYLIB_SHOW" > "$dest/meta.json" <<'EOF'
import datetime, json, sys
path, vid, pybpf, pylib = sys.argv[1:]
v = next(v for v in json.load(open(path)) if v["id"] == vid)
print(json.dumps({
    "label": v.get("label", vid), "default": bool(v.get("default")), "order": v.get("order", 100),
    "pythonbpf": pybpf, "pylibbpf": pylib,
    "built": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%MZ"),
}, indent=2))
EOF
  rm -f "$dest/resolved.json"
  ls -la "$dest/rootfs.squashfs"
  built=$((built + 1))
done < <(python3 -c 'import json, sys
for v in json.load(open(sys.argv[1])):
    print("|".join([v["id"], v["pythonbpf"], v["pylibbpf"]]))' "$GUEST/versions.json")

[ "$built" -gt 0 ] || { echo "no version matched '${ONLY}'"; exit 1; }
