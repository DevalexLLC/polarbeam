//go:build linux

package probes

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// setDontFragment puts a raw ICMP socket in PMTU-probe mode: DF set on
// IPv4 and the kernel's cached path MTU bypassed (PMTUDISC_PROBE), so
// every tested size actually reaches the wire. Received Fragmentation
// Needed messages still update the kernel's route cache; that is harmless
// here because probe mode ignores the cache when sending.
func setDontFragment(c syscall.Conn, v6 bool) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var optErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		if v6 {
			optErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_PROBE)
			if optErr == nil {
				optErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_DONTFRAG, 1)
			}
		} else {
			optErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE)
		}
	})
	if ctrlErr != nil {
		return ctrlErr
	}
	if optErr != nil {
		return fmt.Errorf("set PMTU probe socket options: %w", optErr)
	}
	return nil
}
