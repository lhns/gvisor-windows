// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package boot

import (
	"fmt"
	"io"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/control/controlapi"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
)

// ExecOptions describe a process started in the running container
// (runsc exec).
type ExecOptions struct {
	Argv, Env []string
	Cwd       string
	UID, GID  uint32
	In        io.Reader // nil: end of file
	Out, Err  io.Writer
}

// Exec runs a process in the root container, waits for it and returns its
// exit code (128 plus the signal if it was killed). Its stdio goes through
// in-Sentry pipes and pumps like the container's own (stdio_windows.go).
func (l *Loader) Exec(o ExecOptions) (int, error) {
	if len(o.Argv) == 0 {
		return 0, fmt.Errorf("exec needs a command")
	}
	sctx := l.k.SupervisorContext()
	fdTable := l.k.NewFDTable()
	defer fdTable.DecRef(sctx)
	s := &Stdio{In: o.In, Out: o.Out, Err: o.Err}
	if _, err := s.attach(sctx, l.k, nil, fdTable); err != nil {
		return 0, err
	}
	args := &control.ExecArgs{FDTable: fdTable}
	args.Argv = o.Argv
	args.Envv = o.Env
	args.WorkingDirectory = o.Cwd
	args.KUID = auth.KUID(o.UID)
	args.KGID = auth.KGID(o.GID)
	args.ContainerID = l.sandboxID
	caps := auth.AllCapabilities
	if o.UID != 0 {
		caps = 0
	}
	args.Capabilities = &auth.TaskCapabilities{PermittedCaps: caps, InheritableCaps: caps, EffectiveCaps: caps, BoundingCaps: auth.AllCapabilities}
	tgid, err := l.executeAsync(args)
	if err != nil {
		return 0, err
	}
	var ws uint32
	if err := l.waitPID(tgid, l.sandboxID, &ws); err != nil {
		return 0, err
	}
	s.WaitDrained(5e9)
	st := linux.WaitStatus(ws)
	if st.Signaled() {
		return 128 + int(st.TerminationSignal()), nil
	}
	return int(st.ExitStatus()), nil
}

// Signal sends signal sig to the init process of the container.
func (l *Loader) Signal(sig int32) error {
	return l.signal(l.sandboxID, 0, sig, sentryapi.DeliverToProcess)
}

// Process is one process of the container, as runsc ps shows it.
type Process struct {
	PID, PPID int32
	Cmd       string
}

// Processes lists the processes of the sandbox.
func (l *Loader) Processes() ([]Process, error) {
	var ps []*controlapi.Process
	if err := control.Processes(l.k, l.sandboxID, &ps); err != nil {
		return nil, err
	}
	out := make([]Process, 0, len(ps))
	for _, p := range ps {
		out = append(out, Process{PID: int32(p.PID), PPID: int32(p.PPID), Cmd: p.Cmd})
	}
	return out, nil
}
