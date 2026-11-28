package boot

import (
	"encoding/json"
	"net"
	"net/http"
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
	var nat *winnat.NAT
	nat, err = winnat.New(winnat.Options{UpstreamDNS: conf.NATDNS, HostLoopback: WindowsNATHostLoopback, GatewayHTTP: forwarderAPI(func() *winnat.NAT { return nat })})
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

// forwarderAPI answers the port forwarding API that Podman uses on a Podman
// machine (libpod/networking_machine.go): POST /services/forwarder/expose and
// /unexpose to gateway.containers.internal, with {"local":"127.0.0.1:8080",
// "remote":":8080","protocol":"tcp"}. The engine in the sandbox makes the
// request when a container publishes a port; the NAT then opens (or closes)
// the Windows loopback listener that relays to the same port of the guest,
// where the engine's own port mapping serves it.
func forwarderAPI(nat func() *winnat.NAT) http.Handler {
	type req struct {
		Local    string `json:"local"`
		Remote   string `json:"remote"`
		Protocol string `json:"protocol"`
	}
	port := func(hostport string) (uint16, error) {
		_, p, err := net.SplitHostPort(hostport)
		if err != nil {
			return 0, err
		}
		v, err := strconv.ParseUint(p, 10, 16)
		return uint16(v), err
	}
	handle := func(expose bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var q req
			if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if q.Protocol != "" && q.Protocol != "tcp" {
				http.Error(w, "only tcp is forwarded", http.StatusBadRequest)
				return
			}
			hp, err := port(q.Local)
			if err != nil {
				http.Error(w, "local: "+err.Error(), http.StatusBadRequest)
				return
			}
			if expose {
				gp := hp
				if q.Remote != "" {
					if gp, err = port(q.Remote); err != nil {
						http.Error(w, "remote: "+err.Error(), http.StatusBadRequest)
						return
					}
				}
				_, err = nat().Publish(hp, gp)
				log.Infof("forwarder expose 127.0.0.1:%d -> guest %d: %v", hp, gp, err)
			} else {
				err = nat().Unpublish(hp)
				log.Infof("forwarder unexpose 127.0.0.1:%d: %v", hp, err)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/services/forwarder/expose", handle(true))
	mux.HandleFunc("/services/forwarder/unexpose", handle(false))
	return mux
}
