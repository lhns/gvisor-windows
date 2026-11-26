// nlcheck runs inside a sandbox and checks the Linux behaviours netavark relies on, one line per check:
//   sockopt   NETLINK_DUMP_STRICT_CHK / EXT_ACK / CAP_ACK can be set and read back
//   setlink   RTM_SETLINK with NLM_F_CREATE|NLM_F_EXCL on an existing link is acked with 0
//   ifconf    /proc/sys/net/ipv4/conf/<iface>/{arp_notify,route_localnet} exist and take writes
// Exit code is the number of failed checks.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
)

const (
	solNetlink      = 270
	dumpStrictChk   = 12
	extAck          = 11
	capAck          = 10
	rtmSetLink      = 19
	nlmFRequest     = 0x1
	nlmFAck         = 0x4
	nlmFExcl        = 0x200
	nlmFCreate      = 0x400
	nlmsgError      = 2
	iffUp           = 0x1
	ifInfoMsgLength = 16
)

var failed int

func report(name string, err error) {
	if err != nil {
		failed++
		fmt.Printf("CHECK %s FAIL %v\n", name, err)
		return
	}
	fmt.Printf("CHECK %s PASS\n", name)
}

func sockopt() error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	for _, o := range []int{dumpStrictChk, extAck, capAck} {
		if err := syscall.SetsockoptInt(fd, solNetlink, o, 1); err != nil {
			return fmt.Errorf("set %d: %v", o, err)
		}
		v, err := syscall.GetsockoptInt(fd, solNetlink, o)
		if err != nil || v != 1 {
			return fmt.Errorf("get %d: %d, %v", o, v, err)
		}
	}
	return nil
}

func setlink() error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	// nlmsghdr (16) + ifinfomsg (16): lo is always index 1; set IFF_UP.
	msg := make([]byte, 16+ifInfoMsgLength)
	binary.LittleEndian.PutUint32(msg[0:], uint32(len(msg)))
	binary.LittleEndian.PutUint16(msg[4:], rtmSetLink)
	binary.LittleEndian.PutUint16(msg[6:], nlmFRequest|nlmFAck|nlmFExcl|nlmFCreate)
	binary.LittleEndian.PutUint32(msg[8:], 1)
	binary.LittleEndian.PutUint32(msg[16+4:], 1)     // ifi_index
	binary.LittleEndian.PutUint32(msg[16+8:], iffUp) // ifi_flags
	binary.LittleEndian.PutUint32(msg[16+12:], iffUp)
	if err := syscall.Sendto(fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	if n < 20 || binary.LittleEndian.Uint16(buf[4:]) != nlmsgError {
		return fmt.Errorf("unexpected reply, %d bytes", n)
	}
	if code := int32(binary.LittleEndian.Uint32(buf[16:])); code != 0 {
		return fmt.Errorf("ack error %d", -code)
	}
	return nil
}

func ifconf() error {
	for _, f := range []string{"arp_notify", "route_localnet"} {
		p := "/proc/sys/net/ipv4/conf/lo/" + f
		if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil || len(b) == 0 || b[0] != '1' {
			return fmt.Errorf("%s read back %q, %v", p, b, err)
		}
	}
	return nil
}


func main() {
	report("sockopt", sockopt())
	report("setlink", setlink())
	report("ifconf", ifconf())
	os.Exit(failed)
}
