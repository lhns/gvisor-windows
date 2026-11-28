// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

// Command sentryguest runs one static Linux binary under the real Sentry on
// Windows: the winstub platform, the Windows memory file and an in-memory tmpfs
// root holding only the binary (and the files its stdio uses). It is the
// minimal boot path of "Run a static guest from memory"; runsc/boot is not used.
//
//	sentryguest [-out text] [-code n] [-timeout d] guest [args...]
//	sentryguest [-tar root.tar] [-env K=V,...] [flags] /path/in/tar [args...]
//
// With -tar the tmpfs is filled from the tar archive (directories, files,
// symlinks, hard links) and the first argument is the guest's path inside it,
// so a dynamic program finds its interpreter and libraries, and a shell its
// applets, as in a root filesystem.
//
// It copies the guest's stdout and stderr to its own stdout, compares them with
// -out and the exit status with -code, and prints timings.
package main

import (
	"archive/tar"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	"gvisor.dev/gvisor/pkg/abi"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/gomaxprocs"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/rand"

	"gvisor.dev/gvisor/pkg/sentry/fsimpl/cgroup2fs"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/loader"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub"
	"gvisor.dev/gvisor/pkg/sentry/strace"
	_ "gvisor.dev/gvisor/pkg/sentry/syscalls/linux" // registers the syscall table
	ktime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "sentryguest: FAIL: "+format+"\n", a...)
	os.Exit(1)
}

func must(err error, what string) {
	if err != nil {
		fail("%s: %v", what, err)
	}
}

