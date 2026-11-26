#!/usr/bin/env python3
"""run_shard.py --list L --out R (--sentryguest EXE --tar T | --docker IMAGE --runtime RT)

Runs one shard of the syscall tests under a guest kernel and writes R, a
tab-separated file with one line per test case:
    binary test status code ms
Statuses are the driver's (PASS FAIL SKIP TIMEOUT SIGNAL NOTRUN) plus
CRASH (the Sentry or the sandbox died while the case ran) and HANG (no progress
for --stall seconds). After a crash the rest of the list runs in a fresh kernel.
Failure output tails go to R.out (case TAB escaped tail)."""
import argparse, os, subprocess, sys, tempfile, time


def esc(b):
    """The tail of some output as one line without tabs."""
    s = b.decode('utf-8', 'replace')
    return s.replace(chr(13), '').replace(chr(10), chr(92) + 'n').replace(chr(9), ' ')


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--list', required=True)
    ap.add_argument('--out', required=True)
    ap.add_argument('--sentryguest')
    ap.add_argument('--tar')
    ap.add_argument('--docker')
    ap.add_argument('--runtime', default='runsc')
    ap.add_argument('--case-timeout', type=int, default=60)
    ap.add_argument('--stall', type=int, default=240)
    ap.add_argument('--max-runs', type=int, default=60)
    a = ap.parse_args()

    cases = [tuple(l.rstrip('\n').split('\t', 1)) for l in open(a.list) if l.strip()]
    done = {}
    tails = {}
    workdir = tempfile.mkdtemp()
    t_start = time.time()
    idle_crashes = 0
    for run in range(a.max_runs):
        todo = [c for c in cases if c not in done]
        if not todo:
            break
        lst = os.path.join(workdir, 'list%d.txt' % run)
        with open(lst, 'w') as f:
            f.write(''.join('%s\t%s\n' % c for c in todo))
        log = os.path.join(workdir, 'run%d.log' % run)
        if a.sentryguest:
            cmd = [a.sentryguest, '-tar', a.tar, '-sysmounts', '-stream', '-timeout', '6h',
                   '-files', '%s:/list.txt' % lst,
                   '/bin/driver', '-t', str(a.case_timeout), '/list.txt']
        else:
            cmd = ['docker', 'run', '--rm', '--runtime', a.runtime, '--network', 'none',
                   '-v', '%s:/list.txt:ro' % lst, a.docker, '/bin/driver', '-t', str(a.case_timeout), '/list.txt']
        print('run %d: %d cases left' % (run, len(todo)), flush=True)
        with open(log, 'wb') as lf:
            proc = subprocess.Popen(cmd, stdout=lf, stderr=subprocess.STDOUT)
            last_size, last_change = 0, time.time()
            while proc.poll() is None:
                time.sleep(5)
                size = os.path.getsize(log)
                if size != last_size:
                    last_size, last_change = size, time.time()
                elif time.time() - last_change > a.stall:
                    print('run %d: no output for %ds, killing' % (run, a.stall), flush=True)
                    if os.name == 'nt':
                        subprocess.run(['taskkill', '/F', '/T', '/PID', str(proc.pid)], capture_output=True)
                    else:
                        proc.kill()
                    proc.wait()
                    break
        finished, last = False, None
        before = len(done)
        with open(log, 'rb') as lf:
            for raw in lf:
                line = raw.decode('utf-8', 'replace').rstrip('\r\n')
                if line.startswith('R\t'):
                    p = line.split('\t')
                    if len(p) >= 6:
                        last = (p[1], p[2])
                        done[last] = (p[3], p[4], p[5])
                elif line.startswith('O\t') and last is not None:
                    tails[last] = line[2:]
                elif line.startswith('DONE\t'):
                    finished = True
        if finished:
            continue
        if len(done) == before:
            idle_crashes += 1
        else:
            idle_crashes = 0
        if idle_crashes >= 6:
            # The harness itself fails before any case runs: give up on the shard.
            tail = esc(open(log, "rb").read()[-1500:])
            for c in cases:
                if c not in done:
                    done[c] = ('HARNESS', '-1', '0')
                    tails[c] = tail
            print('run %d: the kernel fails to start the driver, giving up' % run, flush=True)
            break
        # The kernel died or hung: the first case not yet reported is the culprit.
        rest = [c for c in cases if c not in done]
        if rest:
            culprit = rest[0]
            tail = ''
            try:
                tail = open(log, 'rb').read()[-1500:].decode('utf-8', 'replace').replace('\r', '').replace('\n', '\\n').replace('\t', ' ')
            except Exception:
                pass
            status = 'HANG' if (time.time() - last_change) > a.stall else 'CRASH'
            done[culprit] = (status, '-1', '0')
            tails[culprit] = tail
            print('run %d: %s on %s %s' % (run, status, culprit[0], culprit[1]), flush=True)
    with open(a.out, 'w') as f:
        for c in cases:
            st = done.get(c, ('NOTRUN', '-1', '0'))
            f.write('%s\t%s\t%s\t%s\t%s\n' % (c[0], c[1], st[0], st[1], st[2]))
    with open(a.out + '.out', 'w') as f:
        for c, t in tails.items():
            f.write('%s\t%s\t%s\n' % (c[0], c[1], t))
    counts = {}
    for st in done.values():
        counts[st[0]] = counts.get(st[0], 0) + 1
    print('shard done in %ds: %s' % (time.time() - t_start, counts), flush=True)


main()
