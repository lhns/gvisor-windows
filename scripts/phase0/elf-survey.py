#!/usr/bin/env python3
"""Stage D: scan every ELF in an image rootfs tar (stdin, e.g. `docker export`) for PT_LOAD segments
that a 64 KiB-granularity host cannot map 1:1.

Per ELF we count PT_LOAD segments with
  - (p_vaddr - p_offset) % 64K != 0   (file offset and address disagree modulo 64K: a window cannot be
    one contiguous file range)
  - p_vaddr % 64K != 0                (segment start not 64K aligned)
Usage: elf-survey.py <image-name> < rootfs.tar   -> prints one JSON line
"""
import json, struct, sys, tarfile

K64 = 65536
PT_LOAD = 1


def parse(head):
    if head[:4] != b"\x7fELF" or len(head) < 64:
        return None
    cls, data = head[4], head[5]
    e = "<" if data == 1 else ">"
    if cls == 2:
        phoff, = struct.unpack_from(e + "Q", head, 0x20)
        phentsize, phnum = struct.unpack_from(e + "HH", head, 0x36)
    elif cls == 1:
        phoff, = struct.unpack_from(e + "I", head, 0x1C)
        phentsize, phnum = struct.unpack_from(e + "HH", head, 0x2A)
    else:
        return None
    return cls, e, phoff, phentsize, phnum


def segments(f, size):
    head = f.read(64)
    p = parse(head)
    if not p:
        return None
    cls, e, phoff, phentsize, phnum = p
    need = phoff + phentsize * phnum
    if need > size or phnum == 0:
        return []
    rest = f.read(need - 64)
    buf = head + rest
    out = []
    for i in range(phnum):
        o = phoff + i * phentsize
        if cls == 2:
            t, fl, off, va, pa, fsz, msz, al = struct.unpack_from(e + "IIQQQQQQ", buf, o)
        else:
            t, off, va, pa, fsz, msz, fl, al = struct.unpack_from(e + "IIIIIIII", buf, o)
        if t == PT_LOAD:
            out.append((off, va, al))
    return out


def main():
    name = sys.argv[1]
    elfs = files_off = files_va = loads = off_bad = va_bad = 0
    aligns = {}
    seen = set()
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as tf:
        for m in tf:
            if not m.isreg() or m.size < 64:
                continue
            f = tf.extractfile(m)
            segs = segments(f, m.size)
            if segs is None:
                continue
            elfs += 1
            bo = bv = 0
            for off, va, al in segs:
                loads += 1
                aligns[al] = aligns.get(al, 0) + 1
                if (va - off) % K64:
                    bo += 1
                if va % K64:
                    bv += 1
            off_bad += bo; va_bad += bv
            files_off += bool(bo); files_va += bool(bv)
    pct = lambda a, b: round(100.0 * a / b, 1) if b else 0.0
    print(json.dumps({
        "image": name, "elf_files": elfs, "pt_load": loads,
        "load_offset_vaddr_mismatch_64k": off_bad, "load_vaddr_not_64k_aligned": va_bad,
        "files_with_mismatch": files_off, "files_with_mismatch_pct": pct(files_off, elfs),
        "files_with_unaligned_vaddr": files_va, "files_with_unaligned_vaddr_pct": pct(files_va, elfs),
        "pt_load_mismatch_pct": pct(off_bad, loads), "pt_load_unaligned_pct": pct(va_bad, loads),
        "p_align_histogram": {str(k): v for k, v in sorted(aligns.items())},
    }))


main()
