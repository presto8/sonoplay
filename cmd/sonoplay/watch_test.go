package main

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

func TestDecidePoll_FirstPollNothingPlaying(t *testing.T) {
	var st watchState
	base := mustTime(t, "2024-01-01T00:00:00Z")

	got := decidePoll(&st, watchPoll{Time: base, OK: true, Playing: false, URI: ""}, 10*time.Second)

	want := watchDecision{}
	if got != want {
		t.Fatalf("decidePoll() = %+v, want %+v", got, want)
	}
}

func TestDecidePoll_FirstPollAlreadyPlayingFromStart(t *testing.T) {
	var st watchState
	base := mustTime(t, "2024-01-01T00:00:00Z")

	got := decidePoll(&st, watchPoll{Time: base, OK: true, Playing: true, URI: "a", Position: 0}, 10*time.Second)

	if !got.StartNew {
		t.Errorf("StartNew = false, want true")
	}
	if got.ClosePrevious {
		t.Errorf("ClosePrevious = true, want false (nothing was open before)")
	}
	if got.ListenedDelta != 0 {
		t.Errorf("ListenedDelta = %v, want 0", got.ListenedDelta)
	}
}

// TestDecidePoll_NewTrackSequence covers a track starting, continuing for
// one poll interval, and then being replaced by a genuinely different
// track.
func TestDecidePoll_NewTrackSequence(t *testing.T) {
	var st watchState
	t1 := mustTime(t, "2024-01-01T00:00:00Z")
	t2 := t1.Add(5 * time.Second)
	t3 := t2.Add(5 * time.Second)

	// poll1: track "a" starts playing from the beginning.
	d1 := decidePoll(&st, watchPoll{Time: t1, OK: true, Playing: true, URI: "a", Position: 0}, 10*time.Second)
	if !d1.StartNew || d1.ClosePrevious || d1.ListenedDelta != 0 {
		t.Fatalf("poll1: got %+v, want StartNew=true ClosePrevious=false ListenedDelta=0", d1)
	}

	// poll2: same track "a", still playing, 5s later, position advanced
	// by ~5s. ListenedDelta must be exactly the wall-clock interval
	// between poll1 and poll2, NOT derived from Position.
	d2 := decidePoll(&st, watchPoll{Time: t2, OK: true, Playing: true, URI: "a", Position: 5 * time.Second}, 10*time.Second)
	if d2.StartNew {
		t.Errorf("poll2: StartNew = true, want false")
	}
	if d2.ClosePrevious {
		t.Errorf("poll2: ClosePrevious = true, want false")
	}
	if d2.ListenedDelta != 5*time.Second {
		t.Errorf("poll2: ListenedDelta = %v, want exactly 5s", d2.ListenedDelta)
	}

	// poll3: a DIFFERENT track "b" starts playing.
	d3 := decidePoll(&st, watchPoll{Time: t3, OK: true, Playing: true, URI: "b", Position: 0}, 10*time.Second)
	if !d3.StartNew {
		t.Errorf("poll3: StartNew = false, want true")
	}
	if !d3.ClosePrevious {
		t.Errorf("poll3: ClosePrevious = false, want true (closing track a)")
	}
	if d3.ListenedDelta != 0 {
		t.Errorf("poll3: ListenedDelta = %v, want 0", d3.ListenedDelta)
	}
}