func main() {
	wantOut := flag.String("out", "", "expected guest output (exact); unchecked if unset")
	checkOut := flag.Bool("check", false, "compare the output with -out even if empty")
	wantCode := flag.Int("code", 0, "expected exit status")
	timeout := flag.Duration("timeout", 60*time.Second, "give up after")
	verbose := flag.Bool("v", false, "Sentry logging")
	sample := flag.Bool("sample", false, "print where the task creating the process spends its time")
	tarPath := flag.String("tar", "", "fill the tmpfs from this tar; the first argument is then a path inside it")
	extraFiles := flag.String("files", "", "comma-separated hostfile:/guest/path pairs copied into the tmpfs after the tar")
	withNet := flag.Bool("net", false, "give the guest lo and eth0 (user-space NAT, as --network=windows-nat)")
	stallKill := flag.Bool("stall-kill", false, "with -stall-dump: after the dump terminate the newest stub process (workaround for a guest process that never starts)")
	stallDump := flag.Duration("stall-dump", 0, "with -stream: dump all goroutines to stderr when the guest printed nothing for this long (0: off)")
	handleEvery := flag.Duration("handles", 0, "print the process's handle count, goroutines and memory to stderr this often (0: off)")
	stream := flag.Bool("stream", false, "print the guest's output while it runs (so a crash of the Sentry loses little)")
	sysMounts := flag.Bool("sysmounts", false, "mount /proc, /sys, /dev and /dev/pts")
	envList := flag.String("env", "PATH=/bin:/usr/bin:/", "comma-separated environment of the guest")
	cpuProf := flag.String("cpuprofile", "", "write a Go CPU profile of the Sentry process to this file")
	profStats := flag.Bool("profstats", false, "print per-syscall counts and time (kernel.ProfStats)")
	asPool := flag.Int("aspool", 0, "address spaces kept ready (0: none)")
	straceOn := flag.Bool("strace", false, "log every system call of the guest to stderr")
	flag.Parse()
	kernel.ProfStats = kernel.ProfStats || *profStats
	if *cpuProf != "" {
		f, err := os.Create(*cpuProf)
		must(err, "cpuprofile")
		must(pprof.StartCPUProfile(f), "cpuprofile")
	}
	if flag.NArg() < 1 {
		fail("usage: sentryguest [flags] guest [args...]")
	}
	if *verbose || *straceOn {
		log.SetTarget(log.GoogleEmitter{Writer: &log.Writer{Next: os.Stderr}})
	}
	if *verbose {
		log.SetLevel(log.Debug)
	}
	if *straceOn {
		strace.Initialize()
		strace.EnableAll(strace.SinkTypeLog)
	}
	var image []byte
	guestPath := "/guest"
	if *tarPath == "" {
		var err error
		image, err = os.ReadFile(flag.Arg(0))
		must(err, "reading the guest")
	} else {
		guestPath = flag.Arg(0)
	}

	t0 := time.Now()
	cpuid.Initialize()
	must(rand.Init(), "rand")
	must(usage.Init(), "usage")
	gomaxprocs.SetBase(runtime.NumCPU())

	p, err := winstub.New(platform.Options{})
	must(err, "platform")
	k := &kernel.Kernel{Platform: platform.NewASPool(p, *asPool)}
	mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionManual,
		DisableMemoryAccounting: true,
	})
	must(err, "memory file")
	k.SetMemoryFile(mf)
	vdso, err := loader.PrepareVDSO(mf)
	must(err, "vdso")
	tk := kernel.NewTimekeeper()
	params := kernel.NewVDSOParamPage(mf, vdso.ParamsFileRange())
	tk.SetClocks(ktime.NewCalibratedClocks(false), params)
	userns := auth.NewRootUserNamespace()
	netns := inet.NewRootNamespace(nil, nil, userns)
	if *withNet {
		netns = newNetNamespace(tk, userns)
	}
	must(k.Init(kernel.InitKernelArgs{
		FeatureSet:           cpuid.HostFeatureSet().Fixed(),
		Timekeeper:           tk,
		RootUserNamespace:    userns,
		RootNetworkNamespace: netns,
		ApplicationCores:     uint(runtime.NumCPU()),
		Vdso:                 vdso,
		VdsoParams:           params,
		RootUTSNamespace:     kernel.NewUTSNamespace("winstub", "", userns),
		RootIPCNamespace:     kernel.NewIPCNamespace(userns),
		RootPIDNamespace:     kernel.NewRootPIDNamespace(userns),
		MaxFDLimit:           kernel.MaxFdLimit,
		Cgroup2FSInit:        cgroup2fs.NewFilesystem,
	}), "kernel init")
	vfsObj := k.VFS()
	vfsObj.MustRegisterFilesystemType(tmpfs.Name, &tmpfs.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{
		AllowUserMount: true,
		AllowUserList:  true,
	})

	ctx := k.SupervisorContext()
	creds := auth.NewRootCredentials(userns)
	mns, err := vfsObj.NewMountNamespace(ctx, creds, "rootfs", tmpfs.Name, &vfs.MountOptions{}, k)
	must(err, "root mount")
	root := mns.Root(ctx)

	create := func(path string, flags uint32, data []byte) *vfs.FileDescription {
		fd, err := vfsObj.OpenAt(ctx, creds, &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(path)},
			&vfs.OpenOptions{Flags: flags | linux.O_CREAT, Mode: 0o755})
		must(err, "creating "+path)
		for len(data) > 0 {
			n, err := fd.Write(ctx, usermem.BytesIOSequence(data), vfs.WriteOptions{})
			must(err, "writing "+path)
			data = data[n:]
		}
		return fd
	}
	if *tarPath == "" {
		guest := create("/guest", linux.O_WRONLY, image)
		guest.DecRef(ctx)
	} else {
		extract(ctx, vfsObj, creds, root, *tarPath)
	}
	for _, pair := range strings.Split(*extraFiles, ",") {
		if pair == "" {
			continue
		}
		i := strings.LastIndex(pair, ":/")
		if i < 0 {
			fail("bad -files entry %q", pair)
		}
		data, err := os.ReadFile(pair[:i])
		must(err, "reading "+pair[:i])
		create(pair[i+1:], linux.O_WRONLY|linux.O_TRUNC, data).DecRef(ctx)
	}
	if *sysMounts {
		mountSystem(ctx, vfsObj, creds, root)
	}
	stdin := create("/stdin", linux.O_RDONLY, nil)
	stdout := create("/stdout", linux.O_RDWR, nil)
	fdt := k.NewFDTable()
	for i, f := range []*vfs.FileDescription{stdin, stdout, stdout} {
		_, err := fdt.NewFDAt(ctx, int32(i), f, kernel.FDFlags{})
		must(err, "stdio")
	}
	stdin.DecRef(ctx)
	stdout.DecRef(ctx)

	ls := limits.NewLimitSet()
	ls.SetUnchecked(limits.Stack, limits.Limit{Cur: 8 << 20, Max: limits.Infinity})
	ls.SetUnchecked(limits.NumberOfFiles, limits.Limit{Cur: 1024, Max: 4096})
	args := append([]string{guestPath}, flag.Args()[1:]...)
	tInit := time.Since(t0)
	winstub.Trace("boot: kernel ready")

	if *sample {
		go sampler()
	}
	must(k.Start(), "kernel start")
	winstub.Trace("boot: kernel started")
	t1 := time.Now()
	tg, _, err := k.CreateProcess(kernel.CreateProcessArgs{
		Filename:             guestPath,
		Argv:                 args,
		Envv:                 append(strings.Split(*envList, ","), "HOME=/"),
		WorkingDirectory:     "/",
		Credentials:          creds,
		FDTable:              fdt,
		Umask:                0o022,
		Limits:               ls,
		MaxSymlinkTraversals: linux.MaxSymlinkTraversals,
		UTSNamespace:         k.RootUTSNamespace(),
		IPCNamespace:         k.RootIPCNamespace(),
		PIDNamespace:         k.RootPIDNamespace(),
		MountNamespace:       mns,
		ContainerID:          "winstub",
	})
	must(err, "creating the process")
	winstub.Trace("boot: process created")
	fdt.DecRef(ctx)
	k.StartProcess(tg)
	if *handleEvery > 0 {
		go monitorHandles(*handleEvery)
	}
	var streamed int
	stopStream := make(chan struct{})
	streamDone := make(chan struct{})
	if *stream {
		go func() {
			defer close(streamDone)
			lastGrowth, lastDump := time.Now(), time.Time{}
			for {
				select {
				case <-stopStream:
					return
				case <-time.After(20 * time.Millisecond):
				}
				out := readFrom(ctx, vfsObj, creds, root, "/stdout", streamed)
				if len(out) > 0 {
					os.Stdout.Write(out)
					streamed += len(out)
					lastGrowth = time.Now()
				}
				if *stallDump > 0 && time.Since(lastGrowth) > *stallDump && time.Since(lastDump) > 2*time.Minute {
					lastDump = time.Now()
					dumpGoroutines()
					dumpChildren()
					if *stallKill {
						killYoungestChild()
					}
				}
			}
		}()
	} else {
		close(streamDone)
	}
	done := make(chan struct{})
	go func() { k.WaitExited(); close(done) }()
	select {
	case <-done:
	case <-time.After(*timeout):
		winstub.DumpState(os.Stderr)
		var buf bytes.Buffer
		pprof.Lookup("goroutine").WriteTo(&buf, 2)
		fmt.Fprintf(os.Stderr, "%s\n", buf.String())
		fail("the guest did not exit within %v", *timeout)
	}
	tRun := time.Since(t1)
	if *cpuProf != "" {
		pprof.StopCPUProfile()
	}
	if kernel.ProfStats {
		tbl, _ := kernel.LookupSyscallTable(abi.Linux, arch.AMD64)
		kernel.DumpProfStats(os.Stdout, tbl.LookupName)
	}
	winstub.Trace("boot: exit observed")
	ws := tg.ExitStatus()

	close(stopStream)
	<-streamDone
	out := readAll(ctx, vfsObj, creds, root, "/stdout")
	fmt.Print(string(out[streamed:]))
	fmt.Printf("sentryguest: exit status %v (code %d, signaled %v), init %v, run %v\n",
		ws, ws.ExitStatus(), ws.Signaled(), tInit.Round(time.Millisecond), tRun.Round(time.Microsecond))
	bad := false
	if *checkOut || *wantOut != "" {
		if string(out) != *wantOut {
			fmt.Printf("sentryguest: FAIL: output %q, want %q\n", out, *wantOut)
			bad = true
		}
	}
	if ws.Signaled() || int(ws.ExitStatus()) != *wantCode {
		fmt.Printf("sentryguest: FAIL: status %v, want exit code %d\n", ws, *wantCode)
		bad = true
	}
	if bad {
		os.Exit(1)
	}
	fmt.Println("sentryguest: PASS")
	os.Exit(0)
}

