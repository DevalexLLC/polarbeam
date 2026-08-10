package pathwatch

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
)

func run(secs int64, reached bool, hash byte) Run {
	h := bytes.Repeat([]byte{hash}, 32)
	return Run{
		ProbeID:     uuid.NameSpaceDNS,
		TargetID:    uuid.NameSpaceURL,
		Time:        time.Unix(secs, 0).UTC(),
		DestReached: reached,
		PathHash:    h,
		Hops:        []byte(`[{"ttl":1,"addrs":["10.0.0.1"],"rtt_us":[100]}]`),
	}
}

func cur(secs int64, hash byte) *current {
	return &current{
		updatedAt: time.Unix(secs, 0).UTC(),
		pathHash:  bytes.Repeat([]byte{hash}, 32),
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		cur  *current
		r    Run
		want action
	}{
		{"first sighting", nil, run(100, true, 0xaa), actionInsert},
		{"same path refreshes", cur(100, 0xaa), run(200, true, 0xaa), actionRefresh},
		{"changed path", cur(100, 0xaa), run(200, true, 0xbb), actionChange},
		{"out of order replay", cur(200, 0xaa), run(100, true, 0xbb), actionSkip},
		{"same timestamp", cur(200, 0xaa), run(200, true, 0xbb), actionSkip},
		{"incomplete run never participates", cur(100, 0xaa), run(200, false, 0xbb), actionSkip},
		{"incomplete first sighting skipped", nil, run(100, false, 0xaa), actionSkip},
	}
	for _, c := range cases {
		if got := decide(c.cur, c.r); got != c.want {
			t.Errorf("%s: decide = %v, want %v", c.name, got, c.want)
		}
	}

	// A malformed hash on a complete run must not participate either.
	bad := run(100, true, 0xaa)
	bad.PathHash = bad.PathHash[:16]
	if got := decide(nil, bad); got != actionSkip {
		t.Errorf("short hash: decide = %v, want skip", got)
	}
}
