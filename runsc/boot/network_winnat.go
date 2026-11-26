package boot

import (
	"fmt"
	"strconv"
	"strings"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/winnat"
	"gvisor.dev/gvisor/runsc/config"
)

// WindowsNATHostLoopback (runsc --host-loopback) lets the guest reach the
// host's 127.0.0.1 services at the gateway address 10.0.2.2. It exposes every
// loopback listener of the host to the guest, so it is off by default.
var WindowsNATHostLoopback bool

// WindowsNAT is the NAT of the sandbox of this process, nil without
// --network=windows-nat.
var WindowsNAT *winnat.NAT

// ParsePublish parses --publish: comma-separated hostPort:guestPort pairs.
func ParsePublish(s string) ([][2]uint16, error) {
	var out [][2]uint16
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		h, g, ok := strings.Cut(p, ":")
		if !ok {
			return nil, fmt.Errorf("invalid --publish entry %q, want hostPort:guestPort", p)
		}
		hp, err1 := strconv.ParseUint(h, 10, 16)
		gp, err2 := strconv.ParseUint(g, 10, 16)
		if err1 != nil || err2 != nil || gp == 0 {
			return nil, fmt.Errorf("invalid --publish entry %q, want hostPort:guestPort", p)
		}
		out = append(out, [2]uint16{uint16(hp), uint16(gp)})
	}
	return out, nil
}

// setupWindowsNAT attaches eth0 and lo to the sandbox's netstack behind a
// user-space NAT, and publishes the --publish ports on host loopback.
func setupWindowsNAT(conf *config.Config, s *stack.Stack) error {
	ports, err := ParsePublish(conf.Publish)
	if err != nil {
		return err
	}
	nat, err := winnat.New(winnat.Options{HostLoopback: WindowsNATHostLoopback})
	if err != nil {
		return fmt.Errorf("creating NAT: %w", err)
	}
	if err := nat.AttachGuest(s); err != nil {
		nat.Close()
		return fmt.Errorf("attaching the sandbox to the NAT: %w", err)
	}
	for _, p := range ports {
		hp, err := nat.Publish(p[0], p[1])
		if err != nil {
			nat.Close()
			return fmt.Errorf("publishing guest port %d: %w", p[1], err)
		}
		log.Infof("Published guest port %d on 127.0.0.1:%d", p[1], hp)
	}
	// The NAT lives as long as the sandbox process.
	WindowsNAT = nat
	return nil
}
