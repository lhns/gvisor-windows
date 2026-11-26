#!/usr/bin/env python3
"""Merge downloaded stage artifacts (dir layout: <artifact-name>/...) into one markdown summary on stdout."""
import glob, json, os, re, sys

root = sys.argv[1]
out = []
p = out.append

# A/B acceptance steps
cols = {}
for f in sorted(glob.glob(f"{root}/acceptance-*/steps.txt")):
    name = os.path.basename(os.path.dirname(f))[len("acceptance-"):]
    for line in open(f):
        m = re.match(r"STEP (\d+) (PASS|FAIL) (\S+) ?(.*)", line)
        if m:
            cols.setdefault(name, {})[int(m[1])] = (m[2], m[3], m[4])
p("## Acceptance test (A: inside runsc, B: native)\n")
if cols:
    names = list(cols)
    p("| step | " + " | ".join(names) + " |")
    p("|---|" + "---|" * len(names))
    for n in range(1, 11):
        label = next((c[n][1] for c in cols.values() if n in c), "?")
        p(f"| {n} {label} | " + " | ".join(cols[c].get(n, ("-",))[0] for c in names) + " |")
    p("\nFailure details:\n")
    for c in names:
        for n, (st, nm, d) in sorted(cols[c].items()):
            if st == "FAIL":
                p(f"- `{c}` step {n} {nm}: {d}")
p("\n## Benchmarks (seconds, lower is better)\n")
b = {}
for f in glob.glob(f"{root}/bench*/bench.tsv"):
    for line in open(f):
        l, k, v = line.rstrip("\n").split("\t")
        b.setdefault(k, {})[l] = v
if b:
    ls = ["native-host", "docker-runc", "docker-runsc"]
    p("| workload | " + " | ".join(ls) + " | runsc/runc |")
    p("|---|" + "---|" * (len(ls) + 1))
    for k, v in b.items():
        try:
            r = f"{float(v['docker-runsc']) / float(v['docker-runc']):.1f}x"
        except Exception:
            r = "-"
        p(f"| {k} | " + " | ".join(v.get(l, "-") for l in ls) + f" | {r} |")
p("\n## ELF survey (64 KiB mapping)\n")
rows = []
for f in sorted(glob.glob(f"{root}/elf-*/elf.json")):
    rows.append(json.load(open(f)))
if rows:
    p("| image | ELF files | PT_LOAD | LOAD with (vaddr-offset)%64K!=0 | LOAD with vaddr%64K!=0 | files affected (offset mismatch) | files affected (vaddr unaligned) |")
    p("|---|---|---|---|---|---|---|")
    for r in rows:
        p(f"| {r['image']} | {r['elf_files']} | {r['pt_load']} | {r['load_offset_vaddr_mismatch_64k']} ({r['pt_load_mismatch_pct']}%) | "
          f"{r['load_vaddr_not_64k_aligned']} ({r['pt_load_unaligned_pct']}%) | {r['files_with_mismatch']} ({r['files_with_mismatch_pct']}%) | "
          f"{r['files_with_unaligned_vaddr']} ({r['files_with_unaligned_vaddr_pct']}%) |")
p("\n## Image storage (gate 0a.4)\n")
s = {}
for f in glob.glob(f"{root}/storage*/storage.tsv"):
    for line in open(f):
        v, o, sec, rc = line.rstrip("\n").split("\t")
        s.setdefault(o, {})[v] = f"{sec}" + ("" if rc == "0" else f" (rc={rc})")
if s:
    vs = []
    for d in s.values():
        for v in d:
            if v not in vs:
                vs.append(v)
    p("| op | " + " | ".join(vs) + " |")
    p("|---|" + "---|" * len(vs))
    for o, d in s.items():
        p(f"| {o} | " + " | ".join(d.get(v, "-") for v in vs) + " |")
for f in glob.glob(f"{root}/storage*/overlay-dir.txt"):
    p("\n```\n" + open(f).read() + "\n```")
print("\n".join(out))
