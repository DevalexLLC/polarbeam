package mtuwatch

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func run(secs int64, mtu int32, black, usable bool) Run {
	return Run{
		ProbeID:   uuid.NameSpaceDNS,
		TargetID:  uuid.NameSpaceURL,
		Time:      time.Unix(secs, 0).UTC(),
		LargestOK: mtu,
		BlackHole: black,
		Usable:    usable,
	}
}

func cur(secs int64, mtu int32, black bool) *current {
	return &current{updatedAt: time.Unix(secs, 0).UTC(), largestOK: mtu, blackHole: black}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		cur  *current
		r    Run
		want action
	}{
		{"first sighting", nil, run(100, 1500, false, true), actionInsert},
		{"same measurement refreshes", cur(100, 1500, false), run(200, 1500, false, true), actionRefresh},
		{"changed mtu", cur(100, 1500, false), run(200, 1400, false, true), actionChange},
		{"black hole appears at same mtu", cur(100, 1400, false), run(200, 1400, true, true), actionChange},
		{"black hole clears", cur(100, 1400, true), run(200, 1400, false, true), actionChange},
		{"out of order replay", cur(200, 1500, false), run(100, 1400, false, true), actionSkip},
		{"same timestamp", cur(200, 1500, false), run(200, 1400, false, true), actionSkip},
		{"unusable run never participates", cur(100, 1500, false), run(200, 1400, false, false), actionSkip},
		{"unusable first sighting skipped", nil, run(100, 1500, false, false), actionSkip},
	}
	for _, c := range cases {
		if got := decide(c.cur, c.r); got != c.want {
			t.Errorf("%s: decide = %v, want %v", c.name, got, c.want)
		}
	}
}