// TestDecidePoll_Pausing verifies the documented "known approximation":
// a poll that resumes playback after a pause is credited the FULL
// interval since the paused poll, even though the speaker wasn't
// actually playing for all of that interval. This IS the expected (if
// slightly surprising) behavior, not a bug - polling can't know what
// happened between polls.
func TestDecidePoll_Pausing(t *testing.T) {
	var st watchState
	t1 := mustTime(t, "2024-01-01T00:00:00Z")
	t2 := t1.Add(5 * time.Second)
	t3 := t2.Add(5 * time.Second)  // paused poll
	t4 := t3.Add(20 * time.Second) // resumed poll, well after t3

	// poll1: track "a" starts.
	decidePoll(&st, watchPoll{Time: t1, OK: true, Playing: true, URI: "a", Position: 0}, 10*time.Second)
	// poll2: still playing.
	decidePoll(&st, watchPoll{Time: t2, OK: true, Playing: true, URI: "a", Position: 5 * time.Second}, 10*time.Second)

	// poll3: paused (Playing=false), same URI, position unchanged.
	d3 := decidePoll(&st, watchPoll{Time: t3, OK: true, Playing: false, URI: "a", Position: 10 * time.Second}, 10*time.Second)
	if d3.StartNew || d3.ClosePrevious {
		t.Fatalf("poll3 (paused): got %+v, want StartNew=false ClosePrevious=false", d3)
	}
	if d3.ListenedDelta != 0 {
		t.Errorf("poll3 (paused): ListenedDelta = %v, want 0", d3.ListenedDelta)
	}

	// poll4: resumed (Playing=true again), same URI, 20s after poll3.
	// Per the documented approximation, the ENTIRE interval since poll3
	// is credited, even though the speaker was actually paused for
	// (presumably) part of it - this is intentional, not a bug.
	d4 := decidePoll(&st, watchPoll{Time: t4, OK: true, Playing: true, URI: "a", Position: 10 * time.Second}, 10*time.Second)
	if d4.StartNew || d4.ClosePrevious {
		t.Fatalf("poll4 (resumed): got %+v, want StartNew=false ClosePrevious=false", d4)
	}
	if d4.ListenedDelta != 20*time.Second {
		t.Errorf("poll4 (resumed): ListenedDelta = %v, want exactly 20s (full interval since the paused poll, per the documented approximation)", d4.ListenedDelta)
	}
}

// TestDecidePoll_RestartSameTrack exercises the "same URI but still
// counts as new" branch: the track's reported position jumps backward
// by at least restartThreshold (e.g. a repeat), which must be treated as
// the track restarting from the beginning - closing the previous play
// and starting a new one - even though the URI itself never changed.
func TestDecidePoll_RestartSameTrack(t *testing.T) {
	var st watchState
	t1 := mustTime(t, "2024-01-01T00:00:00Z")
	t2 := t1.Add(3*time.Minute + 10*time.Second)
	t3 := t2.Add(5 * time.Second)
	restartThreshold := 10 * time.Second

	// poll1: "a" starts at position 0.
	decidePoll(&st, watchPoll{Time: t1, OK: true, Playing: true, URI: "a", Position: 0}, restartThreshold)

	// poll2: "a" continues, position advanced normally to 3m10s.
	d2 := decidePoll(&st, watchPoll{Time: t2, OK: true, Playing: true, URI: "a", Position: 3*time.Minute + 10*time.Second}, restartThreshold)
	if d2.StartNew {
		t.Fatalf("poll2: StartNew = true, want false")
	}

	// poll3: SAME URI "a", but position has jumped backward to 2s -
	// well more than restartThreshold (10s) less than poll2's position.
	// This is a restart (e.g. repeat), not a track change.
	d3 := decidePoll(&st, watchPoll{Time: t3, OK: true, Playing: true, URI: "a", Position: 2 * time.Second}, restartThreshold)
	if !d3.StartNew {
		t.Errorf("poll3: StartNew = false, want true (restart)")
	}
	if !d3.ClosePrevious {
		t.Errorf("poll3: ClosePrevious = false, want true (closing the just-finished play of track a)")
	}
	if d3.ListenedDelta != 0 {
		t.Errorf("poll3: ListenedDelta = %v, want exactly 0", d3.ListenedDelta)
	}
}

// TestDecidePoll_SmallBackwardSeekIsNotRestart verifies that a small
// backward position jump (smaller than restartThreshold), simulating
// minor player-reported jitter or an intentional small seek-back, is NOT
// treated as a restart.
func TestDecidePoll_SmallBackwardSeekIsNotRestart(t *testing.T) {
	var st watchState
	t1 := mustTime(t, "2024-01-01T00:00:00Z")
	t2 := t1.Add(3*time.Minute + 10*time.Second)
	t3 := t2.Add(5 * time.Second)
	restartThreshold := 10 * time.Second

	decidePoll(&st, watchPoll{Time: t1, OK: true, Playing: true, URI: "a", Position: 0}, restartThreshold)
	decidePoll(&st, watchPoll{Time: t2, OK: true, Playing: true, URI: "a", Position: 3*time.Minute + 10*time.Second}, restartThreshold)

	// poll3: position moves backward by only 3s (< 10s threshold).
	d3 := decidePoll(&st, watchPoll{Time: t3, OK: true, Playing: true, URI: "a", Position: 3*time.Minute + 7*time.Second}, restartThreshold)
	if d3.StartNew {
		t.Errorf("poll3: StartNew = true, want false (small backward jitter should not trigger a restart)")
	}
	if d3.ClosePrevious {
		t.Errorf("poll3: ClosePrevious = true, want false")
	}
	if d3.ListenedDelta != 5*time.Second {
		t.Errorf("poll3: ListenedDelta = %v, want exactly 5s (normal accounting continues)", d3.ListenedDelta)
	}
}

