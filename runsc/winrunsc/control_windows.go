// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package winrunsc

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
)

// The control pipe protocol: the client sends one JSON request; the server
// answers with one JSON response, except for exec, which turns the pipe into
// a stream of JSON frames in both directions.

type request struct {
	Op     string   `json:"op"` // state, start, kill, ps, wait, exec
	Signal int      `json:"signal,omitempty"`
	Argv   []string `json:"argv,omitempty"`
	Env    []string `json:"env,omitempty"`
	Cwd    string   `json:"cwd,omitempty"`
	UID    uint32   `json:"uid,omitempty"`
	GID    uint32   `json:"gid,omitempty"`
}

type response struct {
	OK     bool           `json:"ok"`
	Err    string         `json:"err,omitempty"`
	Status string         `json:"status,omitempty"`
	Exit   int            `json:"exit,omitempty"`
	Procs  []boot.Process `json:"procs,omitempty"`
}

// frame carries exec's stdio: T is "i" (stdin data), "ieof", "o" (stdout),
// "e" (stderr) or "x" (exit code in Exit, or Err).
type frame struct {
	T    string `json:"t"`
	D    []byte `json:"d,omitempty"`
	Exit int    `json:"exit,omitempty"`
	Err  string `json:"err,omitempty"`
}

func dial(id string) (net.Conn, error) {
	d := 5 * time.Second
	return winio.DialPipe(`\\.\pipe\`+pipeName(id), &d)
}

func call(id string, req *request) (*response, error) {
	c, err := dial(id)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, err
	}
	var r response
	if err := json.NewDecoder(c).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// sandboxServer is the sandbox process's side.
type sandboxServer struct {
	in *instance
	id string

	mu       sync.Mutex
	st       *state
	started  bool
	done     chan struct{} // closed when the container has exited
	exitCode int
}

func (s *sandboxServer) setStatus(status string) {
	s.mu.Lock()
	s.st.Status = status
	_ = s.st.save()
	s.mu.Unlock()
}

func (s *sandboxServer) start(stdio *boot.Stdio) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("container %q was already started", s.id)
	}
	s.started = true
	s.mu.Unlock()
	if err := s.in.l.RunInProcess(); err != nil {
		return err
	}
	s.setStatus("running")
	go func() {
		ws := s.in.l.WaitExit()
		stdio.WaitDrained(5 * time.Second)
		code := int(ws.ExitStatus())
		if ws.Signaled() {
			code = 128 + int(ws.TerminationSignal())
		}
		s.mu.Lock()
		s.exitCode = code
		s.st.ExitCode = &code
		s.st.Status = "stopped"
		_ = s.st.save()
		s.mu.Unlock()
		close(s.done)
	}()
	return nil
}

func (s *sandboxServer) status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Status
}

func (s *sandboxServer) serve(c net.Conn, stdio *boot.Stdio) {
	defer c.Close()
	dec := json.NewDecoder(c)
	enc := json.NewEncoder(c)
	var req request
	if err := dec.Decode(&req); err != nil {
		return
	}
	reply := func(r response) { _ = enc.Encode(&r) }
	switch req.Op {
	case "state":
		reply(response{OK: true, Status: s.status()})
	case "start":
		if err := s.start(stdio); err != nil {
			reply(response{Err: err.Error()})
			return
		}
		reply(response{OK: true, Status: "running"})
	case "kill":
		if s.status() != "running" {
			reply(response{Err: fmt.Sprintf("container is %s, not running", s.status())})
			return
		}
		if err := s.in.l.Signal(int32(req.Signal)); err != nil {
			reply(response{Err: err.Error()})
			return
		}
		reply(response{OK: true})
	case "ps":
		ps, err := s.in.l.Processes()
		if err != nil {
			reply(response{Err: err.Error()})
			return
		}
		reply(response{OK: true, Procs: ps})
	case "wait":
		<-s.done
		reply(response{OK: true, Exit: s.exitCode})
	case "exec":
		if s.status() != "running" {
			_ = enc.Encode(&frame{T: "x", Err: fmt.Sprintf("container is %s, not running", s.status())})
			return
		}
		s.exec(&req, dec, enc)
	default:
		reply(response{Err: "unknown request " + req.Op})
	}
}

func (s *sandboxServer) exec(req *request, dec *json.Decoder, enc *json.Encoder) {
	var mu sync.Mutex
	send := func(f *frame) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(f)
	}
	pr, pw := io.Pipe()
	go func() {
		for {
			var f frame
			if err := dec.Decode(&f); err != nil || f.T == "ieof" {
				pw.Close()
				return
			}
			if f.T == "i" {
				pw.Write(f.D)
			}
		}
	}()
	w := func(t string) writerFunc {
		return func(p []byte) (int, error) {
			send(&frame{T: t, D: append([]byte(nil), p...)})
			return len(p), nil
		}
	}
	env := append(append([]string{}, s.in.spec.Process.Env...), req.Env...)
	cwd := req.Cwd
	if cwd == "" {
		cwd = s.in.spec.Process.Cwd
	}
	code, err := s.in.l.Exec(boot.ExecOptions{Argv: req.Argv, Env: env, Cwd: cwd, UID: req.UID, GID: req.GID, In: pr, Out: w("o"), Err: w("e")})
	if err != nil {
		send(&frame{T: "x", Err: err.Error()})
		return
	}
	send(&frame{T: "x", Exit: code})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// sandboxCmd is the hidden command `sandbox`, the process `create` starts.
func sandboxCmd(args []string) int {
	fs := flag.NewFlagSet("sandbox", flag.ContinueOnError)
	id := fs.String("id", "", "")
	bundle := fs.String("bundle", "", "")
	if err := fs.Parse(args); err != nil || *id == "" || *bundle == "" {
		return 2
	}
	in, code := newInstance(*id, *bundle, false)
	if in == nil {
		return code
	}
	defer in.closeLog()
	if in.spec.Process.Terminal {
		return fatalf("a container with a terminal needs `runsc run`; create/start have no console to attach")
	}
	stdio := &boot.Stdio{Out: os.Stdout, Err: os.Stderr}
	in.l.SetStdio(stdio)

	// The state has the sandbox's own pid; the parent wrote "creating".
	st, _ := loadStateRaw(*id)
	if st == nil {
		st = &state{OCIVersion: "1.0.2", ID: *id, Bundle: *bundle, Created: time.Now().UTC()}
	}
	st.PID, st.Status = os.Getpid(), "created"
	st.Annotations = in.spec.Annotations
	s := &sandboxServer{in: in, id: *id, st: st, done: make(chan struct{})}

	sd, err := securityDescriptor()
	if err != nil {
		return fatalf("%v", err)
	}
	l, err := winio.ListenPipe(`\\.\pipe\`+pipeName(*id), &winio.PipeConfig{SecurityDescriptor: sd, InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10})
	if err != nil {
		return fatalf("creating the control pipe: %v", err)
	}
	defer l.Close()
	if in.pf != nil {
		defer func() {}()
	}
	// Forwarders start with the container (they need its NAT).
	go func() {
		<-s.startedC()
		if in.pf != nil {
			if err := in.pf.start(); err != nil {
				log.Warningf("%v", err)
			}
		}
		if in.uf != nil {
			if err := in.uf.start(); err != nil {
				log.Warningf("%v", err)
			}
		}
	}()
	_ = st.save()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go s.serve(c, stdio)
		}
	}()
	<-s.done
	time.Sleep(2 * time.Second) // let a pending wait or state call finish
	return 0
}

// startedC returns a channel that is closed once the container was started.
func (s *sandboxServer) startedC() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for {
			s.mu.Lock()
			ok := s.started
			s.mu.Unlock()
			if ok {
				close(ch)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	return ch
}

// loadStateRaw reads the state file without the liveness check.
func loadStateRaw(id string) (*state, error) {
	b, err := os.ReadFile(stateDir(id) + `\state.json`)
	if err != nil {
		return nil, err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// execCmd runs a process in a running container and relays its stdio.
func execCmd(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	cwd := fs.String("cwd", "", "")
	var env multi
	fs.Var(&env, "env", "")
	fs.Var(&env, "e", "")
	user := fs.String("user", "", "")
	fs.String("u", "", "")
	fs.Bool("tty", false, "")
	fs.Bool("t", false, "")
	fs.String("pid-file", "", "")
	fs.Bool("detach", false, "")
	fs.Bool("d", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() < 2 {
		usage()
		return 2
	}
	req := &request{Op: "exec", Argv: fs.Args()[1:], Env: env, Cwd: *cwd}
	if *user != "" {
		u, g, _ := strings.Cut(*user, ":")
		var uid, gid uint32
		if _, err := fmt.Sscanf(u, "%d", &uid); err != nil {
			return fatalf("--user must be a numeric uid[:gid]")
		}
		gid = uid
		if g != "" {
			if _, err := fmt.Sscanf(g, "%d", &gid); err != nil {
				return fatalf("--user must be a numeric uid[:gid]")
			}
		}
		req.UID, req.GID = uid, gid
	}
	id := fs.Arg(0)
	c, err := dial(id)
	if err != nil {
		return fatalf("container %q: %v", id, err)
	}
	defer c.Close()
	enc := json.NewEncoder(c)
	dec := json.NewDecoder(c)
	if err := enc.Encode(req); err != nil {
		return fatalf("%v", err)
	}
	var mu sync.Mutex
	go func() { // stdin
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				mu.Lock()
				_ = enc.Encode(&frame{T: "i", D: buf[:n]})
				mu.Unlock()
			}
			if err != nil {
				mu.Lock()
				_ = enc.Encode(&frame{T: "ieof"})
				mu.Unlock()
				return
			}
		}
	}()
	for {
		var f frame
		if err := dec.Decode(&f); err != nil {
			return fatalf("lost the sandbox: %v", err)
		}
		switch f.T {
		case "o":
			os.Stdout.Write(f.D)
		case "e":
			os.Stderr.Write(f.D)
		case "x":
			if f.Err != "" {
				return fatalf("exec: %s", f.Err)
			}
			return f.Exit
		}
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

var _ = config.NetworkNone

// serveRunControl serves the control pipe for a container started with `run`,
// so `runsc kill <id> TERM` can reach its init process (a graceful stop of an
// engine started by a script). Only state and kill are answered.
func serveRunControl(id string, l *boot.Loader) (io.Closer, error) {
	sd, err := securityDescriptor()
	if err != nil {
		return nil, err
	}
	ln, err := winio.ListenPipe(`\\.\pipe\`+pipeName(id), &winio.PipeConfig{SecurityDescriptor: sd})
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var req request
				if err := json.NewDecoder(c).Decode(&req); err != nil {
					return
				}
				enc := json.NewEncoder(c)
				switch req.Op {
				case "state":
					_ = enc.Encode(&response{OK: true, Status: "running"})
				case "kill":
					if err := l.Signal(int32(req.Signal)); err != nil {
						_ = enc.Encode(&response{Err: err.Error()})
						return
					}
					_ = enc.Encode(&response{OK: true})
				default:
					_ = enc.Encode(&response{Err: "run supports only state and kill"})
				}
			}()
		}
	}()
	return ln, nil
}
