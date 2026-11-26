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
	"strings"
	"time"
)

var (
	runscExe = flag.String("runsc", "runsc.exe", "runsc.exe")
	rootfs   = flag.String("rootfs", "", "rootfs folder (bin/busybox, bin/hello, ...)")
	workDir  = flag.String("work", "", "writable folder for bundles and logs")
	only     = flag.String("only", "", "comma-separated test names (default all)")
	timeout  = flag.Duration("timeout", 120*time.Second, "per test")
)

type result struct {
	stdout, stderr string
	code           int
}

func main() {
	flag.Parse()
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
		{"net", testNet},
		{"publish", testPublish},
		{"tty", testTTY},
	}
	want := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n != "" {
			want[n] = true
		}
	}
	failed := 0
	for _, t := range tests {
		if len(want) > 0 && !want[t.name] {
			continue
		}
		start := time.Now()
		fmt.Printf("=== RUN  %s\n", t.name)
		if err := t.fn(); err != nil {
			failed++
			fmt.Printf("--- FAIL %s (%v): %v\n", t.name, time.Since(start).Round(time.Millisecond), err)
			if b, err := os.ReadFile(filepath.Join(*workDir, "logs", t.name+".log")); err == nil {
				lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
				if len(lines) > 40 {
					lines = lines[len(lines)-40:]
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
	dir := filepath.Join(*workDir, "bundle-"+name)
	must(os.MkdirAll(dir, 0o755))
	mounts := []map[string]any{
		{"destination": "/proc", "type": "proc", "source": "proc"},
		{"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs"},
	}
	spec := map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"terminal": terminal,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"args":     args,
			"env":      []string{"PATH=/bin", "HOME=/", "TERM=xterm"},
			"cwd":      "/",
		},
		"root":     map[string]any{"path": *rootfs, "readonly": false},
		"hostname": "runsc-win",
		"mounts":   mounts,
		// The files were made by Windows tools and have no Linux modes.
		"annotations": map[string]string{"dev.gvisor.windows.rootfs-options": "file-mode=0755"},
		"linux": map[string]any{"namespaces": []map[string]string{
			{"type": "pid"}, {"type": "network"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"},
		}},
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
