package main

import (
	"reflect"
	"testing"
)

// intsEqual compares two int slices by content, treating nil and an
// empty-but-non-nil slice as equal (several functions under test return
// one or the other depending on code path, which isn't semantically
// meaningful for callers).
func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTrackFileName(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want string
	}{
		{
			name: "plain path",
			uri:  "/music/songs/red.mp3",
			want: "red.mp3",
		},
		{
			name: "url with encoded spaces",
			uri:  "http://host/Amazing%20Grace.mp3",
			want: "Amazing Grace.mp3",
		},
		{
			name: "no slash at all",
			uri:  "red.mp3",
			want: "red.mp3",
		},
		{
			name: "trailing slash yields empty name",
			uri:  "/music/songs/",
			want: "",
		},
		{
			name: "invalid percent-encoding falls back to raw segment",
			uri:  "/music/badname%zz.mp3",
			want: "badname%zz.mp3",
		},
		{
			name: "empty string",
			uri:  "",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := trackFileName(c.uri)
			if got != c.want {
				t.Errorf("trackFileName(%q) = %q, want %q", c.uri, got, c.want)
			}
		})
	}
}

func TestTrackMatches(t *testing.T) {
	cases := []struct {
		name  string
		track queueTrack
		words []string
		want  bool
	}{
		{
			name:  "single word matches title",
			track: queueTrack{Title: "Amazing Grace"},
			words: []string{"amazing"},
			want:  true,
		},
		{
			name:  "single word matches artist",
			track: queueTrack{Title: "Song", Artist: "John Newton"},
			words: []string{"newton"},
			want:  true,
		},
		{
			name:  "single word matches album",
			track: queueTrack{Title: "Song", Album: "Olney Hymns"},
			words: []string{"olney"},
			want:  true,
		},
		{
			name:  "single word matches only file name",
			track: queueTrack{URI: "http://host/Amazing%20Grace.mp3"},
			words: []string{"grace"},
			want:  true,
		},
		{
			name: "multiple words must all match (AND), across different fields",
			track: queueTrack{
				Title:  "Amazing Grace",
				Artist: "John Newton",
				Album:  "Olney Hymns",
			},
			words: []string{"amazing", "newton", "olney"},
			want:  true,
		},
		{
			name: "multiple words where one is missing fails",
			track: queueTrack{
				Title:  "Amazing Grace",
				Artist: "John Newton",
			},
			words: []string{"amazing", "nonexistent"},
			want:  false,
		},
		{
			name:  "case-insensitive match",
			track: queueTrack{Title: "AMAZING GRACE"},
			words: []string{"amazing grace"},
			want:  true,
		},
		{
			name:  "word matching nothing",
			track: queueTrack{Title: "Amazing Grace", Artist: "John Newton", Album: "Olney Hymns"},
			words: []string{"nonexistent"},
			want:  false,
		},
		{
			name:  "empty words slice always returns false",
			track: queueTrack{Title: "Amazing Grace"},
			words: []string{},
			want:  false,
		},
		{
			name:  "nil words slice always returns false",
			track: queueTrack{Title: "Amazing Grace"},
			words: nil,
			want:  false,
		},
		{
			name: "a single word does NOT span two fields across the NUL separator",
			// Title ends with "ama" and Artist starts with "zing"; the
			// haystack joins fields with "\x00", so "amazing" as a
			// whole is never a contiguous substring even though the
			// two halves are adjacent fields.
			track: queueTrack{Title: "Song ama", Artist: "zing Jam"},
			words: []string{"amazing"},
			want:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := trackMatches(c.track, c.words)
			if got != c.want {
				t.Errorf("trackMatches(%+v, %v) = %v, want %v", c.track, c.words, got, c.want)
			}
		})
	}
}

func TestMatchQueue(t *testing.T) {
	tracks := []queueTrack{
		{Title: "Amazing Grace", Artist: "Traditional"},
		{Title: "Beta Song", Artist: "Some Band"},
		{Title: "Gamma Song", Artist: "Amazing Choir"},
		{Title: "Delta Song", Artist: "Other Band"},
	}

	cases := []struct {
		name   string
		tracks []queueTrack
		words  []string
		want   []int
	}{
		{
			name:   "no matches on empty tracks",
			tracks: nil,
			words:  []string{"anything"},
			want:   nil,
		},
		{
			name:   "no matches in non-empty tracks",
			tracks: tracks,
			words:  []string{"nonexistent"},
			want:   nil,
		},
		{
			name:   "one match",
			tracks: tracks,
			words:  []string{"beta"},
			want:   []int{2},
		},
		{
			name:   "multiple matches preserving order",
			tracks: tracks,
			words:  []string{"amazing"},
			want:   []int{1, 3},
		},
		{
			name:   "matches against larger slice with only some matching",
			tracks: tracks,
			words:  []string{"song"},
			want:   []int{2, 3, 4},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matchQueue(c.tracks, c.words)
			if !intsEqual(got, c.want) {
				t.Errorf("matchQueue(%v, %v) = %v, want %v", c.tracks, c.words, got, c.want)
			}
		})
	}
}