// TestDecidePoll_Stopped verifies that the URI going empty (e.g.
// STOPPED, or an empty queue) closes whatever was open, and that a
// subsequent empty-URI poll does NOT double-close (idempotency).
func TestDecidePoll_Stopped(t *testing.T) {
	var st watchState
	t1 := mustTime(t, "2024-01-01T00:00:00Z")
	t2 := t1.Add(5 * time.Second)
	t3 := t2.Add(5 * time.Second)

	decidePoll(&st, watchPoll{Time: t1, OK: true, Playing: true, URI: "a", Position: 0}, 10*time.Second)

	// poll2: URI goes empty - the speaker stopped.
	d2 := decidePoll(&st, watchPoll{Time: t2, OK: true, Playing: false, URI: ""}, 10*time.Second)
	if !d2.ClosePrevious {
		t.Errorf("poll2: ClosePrevious = false, want true")
	}
	if d2.StartNew {
		t.Errorf("poll2: StartNew = true, want false")
	}
	if d2.ListenedDelta != 0 {
		t.Errorf("poll2: ListenedDelta = %v, want 0", d2.ListenedDelta)
	}

	// poll3: still empty - nothing was open to close this time.
	d3 := decidePoll(&st, watchPoll{Time: t3, OK: true, Playing: false, URI: ""}, 10*time.Second)
	if d3.ClosePrevious {
		t.Errorf("poll3: ClosePrevious = true, want false (already closed by the previous poll)")
	}
	if d3.StartNew || d3.ListenedDelta != 0 {
		t.Errorf("poll3: got %+v, want StartNew=false ListenedDelta=0", d3)
	}
}

// TestDecidePoll_UnreachableGapNotCounted verifies the "undercount the
// gap" behavior: time spent with the speaker unreachable is never
// credited as listened time, even once it becomes reachable again
// showing the same track still playing. ListenedDelta is measured from
// the most recent poll (successful OR not), never from further back.
func TestDecidePoll_UnreachableGapNotCounted(t *testing.T) {
	var st watchState
	t1 := mustTime(t, "2024-01-01T00:00:00Z")
	t2 := t1.Add(5 * time.Second)     // unreachable poll
	t3 := t2.Add(2 * 5 * time.Second) // reachable again, 2 poll-intervals after t2

	// poll1: track "a" starts playing.
	decidePoll(&st, watchPoll{Time: t1, OK: true, Playing: true, URI: "a", Position: 0}, 10*time.Second)

	// poll2: speaker unreachable. Expect a completely empty decision,
	// regardless of how much wall-clock time has passed.
	d2 := decidePoll(&st, watchPoll{Time: t2, OK: false}, 10*time.Second)
	if d2 != (watchDecision{}) {
		t.Fatalf("poll2 (unreachable): got %+v, want zero-value watchDecision{}", d2)
	}

	// poll3: reachable again, same track "a" still playing. The gap
	// during the outage must NOT be counted - ListenedDelta must be
	// exactly (t3 - t2), the interval since the UNREACHABLE poll's
	// timestamp, not since poll1 and not the full outage duration.
	d3 := decidePoll(&st, watchPoll{Time: t3, OK: true, Playing: true, URI: "a", Position: 5 * time.Second}, 10*time.Second)
	if d3.StartNew || d3.ClosePrevious {
		t.Fatalf("poll3: got %+v, want StartNew=false ClosePrevious=false", d3)
	}
	want := t3.Sub(t2)
	if d3.ListenedDelta != want {
		t.Errorf("poll3: ListenedDelta = %v, want exactly %v (measured from the unreachable poll, undercounting the outage)", d3.ListenedDelta, want)
	}
}

func TestEstimatedStartTime(t *testing.T) {
	base := mustTime(t, "2024-01-01T00:10:00Z")

	cases := []struct {
		name     string
		position time.Duration
		want     time.Time
	}{
		{"position zero returns time unchanged", 0, base},
		{"position 30s", 30 * time.Second, base.Add(-30 * time.Second)},
		{"position 3m10s", 3*time.Minute + 10*time.Second, base.Add(-(3*time.Minute + 10*time.Second))},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := estimatedStartTime(watchPoll{Time: base, Position: c.position})
			if !got.Equal(c.want) {
				t.Errorf("estimatedStartTime() = %v, want %v", got, c.want)
			}
		})
	}
}
