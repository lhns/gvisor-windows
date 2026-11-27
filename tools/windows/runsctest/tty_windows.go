// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package main

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// conpty is a pseudoconsole with one process attached, driven like a terminal
// emulator: the harness reads the screen bytes and types keys.
type conpty struct {
	hpc      windows.Handle
	in       windows.Handle // keyboard, write end
	out      windows.Handle // screen, read end
	proc     windows.Handle
	mu       sync.Mutex
	screen   strings.Builder
	readDone chan struct{}
}

func startConPTY(cmdline string, cols, rows int16) (*conpty, error) {
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		return nil, err
	}
	c := &conpty{in: inW, out: outR, readDone: make(chan struct{})}
	if err := windows.CreatePseudoConsole(windows.Coord{X: cols, Y: rows}, inR, outW, 0, &c.hpc); err != nil {
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	// The pseudoconsole holds its own references to these ends.
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	// go vet false positive: the attribute value is the HPCON handle itself, not a Go pointer.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, unsafe.Pointer(c.hpc), unsafe.Sizeof(c.hpc)); err != nil {
		return nil, err
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	cl, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, cl, nil, nil, false, windows.EXTENDED_STARTUPINFO_PRESENT, nil, nil, &si.StartupInfo, &pi); err != nil {
		return nil, fmt.Errorf("CreateProcess: %w", err)
	}
	windows.CloseHandle(pi.Thread)
	c.proc = pi.Process
	go c.readLoop()
	return c, nil
}

func (c *conpty) readLoop() {
	defer close(c.readDone)
	buf := make([]byte, 4096)
	for {
		var n uint32
		if err := windows.ReadFile(c.out, buf, &n, nil); err != nil || n == 0 {
			return
		}
		chunk := string(buf[:n])
		// conhost asks where the cursor is and waits for the answer, as a
		// terminal emulator would give it.
		if strings.Contains(chunk, "\x1b[6n") {
			c.write("\x1b[1;1R")
		}
		c.mu.Lock()
		c.screen.WriteString(chunk)
		c.mu.Unlock()
	}
}

func (c *conpty) write(s string) {
	var n uint32
	_ = windows.WriteFile(c.in, []byte(s), &n, nil)
}

// waitFor waits until the screen bytes written after offset contain want, and
// returns the new offset.
func (c *conpty) waitFor(want string, offset int, d time.Duration) (int, error) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		s := c.screen.String()
		c.mu.Unlock()
		if i := strings.Index(s[offset:], want); i >= 0 {
			return offset + i + len(want), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return offset, fmt.Errorf("did not see %q within %v; screen: %q", want, d, c.dump())
}

func (c *conpty) dump() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.screen.String()
}

func (c *conpty) exitCode(d time.Duration) (int, error) {
	ev, err := windows.WaitForSingleObject(c.proc, uint32(d.Milliseconds()))
	if err != nil || ev != windows.WAIT_OBJECT_0 {
		return -1, fmt.Errorf("process still running after %v (wait %d, %v); screen: %q", d, ev, err, c.dump())
	}
	var code uint32
	if err := windows.GetExitCodeProcess(c.proc, &code); err != nil {
		return -1, err
	}
	return int(code), nil
}

func (c *conpty) close() {
	_ = windows.TerminateProcess(c.proc, 99) // no-op when it has exited
	go windows.ClosePseudoConsole(c.hpc)
	select {
	case <-c.readDone:
	case <-time.After(5 * time.Second):
	}
	windows.CloseHandle(c.in)
}

func testTTY() error {
	dir := bundle("tty", []string{"/bin/sh"}, true)
	cmdline := syscall.EscapeArg(*runscExe)
	for _, a := range runscArgs("tty", dir, "--network=none") {
		cmdline += " " + syscall.EscapeArg(a)
	}
	c, err := startConPTY(cmdline, 100, 30)
	if err != nil {
		return err
	}
	defer c.close()

	off := 0
	step := func(send, want string) error {
		c.write(send)
		var err error
		off, err = c.waitFor(want, off, 60*time.Second)
		return err
	}
	// The echo of a typed command shows the command, not its result, so the
	// markers below are computed by the shell.
	if err := step("echo M$((1+2))\r", "M3"); err != nil {
		return fmt.Errorf("shell does not answer: %w", err)
	}
	if err := step("echo T=$(tty)\r", "T=/dev/pts/"); err != nil {
		return fmt.Errorf("tty: %w", err)
	}
	if err := step("echo S=$(stty size)\r", "S=30 100"); err != nil {
		return fmt.Errorf("initial size: %w", err)
	}
	if err := windows.ResizePseudoConsole(c.hpc, windows.Coord{X: 120, Y: 40}); err != nil {
		return err
	}
	time.Sleep(time.Second)
	if err := step("echo S=$(stty size)\r", "S=40 120"); err != nil {
		return fmt.Errorf("size after resize: %w", err)
	}
	// Ctrl-C in the terminal reaches the foreground process group as SIGINT.
	c.write("sleep 100\r")
	time.Sleep(2 * time.Second)
	c.write("\x03")
	time.Sleep(time.Second)
	if err := step("echo C=$?\r", "C=130"); err != nil {
		return fmt.Errorf("sleep not interrupted by ^C: %w", err)
	}
	c.write("exit 4\r")
	code, err := c.exitCode(30 * time.Second)
	if err != nil {
		return err
	}
	if code != 4 {
		return fmt.Errorf("exit code %d, want 4; screen %q", code, c.dump())
	}
	return nil
}
