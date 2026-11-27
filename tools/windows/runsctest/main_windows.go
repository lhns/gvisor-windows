// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

// Command runsctest drives runsc.exe on a Windows runner, as whatever user it
// runs as (CI starts it as a standard user): it writes OCI bundles around a
// rootfs folder holding busybox and a glibc hello, runs them with
// "runsc run" and checks stdout, exit codes, stdin, the windows-nat network, a
// published port and a terminal (under a ConPTY the harness creates).
//
//	runsctest -runsc runsc.exe -rootfs dir -work dir [-only name,...]
package main

import (
	"strconv"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	runscExe = flag.String("runsc", "runsc.exe", "runsc.exe")
	rootfs   = flag.String("rootfs", "", "rootfs folder (bin/busybox, bin/hello, ...)")
	workDir  = flag.String("work", "", "writable folder for bundles and logs")
	image    = flag.String("image", "", "erofs image of the same rootfs (enables the erofs test)")
	only     = flag.String("only", "", "comma-separated test names (default all)")
	timeout  = flag.Duration("timeout", 120*time.Second, "per test")
	stressWorkers = flag.Int("stress-workers", 6, "fsstress: parallel mount namespaces")
	stressStrace  = flag.Bool("stress-strace", false, "fsstress: run with runsc --strace")
	stressIters   = flag.Int("stress-iters", 600, "fsstress: iterations per worker")
	winstubDebug  = flag.Bool("winstub-debug", false, "WINSTUB_DEBUG=1 for runsc (patching log lines in stderr)")
	gcSecs        = flag.Int("gc-secs", 20, "gogc: seconds to run")
	fsMode   = flag.String("fsmode", "", "WINSTUB_FSMODE for runsc (native or rewrite)")
	fsLog    = flag.Bool("fslog", false, "WINSTUB_FSLOG=1 for runsc (threads moved out of %fs veneers, in stderr)")
	fsUD2    = flag.Bool("fsud2", false, "WINSTUB_FSUD2=1 for runsc (every %fs instruction through the debugger)")
	fpMode   = flag.String("fpmode", "", "WINSTUB_FPMODE for runsc (fxsave)")
)

type result struct {
	stdout, stderr string
	code           int
}

func main() {
	flag.Parse()
	// (the CI runs this as another user, whose environment cannot be set from outside)
	if *winstubDebug {
		os.Setenv("WINSTUB_DEBUG", "1")
	}
	os.Setenv("WINSTUB_TRIPWIRE", "1") // a syscall that runs on Windows fails the test
	if *fsMode != "" {
		os.Setenv("WINSTUB_FSMODE", *fsMode)
	}
	if *fsLog {
		os.Setenv("WINSTUB_FSLOG", "1")
	}
	if *fsUD2 {
		os.Setenv("WINSTUB_FSUD2", "1")
	}
	if *fpMode != "" {
		os.Setenv("WINSTUB_FPMODE", *fpMode)
	}
	if *rootfs == "" || *workDir == "" {
		fmt.Fprintln(os.Stderr, "usage: runsctest -rootfs dir -work dir [-runsc exe] [-only names]")
		os.Exit(2)
	}
	*runscExe, _ = filepath.Abs(*runscExe)
	*rootfs, _ = filepath.Abs(*rootfs)
	*workDir, _ = filepath.Abs(*workDir)
	must(os.MkdirAll(filepath.Join(*workDir, "logs"), 0o755))

	tests := []struct {
		name string
		fn   func() error
	}{
		{"hello", testHello},
		{"busybox", testBusybox},
		{"stdin", testStdin},
		{"fchmod", testFchmod},
		{"strace", testStrace},
		{"restart", testRestart},
		{"net", testNet},
		{"publish", testPublish},
		{"tty", testTTY},
		{"hostfs", testHostfs},
		{"case", testCase},
		{"lifecycle", testLifecycle},
		{"sockfs", testSockfs},
		{"fsstress", testFsStress},
		{"aptrename", testAptRename},
		{"mountkeep", testMountKeep},
		{"devnull", testDevNull},
		{"ovlsub", testOvlSub},
		{"ovlopaque", testOvlOpaque},
		{"cfr", testCfr},
		{"debian", testDebian},
		{"gogc", testGoGC},
		{"bench", testBench},
		{"forward", testForward},
		{"mounts", func() error { return profileRun("mounts", "cat /proc/self/mountinfo; pwd; ls -la /tmp") }},
		{"untar-trace", func() error { return profileRun("untar-trace", "mkdir -p /tmp/u && tar -xf /small.tar -C /tmp/u ./pkg0 ./pkg1") }},
		{"noop", func() error { return profileRun("noop", "/bin/true") }},
		{"untar", func() error { return profileRun("untar", "mkdir -p /tmp/u && tar -xf /small.tar -C /tmp/u") }},
		{"forkexec", func() error { return profileRun("forkexec", "i=0; while [ $i -lt 100 ]; do /bin/true; i=$((i+1)); done") }},
		// Not in the default run: fails today (docs/results/3-runsc-boot.md).
		{"erofs", testErofs},
	}
	want := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n != "" {
			want[n] = true
		}
	}
	failed := 0
	for _, t := range tests {
		if (len(want) > 0 && !want[t.name]) || (len(want) == 0 && (t.name == "erofs" || t.name == "hostfs" || t.name == "case" || t.name == "strace" || t.name == "restart" || t.name == "aptrename" || t.name == "mountkeep" || t.name == "devnull" || t.name == "ovlsub" || t.name == "ovlopaque" || t.name == "lifecycle" || t.name == "bench" || t.name == "noop" || t.name == "mounts" || t.name == "untar-trace" || t.name == "untar" || t.name == "forkexec" || t.name == "sockfs" || t.name == "fsstress" || t.name == "cfr" || t.name == "debian" || t.name == "gogc")) {
			continue
		}
		start := time.Now()
		fmt.Printf("=== RUN  %s\n", t.name)
		if err := t.fn(); err != nil {
			failed++
			fmt.Printf("--- FAIL %s (%v): %v\n", t.name, time.Since(start).Round(time.Millisecond), err)
			if b, err := os.ReadFile(filepath.Join(*workDir, "logs", t.name+".log")); err == nil {
				lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
				if keep := 40 + 100*len(stressExtra); len(lines) > keep {
					lines = lines[len(lines)-keep:]
				}
				fmt.Printf("--- tail of the runsc debug log:\n%s\n", strings.Join(lines, "\n"))
			}
		} else {
			fmt.Printf("--- PASS %s (%v)\n", t.name, time.Since(start).Round(time.Millisecond))
		}
	}
	if failed > 0 {
		fmt.Printf("runsctest: %d test(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Println("runsctest: PASS")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "runsctest:", err)
		os.Exit(2)
	}
}

