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
//
// It copies the guest's stdout and stderr to its own stdout, compares them with
// -out and the exit status with -code, and prints timings.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/gomaxprocs"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/rand"

	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/loader"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub"
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
	flag.Parse()
	if flag.NArg() < 1 {
		fail("usage: sentryguest [flags] guest [args...]")
	}
	if *verbose {
		log.SetLevel(log.Debug)
		log.SetTarget(log.GoogleEmitter{Writer: &log.Writer{Next: os.Stderr}})
	}
	image, err := os.ReadFile(flag.Arg(0))
	must(err, "reading the guest")

	t0 := time.Now()
	cpuid.Initialize()
	must(rand.Init(), "rand")
	must(usage.Init(), "usage")
	gomaxprocs.SetBase(runtime.NumCPU())

	p, err := winstub.New(platform.Options{})
	must(err, "platform")
	k := &kernel.Kernel{Platform: p}
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
	guest := create("/guest", linux.O_WRONLY, image)
	guest.DecRef(ctx)
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
	args := append([]string{"/guest"}, flag.Args()[1:]...)
	tInit := time.Since(t0)

	must(k.Start(), "kernel start")
	t1 := time.Now()
	tg, _, err := k.CreateProcess(kernel.CreateProcessArgs{
		Filename:             "/guest",
		Argv:                 args,
		Envv:                 []string{"PATH=/", "HOME=/"},
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
	fdt.DecRef(ctx)
	k.StartProcess(tg)
	done := make(chan struct{})
	go func() { k.WaitExited(); close(done) }()
	select {
	case <-done:
	case <-time.After(*timeout):
		fail("the guest did not exit within %v", *timeout)
	}
	tRun := time.Since(t1)
	ws := tg.ExitStatus()

	out := readAll(ctx, vfsObj, creds, root, "/stdout")
	fmt.Print(string(out))
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