// sampler prints, every 2 ms, the innermost frames of the goroutine inside
// kernel.CreateProcess (a poor man's profile of a run too short for pprof).
func sampler() {
	for {
		time.Sleep(2 * time.Millisecond)
		var buf bytes.Buffer
		pprof.Lookup("goroutine").WriteTo(&buf, 2)
		for _, g := range strings.Split(buf.String(), "\n\n") {
			if !strings.Contains(g, "Kernel).CreateProcess") {
				continue
			}
			var fr []string
			for _, l := range strings.Split(g, "\n") {
				if strings.HasPrefix(l, "gvisor.dev/gvisor/") || strings.HasPrefix(l, "syscall.") || strings.HasPrefix(l, "runtime.") {
					fr = append(fr, strings.TrimPrefix(strings.SplitN(l, "(", 2)[0], "gvisor.dev/gvisor/pkg/"))
				}
				if len(fr) == 7 {
					break
				}
			}
			winstub.Trace("sample: %s", strings.Join(fr, " < "))
		}
	}
}

// readFrom reads what the file holds beyond offset off.
func readFrom(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, path string, off int) []byte {
	fd, err := vfsObj.OpenAt(ctx, creds, &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(path)},
		&vfs.OpenOptions{Flags: linux.O_RDONLY})
	if err != nil {
		return nil
	}
	defer fd.DecRef(ctx)
	var out []byte
	buf := make([]byte, 64<<10)
	for {
		n, err := fd.PRead(ctx, usermem.BytesIOSequence(buf), int64(off+len(out)), vfs.ReadOptions{})
		out = append(out, buf[:n]...)
		if n == 0 || err != nil {
			return out
		}
	}
}