// bundle writes an OCI bundle for args and returns its folder.
func bundle(name string, args []string, terminal bool) string {
	rootPath := *rootfs
	if name == "erofs" {
		rootPath = *image
	}
	dir := filepath.Join(*workDir, "bundle-"+name)
	must(os.MkdirAll(dir, 0o755))
	mounts := []map[string]any{}
	mounts = append(mounts, extraMounts...)
	mounts = append(mounts, []map[string]any{
		{"destination": "/proc", "type": "proc", "source": "proc"},
		{"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs"},
	}...)
	spec := map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"terminal": terminal,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"args":     args,
			"env":      []string{"PATH=/bin", "HOME=/", "TERM=xterm"},
			"cwd":      "/",
		},
		"root":     map[string]any{"path": rootPath, "readonly": false},
		"hostname": "runsc-win",
		"mounts":   mounts,
		// The files were made by Windows tools and have no Linux modes.
		"annotations": map[string]string{"dev.gvisor.windows.rootfs-options": rootfsOptions()},
		"linux": map[string]any{"namespaces": []map[string]string{
			{"type": "pid"}, {"type": "network"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"},
		}, "devices": []map[string]any{
			{"path": "/dev/null", "type": "c", "major": 1, "minor": 3, "fileMode": 438},
			{"path": "/dev/zero", "type": "c", "major": 1, "minor": 5, "fileMode": 438},
			{"path": "/dev/urandom", "type": "c", "major": 1, "minor": 9, "fileMode": 438},
		}},
	}
	if len(extraCaps) > 0 {
		spec["process"].(map[string]any)["capabilities"] = map[string]any{"bounding": extraCaps, "effective": extraCaps, "permitted": extraCaps, "inheritable": extraCaps}
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(dir, "config.json"), b, 0o644))
	return dir
}

func runscArgs(name, dir string, extra ...string) []string {
	logs := filepath.Join(*workDir, "logs", name+".log")
	a := []string{"--debug", "--debug-log=" + logs}
	a = append(a, extra...)
	return append(a, "run", "-bundle", dir, "ci-"+name)
}

// runsc runs "runsc run" and waits for it.
func runsc(name string, args []string, stdin io.Reader, extra ...string) (result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, *runscExe, runscArgs(name, bundle(name, args, false), extra...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &out, &errb, stdin
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.code = ee.ExitCode()
		err = nil
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return r, err
}

func expect(r result, err error, stdout string, code int) error {
	if err != nil {
		return fmt.Errorf("%v; stdout %q stderr %q", err, r.stdout, r.stderr)
	}
	if r.stdout != stdout || r.code != code {
		return fmt.Errorf("stdout %q exit %d, want stdout %q exit %d; stderr %q", r.stdout, r.code, stdout, code, r.stderr)
	}
	return nil
}

func testHello() error {
	r, err := runsc("hello", []string{"/bin/hello"}, nil)
	return expect(r, err, "hello from glibc: argc=1 tv=5 work=10 heap=ok\n", 7)
}

func testBusybox() error {
	r, err := runsc("busybox", []string{"/bin/sh", "-c", "echo hi; ls /; cat /etc/hostname; exit 3"}, nil)
	if err != nil {
		return err
	}
	// ls output depends on the rootfs; check the pieces.
	for _, want := range []string{"hi\n", "bin\n", "etc\n", "ciguest\n"} {
		if !strings.Contains(r.stdout, want) {
			return fmt.Errorf("stdout %q lacks %q (stderr %q)", r.stdout, want, r.stderr)
		}
	}
	if !strings.HasSuffix(r.stdout, "ciguest\n") || r.code != 3 {
		return fmt.Errorf("stdout %q exit %d (stderr %q), want hostname file last and exit 3", r.stdout, r.code, r.stderr)
	}
	return nil
}

func testErofs() error {
	if *image == "" {
		fmt.Println("no -image, skipped")
		return nil
	}
	*image, _ = filepath.Abs(*image)
	r, err := runsc("erofs", []string{"/bin/sh", "-c", "echo hi; cat /etc/hostname; echo x > /tmp/f && cat /tmp/f; exit 3"}, nil)
	return expect(r, err, "hi\nciguest\nx\n", 3)
}

func testStdin() error {
	r, err := runsc("stdin", []string{"/bin/cat"}, strings.NewReader("abc\n"))
	return expect(r, err, "abc\n", 0)
}

// hostIP is an address of this machine that is not loopback.
func hostIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 address")
}

