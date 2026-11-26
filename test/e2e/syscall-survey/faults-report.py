#!/usr/bin/env python3
"""Parse the profiling-metrics CSVs written by faults.sh and print faults/syscalls per fork+exec."""
import glob, os, re, sys
res = sys.argv[1]
rows = {}
for f in sorted(glob.glob(os.path.join(res, "f-*.csv"))):
    cols, last = None, None
    for line in open(f, errors="replace"):
        parts = line.rstrip("\n").split("\t")
        if cols is None and any("/task/faults" in p for p in parts):
            cols = [p.strip() for p in parts]
            continue
        if all(re.fullmatch(r"-?\d+", p) for p in parts) and cols and len(parts) == len(cols):
            v = list(map(int, parts))
            last = v if last is None else [max(a, b) for a, b in zip(last, v)]
    if cols is None or last is None:
        print("no data in", f, file=sys.stderr)
        continue
    d = dict(zip(cols, last))
    key = os.path.basename(f)[2:-4]
    m = re.match(r"(.*)-(\d+)$", key)
    rows.setdefault(m.group(1), {})[int(m.group(2))] = (d.get("/task/faults"), d.get("/task/syscalls"))
print("workload\tN\tfaults\tsyscalls\tfaults_per_exec\tsyscalls_per_exec")
for w, byn in rows.items():
    base = byn.get(0)
    for n in sorted(byn):
        fl, sc = byn[n]
        if n and base:
            print(f"{w}\t{n}\t{fl}\t{sc}\t{(fl - base[0]) / n:.1f}\t{(sc - base[1]) / n:.1f}")
        else:
            print(f"{w}\t{n}\t{fl}\t{sc}\t-\t-")
