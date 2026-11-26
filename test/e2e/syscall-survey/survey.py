#!/usr/bin/env python3
"""Static syscall-site survey of every x86-64 ELF under a root directory.

Usage: survey.py ROOT OUT_PREFIX     -> OUT_PREFIX.json (per-file counts) and OUT_PREFIX.sites.json.gz

Every `syscall` (0f 05), `sysenter` and `int $0x80` found by objdump's linear sweep is classified by the
instructions before it (same function, up to 10 instructions back):
  1  directly preceded by `mov $imm32,%eax` (b8 imm32): the 7-byte sequence gVisor's usertrap patches
  2  eax/rax set from an immediate or zeroed (xor) earlier, or with a different encoding (48 c7 c0, ...)
  3  eax/rax loaded from a register or memory (Go Syscall6, glibc syscall(), computed numbers)
  4  no write to eax/rax found (number set far away, in a caller, or clobbered by a call)
"reloc" = a contiguous, position-independent window of >= 5 bytes around the site (up to 3 instructions
before and 3 after, no rip-relative operand, no branch/call, no direct-branch target on an interior
instruction start) that a 5-byte `jmp rel32` could replace. Indirect jump targets are not known, so this is
an optimistic approximation.
"""
import gzip, hashlib, json, os, re, subprocess, sys
from collections import deque
from multiprocessing import Pool

WIN = 10
PREFIXES = {"lock", "rep", "repz", "repnz", "bnd", "notrack", "data16", "cs", "ds", "es", "ss", "fs", "gs",
            "addr32", "rex.W", "rex", "xacquire", "xrelease"}
RAX = {"%eax", "%rax", "%ax", "%al", "%ah"}
SEC_RE = re.compile(r"^\s*\d+\s+(\S+)\s+([0-9a-f]+)\s+([0-9a-f]+)\s+[0-9a-f]+\s+([0-9a-f]+)\s+2\*\*")
TARGET_RE = re.compile(r"^([0-9a-f]+)\b")
NOWRITE_PREFIX = ("cmp", "test", "push", "nop", "j", "prefetch", "ucomi", "comi", "ret", "bt ", "fxrstor",
                  "xrstor", "ud2", "hlt", "leave", "endbr")


def split_ops(s):
    out, depth, cur = [], 0, ""
    for ch in s:
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        if ch == "," and depth == 0:
            out.append(cur.strip())
            cur = ""
        else:
            cur += ch
    if cur.strip():
        out.append(cur.strip())
    return out


def rax_write(mnem, ops_s):
    """None | 'imm' | 'dyn' | 'opaque' : how this instruction writes rax/eax."""
    if mnem in ("syscall", "sysenter", "int", "cpuid", "rdtsc", "rdtscp", "xgetbv") or mnem.startswith("call"):
        return "opaque"
    if mnem in ("cltq", "cwtl", "cbtw", "lods", "lodsb", "lodsw", "lodsl", "lodsq"):
        return "dyn"
    ops = split_ops(ops_s.split("#")[0].strip())
    if mnem in ("xchg", "cmpxchg", "xadd"):
        return "dyn" if any(o in RAX for o in ops) else None
    if any(mnem == p.strip() or mnem.startswith(p) for p in NOWRITE_PREFIX):
        return None
    if not ops:
        return None
    if mnem in ("mul", "imul", "div", "idiv", "mulq", "divq", "mull", "divl") and len(ops) == 1:
        return "dyn"
    dest = ops[-1]
    if dest not in RAX:
        return None
    if mnem in ("xor", "sub") and len(ops) == 2 and ops[0] == dest:
        return "imm"
    if mnem in ("mov", "movl", "movq", "movabs") and ops[0].startswith("$"):
        return "imm"
    return "dyn"


def is_pi(mnem, ops_s):
    if "%rip" in ops_s:
        return False
    if mnem[0] == "j" or mnem.startswith(("call", "loop", "ret", "syscall", "sysenter", "int", "ud2", "hlt")):
        return False
    return True


def classify(prev):
    for i, (addr, n, mnem, ops, raw) in enumerate(prev):
        w = rax_write(mnem, ops)
        if w is None:
            continue
        if w == "imm":
            return 1 if (i == 0 and n == 5 and raw.startswith("b8")) else 2
        return 3 if w == "dyn" else 4
    return 4


def reloc(prev, follow, site_addr, targets):
    """Smallest PI window >= 5 bytes containing the site; returns 'fwd', 'bwd' or None."""
    for tot in range(0, 7):
        for p in range(0, 4):
            f = tot - p
            if f < 0 or f > 3 or p > len(prev) or f > len(follow):
                continue
            before = prev[:p][::-1]
            after = follow[:f]
            size = sum(x[1] for x in before) + 2 + sum(x[1] for x in after)
            if size < 5:
                continue
            if not all(is_pi(x[2], x[3]) for x in before + after):
                continue
            starts = [x[0] for x in before[1:]] + ([site_addr] if before else []) + [x[0] for x in after]
            if any(a in targets for a in starts):
                continue
            return "bwd" if p else "fwd"
    return None