func testNet() error {
	ip, err := hostIP()
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp4", ip+":0")
	if err != nil {
		return err
	}
	defer l.Close()
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "hello-from-host") }))
	url := "http://" + l.Addr().String() + "/"
	r, err := runsc("net", []string{"/bin/wget", "-q", "-O", "-", url}, nil, "--network=windows-nat")
	return expect(r, err, "hello-from-host", 0)
}

func testPublish() error {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	dir := bundle("publish", []string{"/bin/httpd", "-f", "-p", "8080", "-h", "/www"}, false)
	var errb bytes.Buffer
	cmd := exec.CommandContext(ctx, *runscExe, runscArgs("publish", dir, "--network=windows-nat", fmt.Sprintf("--publish=%d:8080", port))...)
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Kill() }()
	url := fmt.Sprintf("http://127.0.0.1:%d/index.html", port)
	var last error
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	for ctx.Err() == nil {
		select {
		case <-exited:
			return fmt.Errorf("runsc exited early (stderr %q)", errb.String())
		default:
		}
		resp, err := http.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && strings.TrimSpace(string(b)) == "hello-from-guest" {
				return nil
			}
			last = fmt.Errorf("status %d body %q", resp.StatusCode, b)
		} else {
			last = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("published port never answered: %v (runsc stderr %q)", last, errb.String())
}

var extraMounts []map[string]any

// extraCaps, when set, are the capabilities of the guest process (the default has no CAP_SYS_ADMIN).
var extraCaps []string

// stressExtra are extra runsc flags of the fsstress test (for example --strace).
var stressExtra []string

// testHostfs is the reproducer for the rename and rmdir failures Podman's
// storage hits on a host-folder bind mount ("rename .tmp-images.json
// images.json: permission denied", "remove /.pivot_root: permission denied").
func testHostfs() error {
	dir := filepath.Join(*workDir, "hostfs")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	extraMounts = []map[string]any{{"destination": "/w", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}}}
	defer func() { extraMounts = nil }()
	script := `cd /w; set -x; echo a >x; echo b >y; mv x y && echo replace-ok; cat y; mkdir d; mkdir d/e; rmdir d/e && echo rmdir-ok; mv d d2 && echo dirmove-ok; rmdir d2 && echo rmdir2-ok; echo c >z; rm z && echo rm-ok; mkfifo p; echo fifo-data >p & cat p; wait; echo fifo-ok; echo link-data >lt; ln -s /w/lt sl-abs; cat sl-abs && echo abs-symlink-ok; ln -s lt sl-rel; cat sl-rel && echo rel-symlink-ok; ln lt hl; cat hl && echo hardlink-ok; mkdir -p ld; ln -s /w/lt ld/00-a%3ab.deb; cat ld/00-a%3ab.deb && echo odd-name-symlink-ok; ln -s /w/lt /w/ld/abs2; cat /w/ld/abs2 && echo abs-in-dir-ok; mkdir -p arch/partial tmpd; echo deb >arch/partial/p%3a1.deb; mv arch/partial/p%3a1.deb arch/p%3a1.deb; ln arch/p%3a1.deb tmpd/00-p%3a1.deb; cat tmpd/00-p%3a1.deb && echo renamed-hardlink-ok; ln -s /w/arch/p%3a1.deb tmpd/01-p; cat tmpd/01-p && echo renamed-symlink-ok; echo dpkg-stat; ls -l tmpd; for n in q%3a1.deb plain.deb; do echo x >arch/partial/$n; chown 42:65534 arch/partial/$n; touch -d 2024-01-01 arch/partial/$n; chmod 0640 arch/partial/$n; mv arch/partial/$n arch/$n && echo apt-rename-ok; done; ls -ln arch; mkdir -p lists/partial; : >lists/partial/deb.debian.org_debian_dists_bookworm-updates_InRelease; chown 42:65534 lists/partial/deb.debian.org_debian_dists_bookworm-updates_InRelease; touch -d 2024-01-01 lists/partial/deb.debian.org_debian_dists_bookworm-updates_InRelease; mv lists/partial/deb.debian.org_debian_dists_bookworm-updates_InRelease lists/deb.debian.org_debian_dists_bookworm-updates_InRelease && echo apt-empty-rename-ok; echo data >lists/partial/a-b; mv lists/partial/a-b lists/a-b && echo hyphen-rename-ok; ls arch >/dev/null; [ -e arch/neg%3a1.deb ]; echo n >arch/partial/neg%3a1.deb; chown 0:0 arch/partial/neg%3a1.deb; mv arch/partial/neg%3a1.deb arch/neg%3a1.deb; ls arch >/dev/null; [ -e arch/neg%3a1.deb ] && echo neg-rename-ok; cat arch/neg%3a1.deb; exit 0`
	r, err := runsc("hostfs", []string{"/bin/sh", "-c", script}, nil)
	if err != nil {
		return err
	}
	for _, want := range []string{"replace-ok", "rmdir-ok", "dirmove-ok", "rmdir2-ok", "rm-ok", "fifo-data", "fifo-ok", "abs-symlink-ok", "rel-symlink-ok", "hardlink-ok", "odd-name-symlink-ok", "abs-in-dir-ok", "renamed-hardlink-ok", "renamed-symlink-ok", "apt-rename-ok", "apt-empty-rename-ok", "hyphen-rename-ok", "neg-rename-ok"} {
		if !strings.Contains(r.stdout, want) {
			return fmt.Errorf("%q missing; stdout %q stderr %q", want, r.stdout, r.stderr)
		}
	}
	return nil
}

