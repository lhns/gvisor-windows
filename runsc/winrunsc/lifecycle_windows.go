// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package winrunsc

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/runsc/flag"
)

// Container lifecycle (ADR 0019). `create` starts a sandbox process (the
// hidden command `sandbox`) that outlives runsc.exe: it boots the Loader, serves a control
// pipe (\\.\pipe\runsc-<id>, current user only) and keeps the state file.
// The other commands read the state file or talk to the pipe.

// state is the content of <root>\<id>\state.json (runc's `state` output plus
// what runsc.exe needs).
type state struct {
	OCIVersion  string            `json:"ociVersion"`
	ID          string            `json:"id"`
	PID         int               `json:"pid"` // the sandbox process
	Status      string            `json:"status"`
	Bundle      string            `json:"bundle"`
	Created     time.Time         `json:"created"`
	ExitCode    *int              `json:"exitCode,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// rootDir is where container state lives: --root, else %LOCALAPPDATA%\runsc.
func rootDir() string {
	if f := flag.Lookup("root"); f != nil && f.Value.String() != "" {
		return f.Value.String()
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base, _ = os.UserCacheDir()
	}
	return filepath.Join(base, "runsc")
}

func stateDir(id string) string { return filepath.Join(rootDir(), id) }

func pipeName(id string) string { return "runsc-" + id }

func validID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
			return false
		}
	}
	return true
}

func (s *state) save() error {
	dir := stateDir(s.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "state.json"))
}

// loadState reads the state of a container and turns "created" or "running"
// into "stopped" if the sandbox process is gone.
func loadState(id string) (*state, error) {
	b, err := os.ReadFile(filepath.Join(stateDir(id), "state.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("container %q does not exist", id)
		}
		return nil, err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if (s.Status == "created" || s.Status == "running" || s.Status == "creating") && !processAlive(s.PID) {
		s.Status = "stopped"
	}
	return &s, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
}

func killProcess(pid int) {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.TerminateProcess(h, 137)
	_, _ = windows.WaitForSingleObject(h, 10000)
}

// parseSignal accepts 9, KILL and SIGKILL.
func parseSignal(s string) (int, error) {
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 65 {
		return n, nil
	}
	names := map[string]int{"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "BUS": 7, "FPE": 8, "KILL": 9, "USR1": 10, "SEGV": 11,
		"USR2": 12, "PIPE": 13, "ALRM": 14, "TERM": 15, "CHLD": 17, "CONT": 18, "STOP": 19, "TSTP": 20, "WINCH": 28}
	if n, ok := names[strings.TrimPrefix(strings.ToUpper(s), "SIG")]; ok {
		return n, nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}

// globalArgs returns the flags given before the subcommand, to hand to the
// sandbox process.
func globalArgs(all []string) []string {
	n := len(all) - flag.CommandLine.NArg()
	if n < 0 {
		n = 0
	}
	return all[:n]
}

func createCmd(global []string, args []string) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	bundle := fs.String("bundle", ".", "")
	fs.StringVar(bundle, "b", ".", "")
	pidFile := fs.String("pid-file", "", "")
	fs.String("console-socket", "", "")
	fs.Bool("no-pivot", false, "")
	fs.Bool("no-new-keyring", false, "")
	fs.String("user-log", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		usage()
		return 2
	}
	id := fs.Arg(0)
	if !validID(id) {
		return fatalf("invalid container id %q", id)
	}
	if st, err := loadState(id); err == nil && st.Status != "stopped" {
		return fatalf("container %q already exists", id)
	} else if err == nil {
		_ = os.RemoveAll(stateDir(id))
	}
	abs, err := filepath.Abs(*bundle)
	if err != nil {
		return fatalf("%v", err)
	}
	dir := stateDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fatalf("%v", err)
	}
	out, err := os.OpenFile(filepath.Join(dir, "stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fatalf("%v", err)
	}
	defer out.Close()
	errf, err := os.OpenFile(filepath.Join(dir, "stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fatalf("%v", err)
	}
	defer errf.Close()

	argv := append(append([]string{}, global...), "--root="+rootDir(), "sandbox", "-id", id, "-bundle", abs)
	start := func(flags uint32) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], argv...)
		cmd.Stdout, cmd.Stderr = out, errf // stdin is NUL
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		return cmd, cmd.Start()
	}
	const detached = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP
	// Leave a job object that would kill the sandbox with its creator, if allowed.
	cmd, err := start(detached | windows.CREATE_BREAKAWAY_FROM_JOB)
	if err != nil {
		cmd, err = start(detached)
	}
	if err != nil {
		return fatalf("starting the sandbox: %v", err)
	}
	st := &state{OCIVersion: "1.0.2", ID: id, PID: cmd.Process.Pid, Status: "creating", Bundle: abs, Created: time.Now().UTC()}
	_ = st.save()
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	for end := time.Now().Add(120 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if r, err := call(id, &request{Op: "state"}); err == nil && r.OK {
			if *pidFile != "" {
				_ = os.WriteFile(*pidFile, []byte(strconv.Itoa(st.PID)), 0o644)
			}
			return 0
		}
		select {
		case <-exited:
			b, _ := os.ReadFile(filepath.Join(dir, "stderr.log"))
			return fatalf("the sandbox exited while being created: %s", strings.TrimSpace(string(b)))
		default:
		}
	}
	killProcess(st.PID)
	return fatalf("the sandbox did not come up within 120 s")
}

func startCmd(args []string) int {
	if len(args) != 1 {
		usage()
		return 2
	}
	r, err := call(args[0], &request{Op: "start"})
	if err != nil {
		return fatalf("container %q: %v", args[0], err)
	}
	if !r.OK {
		return fatalf("starting %q: %s", args[0], r.Err)
	}
	return 0
}

func stateCmd(args []string) int {
	if len(args) != 1 {
		usage()
		return 2
	}
	st, err := loadState(args[0])
	if err != nil {
		return fatalf("%v", err)
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	fmt.Println(string(b))
	return 0
}

func killCmd(args []string) int {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	fs.Bool("all", false, "")
	fs.Bool("a", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() < 1 || fs.NArg() > 2 {
		usage()
		return 2
	}
	sig := 15
	if fs.NArg() == 2 {
		var err error
		if sig, err = parseSignal(fs.Arg(1)); err != nil {
			return fatalf("%v", err)
		}
	}
	id := fs.Arg(0)
	// A container started with `run` has a control pipe but no state file.
	if st, err := loadState(id); err == nil && st.Status != "running" && st.Status != "created" {
		return fatalf("container %q is %s, not running", id, st.Status)
	}
	r, err := call(id, &request{Op: "kill", Signal: sig})
	if err != nil {
		return fatalf("container %q: %v", id, err)
	}
	if !r.OK {
		return fatalf("%s", r.Err)
	}
	return 0
}

func deleteCmd(args []string) int {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	force := fs.Bool("force", false, "")
	fs.BoolVar(force, "f", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		usage()
		return 2
	}
	id := fs.Arg(0)
	st, err := loadState(id)
	if err != nil {
		if *force {
			return 0
		}
		return fatalf("%v", err)
	}
	if st.Status != "stopped" {
		if !*force {
			return fatalf("cannot delete container %q that is %s; kill it first or use --force", id, st.Status)
		}
		killProcess(st.PID)
	}
	// A sandbox that just reported its exit lingers a moment (it answers a
	// pending wait); it holds the log files until it is gone.
	for i := 0; i < 50 && processAlive(st.PID); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(st.PID) {
		killProcess(st.PID)
	}
	if err := os.RemoveAll(stateDir(id)); err != nil {
		return fatalf("%v", err)
	}
	return 0
}

func listCmd(args []string) int {
	entries, _ := os.ReadDir(rootDir())
	var sts []*state
	for _, e := range entries {
		if e.IsDir() {
			if st, err := loadState(e.Name()); err == nil {
				sts = append(sts, st)
			}
		}
	}
	sort.Slice(sts, func(i, j int) bool { return sts[i].ID < sts[j].ID })
	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPID\tSTATUS\tBUNDLE\tCREATED")
	for _, st := range sts {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", st.ID, st.PID, st.Status, st.Bundle, st.Created.Format(time.RFC3339))
	}
	w.Flush()
	return 0
}

func psCmd(args []string) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	format := fs.String("format", "table", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		usage()
		return 2
	}
	r, err := call(fs.Arg(0), &request{Op: "ps"})
	if err != nil {
		return fatalf("container %q: %v", fs.Arg(0), err)
	}
	if !r.OK {
		return fatalf("%s", r.Err)
	}
	if *format == "json" {
		b, _ := json.Marshal(r.Procs)
		fmt.Println(string(b))
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "PID\tPPID\tCMD")
	for _, p := range r.Procs {
		fmt.Fprintf(w, "%d\t%d\t%s\n", p.PID, p.PPID, p.Cmd)
	}
	w.Flush()
	return 0
}

func waitCmd(args []string) int {
	if len(args) != 1 {
		usage()
		return 2
	}
	r, err := call(args[0], &request{Op: "wait"})
	if err != nil {
		// The sandbox is gone: the state file has the answer.
		st, serr := loadState(args[0])
		if serr != nil || st.ExitCode == nil {
			return fatalf("container %q: %v", args[0], err)
		}
		return *st.ExitCode
	}
	return r.Exit
}
