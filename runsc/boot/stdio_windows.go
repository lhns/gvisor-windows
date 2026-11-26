// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package boot

import (
	"fmt"
	"io"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/pipefs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Stdio connects the root container's stdin, stdout and stderr to Windows
// handles with goroutines that pump bytes between them and in-Sentry pipes
// (ADR 0008; no host file descriptor, hence no fdnotifier).
type Stdio struct {
	// In, Out and Err are the runsc process's own handles. In may be nil.
	In       io.Reader
	Out, Err io.Writer

	// Terminal, if set, replaces the three pipes with a Sentry pseudoterminal
	// whose master is connected to the console; see terminal_windows.go.
	Terminal *Terminal

	pumps sync.WaitGroup
}

// SetStdio makes l give its root container the stdio s describes. It must be
// called before RunInProcess.
func (l *Loader) SetStdio(s *Stdio) {
	l.WindowsStdio = func(ctx context.Context, mns *vfs.MountNamespace, fdTable *kernel.FDTable) (*kernel.TTY, error) {
		return s.attach(ctx, l.k, mns, fdTable)
	}
}

// RunInProcess starts the root container in this process; the sandbox has no
// control server, so this replaces Run.
func (l *Loader) RunInProcess() error {
	return l.run()
}

// WaitDrained waits until everything the container wrote to its stdout and
// stderr has reached the Windows handles, for at most timeout. The stdin pump
// is never waited for: it can sit in a blocking read of a handle nobody writes.
func (s *Stdio) WaitDrained(timeout time.Duration) {
	done := make(chan struct{})
	go func() { s.pumps.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Warningf("stdio did not drain within %v", timeout)
	}
}

func (s *Stdio) attach(ctx context.Context, k *kernel.Kernel, mns *vfs.MountNamespace, fdTable *kernel.FDTable) (*kernel.TTY, error) {
	if s.Terminal != nil {
		return s.Terminal.attach(ctx, k, mns, fdTable, s)
	}
	sctx := k.SupervisorContext()
	pipeFDs := func() (r, w *vfs.FileDescription, err error) {
		return pipefs.NewConnectedPipeFDs(sctx, k.PipeMount(), 0)
	}
	// stdin: the pump writes into w, the container reads r.
	inR, inW, err := pipeFDs()
	if err != nil {
		return nil, fmt.Errorf("creating the stdin pipe: %w", err)
	}
	outR, outW, err := pipeFDs()
	if err != nil {
		return nil, fmt.Errorf("creating the stdout pipe: %w", err)
	}
	errR, errW, err := pipeFDs()
	if err != nil {
		return nil, fmt.Errorf("creating the stderr pipe: %w", err)
	}
	for i, f := range []*vfs.FileDescription{inR, outW, errW} {
		if _, err := fdTable.NewFDAt(ctx, int32(i), f, kernel.FDFlags{}); err != nil {
			return nil, fmt.Errorf("installing stdio fd %d: %w", i, err)
		}
		f.DecRef(sctx) // The table holds its own reference.
	}

	go pumpIn(sctx, s.In, inW)
	s.pumps.Add(2)
	go func() { defer s.pumps.Done(); pumpOut(sctx, outR, s.Out) }()
	go func() { defer s.pumps.Done(); pumpOut(sctx, errR, s.Err) }()
	return nil, nil
}

// pumpIn copies r into the pipe w until r ends, then closes w so the container
// sees end of file. A nil r (no stdin handle) is end of file at once.
func pumpIn(ctx context.Context, r io.Reader, w *vfs.FileDescription) {
	defer w.DecRef(ctx)
	copyIn(ctx, r, w)
}

// copyIn is pumpIn without closing w.
func copyIn(ctx context.Context, r io.Reader, w *vfs.FileDescription) {
	if r == nil {
		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := writeAll(ctx, w, buf[:n]); werr != nil {
				return // The container closed its stdin.
			}
		}
		if err != nil {
			return
		}
	}
}

// pumpOut copies the pipe r to w until every writer of the pipe is gone.
func pumpOut(ctx context.Context, r *vfs.FileDescription, w io.Writer) {
	defer r.DecRef(ctx)
	copyOut(ctx, r, w)
}

// copyOut is pumpOut without closing r.
func copyOut(ctx context.Context, r *vfs.FileDescription, w io.Writer) {
	buf := make([]byte, 32<<10)
	for {
		n, err := readSome(ctx, r, buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				log.Warningf("writing the container output: %v", werr)
				drain(ctx, r)
				return
			}
		}
		if err != nil || n == 0 {
			return
		}
	}
}

// drain discards what the container still writes, so that it does not block
// on a full pipe after the Windows side went away.
func drain(ctx context.Context, r *vfs.FileDescription) {
	buf := make([]byte, 32<<10)
	for {
		if n, err := readSome(ctx, r, buf); n == 0 || err != nil {
			return
		}
	}
}

// readSome reads at least one byte, waiting while the file is empty. It
// returns 0 at end of file.
func readSome(ctx context.Context, f *vfs.FileDescription, buf []byte) (int, error) {
	e, ch := waiter.NewChannelEntry(waiter.ReadableEvents | waiter.EventHUp)
	if err := f.EventRegister(&e); err != nil {
		return 0, err
	}
	defer f.EventUnregister(&e)
	for {
		n, err := f.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
		if n > 0 {
			return int(n), nil
		}
		if err == nil {
			return 0, nil
		}
		if err != linuxerr.ErrWouldBlock {
			return 0, err
		}
		<-ch
	}
}

// writeAll writes all of buf, waiting while the file is full.
func writeAll(ctx context.Context, f *vfs.FileDescription, buf []byte) error {
	e, ch := waiter.NewChannelEntry(waiter.WritableEvents | waiter.EventErr)
	if err := f.EventRegister(&e); err != nil {
		return err
	}
	defer f.EventUnregister(&e)
	for len(buf) > 0 {
		n, err := f.Write(ctx, usermem.BytesIOSequence(buf), vfs.WriteOptions{})
		buf = buf[n:]
		if err == linuxerr.ErrWouldBlock {
			<-ch
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