// testCase checks that two names differing only in case coexist on a bind
// mount with the case-sensitive option (Podman's storage needs it).
func testCase() error {
	dir := filepath.Join(*workDir, "case")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	// What Windows itself says about per-directory case sensitivity for this user.
	probe := filepath.Join(*workDir, "case-probe")
	_ = os.RemoveAll(probe)
	must(os.MkdirAll(probe, 0o755))
	o, perr := exec.Command("fsutil", "file", "setCaseSensitiveInfo", probe, "enable").CombinedOutput()
	fmt.Println("fsutil setCaseSensitiveInfo as this user:", strings.TrimSpace(string(o)), perr)
	o, _ = exec.Command("fsutil", "file", "queryCaseSensitiveInfo", probe).CombinedOutput()
	fmt.Println("fsutil queryCaseSensitiveInfo:", strings.TrimSpace(string(o)))
	extraMounts = []map[string]any{{"destination": "/w", "type": "bind", "source": dir, "options": []string{"rbind", "rw", "case-sensitive"}}}
	defer func() { extraMounts = nil }()
	r, err := runsc("case", []string{"/bin/sh", "-c", "cd /w; echo lower >f; echo upper >F; mkdir d; echo dl >d/a; echo du >d/A; ls; cat f F d/a d/A"}, nil)
	if lb, lerr := os.ReadFile(filepath.Join(*workDir, "logs", "case.log")); lerr == nil {
		for _, l := range strings.Split(string(lb), string(rune(10))) {
			if strings.Contains(l, "case sensitive") {
				fmt.Println("runsc log:", strings.TrimSpace(l))
			}
		}
	}
	if err != nil {
		return err
	}
	for _, want := range []string{"lower\n", "upper\n", "dl\n", "du\n"} {
		if !strings.Contains(r.stdout, want) {
			return fmt.Errorf("%q missing; stdout %q stderr %q", want, r.stdout, r.stderr)
		}
	}
	return nil
}

// testForward POSTs expose and unexpose to the gateway from inside the guest,
// as Podman does on a machine, and checks that the 127.0.0.1 listener opens,
// forwards to the guest and closes again.
func testForward() error {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	post := func(what string) string {
		return fmt.Sprintf(`wget -q -O- --header 'Content-Type: application/json' --post-data '{"local":"127.0.0.1:%d","remote":":9090","protocol":"tcp"}' http://10.0.2.2/services/forwarder/%s; echo %s=$?`, port, what, what)
	}
	script := "httpd -p 9090 -h /www & sleep 1; " + post("expose") + "; sleep 15; " + post("unexpose") + "; sleep 15"
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	dir := bundle("forward", []string{"/bin/sh", "-c", script}, false)
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, *runscExe, runscArgs("forward", dir, "--network=windows-nat")...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Kill() }()
	url := fmt.Sprintf("http://127.0.0.1:%d/index.html", port)
	opened := false
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end) && !opened; time.Sleep(300 * time.Millisecond) {
		if resp, err := http.Get(url); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			opened = strings.TrimSpace(string(b)) == "hello-from-guest"
		}
	}
	if !opened {
		return fmt.Errorf("the exposed port never forwarded (guest output %q, stderr %q)", out.String(), errb.String())
	}
	closed := false
	for end := time.Now().Add(25 * time.Second); time.Now().Before(end) && !closed; time.Sleep(300 * time.Millisecond) {
		c, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err != nil {
			closed = true
		} else {
			c.Close()
		}
	}
	if !closed {
		return fmt.Errorf("the listener stayed open after unexpose (guest output %q)", out.String())
	}
	return nil
}

