package outage

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func result(secs int64, ok bool) Result {
	code := int16(1)
	if !ok {
		code = 2 // TIMEOUT
	}
	return Result{
		ProbeID:    uuid.NameSpaceDNS,
		TargetID:   uuid.NameSpaceURL,
		ProbeType:  1,
		Time:       time.Unix(secs, 0).UTC(),
		OK:         ok,
		StatusCode: code,
	}
}

// fold runs a sequence through step, executing actions the way Apply does
// (open sets OpenEventID, close clears it), and returns the actions taken.
func fold(st State, results []Result) (State, []action) {
	var actions []action
	eventID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	for _, r := range results {
		var act action
		st, act = step(st, r)
		actions = append(actions, act)
		switch act {
		case actionOpen:
			id := eventID
			st.OpenEventID = &id
		case actionClose:
			st.OpenEventID = nil
		}
	}
	return st, actions
}

func seq(pattern string) []Result {
	// pattern: 'o' = OK, 'f' = failure; timestamps ascend one second apart.
	rs := make([]Result, len(pattern))
	for i, c := range pattern {
		rs[i] = result(int64(100+i), c == 'o')
	}
	return rs
}

func countAction(actions []action, want action) int {
	n := 0
	for _, a := range actions {
		if a == want {
			n++
		}
	}
	return n
}

func TestOpensAtThirdConsecutiveFailure(t *testing.T) {
	st, actions := fold(State{}, seq("offfff"))
	if got := countAction(actions, actionOpen); got != 1 {
		t.Fatalf("opens = %d, want exactly 1", got)
	}
	if actions[3] != actionOpen {
		t.Errorf("open fired at index %v, want index 3 (third failure)", actions)
	}
	// opened_at is the FIRST failure of the streak.
	if !st.FirstFailAt.Equal(time.Unix(101, 0).UTC()) {
		t.Errorf("first_fail_at = %v, want t=101", st.FirstFailAt)
	}
	if st.OpenEventID == nil {
		t.Error("open event must be recorded in state")
	}
}

func TestClosesAtThirdConsecutiveSuccess(t *testing.T) {
	st, actions := fold(State{}, seq("fffooo"))
	if countAction(actions, actionOpen) != 1 || countAction(actions, actionClose) != 1 {
		t.Fatalf("actions = %v, want one open and one close", actions)
	}
	if actions[5] != actionClose {
		t.Errorf("close fired at wrong position: %v", actions)
	}
	// closed_at is the FIRST success of the closing streak.
	if !st.FirstOKAt.Equal(time.Unix(103, 0).UTC()) {
		t.Errorf("first_ok_at = %v, want t=103", st.FirstOKAt)
	}
	if st.OpenEventID != nil {
		t.Error("close must clear the open event")
	}
}

func TestPartialRecoveryDoesNotClose(t *testing.T) {
	// Two OKs inside an open outage are not enough; a failure resets the
	// success streak without opening a second event.
	_, actions := fold(State{}, seq("fffoofoo"))
	if countAction(actions, actionOpen) != 1 {
		t.Errorf("partial recovery must not reopen: %v", actions)
	}
	if countAction(actions, actionClose) != 0 {
		t.Errorf("partial recovery must not close: %v", actions)
	}
}

func TestFlappingNeverReachesThreshold(t *testing.T) {
	_, actions := fold(State{}, seq("ffoffoffo"))
	if countAction(actions, actionOpen) != 0 {
		t.Errorf("flapping below threshold must never open: %v", actions)
	}
}

func TestOutOfOrderResultsIgnored(t *testing.T) {
	st, _ := fold(State{}, seq("fff")) // open, last_time = 102
	// A replayed older OK must not disturb the streak.
	st2, act := step(st, result(50, true))
	if act != actionNone || st2.ConsecFails != st.ConsecFails || st2.ConsecOKs != 0 {
		t.Errorf("older result changed state: %+v action %v", st2, act)
	}
	// Same timestamp is also ignored (dedupe boundary).
	st3, act := step(st, result(102, true))
	if act != actionNone || st3.ConsecFails != st.ConsecFails {
		t.Errorf("same-time result changed state: %+v action %v", st3, act)
	}
}

func TestReplayedHistoryOpensAndCloses(t *testing.T) {
	// A spool replay carrying a whole outage and its recovery in one batch
	// must produce exactly one open and one close.
	_, actions := fold(State{}, seq("ooofffffooo"))
	if countAction(actions, actionOpen) != 1 || countAction(actions, actionClose) != 1 {
		t.Errorf("replayed outage: actions = %v", actions)
	}
}

func TestDriftHealsWithOpenPastThreshold(t *testing.T) {
	// State says no open event but the streak is already past threshold
	// (crash between event insert and state upsert): the next failure must
	// still try to open.
	st := State{ConsecFails: 5, LastTime: time.Unix(100, 0).UTC()}
	_, act := step(st, result(101, false))
	if act != actionOpen {
		t.Errorf("drifted state must re-attempt open, got %v", act)
	}
}
