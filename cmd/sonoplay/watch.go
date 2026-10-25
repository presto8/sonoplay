// This file implements the pure decision logic behind "sonoplay watch":
// given one poll's worth of observed speaker state and the watcher's
// running memory of previous polls, decide whether to start a new play
// record, add listened time to the currently-open one, or do nothing.
//
// Deliberately, nothing here touches the network or a database - it's
// plain data in, plain data (plus an explicit, caller-owned state
// struct) out, so it's fully unit-testable without either. The actual
// polling loop and the startPlay/updatePlayProgress calls live in
// runWatch (built separately, in main.go), which calls decidePoll once
// per poll and acts on the watchDecision it returns.
package main

import "time"

// watchPoll is a single poll's worth of observed speaker state, as
// collected by runWatch (built separately, elsewhere) from
// getTransportState/getPositionInfo/parseDIDLItem. decidePoll never
// touches the network or a database itself - it only reasons about this
// already-extracted data, which is what makes it unit-testable without
// either.
type watchPoll struct {
	Time     time.Time // wall-clock time this poll was taken
	OK       bool      // false if the speaker couldn't be reached this poll (e.g. a network error) - if false, every other field is meaningless/zero and must be ignored
	Playing  bool      // true iff the speaker was reachable (OK) AND its transport state was exactly "PLAYING" (not PAUSED_PLAYBACK, not TRANSITIONING, not STOPPED)
	URI      string    // the current track's URI; "" if nothing is loaded (e.g. STOPPED with an empty queue, or not yet played anything this session)
	Title    string
	Artist   string
	Album    string
	Position time.Duration // elapsed position within the current track; 0 if unknown or not applicable (e.g. URI == "")
	Duration time.Duration // the current track's total duration; 0 if unknown
}

// watchState is the watcher's memory between polls - the caller (runWatch)
// owns one of these for the lifetime of the "sonoplay watch" process and
// passes the same pointer to every decidePoll call, in poll order.
// Its zero value is the correct starting state (no play open yet, no
// prior poll to compare against).
type watchState struct {
	openURI      string        // URI of the currently-open play record, "" if none is open
	lastPosition time.Duration // the previous poll's Position for openURI, used to detect the track restarting (e.g. repeat); meaningless/stale once openURI changes, but that's fine since it's only consulted in the "same URI" branch
	lastPollTime time.Time     // wall-clock time of the previous call to decidePoll (any previous call - OK or not); zero if this is the very first call
}

// watchDecision is what runWatch should do in response to one poll, as
// decided by decidePoll. It's plain data (not an action decidePoll takes
// itself) specifically so the decision logic stays side-effect-free and
// testable: runWatch is responsible for turning a watchDecision into the
// actual startPlay/updatePlayProgress calls, using the watchPoll it
// already has in hand for the new-track metadata.
type watchDecision struct {
	// ClosePrevious is true when a previously-open play has just ended
	// (the track changed, playback stopped, or the same track
	// restarted - see StartNew). There is nothing further for runWatch
	// to DO about the closed play here - its listened time was already
	// fully accounted for by previous decidePoll calls' ListenedDelta -
	// this field exists purely so runWatch can, if it wants, log/print
	// something like "stopped: <previous track>" for user feedback. Do
	// not require callers to act on this field for correctness.
	ClosePrevious bool

	// StartNew is true when runWatch should call startPlay for the
	// CURRENT poll's track (the watchPoll passed to this same
	// decidePoll call has the URI/Title/Artist/Album/Duration to use).
	// This covers three situations: the very first track seen all
	// session, a genuinely different track than whatever was playing
	// before, and the same track restarting from the beginning (e.g.
	// repeat - see restartThreshold).
	StartNew bool

	// ListenedDelta is additional listened time runWatch should add (via
	// updatePlayProgress) to whichever play is open as of THIS poll
	// (the one just started, if StartNew is also true this same call;
	// otherwise the one that was already open). Always exactly 0 when
	// StartNew is true (a freshly-started play has no elapsed listened
	// time yet as of the instant it starts - time starts accumulating
	// from the NEXT poll onward).
	ListenedDelta time.Duration
}

