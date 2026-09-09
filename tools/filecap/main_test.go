//go:build linux

package main

import (
	"encoding/hex"
	"testing"
)

// TestVFSCapV2 pins the blob to the bytes `setcap cap_net_raw+ep` writes
// (VFS_CAP_REVISION_2 | effective, permitted low word = 1<<13). Debian's
// /bin/ping carries this exact record.
func TestVFSCapV2(t *testing.T) {
	got := vfsCapV2(1<<capNetRaw, true)
	const want = "0100000200200000000000000000000000000000"
	if hex.EncodeToString(got) != want {
		t.Fatalf("vfsCapV2 = %x, want %s", got, want)
	}
	if len(got) != 20 {
		t.Fatalf("len = %d, want 20 (XATTR_CAPS_SZ_2)", len(got))
	}
	// Without the effective flag the kernel would exec the file even when
	// the capability cannot be granted — the fail-loud contract depends on
	// the flag being set, so make sure the switch does what it says.
	if noEff := vfsCapV2(1<<capNetRaw, false); noEff[0] != 0x00 || noEff[3] != 0x02 {
		t.Fatalf("effective=false: magic bytes = %x", noEff[:4])
	}
	// The high permitted word lands in data[1].
	if hi := vfsCapV2(1<<40, true); hi[12] != 0x00 || hi[13] != 0x01 {
		t.Fatalf("high word: bytes[12:16] = %x", hi[12:16])
	}
}
