// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package winrunsc

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip/winnat"
	"gvisor.dev/gvisor/runsc/boot"
)

// unixForward serves an AF_UNIX socket file on the Windows host (Windows 10
// 1803 and later) and relays each connection to a TCP port of the sandbox. The
// stock podman client cannot dial named pipes (its bindings know unix, tcp and
// ssh only); it can dial this socket. Access is the access to the socket's
// folder: put it in a folder only the current user can enter (ADR 0018).
type unixForward struct {
	guestPort uint16
	path      string
}

// parseUnixForward parses GUESTPORT:PATH (the path may contain a drive colon).
func parseUnixForward(s string) (*unixForward, error) {
	p, path, ok := strings.Cut(s, ":")
	port, err := strconv.ParseUint(p, 10, 16)
	if !ok || err != nil || port == 0 || path == "" {
		return nil, fmt.Errorf("invalid --unix-forward %q, want GUESTPORT:PATH", s)
	}
	return &unixForward{guestPort: uint16(port), path: path}, nil
}

func (f *unixForward) start() error {
	_ = os.Remove(f.path)
	l, err := net.Listen("unix", f.path)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", f.path, err)
	}
	log.Infof("socket %s forwards to guest port %d", f.path, f.guestPort)
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
					log.Warningf("socket %s: dialing guest port %d: %v", f.path, f.guestPort, err)
					c.Close()
					return
				}
				winnat.Relay(c, g)
			}()
		}
	}()
	return nil
}
