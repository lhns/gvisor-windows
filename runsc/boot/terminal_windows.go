// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package boot

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/devpts"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// IsUsableHandle reports whether f is a handle that can be read: Windows gives
// a process without a stdin an invalid or null handle.
func IsUsableHandle(f *os.File) bool {
	if f == nil {
		return false
	}
	h := windows.Handle(f.Fd())
	if h == 0 || h == windows.InvalidHandle {
		return false
	}
	t, err := windows.GetFileType(h)
	return err == nil && t != windows.FILE_TYPE_UNKNOWN
}

// Terminal connects the console of the runsc process to a pty of the Sentry,
// replacing stdin, stdout and stderr of the container.
//
// The console is the one runsc runs in (a Windows Terminal or conhost window,
// which are ConPTY hosts themselves, or a pseudoconsole a test harness made
// with CreatePseudoConsole). It is put in raw VT mode, so that Ctrl-C arrives
// as the byte 0x03 and the pty's line discipline turns it into SIGINT for the
// foreground process group, and keys arrive as the VT sequences a Linux
// terminal sends. The pty's window size follows the console's.
type Terminal struct {
	in, out           windows.Handle
	inMode, outMode   uint32
	master            *vfs.FileDescription
	rows, cols        uint16
	restored, started bool
}

// NewConsoleTerminal checks that stdin and stdout are consoles and switches
// them to raw VT mode. Restore undoes it.
func NewConsoleTerminal() (*Terminal, error) {
	t := &Terminal{in: windows.Handle(os.Stdin.Fd()), out: windows.Handle(os.Stdout.Fd())}
	if err := windows.GetConsoleMode(t.in, &t.inMode); err != nil {
		return nil, fmt.Errorf("stdin is not a console (run runsc in a terminal, or under a pseudoconsole): %w", err)
	}
	if err := windows.GetConsoleMode(t.out, &t.outMode); err != nil {
		return nil, fmt.Errorf("stdout is not a console: %w", err)
	}
	if _, _, err := t.size(); err != nil {
		return nil, err
	}
	raw := t.inMode&^(windows.ENABLE_ECHO_INPUT|windows.ENABLE_LINE_INPUT|windows.ENABLE_PROCESSED_INPUT) |
		windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(t.in, raw); err != nil {
		return nil, fmt.Errorf("setting the console input mode: %w", err)
	}
	// The pty adds the carriage returns itself (ONLCR).
	vt := t.outMode | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(t.out, vt); err != nil {
		_ = windows.SetConsoleMode(t.in, t.inMode)
		return nil, fmt.Errorf("setting the console output mode: %w", err)
	}
	return t, nil
}

// Restore puts the console modes back.
func (t *Terminal) Restore() {
	if t.restored {
		return
	}
	t.restored = true
	_ = windows.SetConsoleMode(t.in, t.inMode)
	_ = windows.SetConsoleMode(t.out, t.outMode)
}

func (t *Terminal) size() (rows, cols uint16, err error) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(t.out, &info); err != nil {
		return 0, 0, fmt.Errorf("reading the console size: %w", err)
	}
	return uint16(info.Window.Bottom - info.Window.Top + 1), uint16(info.Window.Right - info.Window.Left + 1), nil
}

func (t *Terminal) attach(ctx context.Context, k *kernel.Kernel, mns *vfs.MountNamespace, fdTable *kernel.FDTable, s *Stdio) (*kernel.TTY, error) {
	sctx := k.SupervisorContext()
	creds := auth.CredentialsFromContext(ctx)
	root := mns.Root(ctx)
	defer root.DecRef(ctx)
	vfsObj := root.Mount().Filesystem().VirtualFilesystem()
	open := func(path string, flags uint32) (*vfs.FileDescription, error) {
		return vfsObj.OpenAt(ctx, creds, &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(path), FollowFinalSymlink: true},
			&vfs.OpenOptions{Flags: flags | linux.O_NOCTTY})
	}

	// /dev/pts must be a devpts mount: the spec of a container with a terminal
	// has one, as runc's default does.
	master, err := open("/dev/pts/ptmx", linux.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("opening /dev/pts/ptmx (the spec needs a devpts mount at /dev/pts): %w", err)
	}
	idx, ok := devpts.MasterIndex(master)
	if !ok {
		return nil, fmt.Errorf("/dev/pts/ptmx is not a devpts master")
	}
	nodeVD, err := vfsObj.GetDentryAt(ctx, creds, &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse("/dev/pts/" + strconv.Itoa(int(idx)))}, &vfs.GetDentryOptions{})
	if err != nil {
		return nil, fmt.Errorf("finding the replica node: %w", err)
	}
	defer nodeVD.DecRef(ctx)
	replica, tty, err := devpts.OpenReplicaWithoutTask(ctx, master, nodeVD, linux.O_RDWR|linux.O_NOCTTY)
	if err != nil {
		return nil, fmt.Errorf("opening the replica: %w", err)
	}
	for i := int32(0); i < 3; i++ {
		if _, err := fdTable.NewFDAt(ctx, i, replica, kernel.FDFlags{}); err != nil {
			return nil, fmt.Errorf("installing stdio fd %d: %w", i, err)
		}
	}
	replica.DecRef(ctx) // The table holds its own references.

	t.master = master
	if rows, cols, err := t.size(); err == nil {
		t.rows, t.cols = rows, cols
		devpts.SetMasterWindowSize(master, rows, cols)
	}
	// The master stays open until the pty has nothing more to say (the
	// replica is closed when the container is gone).
	go copyIn(sctx, os.Stdin, master) // keyboard to the pty
	s.pumps.Add(1)
	go func() { // pty to the screen
		defer s.pumps.Done()
		defer master.DecRef(sctx)
		copyOut(sctx, master, os.Stdout)
	}()
	go t.watchSize()
	return tty, nil
}

// watchSize follows console resizes. Polling at 4 Hz is simpler than reading
// WINDOW_BUFFER_SIZE events, which are mixed into the input stream.
func (t *Terminal) watchSize() {
	for {
		time.Sleep(250 * time.Millisecond)
		rows, cols, err := t.size()
		if err != nil {
			return
		}
		if rows != t.rows || cols != t.cols {
			t.rows, t.cols = rows, cols
			log.Debugf("console resized to %dx%d", cols, rows)
			devpts.SetMasterWindowSize(t.master, rows, cols)
		}
	}
}
