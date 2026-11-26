// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

// Package winrunsc is the Windows command line of runsc (ADR 0008, 0017). The
// Linux command side (namespaces, cgroups, gofer and sandbox processes) is
// not built on Windows; this package reads the same flags and OCI bundles and
// boots the Sentry in the runsc process itself.
package winrunsc

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/rootfs"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/specutils"
	"gvisor.dev/gvisor/runsc/version"
)

var pipeForwardFlag string

// Main runs runsc.exe and does not return.
func Main() {
	os.Exit(run(os.Args[1:]))
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: runsc [flags] run [-bundle dir] [-t] container-id

Windows build of runsc. Commands: run, version. The others of the OCI runtime
interface (create, start, state, kill, delete, exec, ps, list, spec, do) need
the sandbox process and its control pipe and are not implemented yet (ADR 0008).
Global flags are runsc's, e.g. --network=windows-nat --publish=8080:80
--debug --debug-log=file --overlay2=none.
`)
}

func run(args []string) int {
	config.RegisterFlags(flag.CommandLine)
	flag.StringVar(&pipeForwardFlag, "pipe-forward", "", "GUESTPORT:PIPENAME: serve \\.\pipe\PIPENAME (current user only) and relay it to a TCP port of the sandbox (needs --network=windows-nat)")
	flag.BoolVar(&boot.WindowsNATHostLoopback, "host-loopback", false, "windows-nat: the guest reaches the host's 127.0.0.1 services at 10.0.2.2")
	showVersion := flag.Bool("version", false, "show version and exit")
	flag.CommandLine.Usage = usage
	if err := flag.CommandLine.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Printf("runsc version %s\nspec: %s\n", version.Version(), specutils.Version)
		return 0
	}
	if flag.CommandLine.NArg() == 0 {
		usage()
		return 2
	}
	switch cmd := flag.CommandLine.Arg(0); cmd {
	case "run":
		return runCmd(flag.CommandLine.Args()[1:])
	case "version":
		fmt.Printf("runsc version %s\nspec: %s\n", version.Version(), specutils.Version)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "runsc: command %q is not implemented on Windows yet\n", cmd)
		usage()
		return 2
	}
}

func fatalf(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "runsc: "+format+"\n", a...)
	log.Warningf("runsc: "+format, a...)
	return 128
}

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	bundle := fs.String("bundle", ".", "path to the root of the OCI bundle")
	fs.StringVar(bundle, "b", ".", "")
	terminal := fs.Bool("t", false, "give the container a terminal even if the spec has none")
	// Accepted for compatibility with callers of runc; they have no effect.
	fs.Bool("detach", false, "")
	fs.Bool("d", false, "")
	fs.String("pid-file", "", "")
	fs.String("console-socket", "", "")
	fs.Bool("no-pivot", false, "")
	fs.Bool("no-new-keyring", false, "")
	fs.String("user-log", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		usage()
		return 2
	}
	id := fs.Arg(0)

	// Logs must not reach stderr, which belongs to the container.
	log.SetTarget(discard{})
	conf, err := config.NewFromFlags(flag.CommandLine)
	if err != nil {
		return fatalf("%v", err)
	}
	if conf.Debug {
		log.SetLevel(log.Debug)
	}
	if conf.DebugLog != "" {
		f, err := openDebugLog(conf.DebugLog, id)
		if err != nil {
			return fatalf("opening the debug log: %v", err)
		}
		defer f.Close()
		log.SetTarget(log.GoogleEmitter{Writer: &log.Writer{Next: f}})
	}
	log.Infof("runsc %s %s, %d CPUs, args %v", version.Version(), runtime.Version(), runtime.NumCPU(), os.Args)

	spec, err := specutils.ReadSpec(*bundle, specutils.SpecOpts{Conf: conf})
	if err != nil {
		return fatalf("reading the bundle: %v", err)
	}
	if err := specutils.FixConfig(conf, spec); err != nil {
		return fatalf("applying the spec annotations: %v", err)
	}
	if *terminal {
		spec.Process.Terminal = true
	}
	applyWindowsConfig(conf)

	mountConfs, err := goferMountConfs(spec, conf)
	var goferFDs []int
	if err == nil && mountConfs[0].Lower == specutils.Erofs {
		var h int
		if h, err = rootfs.OpenImage(spec.Root.Path); err == nil {
			goferFDs = []int{h}
		}
	}
	if err != nil {
		return fatalf("%v", err)
	}

	cpuid.Initialize()
	l, err := boot.New(boot.Args{
		ID:               id,
		Spec:             spec,
		Conf:             conf,
		ControllerFD:     -1,
		PinRingFD:        -1,
		CPUDMALatencyFD:  -1,
		DevGoferFD:       -1,
		ExecFD:           -1,
		ProcSelfFD:       -1,
		UserLogFD:        -1,
		PodInitConfigFD:  -1,
		RootfsUpperTarFD: -1,
		GoferMountConfs:  mountConfs,
		GoferFDs:         goferFDs,
		NumCPU:           runtime.NumCPU(),
	})
	if err != nil {
		return fatalf("creating the sandbox: %v", err)
	}

	var pf *pipeForward
	if pipeForwardFlag != "" {
		if conf.Network != config.NetworkWindowsNAT {
			return fatalf("--pipe-forward needs --network=windows-nat")
		}
		if pf, err = parsePipeForward(pipeForwardFlag); err != nil {
			return fatalf("%v", err)
		}
	}

	stdio := &boot.Stdio{Out: os.Stdout, Err: os.Stderr}
	if boot.IsUsableHandle(os.Stdin) {
		stdio.In = os.Stdin
	}
	if spec.Process.Terminal {
		t, err := boot.NewConsoleTerminal()
		if err != nil {
			return fatalf("the container wants a terminal: %v", err)
		}
		defer t.Restore()
		stdio.Terminal = t
	}
	l.SetStdio(stdio)

	if err := l.RunInProcess(); err != nil {
		return fatalf("starting the container: %v", err)
	}
	if pf != nil {
		if err := pf.start(); err != nil {
			return fatalf("%v", err)
		}
	}
	ws := l.WaitExit()
	stdio.WaitDrained(5 * time.Second)
	if ws.Signaled() {
		return 128 + int(ws.TerminationSignal())
	}
	return int(ws.ExitStatus())
}

// applyWindowsConfig sets what the Windows sandbox needs, whatever the flags
// say: the one platform, no seccomp (no filters exist), no directfs (the
// host-folder backend opens files itself).
func applyWindowsConfig(conf *config.Config) {
	conf.Platform = "winstub"
	conf.DisableSeccomp = true
	conf.DirectFS = false
	conf.FileAccess = config.FileAccessExclusive
}

// goferMountConfs lists how the root and each bind mount are served: by the
// host-folder backend (the "lisafs" lower layer is only a name here), with a
// memory overlay on the root unless --overlay2=none (a container's root does
// not survive it, ADR 0015) and none on bind mounts, which write through to
// the Windows folder.
func goferMountConfs(spec *specs.Spec, conf *config.Config) ([]specutils.GoferMountConf, error) {
	fi, err := os.Stat(spec.Root.Path)
	if err != nil {
		return nil, fmt.Errorf("root filesystem %q: %w", spec.Root.Path, err)
	}
	erofsRoot := !fi.IsDir()
	o := conf.GetOverlay2()
	rootConf := specutils.GoferMountConf{Lower: specutils.Lisafs, Upper: specutils.NoOverlay}
	if erofsRoot {
		// An image is read-only: its changes live in a memory overlay (ADR 0015).
		rootConf.Lower = specutils.Erofs
		rootConf.Upper = specutils.MemoryOverlay
	} else if o.RootOverlayMedium() != config.NoOverlay {
		rootConf.Upper = specutils.MemoryOverlay
		rootConf.Size = o.RootOverlaySize()
	}
	confs := []specutils.GoferMountConf{rootConf}
	for _, m := range spec.Mounts {
		if !specutils.HasMountConfig(m) {
			continue
		}
		if specutils.IsErofsMount(m) {
			return nil, fmt.Errorf("mount %q: erofs mounts are not wired into runsc.exe yet", m.Destination)
		}
		mc := specutils.GoferMountConf{Lower: specutils.Lisafs, Upper: specutils.NoOverlay}
		if o.SubMountOverlayMedium() != config.NoOverlay {
			mc.Upper = specutils.MemoryOverlay
			mc.Size = o.SubMountOverlaySize()
		}
		confs = append(confs, mc)
	}
	return confs, nil
}

// openDebugLog opens the debug log: path itself, or runsc.<id>.log inside it
// if it names a folder (ends with a separator or exists as one).
func openDebugLog(path, id string) (*os.File, error) {
	if strings.HasSuffix(path, "/") || strings.HasSuffix(path, `\`) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, err
		}
	}
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, "runsc."+id+".log")
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
}

type discard struct{}

func (discard) Emit(int, log.Level, time.Time, string, ...any) {}
