#!/usr/bin/env python3
"""gtest_list.py OUT: runs every test binary of OUT/bin with --gtest_list_tests
(natively; they are static) and writes OUT/list.txt with "binary<TAB>Suite.Case"
lines."""
import os, subprocess, sys

out = sys.argv[1]
bindir = os.path.join(out, 'bin')
tests = [l.strip() for l in open(os.path.join(out, 'tests.txt')) if l.strip()]
lines, failed = [], []
for name in tests:
    exe = os.path.join(bindir, name)
    if not os.path.exists(exe):
        continue
    try:
        r = subprocess.run([exe, '--gtest_list_tests'], capture_output=True, text=True, timeout=60)
    except Exception as e:
        failed.append((name, str(e)))
        continue
    if r.returncode != 0:
        failed.append((name, 'exit %d' % r.returncode))
        continue
    suite = ''
    for l in r.stdout.splitlines():
        if not l.strip():
            continue
        l = l.split('#')[0].rstrip()
        if not l.startswith(' '):
            suite = l.strip()
        else:
            case = l.strip()
            if case.startswith('DISABLED_') or suite.startswith('DISABLED_'):
                continue
            lines.append('%s\t%s%s' % (name, suite, case))
with open(os.path.join(out, 'list.txt'), 'w') as f:
    f.write('\n'.join(lines) + '\n')
with open(os.path.join(out, 'list-failed.txt'), 'w') as f:
    f.write('\n'.join('%s\t%s' % x for x in failed) + '\n')
print('%d binaries listed, %d test cases, %d binaries failed to list: %s' % (len(tests) - len(failed), len(lines), len(failed), failed[:10]))
