package syslogfwd

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestKeepAliveOptionsApplied reads the effective TCP keepalive settings
// back from the dialed socket: without Enable the dialer would substitute
// the kernel defaults and the advertised ~1 min idle detection would be
// silent fiction.
func TestKeepAliveOptionsApplied(t *testing.T) {
	r := newReceiver(t, nil, false)
	f := newTestForwarder(t)
	if err := f.Apply(tcpConfig(r), time.Now()); err != nil {
		t.Fatal(err)
	}
	waitState(t, f, StateConnected)
	s := f.c.current()
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("conn is %T", conn)
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]int{}
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		for _, opt := range []int{unix.TCP_KEEPIDLE, unix.TCP_KEEPINTVL, unix.TCP_KEEPCNT} {
			v, e := unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, opt)
			if e != nil {
				sockErr = e
				return
			}
			got[opt] = v
		}
		v, e := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_KEEPALIVE)
		if e != nil {
			sockErr = e
			return
		}
		got[unix.SO_KEEPALIVE] = v
	})
	if err != nil || sockErr != nil {
		t.Fatalf("getsockopt: %v %v", err, sockErr)
	}
	if got[unix.SO_KEEPALIVE] != 1 || got[unix.TCP_KEEPIDLE] != 30 || got[unix.TCP_KEEPINTVL] != 10 || got[unix.TCP_KEEPCNT] != 3 {
		t.Errorf("keepalive = %v, want enabled idle=30 interval=10 count=3", got)
	}
	_ = syscall.Getpid
	_ = context.Background
}