def analyse(path):
    with open(path, "rb") as fh:
        data = fh.read()
    sha = hashlib.sha1(data).hexdigest()
    size = len(data)
    if not (b"\x0f\x05" in data or b"\x0f\x34" in data or b"\xcd\x80" in data):
        return {"sha": sha, "size": size, "sites": [], "kinds": {}}
    del data
    secs = []
    h = subprocess.run(["objdump", "-h", path], capture_output=True, text=True, errors="replace").stdout
    for line in h.splitlines():
        m = SEC_RE.match(line)
        if m:
            secs.append((int(m.group(3), 16), int(m.group(2), 16), int(m.group(4), 16)))  # vma, size, fileoff

    def fileoff(addr):
        for vma, sz, off in secs:
            if vma <= addr < vma + sz:
                return addr - vma + off
        return -1

    proc = subprocess.Popen(["objdump", "-d", "-w", path], stdout=subprocess.PIPE, text=True, errors="replace",
                            bufsize=1 << 20)
    prev = deque(maxlen=WIN)
    pending, done, targets = [], [], set()
    for line in proc.stdout:
        c = line[:1]
        if c != " ":
            if c.isalnum():  # function header or "Disassembly of section"
                prev.clear()
            continue
        a, sep, rest = line.partition(":\t")
        if not sep:
            continue
        b, sep, txt = rest.partition("\t")
        if not sep:
            continue
        try:
            addr = int(a, 16)
        except ValueError:
            continue
        raw = b.strip()
        n = (len(raw) + 1) // 3
        toks = txt.split(None, 1)
        while toks and toks[0] in PREFIXES and len(toks) > 1:
            toks = toks[1].split(None, 1)
        if not toks:
            continue
        mnem = toks[0]
        ops = toks[1].strip() if len(toks) > 1 else ""
        ins = (addr, n, mnem, ops, raw)
        for s in pending:
            s["follow"].append(ins)
        while pending and len(pending[0]["follow"]) >= 3:
            done.append(pending.pop(0))
        if mnem[0] == "j" or mnem.startswith(("call", "loop")):
            m = TARGET_RE.match(ops)
            if m:
                targets.add(int(m.group(1), 16))
        kind = None
        if mnem == "syscall":
            kind = "syscall"
        elif mnem == "sysenter":
            kind = "sysenter"
        elif mnem == "int" and ops.startswith("$0x80"):
            kind = "int80"
        if kind:
            pending.append({"addr": addr, "kind": kind, "prev": list(reversed(prev)), "follow": []})
        prev.append(ins)
    proc.wait()
    done.extend(pending)
    sites, kinds = [], {}
    for s in done:
        kinds[s["kind"]] = kinds.get(s["kind"], 0) + 1
        if s["kind"] != "syscall":
            continue
        cls = classify(s["prev"])
        r = reloc(s["prev"], s["follow"], s["addr"], targets)
        sites.append((fileoff(s["addr"]), cls, r or ""))
    return {"sha": sha, "size": size, "sites": sites, "kinds": kinds}


def is_elf64(path):
    try:
        with open(path, "rb") as f:
            h = f.read(20)
    except OSError:
        return False
    return len(h) == 20 and h[:4] == b"\x7fELF" and h[4] == 2 and h[5] == 1 and h[18] == 62


def main():
    root, outp = sys.argv[1], sys.argv[2]
    paths, seen = [], set()
    for dp, dns, fns in os.walk(root):
        if dp == root:
            dns[:] = [d for d in dns if d not in ("proc", "sys", "dev")]
        for fn in fns:
            p = os.path.join(dp, fn)
            try:
                st = os.lstat(p)
            except OSError:
                continue
            if os.path.islink(p) or not os.path.isfile(p) or st.st_size < 64 or (st.st_dev, st.st_ino) in seen:
                continue
            if is_elf64(p):
                seen.add((st.st_dev, st.st_ino))
                paths.append(p)
    with Pool(os.cpu_count() or 2) as pool:
        results = pool.map(analyse, paths, chunksize=1)
    files, sites_by_sha, path_sha, by_sha = [], {}, {}, {}
    for p, r in zip(paths, results):
        rel = "/" + os.path.relpath(p, root).replace(os.sep, "/")
        sha = r["sha"]
        path_sha[rel] = sha
        if sha in by_sha:
            by_sha[sha]["paths"].append(rel)
            continue
        cnt = {"1": 0, "2": 0, "3": 0, "4": 0}
        rel5 = {"1": 0, "2": 0, "3": 0, "4": 0}
        for off, cls, rl in r["sites"]:
            cnt[str(cls)] += 1
            rel5[str(cls)] += bool(rl)
        e = {"paths": [rel], "sha": sha, "size": r["size"], "classes": cnt, "reloc5": rel5,
             "other_insns": {k: v for k, v in r["kinds"].items() if k != "syscall"}}
        by_sha[sha] = e
        files.append(e)
        sites_by_sha[sha] = r["sites"]
    with open(outp + ".json", "w") as f:
        json.dump({"root": root, "elf_paths": len(paths), "unique_elfs": len(files), "files": files}, f)
    with gzip.open(outp + ".sites.json.gz", "wt") as f:
        json.dump({"path_sha": path_sha, "sites": sites_by_sha}, f)
    tot = {k: sum(e["classes"][k] for e in files) for k in "1234"}
    print("elf paths", len(paths), "unique", len(files), "sites by class", tot)


if __name__ == "__main__":
    main()