// rs runs runsc.exe with the given arguments after the global flags (a state
// root of its own, a debug log) and returns its output and exit code.
func rs(name string, args ...string) (result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	g := []string{"--root=" + filepath.Join(*workDir, "lifecycle-root"), "--debug", "--debug-log=" + filepath.Join(*workDir, "logs", name+".log")}
	cmd := exec.CommandContext(ctx, *runscExe, append(g, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.code, err = ee.ExitCode(), nil
	}
	return r, err
}

// testLifecycle drives create, state, start, exec, ps, kill, wait, list and
// delete on a container that sleeps.
func testLifecycle() error {
	_ = os.RemoveAll(filepath.Join(*workDir, "lifecycle-root"))
	dir := bundle("lifecycle", []string{"/bin/sh", "-c", "echo up; sleep 300"}, false)
	id := "lc1"
	step := func(what string, r result, err error, wantCode int, wantOut string) error {
		if err != nil {
			return fmt.Errorf("%s: %v", what, err)
		}
		if r.code != wantCode || !strings.Contains(r.stdout, wantOut) {
			return fmt.Errorf("%s: exit %d (want %d), stdout %q (want %q), stderr %q", what, r.code, wantCode, r.stdout, wantOut, r.stderr)
		}
		return nil
	}
	r, err := rs("lifecycle", "create", "-bundle", dir, id)
	if e := step("create", r, err, 0, ""); e != nil {
		return e
	}
	r, err = rs("lifecycle", "state", id)
	if e := step("state after create", r, err, 0, `"status": "created"`); e != nil {
		return e
	}
	r, err = rs("lifecycle", "exec", id, "/bin/true")
	if e := step("exec before start must fail", r, err, 128, ""); e != nil {
		return e
	}
	r, err = rs("lifecycle", "start", id)
	if e := step("start", r, err, 0, ""); e != nil {
		return e
	}
	r, err = rs("lifecycle", "state", id)
	if e := step("state after start", r, err, 0, `"status": "running"`); e != nil {
		return e
	}
	r, err = rs("lifecycle", "exec", id, "/bin/sh", "-c", "echo hi from exec; exit 5")
	if e := step("exec", r, err, 5, "hi from exec"); e != nil {
		return e
	}
	r, err = rs("lifecycle", "ps", id)
	if e := step("ps", r, err, 0, "sleep"); e != nil {
		return e
	}
	r, err = rs("lifecycle", "list")
	if e := step("list", r, err, 0, id); e != nil {
		return e
	}
	r, err = rs("lifecycle", "delete", id)
	if e := step("delete of a running container must fail", r, err, 128, ""); e != nil {
		return e
	}
	r, err = rs("lifecycle", "kill", id, "KILL")
	if e := step("kill", r, err, 0, ""); e != nil {
		return e
	}
	r, err = rs("lifecycle", "wait", id)
	if e := step("wait", r, err, 137, ""); e != nil {
		return e
	}
	r, err = rs("lifecycle", "state", id)
	if e := step("state after kill", r, err, 0, `"status": "stopped"`); e != nil {
		return e
	}
	r, err = rs("lifecycle", "delete", id)
	if e := step("delete", r, err, 0, ""); e != nil {
		return e
	}
	r, err = rs("lifecycle", "state", id)
	return step("state after delete must fail", r, err, 128, "")
}

// testBench runs the Phase 0 workloads in the guest (tools/windows/bench/bench.sh,
// rootfs /bench.sh and /small.tar) and times runsc run from start to exit of a
// hello. It only prints; "RESULT name seconds" lines go to the CI log.
func testBench() error {
	dir := filepath.Join(*workDir, "bench-bind")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	extraMounts = []map[string]any{{"destination": "/bm", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}}}
	defer func() { extraMounts = nil }()
	var hello []float64
	for i := 0; i < 5; i++ {
		t := time.Now()
		r, err := runsc("bench-hello", []string{"/bin/hello"}, nil)
		if err != nil || r.code != 7 {
			return fmt.Errorf("hello: %v, exit %d", err, r.code)
		}
		hello = append(hello, time.Since(t).Seconds())
	}
	sort.Float64s(hello)
	fmt.Printf("RESULT hello_start_to_exit_median %.2f\n", hello[len(hello)/2])
	fmt.Printf("RESULT hello_start_to_exit_min %.2f\n", hello[0])
	t := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, *runscExe, runscArgs("bench", bundle("bench", []string{"/bin/sh", "/bench.sh", "/small.tar", "/bm", "/root/untar", "/tmp/untar", "100"}, false))...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.MultiWriter(os.Stdout, &out), &errb // results show as they come
	err := cmd.Run()
	fmt.Printf("RESULT bench_wall_total %.2f\n", time.Since(t).Seconds())
	if err != nil || !strings.Contains(out.String(), "cpu_gzip_512MiB") {
		return fmt.Errorf("bench: %v, stderr %q", err, errb.String())
	}
	return nil
}

// profileRun times one shell command through runsc run (docs/results/1i-profile.md); the
// Sentry's own counters come from GVISOR_PROFSTATS and RUNSC_CPUPROFILE in the environment.
func profileRun(name, script string) error {
	t := time.Now()
	r, err := runsc("prof-"+name, []string{"/bin/sh", "-c", script}, nil, strings.Fields(os.Getenv("PROF_RUNSC_FLAGS"))...)
	fmt.Printf("RESULT %s_wall %.2f\n", name, time.Since(t).Seconds())
	fmt.Print(r.stdout, r.stderr)
	if err != nil || r.code != 0 {
		return fmt.Errorf("%s: %v, exit %d, stdout %q", name, err, r.code, r.stdout)
	}
	return nil
}

