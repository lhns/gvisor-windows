#!/usr/bin/env python3
"""mkshards.py OUT N: splits OUT/list.txt into N shards balanced by test count,
each with a root file system tar (the shard's binaries, the driver, busybox,
/etc, /tmp) in OUT/shards/<i>/."""
import collections, io, os, sys, tarfile

out, n = sys.argv[1], int(sys.argv[2])
per = collections.OrderedDict()
for l in open(os.path.join(out, 'list.txt')):
    l = l.rstrip('\n')
    if l:
        per.setdefault(l.split('\t')[0], []).append(l)
shards = [[0, []] for _ in range(n)]
for name, ls in sorted(per.items(), key=lambda kv: -len(kv[1])):
    s = min(shards, key=lambda s: s[0])
    s[0] += len(ls) + 3
    s[1].append(name)
hp = os.path.join(out, 'helpers.txt')
helpers = [l.strip() for l in open(hp)] if os.path.exists(hp) else []


def norm(ti):
    ti.mode |= 0o111 if ti.mode & 0o100 else 0
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = ''
    return ti


for i, (cost, names) in enumerate(shards):
    d = os.path.join(out, 'shards', str(i))
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, 'list.txt'), 'w') as f:
        for name in sorted(names):
            f.write('\n'.join(per[name]) + '\n')
    with tarfile.open(os.path.join(d, 'rootfs.tar'), 'w') as tf:
        def add_dir(p, mode=0o755):
            ti = tarfile.TarInfo(p)
            ti.type = tarfile.DIRTYPE
            ti.mode = mode
            tf.addfile(ti)

        def add_file(p, src=None, data=None, mode=0o755):
            if src is not None:
                tf.add(src, arcname=p, filter=norm)
            else:
                ti = tarfile.TarInfo(p)
                ti.size = len(data)
                ti.mode = mode
                tf.addfile(ti, io.BytesIO(data))

        for d_ in ['bin', 'tmp', 'etc', 'runfiles', 'runfiles/test', 'runfiles/test/syscalls', 'runfiles/test/syscalls/linux',
                   'proc', 'sys', 'dev', 'var', 'var/tmp', 'root', 'home']:
            add_dir(d_, 0o1777 if d_ in ('tmp', 'var/tmp') else 0o755)
        for name in names:
            add_file('bin/' + name, src=os.path.join(out, 'bin', name))
        for h in helpers:
            p = os.path.join(out, 'bin', h)
            if os.path.exists(p):
                add_file('bin/' + h, src=p)
                add_file('runfiles/test/syscalls/linux/' + h, src=p)
        add_file('bin/driver', src=os.path.join(out, 'driver'))
        add_file('bin/busybox', src=os.path.join(out, 'busybox'))
        for a in ['sh', 'cat', 'ls', 'rm', 'true', 'false', 'sleep', 'echo', 'mkdir', 'cp', 'kill', 'ps', 'id']:
            ti = tarfile.TarInfo('bin/' + a)
            ti.type = tarfile.SYMTYPE
            ti.linkname = 'busybox'
            tf.addfile(ti)
        add_file('etc/passwd', data=b'root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/bin/false\n', mode=0o644)
        add_file('etc/group', data=b'root:x:0:\nnogroup:x:65534:\n', mode=0o644)
        add_file('etc/hosts', data=b'127.0.0.1 localhost\n', mode=0o644)
    print('shard %d: %d binaries, %d cases, tar %d MB' % (i, len(names), sum(len(per[x]) for x in names), os.path.getsize(os.path.join(d, 'rootfs.tar')) >> 20))