// decidePoll decides what runWatch should do for one poll of the
// speaker, given the watcher's state so far (st, updated in place by
// this call) and the poll just taken (p). Calls must be made in
// chronological order (p.Time strictly increasing across calls, or at
// least non-decreasing) for sensible results, since ListenedDelta is
// computed as a time difference from the previous call.
//
// restartThreshold controls restart detection: if the SAME track's
// Position has moved BACKWARD by at least this much since the last poll
// (e.g. jumped from 3:10 to 0:02, which a simple repeat would do), that's
// treated as the track restarting from the beginning - the previous play
// is closed and a new one started - rather than as ordinary small
// seek/reporting jitter, which should NOT trigger a restart. Pass
// something like 10 * time.Second as a reasonable default (the caller,
// runWatch, decides the actual value - this function just takes it as a
// parameter, don't hard-code it here).
//
// KNOWN APPROXIMATION (document this prominently, it's intentional):
// ListenedDelta for a "same track continuing" poll is the FULL interval
// since the previous poll whenever the CURRENT poll shows Playing ==
// true, regardless of whether playback was ALSO true for the entire
// interval in between (which isn't knowable from polling alone - e.g. a
// brief pause-then-resume entirely within one ~5s poll interval isn't
// detected, and is simply counted as fully listened). This is an
// accepted tradeoff for a polling-based watcher with a short interval;
// do not try to "fix" this with sub-interval estimation, that's out of
// scope.
//
// Also note: when the speaker is unreachable for one or more consecutive
// polls (p.OK == false) and then becomes reachable again showing the
// SAME track still playing, the unreachable gap is NOT counted as
// listened time (ListenedDelta only ever spans from the most recent
// poll - successful OR not - to now), which UNDERcounts rather than
// OVERcounts across an outage. This is intentional: we don't know what
// actually happened during the gap, so the conservative choice is to
// not credit it.
func decidePoll(st *watchState, p watchPoll, restartThreshold time.Duration) watchDecision {
	// Whatever happens below, the next call's delta should be measured
	// from this poll's timestamp - including when this poll failed, so
	// an outage's duration is never credited as listened time (see the
	// doc comment above). Do this via defer so every return path below
	// gets it, rather than repeating the assignment in each branch.
	defer func() { st.lastPollTime = p.Time }()

	if !p.OK {
		// Unreachable this poll: every other field of p is meaningless,
		// so there's nothing to decide - do nothing, and (per the defer
		// above) just remember when this attempt was made.
		return watchDecision{}
	}

	if p.URI == "" {
		// Nothing loaded (e.g. STOPPED with an empty queue). Close
		// whatever was open, if anything; if nothing was open, this is
		// a no-op (idempotent across repeated empty-URI polls).
		var d watchDecision
		if st.openURI != "" {
			d.ClosePrevious = true
		}
		st.openURI = ""
		st.lastPosition = 0
		return d
	}

	restarted := p.URI == st.openURI && st.lastPosition-p.Position >= restartThreshold

	if p.URI != st.openURI || restarted {
		// Either a genuinely different track than whatever was open
		// before (including the very first track seen all session,
		// where st.openURI == ""), or the same track restarting from
		// the beginning (e.g. repeat). Either way: close whatever was
		// open (if anything) and start a new play for p.
		var d watchDecision
		if st.openURI != "" {
			d.ClosePrevious = true
		}
		d.StartNew = true
		// ListenedDelta is always exactly 0 here: a freshly-started
		// play has no elapsed listened time yet as of the instant it
		// starts.
		st.openURI = p.URI
		st.lastPosition = p.Position
		return d
	}

	// Same track continuing. Credit the full interval since the
	// previous poll if this poll shows Playing == true (see the "KNOWN
	// APPROXIMATION" doc comment above) - but only if there WAS a
	// previous poll to diff against (st.lastPollTime is the zero value
	// on the very first call).
	var delta time.Duration
	if p.Playing && !st.lastPollTime.IsZero() {
		delta = p.Time.Sub(st.lastPollTime)
	}
	st.lastPosition = p.Position
	return watchDecision{ListenedDelta: delta}
}

// estimatedStartTime back-calculates when the track in p must have
// started playing, based on its current elapsed Position - used by
// runWatch specifically for the very first poll of a "sonoplay watch"
// process if it finds a track ALREADY playing partway through (so the
// recorded play's StartedAt reflects when the song actually started,
// not merely when the watcher happened to start up). If p.Position is 0
// (unknown, or genuinely at the start), this just returns p.Time
// unchanged.
func estimatedStartTime(p watchPoll) time.Time {
	return p.Time.Add(-p.Position)
}