// rootfsOptions are the folder options of the rootfs; PROF_ROOTFS_OPTIONS adds more (e.g. cache=shared).
func rootfsOptions() string {
	if o := os.Getenv("PROF_ROOTFS_OPTIONS"); o != "" {
		return "file-mode=0755," + o
	}
	return "file-mode=0755"
}
// testSockfs is the reproducer for conmon's attach socket (Podman: "dial unixpacket
// /proc/self/fd/N: connect: connection refused"): test/guests/unixsock binds a
// SOCK_SEQPACKET socket and connects to it directly and through /proc/self/fd, on tmpfs
// (control) and on a host-folder bind mount. It fails if any step does.
func testSockfs() error {
	dir := filepath.Join(*workDir, "sockfs")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	extraMounts = []map[string]any{{"destination": "/w", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}}}
	defer func() { extraMounts = nil }()
	for _, where := range []string{"/tmp", "/w"} {
		r, err := runsc("sockfs", []string{"/bin/unixsock", where}, nil)
		if err != nil {
			return err
		}
		fmt.Println("sockfs on", where, r.stdout, r.code, r.stderr)
		for _, want := range []string{"bind+listen: Success", "child direct: Success", "child proc-fd: Success", "accepted and read from the child: Success", "parent direct: Success"} {
			if !strings.Contains(r.stdout, want) {
				return fmt.Errorf("sockfs on %s: %q missing; stdout %q", where, want, r.stdout)
			}
		}
	}
	return nil
}

// testFsStress runs test/guests/fsstress: parallel mount namespaces creating container-like
// directories on a tmpfs and on a host-folder bind mount (docs/results/5-podman-windows.md, fifth round).
func testFsStress() error {
	if *stressStrace {
		stressExtra = []string{"--strace"}
	}
	dir := filepath.Join(*workDir, "fsstress")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	extraMounts = []map[string]any{{"destination": "/w", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}}}
	extraCaps = []string{"CAP_SYS_ADMIN", "CAP_MKNOD", "CAP_DAC_OVERRIDE", "CAP_CHOWN", "CAP_FOWNER"}
	defer func() { extraMounts, extraCaps = nil, nil }()
	bad := 0
	for _, where := range []string{"/tmp/fs", "/w/fs"} {
		r, err := runsc("fsstress", []string{"/bin/fsstress", where, strconv.Itoa(*stressWorkers), strconv.Itoa(*stressIters)}, nil, stressExtra...)
		if err != nil {
			return err
		}
		fmt.Println("fsstress on", where, "exit", r.code, "stderr", r.stderr)
		fmt.Print(r.stdout)
		if r.code != 0 {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("fsstress: %d of 2 runs failed", bad)
	}
	return nil
}

// testAptRename runs test/guests/aptrename: apt's download-rename-reopen pattern and an engine process
// that holds the names open and removes the trees, on a host-folder bind mount (docs/results/5-podman-windows.md).
func testAptRename() error {
	dir := filepath.Join(*workDir, "aptrename")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	extraMounts = []map[string]any{{"destination": "/w", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}}}
	defer func() { extraMounts = nil }()
	r, err := runsc("aptrename", []string{"/bin/aptrename", "/w/ar", strconv.Itoa(*stressIters)}, nil)
	if err != nil {
		return err
	}
	fmt.Print(r.stdout)
	if r.code != 0 {
		return fmt.Errorf("aptrename exit %d, stderr %q", r.code, r.stderr)
	}
	return nil
}

// testDevNull runs test/guests/devnull: an unprivileged uid (after setgroups/setgid/setuid and exec) opens a mknod'd
// /dev/null on a tmpfs mounted in a host folder, and apt's download-rename-symlink-stat pattern, in a loop
// (docs/results/2r-step5-overlay.md).
func testDevNull() error {
	dir := filepath.Join(*workDir, "devnull")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	extraMounts = []map[string]any{{"destination": "/w", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}}}
	extraCaps = []string{"CAP_SYS_ADMIN", "CAP_MKNOD", "CAP_DAC_OVERRIDE", "CAP_CHOWN", "CAP_FOWNER", "CAP_SETUID", "CAP_SETGID"}
	defer func() { extraMounts, extraCaps = nil, nil }()
	// Windows changes the folder while the loop runs (the engine's storage changes under a build); an overflow of the
	// change stream used to drop the container's /dev mount, which left /dev/null a file only root could open.
	stopFlood := startFlood(dir)
	defer stopFlood()
	r, err := runsc("devnull", []string{"/bin/devnull", "/w/dn", strconv.Itoa(*stressIters)}, nil)
	if err != nil {
		return err
	}
	fmt.Print(r.stdout)
	if r.code != 0 {
		return fmt.Errorf("devnull exit %d, stderr %q", r.code, r.stderr)
	}
	return nil
}

