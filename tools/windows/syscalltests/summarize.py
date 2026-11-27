#!/usr/bin/env python3
"""summarize.py LINUX_DIR WINDOWS_DIR OUT_DIR: merges the shard results of the
Linux (runsc systrap) baseline and of the Windows run, and writes
OUT/summary.md, OUT/per-binary.csv, OUT/per-case.csv and OUT/windows-only.csv.

A "Windows-only failure" is a case that passed (or was skipped) on Linux and
did not on Windows. Cases that fail on both are upstream/harness failures and
are the baseline."""
import collections, csv, glob, os, re, sys

lin_dir, win_dir, out = sys.argv[1:4]
os.makedirs(out, exist_ok=True)
OK = ('PASS', 'SKIP')


def load(d):
    res, tails = {}, {}
    for f in glob.glob(os.path.join(d, '**', '*.tsv'), recursive=True):
        for l in open(f, encoding='utf-8', errors='replace'):
            p = l.rstrip('\n').split('\t')
            if len(p) >= 5:
                res[(p[0], p[1])] = (p[2], p[3], p[4])
        o = f + '.out'
        if os.path.exists(o):
            for l in open(o, encoding='utf-8', errors='replace'):
                p = l.rstrip('\n').split('\t', 2)
                if len(p) == 3:
                    tails[(p[0], p[1])] = p[2]
    return res, tails


lin, lin_t = load(lin_dir)
win, win_t = load(win_dir)
keys = sorted(set(lin) | set(win))


def st(r, k):
    return r.get(k, ('MISSING', '', ''))[0]


# Causes, matched against the failure output of the Windows case.
CAUSES = [
    ('no network stack in the harness (socket/AF_INET/netlink)', r'Address family not supported|socket\(.*(AF_INET|AF_NETLINK|AF_PACKET)|Protocol not supported|ENETUNREACH|Network is unreachable|socket.*Operation not supported'),
    ('syscall not implemented in the Windows build (ENOSYS)', r'Function not implemented|ENOSYS'),
    ('/proc, /sys or /dev entry missing or different', r'/proc/|/sys/|/dev/(?!null|zero|full|random|urandom)'),
    ('guest crash (signal)', r'SIGSEGV|SIGBUS|SIGILL|SIGABRT|SIGFPE|signal'),
    ('time or clock differences', r'clock_|CLOCK_|timer|nanosleep|gettimeofday'),
    ('ptrace/debug/seccomp/prctl', r'ptrace|seccomp|prctl|PTRACE'),
    ('mmap/madvise/memory behaviour', r'mmap|munmap|madvise|mprotect|mremap|MAP_|brk|mlock|memfd|hugepage'),
    ('file system behaviour (tmpfs/overlay/xattr/locks)', r'xattr|flock|fcntl|inotify|fallocate|sendfile|splice|O_DIRECT|statfs|mount'),
    ('signals and threads (futex, clone, tgkill, sigaltstack)', r'futex|clone|tgkill|sigaltstack|sigprocmask|rt_sig|pthread|setitimer'),
    ('credentials, namespaces, capabilities', r'setuid|setgid|setgroups|capset|CAP_|unshare|setns|userns|EPERM|Operation not permitted'),
]


def cause(k):
    s = win.get(k, ('MISSING',))[0]
    if s in ('CRASH', 'HANG', 'HARNESS'):
        t0 = win_t.get(k, '')
        m = re.search(r'(panic: [^\\]*|fatal error: [^\\]*|sentryguest: FAIL[^\\]*)', t0)
        what = re.sub(r'0x[0-9a-f]+|\d{3,}', 'N', m.group(1))[:120] if m else 'no message'
        return '%s: %s' % ({'CRASH': 'Sentry crash', 'HANG': 'hang that stalled the kernel', 'HARNESS': 'kernel did not start'}[s], what)
    if s == 'TIMEOUT':
        return 'timeout (case did not finish in time)'
    t = win_t.get(k, '')
    # The first failed check (file:line and what it says) is the signature.
    m = re.search(r'(\S+\.cc:\d+): Failure\\n([^\\]*(?:\\n[^\\]*)?)', t)
    where = ''
    if m:
        where = '%s %s' % (m.group(1), re.sub(r'\s+', ' ', re.sub(r'\d{3,}', 'N', m.group(2)))[:110])
    if s == 'SIGNAL':
        return 'killed by signal %s%s' % (win.get(k, ('', '?', ''))[1], (' after ' + where) if where else ' in ' + k[0])
    if where:
        return where
    for name, rx in CAUSES:
        if re.search(rx, t):
            return name
    return 'other: exit code ' + win.get(k, ('', '?'))[1] + ' in ' + k[0]


