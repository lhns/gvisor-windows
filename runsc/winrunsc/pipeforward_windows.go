// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package winrunsc

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip/winnat"
	"gvisor.dev/gvisor/runsc/boot"
)

// pipeForward serves \.\pipe\NAME for the current user only and relays each
// connection to TCP port GUEST of the sandbox (ADR 0018). The engine in the
// sandbox listens on a TCP port (podman system service tcp://0.0.0.0:8888),
// which is the same arrangement as the Linux harness; a guest Unix socket
// would need an in-Sentry dial and is left open.
type pipeForward struct {
	guestPort uint16
	name      string
}

// parsePipeForward parses GUESTPORT:PIPENAME.
func parsePipeForward(s string) (*pipeForward, error) {
	p, name, ok := strings.Cut(s, ":")
	port, err := strconv.ParseUint(p, 10, 16)
	if !ok || err != nil || port == 0 || name == "" || strings.ContainsAny(name, `\/`) {
		return nil, fmt.Errorf("invalid --pipe-forward %q, want GUESTPORT:PIPENAME", s)
	}
	return &pipeForward{guestPort: uint16(port), name: name}, nil
}

// securityDescriptor allows the current user and nobody else: network logons
// are denied explicitly, and the protected DACL has no entry for anyone else
// (so not even the Administrators group, apart from its take-ownership right).
func securityDescriptor() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("D:P(D;;GA;;;NU)(A;;GA;;;%s)", u.User.Sid.String()), nil
}

func (f *pipeForward) start() error {
	sd, err := securityDescriptor()
	if err != nil {
		return fmt.Errorf("reading the current user: %w", err)
	}
	path := `\.\pipe\` + f.name
	l, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sd, MessageMode: false, InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10})
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	log.Infof("pipe %s (%s) forwards to guest port %d", path, sd, f.guestPort)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				nat := boot.WindowsNAT
				if nat == nil {
					c.Close()
					return
				}
				g, err := nat.DialGuest(f.guestPort)
				if err != nil {
					log.Warningf("pipe %s: dialing guest port %d: %v", f.name, f.guestPort, err)
					c.Close()
					return
				}
				winnat.Relay(c, g)
			}()
		}
	}()
	return nil
}

var _ net.Conn