// testOvlSub runs test/guests/ovlsub: the Sentry's overlay with layers on a host folder, whiteouts as character
// devices 0:0, RemoveAll of a directory they empty (c/storage's overlay probe; docs/results/2r-step5-overlay.md).
func testOvlSub() error {
	dir := filepath.Join(*workDir, "ovlsub")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	extraMounts = []map[string]any{{"destination": "/w", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}}}
	extraCaps = []string{"CAP_SYS_ADMIN", "CAP_MKNOD", "CAP_DAC_OVERRIDE", "CAP_CHOWN", "CAP_FOWNER"}
	defer func() { extraMounts, extraCaps = nil, nil }()
	r, err := runsc("ovlsub", []string{"/bin/ovlsub", "/w/ov"}, nil)
	if err != nil {
		return err
	}
	fmt.Print(r.stdout)
	if r.code != 0 {
		return fmt.Errorf("ovlsub exit %d, stderr %q", r.code, r.stderr)
	}
	return nil
}

// testOvlOpaque runs test/guests/ovlopaque: c/storage's native-diff probe, a lower layer's trusted.overlay.opaque must
// not be copied up (docs/results/2s-overlay-default.md).
func testOvlOpaque() error {
	dir := filepath.Join(*workDir, "ovlopaque")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	extraMounts = []map[string]any{{"destination": "/w", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}}}
	extraCaps = []string{"CAP_SYS_ADMIN", "CAP_MKNOD", "CAP_DAC_OVERRIDE", "CAP_CHOWN", "CAP_FOWNER"}
	defer func() { extraMounts, extraCaps = nil, nil }()
	r, err := runsc("ovlopaque", []string{"/bin/ovlopaque", "/w/ov"}, nil)
	if err != nil {
		return err
	}
	fmt.Print(r.stdout)
	if r.code != 0 {
		return fmt.Errorf("ovlopaque exit %d, stderr %q", r.code, r.stderr)
	}
	return nil
}

// startFlood creates and deletes many files in dir/flood from Windows until the returned function is called, which
// overflows the change stream of the host folder the sandbox watches.
func startFlood(dir string) (stop func()) {
	must(os.MkdirAll(filepath.Join(dir, "flood"), 0o755))
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		flood := filepath.Join(dir, "flood")
		for round := 0; ; round++ {
			select {
			case <-stopCh:
				return
			default:
			}
			for i := 0; i < 3000; i++ {
				_ = os.WriteFile(filepath.Join(flood, fmt.Sprintf("file-with-a-long-name-to-fill-the-buffer-%05d-%d", i, round%2)), []byte("x"), 0o644)
			}
			for i := 0; i < 3000; i++ {
				_ = os.Remove(filepath.Join(flood, fmt.Sprintf("file-with-a-long-name-to-fill-the-buffer-%05d-%d", i, round%2)))
			}
		}
	}()
	return func() {
		close(stopCh)
		<-done
	}
}

// testMountKeep: a tmpfs mounted on a directory of a host folder survives changes made to the folder from
// Windows, enough of them to overflow the change stream (the container /dev and /tmp vanished in the Podman acceptance).
func testMountKeep() error {
	dir := filepath.Join(*workDir, "mountkeep")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(filepath.Join(dir, "flood"), 0o755))
	extraMounts = []map[string]any{{"destination": "/w", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}}}
	extraCaps = []string{"CAP_SYS_ADMIN", "CAP_DAC_OVERRIDE", "CAP_CHOWN", "CAP_FOWNER"}
	defer func() { extraMounts, extraCaps = nil, nil }()
	stopFlood := startFlood(dir)
	r, err := runsc("mountkeep", []string{"/bin/mountkeep", "/w", "20"}, nil)
	stopFlood()
	if err != nil {
		return err
	}
	fmt.Print(r.stdout)
	if r.code != 0 {
		return fmt.Errorf("mountkeep exit %d, stderr %q", r.code, r.stderr)
	}
	return nil
}

// testFchmod: conmon changes the mode of its standard streams (pipes) and warns when that fails.
func testFchmod() error {
	r, err := runsc("fchmod", []string{"/bin/sh", "-c", "chmod 0777 /proc/self/fd/1 && chmod 0600 /proc/self/fd/1 && echo ok"}, nil)
	return expect(r, err, "ok"+string(rune(10)), 0)
}

// testStrace: a run with --strace must leave a complete debug log (the log of the engine sandbox
// of podman-diag stopped after nine lines).
func testStrace() error {
	r, err := runsc("strace", []string{"/bin/hello"}, nil, "--strace")
	if err != nil || r.code != 7 {
		return fmt.Errorf("hello: %v exit %d", err, r.code)
	}
	b, err := os.ReadFile(filepath.Join(*workDir, "logs", "strace.log"))
	if err != nil {
		return err
	}
	fmt.Println("strace stderr:", r.stderr, "stdout:", r.stdout)
	fmt.Println("strace log bytes:", len(b), "lines:", strings.Count(string(b), string(rune(10))))
	if !strings.Contains(string(b), "exit_group") || !strings.Contains(string(b), "write(") {
		tail := string(b)
		if len(tail) > 1500 {
			tail = tail[len(tail)-1500:]
		}
		return fmt.Errorf("the strace log is incomplete (%d bytes), ends with %q", len(b), tail)
	}
	return nil
}