func readAll(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, path string) []byte {
	fd, err := vfsObj.OpenAt(ctx, creds, &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(path)},
		&vfs.OpenOptions{Flags: linux.O_RDONLY})
	must(err, "opening "+path)
	defer fd.DecRef(ctx)
	var out []byte
	buf := make([]byte, 64<<10)
	for {
		n, err := fd.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
		out = append(out, buf[:n]...)
		if n == 0 || err != nil {
			return out
		}
	}
}

// extract fills the tmpfs from a tar archive.
func extract(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, tarPath string) {
	f, err := os.Open(tarPath)
	must(err, "opening the tar")
	defer f.Close()
	pop := func(p string) *vfs.PathOperation {
		return &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(p)}
	}
	made := map[string]bool{"/": true}
	var mkdirs func(dir string)
	mkdirs = func(dir string) {
		if made[dir] {
			return
		}
		mkdirs(path.Dir(dir))
		err := vfsObj.MkdirAt(ctx, creds, pop(dir), &vfs.MkdirOptions{Mode: 0o755})
		if err != nil && !strings.Contains(err.Error(), "exists") {
			must(err, "mkdir "+dir)
		}
		made[dir] = true
	}
	contents := map[string][]byte{}
	writeFile := func(name string, mode uint32, data []byte) {
		mkdirs(path.Dir(name))
		fd, err := vfsObj.OpenAt(ctx, creds, pop(name), &vfs.OpenOptions{Flags: linux.O_WRONLY | linux.O_CREAT | linux.O_TRUNC, Mode: linux.FileMode(mode)})
		must(err, "creating "+name)
		defer fd.DecRef(ctx)
		for d := data; len(d) > 0; {
			n, err := fd.Write(ctx, usermem.BytesIOSequence(d), vfs.WriteOptions{})
			must(err, "writing "+name)
			d = d[n:]
		}
	}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		must(err, "reading the tar")
		name := path.Clean("/" + h.Name)
		switch h.Typeflag {
		case tar.TypeDir:
			mkdirs(name)
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			must(err, "reading "+name)
			contents[name] = data
			writeFile(name, uint32(h.Mode&0o777), data)
		case tar.TypeLink:
			writeFile(name, uint32(h.Mode&0o777), contents[path.Clean("/"+h.Linkname)])
		case tar.TypeSymlink:
			mkdirs(path.Dir(name))
			must(vfsObj.SymlinkAt(ctx, creds, pop(name), h.Linkname), "symlink "+name)
		}
	}
}
