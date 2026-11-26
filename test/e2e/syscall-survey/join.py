#!/usr/bin/env python3
"""Join tracer output (dynamic syscalls per site) with the static survey.
Usage: join.py SURVEY_PREFIX TRACE.tsv [...]  -> JSON on stdout: per trace, dynamic syscall share per class."""
import gzip, json, sys
from collections import Counter

prefix = sys.argv[1]
db = json.load(gzip.open(prefix + ".sites.json.gz", "rt"))
path_sha, sites = db["path_sha"], db["sites"]
look = {}
for sha, lst in sites.items():
    look[sha] = {off: (cls, rl) for off, cls, rl in lst}
out = {}
for tsv in sys.argv[2:]:
    byc, reloc_by, miss, total, top = Counter(), Counter(), Counter(), 0, Counter()
    for line in open(tsv):
        if line.startswith("#"):
            continue
        path, off, nr, cnt = line.rstrip("\n").split("\t")
        off, nr, cnt = int(off), int(nr), int(cnt)
        total += cnt
        sha = path_sha.get(path)
        hit = look.get(sha, {}).get(off) if sha else None
        if hit is None:
            miss[path if sha is None else path + " (offset not a classified site)"] += cnt
            byc["unmatched"] += cnt
            continue
        cls, rl = hit
        byc[str(cls)] += cnt
        if rl:
            reloc_by[str(cls)] += cnt
        top[(path, off, nr, cls)] += cnt
    out[tsv] = {"total": total, "by_class": dict(byc), "reloc5_by_class": dict(reloc_by),
                "unmatched_top": miss.most_common(5),
                "top_sites": [[p, hex(o), nr, c, n] for (p, o, nr, c), n in top.most_common(8)]}
json.dump(out, sys.stdout, indent=1)
