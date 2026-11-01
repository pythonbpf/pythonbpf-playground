"""Release notes for a playground bundle, from each image's meta.json.

Usage: python3 tools/release-notes.py <tag> guest/out/images/*/meta.json
"""
import json
import os
import sys


def main():
    tag, metas = sys.argv[1], sys.argv[2:]
    rows = []
    for path in metas:
        with open(path) as f:
            m = json.load(f)
        vid = os.path.basename(os.path.dirname(path))
        label = m.get("label", vid) + (" (default)" if m.get("default") else "")
        rows.append((m.get("order", 100), vid, label, m.get("pythonbpf", "?"), m.get("pylibbpf", "?")))
    rows.sort()
    print(f"Deploy with `deploy/deploy.sh <host> {tag}`.")
    print()
    print("| Version | pythonbpf | pylibbpf |")
    print("|---|---|---|")
    for _, _, label, pybpf, pylib in rows:
        print(f"| {label} | {pybpf} | {pylib} |")


if __name__ == "__main__":
    main()
