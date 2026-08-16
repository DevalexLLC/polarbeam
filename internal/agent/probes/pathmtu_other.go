//go:build !linux

package probes

import (
	"fmt"
	"syscall"
)

// setDontFragment requires Linux PMTU-probe socket options; elsewhere the
// prober reports an explicit ERROR result, never a silent skip.
func setDontFragment(_ syscall.Conn, _ bool) error {
	return fmt.Errorf("path MTU probing is only supported on Linux agents (IP_MTU_DISCOVER probe mode)")
}