by_bin = collections.defaultdict(lambda: collections.Counter())
rows = []
wonly = []
for k in keys:
    l, w = st(lin, k), st(win, k)
    rows.append((k[0], k[1], l, w))
    by_bin[k[0]]['cases'] += 1
    by_bin[k[0]]['lin_ok'] += l in OK
    by_bin[k[0]]['lin_pass'] += l == 'PASS'
    by_bin[k[0]]['win_ok'] += w in OK
    by_bin[k[0]]['win_pass'] += w == 'PASS'
    if l in OK and w not in OK:
        wonly.append(k)
        by_bin[k[0]]['win_only_fail'] += 1

with open(os.path.join(out, 'per-case.csv'), 'w', newline='') as f:
    w_ = csv.writer(f)
    w_.writerow(['binary', 'test', 'linux', 'windows'])
    w_.writerows(rows)
with open(os.path.join(out, 'per-binary.csv'), 'w', newline='') as f:
    w_ = csv.writer(f)
    w_.writerow(['binary', 'cases', 'linux_pass', 'windows_pass', 'windows_only_failures'])
    for b in sorted(by_bin):
        c = by_bin[b]
        w_.writerow([b, c['cases'], c['lin_pass'], c['win_pass'], c['win_only_fail']])
groups = collections.Counter()
members = collections.defaultdict(list)
for k in wonly:
    g = cause(k)
    groups[g] += 1
    members[g].append(k)
with open(os.path.join(out, 'windows-only.csv'), 'w', newline='') as f:
    w_ = csv.writer(f)
    w_.writerow(['binary', 'test', 'linux', 'windows', 'cause', 'output'])
    for k in wonly:
        w_.writerow([k[0], k[1], st(lin, k), st(win, k), cause(k), win_t.get(k, '')[:400]])


def count(r, s):
    return sum(1 for v in r.values() if v[0] in s)


n = len(keys)
lin_pass, win_pass = count(lin, ('PASS',)), count(win, ('PASS',))
md = []
md.append('## Syscall tests: Windows port against runsc (systrap) on Linux\n')
md.append('| | Linux runsc | Windows Sentry |')
md.append('|---|---|---|')
md.append('| test cases run | %d | %d |' % (len(lin), len(win)))
md.append('| PASS | %d (%.1f%%) | %d (%.1f%%) |' % (lin_pass, 100.0 * lin_pass / max(n, 1), win_pass, 100.0 * win_pass / max(n, 1)))
md.append('| SKIP | %d | %d |' % (count(lin, ('SKIP',)), count(win, ('SKIP',))))
for s in ('FAIL', 'TIMEOUT', 'SIGNAL', 'CRASH', 'HANG', 'HARNESS', 'NOTRUN'):
    md.append('| %s | %d | %d |' % (s, count(lin, (s,)), count(win, (s,))))
md.append('')
md.append('Windows-only failures (passed or skipped on Linux, not on Windows): **%d** of %d cases; Windows pass rate relative to the Linux baseline (cases that pass on Windows / cases that pass on Linux): **%.1f%%**.\n'
          % (len(wonly), n, 100.0 * sum(1 for k in keys if st(win, k) == 'PASS' and st(lin, k) == 'PASS') / max(lin_pass, 1)))
md.append('### Windows-only failures by cause (top 10)\n')
md.append('| cases | cause | examples |')
md.append('|---|---|---|')
for g, c in groups.most_common(10):
    ex = ', '.join('%s %s' % k for k in members[g][:2])
    md.append('| %d | %s | %s |' % (c, g, ex.replace('|', '/')))
md.append('')
md.append('### Binaries with the most Windows-only failures\n')
md.append('| binary | cases | Linux pass | Windows pass | Windows-only failures |')
md.append('|---|---|---|---|---|')
for b, c in sorted(by_bin.items(), key=lambda kv: -kv[1]['win_only_fail'])[:25]:
    md.append('| %s | %d | %d | %d | %d |' % (b, c['cases'], c['lin_pass'], c['win_pass'], c['win_only_fail']))
md.append('')
full = [b for b, c in by_bin.items() if c['win_pass'] == c['cases']]
md.append('Binaries where every case passes on Windows: %d of %d.\n' % (len(full), len(by_bin)))
open(os.path.join(out, 'summary.md'), 'w').write('\n'.join(md) + '\n')
print('\n'.join(md))