func TestResolveSelectors(t *testing.T) {
	tracks := []queueTrack{
		{Title: "Alpha Song", Artist: "Artist One"},    // 1
		{Title: "Beta Song", Artist: "Artist Two"},     // 2
		{Title: "Gamma Alpha", Artist: "Artist Three"}, // 3
		{Title: "Delta Song", Artist: "Artist Four"},   // 4
		{Title: "Epsilon Song", Artist: "Artist Five"}, // 5
	}

	t.Run("single number selector", func(t *testing.T) {
		pos, hits, err := resolveSelectors([]string{"3"}, tracks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !intsEqual(pos, []int{3}) {
			t.Errorf("positions = %v, want [3]", pos)
		}
		if len(hits) != 0 {
			t.Errorf("patternHits = %v, want none", hits)
		}
	})

	t.Run("range with dash form", func(t *testing.T) {
		pos, _, err := resolveSelectors([]string{"2-4"}, tracks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !intsEqual(pos, []int{2, 3, 4}) {
			t.Errorf("positions = %v, want [2 3 4]", pos)
		}
	})

	t.Run("range with dotdot form", func(t *testing.T) {
		pos, _, err := resolveSelectors([]string{"2..4"}, tracks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !intsEqual(pos, []int{2, 3, 4}) {
			t.Errorf("positions = %v, want [2 3 4]", pos)
		}
	})

	t.Run("open-ended range to end of queue", func(t *testing.T) {
		pos, _, err := resolveSelectors([]string{"4-"}, tracks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !intsEqual(pos, []int{4, 5}) {
			t.Errorf("positions = %v, want [4 5]", pos)
		}
	})

	t.Run("pattern selector matching exactly one track", func(t *testing.T) {
		pos, hits, err := resolveSelectors([]string{"@beta"}, tracks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !intsEqual(pos, []int{2}) {
			t.Errorf("positions = %v, want [2]", pos)
		}
		if len(hits) != 1 || hits[0].pattern != "beta" || !intsEqual(hits[0].positions, []int{2}) {
			t.Errorf("patternHits = %+v, want one hit for %q matching [2]", hits, "beta")
		}
	})

	t.Run("pattern selector matching several tracks records all in patternHits", func(t *testing.T) {
		pos, hits, err := resolveSelectors([]string{"@song"}, tracks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantPos := []int{1, 2, 4, 5}
		if !intsEqual(pos, wantPos) {
			t.Errorf("positions = %v, want %v", pos, wantPos)
		}
		if len(hits) != 1 || hits[0].pattern != "song" || !intsEqual(hits[0].positions, wantPos) {
			t.Errorf("patternHits = %+v, want one hit for %q matching %v", hits, "song", wantPos)
		}
	})

	t.Run("mixing number and pattern selectors dedups and sorts", func(t *testing.T) {
		// "3" overlaps with "@alpha" (which matches 1 and 3); final
		// result should be deduped and sorted ascending.
		pos, hits, err := resolveSelectors([]string{"3", "@alpha"}, tracks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantPos := []int{1, 3}
		if !intsEqual(pos, wantPos) {
			t.Errorf("positions = %v, want %v", pos, wantPos)
		}
		if len(hits) != 1 || hits[0].pattern != "alpha" || !intsEqual(hits[0].positions, []int{1, 3}) {
			t.Errorf("patternHits = %+v, want one hit for %q matching [1 3]", hits, "alpha")
		}
	})

	t.Run("out of range number selector is an error", func(t *testing.T) {
		_, _, err := resolveSelectors([]string{"99"}, tracks)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("invalid range syntax is an error", func(t *testing.T) {
		_, _, err := resolveSelectors([]string{"not-a-number"}, tracks)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("start greater than end is an error", func(t *testing.T) {
		_, _, err := resolveSelectors([]string{"4-2"}, tracks)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("pattern matching no tracks is an error", func(t *testing.T) {
		_, _, err := resolveSelectors([]string{"@nonexistent"}, tracks)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("bare @ with nothing after it is an error", func(t *testing.T) {
		_, _, err := resolveSelectors([]string{"@"}, tracks)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("empty args slice yields empty positions and nil error", func(t *testing.T) {
		pos, hits, err := resolveSelectors([]string{}, tracks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(pos) != 0 {
			t.Errorf("positions = %v, want empty", pos)
		}
		if len(hits) != 0 {
			t.Errorf("patternHits = %v, want empty", hits)
		}
	})

	t.Run("nil args slice yields empty positions and nil error", func(t *testing.T) {
		pos, hits, err := resolveSelectors(nil, tracks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(pos) != 0 {
			t.Errorf("positions = %v, want empty", pos)
		}
		if len(hits) != 0 {
			t.Errorf("patternHits = %v, want empty", hits)
		}
	})
}

func TestAmbiguousPatternHits(t *testing.T) {
	cases := []struct {
		name string
		hits []resolvePatternHit
		want []resolvePatternHit
	}{
		{
			name: "nil hits yields nil",
			hits: nil,
			want: nil,
		},
		{
			name: "all single-match hits yields nil",
			hits: []resolvePatternHit{
				{pattern: "a", positions: []int{1}},
				{pattern: "b", positions: []int{2}},
			},
			want: nil,
		},
		{
			name: "one multi-match hit among single-match ones is kept, order preserved",
			hits: []resolvePatternHit{
				{pattern: "a", positions: []int{1}},
				{pattern: "b", positions: []int{2, 3}},
				{pattern: "c", positions: []int{4}},
			},
			want: []resolvePatternHit{
				{pattern: "b", positions: []int{2, 3}},
			},
		},
		{
			name: "multiple multi-match hits are all kept, in order",
			hits: []resolvePatternHit{
				{pattern: "a", positions: []int{1, 2}},
				{pattern: "b", positions: []int{3}},
				{pattern: "c", positions: []int{4, 5}},
			},
			want: []resolvePatternHit{
				{pattern: "a", positions: []int{1, 2}},
				{pattern: "c", positions: []int{4, 5}},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ambiguousPatternHits(c.hits)
			if len(got) != len(c.want) {
				t.Fatalf("ambiguousPatternHits(%+v) = %+v, want %+v", c.hits, got, c.want)
			}
			for i := range got {
				if got[i].pattern != c.want[i].pattern || !intsEqual(got[i].positions, c.want[i].positions) {
					t.Errorf("ambiguousPatternHits(%+v)[%d] = %+v, want %+v", c.hits, i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestTerminalSafetyMargin(t *testing.T) {
	cases := []struct {
		rows int
		want int
	}{
		{rows: 0, want: 0},
		{rows: 1, want: 1},
		{rows: 9, want: 1},
		{rows: 10, want: 1},
		{rows: 11, want: 2},
		{rows: 20, want: 2},
		{rows: 24, want: 3},
		{rows: 25, want: 3},
		{rows: 100, want: 10},
	}
	for _, c := range cases {
		got := terminalSafetyMargin(c.rows)
		if got != c.want {
			t.Errorf("terminalSafetyMargin(%d) = %d, want %d", c.rows, got, c.want)
		}
	}
}

func TestVisibleWindow(t *testing.T) {
	type want struct {
		start, end               int
		topEllipsis, botEllipsis bool
	}
	cases := []struct {
		name               string
		total, anchor, max int
		want               want
	}{
		{
			name:  "total fits within maxLines: no truncation",
			total: 5, anchor: 2, max: 10,
			want: want{0, 5, false, false},
		},
		{
			name:  "total equal to maxLines: no truncation",
			total: 10, anchor: 3, max: 10,
			want: want{0, 10, false, false},
		},
		{
			name:  "maxLines too small (2) returns everything untruncated",
			total: 100, anchor: 50, max: 2,
			want: want{0, 100, false, false},
		},
		{
			name:  "maxLines too small (1) returns everything untruncated",
			total: 100, anchor: 50, max: 1,
			want: want{0, 100, false, false},
		},
		{
			name:  "maxLines too small (0) returns everything untruncated",
			total: 100, anchor: 50, max: 0,
			want: want{0, 100, false, false},
		},
		{
			name:  "anchor near start of long list: top window, bottom ellipsis only",
			total: 100, anchor: 0, max: 10,
			want: want{0, 9, false, true},
		},
		{
			name:  "anchor still within start region",
			total: 100, anchor: 5, max: 10,
			want: want{0, 9, false, true},
		},
		{
			name:  "anchor in the middle of a long list: both ellipses",
			total: 100, anchor: 50, max: 10,
			want: want{46, 54, true, true},
		},
		{
			name:  "anchor near end of long list: top ellipsis only",
			total: 100, anchor: 95, max: 10,
			want: want{91, 100, true, false},
		},
		{
			name:  "anchor at the very last item",
			total: 100, anchor: 99, max: 10,
			want: want{91, 100, true, false},
		},
		{
			name:  "anchor == -1 behaves like anchor == 0",
			total: 100, anchor: -1, max: 10,
			want: want{0, 9, false, true},
		},
		{
			name:  "minimum useful maxLines (3) with anchor in the middle",
			total: 10, anchor: 5, max: 3,
			want: want{5, 6, true, true},
		},
		{
			name:  "minimum useful maxLines (3) with anchor near the end",
			total: 10, anchor: 8, max: 3,
			want: want{8, 10, true, false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, end, topE, botE := visibleWindow(c.total, c.anchor, c.max)
			got := want{start, end, topE, botE}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("visibleWindow(%d, %d, %d) = %+v, want %+v", c.total, c.anchor, c.max, got, c.want)
			}
		})
	}
}