// testRestart models the engine restart of the acceptance (step 9): the storage folder is a
// bind mount with a tmpfs on one of its subdirectories (the container directories), the first run
// writes a volume file and a container directory, the second run (a new sandbox on the same host
// folder) must see the volume file, create a new container directory with a file in it, and
// delete and recreate files. runsctest -only restart.
func testRestart() error {
	dir := filepath.Join(*workDir, "restart")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	extraMounts = []map[string]any{
		{"destination": "/s", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}},
		{"destination": "/s/vfs-containers", "type": "tmpfs", "source": "tmpfs", "options": []string{"mode=755"}},
	}
	defer func() { extraMounts = nil }()
	r, err := runsc("restart", []string{"/bin/sh", "-c", "mkdir -p /s/vol/accvol/_data /s/vfs-containers/a/userdata && echo keep >/s/vol/accvol/_data/f && echo cfg >/s/vfs-containers/a/userdata/config.json && echo db >/s/db.sql && sync && echo first-ok"}, nil)
	if e := expect(r, err, "first-ok\n", 0); e != nil {
		return fmt.Errorf("first run: %w", e)
	}
	script := "cat /s/vol/accvol/_data/f; cat /s/db.sql; ls /s/vfs-containers; mkdir -p /s/vfs-containers/b/userdata && echo cfg2 >/s/vfs-containers/b/userdata/config.json && cat /s/vfs-containers/b/userdata/config.json; echo again >/s/db.sql && mv /s/db.sql /s/db.old && echo db2 >/s/db.sql && cat /s/db.sql; echo second-ok"
	r, err = runsc("restart2", []string{"/bin/sh", "-c", script}, nil)
	return expect(r, err, "keep\ndb\ncfg2\ndb2\nsecond-ok\n", 0)
}

// testCfr: copy_file_range onto a host folder (the vfs storage driver's layer copy), then reads
// of the copy from the same and from another process, and an exec of it (cfr.c), with the
// folder revalidated (default) and trusted (cache=exclusive).
func testCfr() error {
	for _, cache := range []string{"", "cache=exclusive"} {
		dir := filepath.Join(*workDir, "cfr")
		_ = os.RemoveAll(dir)
		must(os.MkdirAll(dir, 0o755))
		opts := []string{"rbind", "rw"}
		if cache != "" {
			opts = append(opts, cache)
		}
		extraMounts = []map[string]any{{"destination": "/w", "type": "bind", "source": dir, "options": opts}}
		r, err := runsc("cfr", []string{"/bin/cfr", "/w", "/bin/busybox"}, nil)
		extraMounts = nil
		if err != nil {
			return err
		}
		fmt.Printf("cfr (%q): exit %d\n%s%s\n", cache, r.code, r.stdout, r.stderr)
		if r.code != 0 {
			return fmt.Errorf("cfr with %q: exit %d, stdout %q", cache, r.code, r.stdout)
		}
	}
	return nil
}

// testDebian runs Debian 12's dynamic loader and echo from the root folder (a lower layer of the
// root overlay) and from a bind mounted host folder: "libc.so.6: file too short" in Podman.
func testDebian() error {
	dir := filepath.Join(*workDir, "debian")
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	for _, f := range []string{"ld-linux-x86-64.so.2", "libc.so.6", "echo"} {
		b, err := os.ReadFile(filepath.Join(*rootfs, "dbin", f))
		must(err)
		must(os.WriteFile(filepath.Join(dir, f), b, 0o755))
	}
	extraMounts = []map[string]any{{"destination": "/dw", "type": "bind", "source": dir, "options": []string{"rbind", "rw", "file-mode=0755"}}}
	defer func() { extraMounts = nil }()
	bad := 0
	for _, d := range []string{"/dbin", "/dw"} {
		r, err := runsc("debian", []string{d + "/ld-linux-x86-64.so.2", "--library-path", d, d + "/echo", "ok-from-" + d}, nil, "--strace")
		if err != nil {
			return err
		}
		fmt.Printf("debian ld.so from %s: exit %d stdout %q stderr %q\n", d, r.code, r.stdout, r.stderr)
		if r.code != 0 {
			bad++
			if *winstubDebug {
				fmt.Println("runsc stderr:", r.stderr)
			}
			if b, err := os.ReadFile(filepath.Join(*workDir, "logs", "debian.log")); err == nil {
				for _, l := range strings.Split(string(b), "\n") {
					if (strings.Contains(l, " X ") && (strings.Contains(l, "ld-linux") || strings.Contains(l, " echo "))) || strings.Contains(l, "gofer Translate") {
						fmt.Println(l)
					}
				}
			}
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d of 2 runs failed", bad)
	}
	return nil
}

// testGoGC runs test/guests/gogc (a Go program that keeps the collector busy on 8 threads) for
// -gcsecs seconds: the guest dies with SIGSEGV at 0xfffffffffffffff8 when a thread's %fs base is lost
// and not restored (docs/results/1m-platform-repair.md).
func testGoGC() error {
	r, err := runsc("gogc", []string{"/bin/gogc", strconv.Itoa(*gcSecs)}, nil)
	if err != nil {
		return err
	}
	fmt.Printf("gogc: exit %d stdout %q stderr head %q tail %q\n", r.code, r.stdout, head(r.stderr, 1200), tail(r.stderr, 800))
	if r.code != 0 || !strings.Contains(r.stdout, "gogc ok") {
		return fmt.Errorf("gogc: exit %d", r.code)
	}
	return nil
}

func head(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
