//go:build linux

// Command filecap writes the cap_net_raw+ep file capability onto a binary
// without libcap. The agent release image is distroless (no apk, no
// setcap), so the capability is set here, in the Go build stage, and
// travels with the file through COPY --from. Writing security.capability
// needs CAP_SETFCAP, which the BuildKit RUN sandbox grants — the same
// privilege setcap needed in the old alpine release stage.
//
// Only this one capability is supported on purpose: the blob is the exact
// 20-byte VFS_CAP_REVISION_2 record `setcap cap_net_raw+ep` writes, and a
// readback compares bytes so a filesystem that drops or rewrites the xattr
// fails the image build instead of shipping an agent without raw ICMP.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const (
	// capNetRaw is CAP_NET_RAW from <linux/capability.h>.
	capNetRaw = 13
	// vfsCapRevision2 and vfsCapFlagsEffective are the magic_etc bits of
	// struct vfs_cap_data; revision 2 carries two 32-bit words per set.
	vfsCapRevision2      = 0x02000000
	vfsCapFlagsEffective = 0x00000001
	xattrCaps            = "security.capability"
)

// vfsCapV2 encodes a VFS_CAP_REVISION_2 capability record: magic_etc,
// then {permitted, inheritable} for the low and high 32 bits, all
// little-endian by spec regardless of host byte order. Inheritable is
// always zero — the agent never hands capabilities to children.
func vfsCapV2(permitted uint64, effective bool) []byte {
	magic := uint32(vfsCapRevision2)
	if effective {
		magic |= vfsCapFlagsEffective
	}
	out := make([]byte, 20)
	binary.LittleEndian.PutUint32(out[0:], magic)
	binary.LittleEndian.PutUint32(out[4:], uint32(permitted))      // data[0].permitted
	binary.LittleEndian.PutUint32(out[8:], 0)                      // data[0].inheritable
	binary.LittleEndian.PutUint32(out[12:], uint32(permitted>>32)) // data[1].permitted
	binary.LittleEndian.PutUint32(out[16:], 0)                     // data[1].inheritable
	return out
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: filecap <path>   (sets cap_net_raw+ep on <path>)")
		os.Exit(2)
	}
	path := os.Args[1]
	want := vfsCapV2(1<<capNetRaw, true)
	if err := unix.Setxattr(path, xattrCaps, want, 0); err != nil {
		fmt.Fprintf(os.Stderr, "filecap: setxattr %s on %s: %v\n", xattrCaps, path, err)
		os.Exit(1)
	}
	got := make([]byte, 64)
	n, err := unix.Getxattr(path, xattrCaps, got)
	if err != nil {
		fmt.Fprintf(os.Stderr, "filecap: getxattr %s on %s: %v\n", xattrCaps, path, err)
		os.Exit(1)
	}
	if !bytes.Equal(got[:n], want) {
		fmt.Fprintf(os.Stderr, "filecap: %s readback mismatch on %s: wrote %x, read %x\n",
			xattrCaps, path, want, got[:n])
		os.Exit(1)
	}
	fmt.Printf("filecap: %s = cap_net_raw+ep (%x)\n", path, want)
}
