package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func mustOpenTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sonoplay.db")
	db, err := openDB(path, false)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestStartPlayAndListPlaysRoundTrip(t *testing.T) {
	db := mustOpenTestDB(t)

	startedAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	id, err := startPlay(db, startedAt, "spotify:track:a", "Title A", "Artist A", "Album A", 180)
	if err != nil {
		t.Fatalf("startPlay (with duration): %v", err)
	}
	if id == 0 {
		t.Error("startPlay returned id 0, want a positive id")
	}

	id2, err := startPlay(db, startedAt.Add(time.Minute), "spotify:track:b", "Title B", "Artist B", "Album B", 0)
	if err != nil {
		t.Fatalf("startPlay (no duration): %v", err)
	}
	if id2 == id {
		t.Error("startPlay returned the same id for two different plays")
	}

	plays, err := listPlays(db, 0, time.Time{})
	if err != nil {
		t.Fatalf("listPlays: %v", err)
	}
	if len(plays) != 2 {
		t.Fatalf("listPlays returned %d plays, want 2", len(plays))
	}

	// Newest first.
	a, b := plays[1], plays[0]
	if a.URI != "spotify:track:a" || a.Title != "Title A" || a.Artist != "Artist A" || a.Album != "Album A" {
		t.Errorf("plays[1] = %+v, want track a's metadata", a)
	}
	if a.DurationSeconds != 180 {
		t.Errorf("plays[1].DurationSeconds = %d, want 180", a.DurationSeconds)
	}
	if a.ListenedSeconds != 0 {
		t.Errorf("plays[1].ListenedSeconds = %d, want 0 (just started)", a.ListenedSeconds)
	}
	if !a.StartedAt.Equal(startedAt) {
		t.Errorf("plays[1].StartedAt = %v, want %v", a.StartedAt, startedAt)
	}
	if !a.LastSeenAt.Equal(startedAt) {
		t.Errorf("plays[1].LastSeenAt = %v, want %v (equal to StartedAt initially)", a.LastSeenAt, startedAt)
	}

	if b.URI != "spotify:track:b" || b.DurationSeconds != 0 {
		t.Errorf("plays[0] = %+v, want track b with DurationSeconds 0 (unknown)", b)
	}
}

func TestUpdatePlayProgressAccumulatesAndBumpsLastSeenAt(t *testing.T) {
	db := mustOpenTestDB(t)

	startedAt := time.Now().UTC().Add(-time.Hour)
	id, err := startPlay(db, startedAt, "spotify:track:a", "A", "", "", 0)
	if err != nil {
		t.Fatalf("startPlay: %v", err)
	}

	deltas := []int{10, 15, 20}
	var lastSeen time.Time
	for i, d := range deltas {
		if err := updatePlayProgress(db, id, d); err != nil {
			t.Fatalf("updatePlayProgress #%d: %v", i, err)
		}
		plays, err := listPlays(db, 0, time.Time{})
		if err != nil {
			t.Fatalf("listPlays after update #%d: %v", i, err)
		}
		if len(plays) != 1 {
			t.Fatalf("listPlays after update #%d returned %d plays, want 1", i, len(plays))
		}
		p := plays[0]
		if p.LastSeenAt.Before(startedAt) {
			t.Errorf("after update #%d, LastSeenAt = %v, want >= StartedAt %v", i, p.LastSeenAt, startedAt)
		}
		if !lastSeen.IsZero() && p.LastSeenAt.Before(lastSeen) {
			t.Errorf("after update #%d, LastSeenAt = %v, want >= previous LastSeenAt %v", i, p.LastSeenAt, lastSeen)
		}
		lastSeen = p.LastSeenAt
	}

	plays, err := listPlays(db, 0, time.Time{})
	if err != nil {
		t.Fatalf("listPlays: %v", err)
	}
	wantTotal := 10 + 15 + 20
	if plays[0].ListenedSeconds != wantTotal {
		t.Errorf("ListenedSeconds = %d, want %d (sum of all updates)", plays[0].ListenedSeconds, wantTotal)
	}
}

func TestUpdatePlayProgressOnNonexistentIDIsANoop(t *testing.T) {
	db := mustOpenTestDB(t)

	// No plays exist at all; updating a made-up id should not error -
	// see updatePlayProgress's doc comment for why this is a no-op
	// rather than an error.
	if err := updatePlayProgress(db, 12345, 30); err != nil {
		t.Errorf("updatePlayProgress(nonexistent id): got error %v, want nil", err)
	}

	plays, err := listPlays(db, 0, time.Time{})
	if err != nil {
		t.Fatalf("listPlays: %v", err)
	}
	if len(plays) != 0 {
		t.Errorf("listPlays = %d plays, want 0 (nothing should have been created)", len(plays))
	}
}

func TestListPlaysOrderingAndLimit(t *testing.T) {
	db := mustOpenTestDB(t)

	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	var uris []string
	for i := 0; i < 5; i++ {
		uri := "spotify:track:" + string(rune('a'+i))
		uris = append(uris, uri)
		if _, err := startPlay(db, base.Add(time.Duration(i)*time.Hour), uri, "", "", "", 0); err != nil {
			t.Fatalf("startPlay #%d: %v", i, err)
		}
	}

	all, err := listPlays(db, 0, time.Time{})
	if err != nil {
		t.Fatalf("listPlays (no limit): %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("listPlays (no limit) = %d plays, want 5", len(all))
	}
	// Newest (last inserted, uris[4]) first.
	for i, want := range []string{"spotify:track:e", "spotify:track:d", "spotify:track:c", "spotify:track:b", "spotify:track:a"} {
		if all[i].URI != want {
			t.Errorf("all[%d].URI = %q, want %q (newest first)", i, all[i].URI, want)
		}
	}

	limited, err := listPlays(db, 2, time.Time{})
	if err != nil {
		t.Fatalf("listPlays (limit=2): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("listPlays (limit=2) = %d plays, want 2", len(limited))
	}
	if limited[0].URI != "spotify:track:e" || limited[1].URI != "spotify:track:d" {
		t.Errorf("listPlays (limit=2) = %+v, want the 2 newest", limited)
	}
}

func TestListPlaysSinceFiltering(t *testing.T) {
	db := mustOpenTestDB(t)

	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	for i, uri := range []string{"old1", "old2", "new1", "new2"} {
		if _, err := startPlay(db, base.Add(time.Duration(i)*time.Hour), uri, "", "", "", 0); err != nil {
			t.Fatalf("startPlay(%q): %v", uri, err)
		}
	}

	since := base.Add(90 * time.Minute) // between old2 (+1h) and new1 (+2h)
	recent, err := listPlays(db, 0, since)
	if err != nil {
		t.Fatalf("listPlays since %v: %v", since, err)
	}
	if len(recent) != 2 {
		t.Fatalf("listPlays since %v = %d plays, want 2", since, len(recent))
	}
	if recent[0].URI != "new2" || recent[1].URI != "new1" {
		t.Errorf("listPlays since %v = %+v, want new1/new2 only, newest first", since, recent)
	}
}

func TestTopPlayedCountsOnlyAboveThresholdButSumsAllListenedTime(t *testing.T) {
	db := mustOpenTestDB(t)

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Song "popular": 3 plays, 2 of them long enough to count, one a
	// quick skip.
	for i, secs := range []int{45, 50, 5} {
		id, err := startPlay(db, base.Add(time.Duration(i)*time.Hour), "popular", "Popular", "Art", "Alb", 200)
		if err != nil {
			t.Fatalf("startPlay popular #%d: %v", i, err)
		}
		if err := updatePlayProgress(db, id, secs); err != nil {
			t.Fatalf("updatePlayProgress popular #%d: %v", i, err)
		}
	}

	// Song "rare": 1 play, long enough to count.
	id, err := startPlay(db, base.Add(10*time.Hour), "rare", "Rare", "Art2", "Alb2", 200)
	if err != nil {
		t.Fatalf("startPlay rare: %v", err)
	}
	if err := updatePlayProgress(db, id, 40); err != nil {
		t.Fatalf("updatePlayProgress rare: %v", err)
	}

	const minListenedSeconds = 30
	ranked, err := topPlayed(db, minListenedSeconds, 0)
	if err != nil {
		t.Fatalf("topPlayed: %v", err)
	}
	if len(ranked) != 2 {
		t.Fatalf("topPlayed = %d rows, want 2", len(ranked))
	}
	if ranked[0].URI != "popular" {
		t.Errorf("ranked[0].URI = %q, want %q (most play count first)", ranked[0].URI, "popular")
	}
	if ranked[0].PlayCount != 2 {
		t.Errorf("ranked[0].PlayCount = %d, want 2 (only plays >= %ds)", ranked[0].PlayCount, minListenedSeconds)
	}
	if ranked[0].TotalListenedSeconds != 45+50+5 {
		t.Errorf("ranked[0].TotalListenedSeconds = %d, want %d (sum of ALL plays, including the skip)", ranked[0].TotalListenedSeconds, 45+50+5)
	}
	if ranked[1].URI != "rare" || ranked[1].PlayCount != 1 {
		t.Errorf("ranked[1] = %+v, want rare with PlayCount 1", ranked[1])
	}

	limited, err := topPlayed(db, minListenedSeconds, 1)
	if err != nil {
		t.Fatalf("topPlayed (limit=1): %v", err)
	}
	if len(limited) != 1 || limited[0].URI != "popular" {
		t.Errorf("topPlayed (limit=1) = %+v, want only popular", limited)
	}
}

func TestMostRecentPlay(t *testing.T) {
	db := mustOpenTestDB(t)

	t.Run("no plays at all", func(t *testing.T) {
		_, ok, err := mostRecentPlay(db, "")
		if err != nil {
			t.Fatalf("mostRecentPlay: %v", err)
		}
		if ok {
			t.Error("ok = true, want false when there are no plays")
		}
	})

	base := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	if _, err := startPlay(db, base, "first", "First", "", "", 0); err != nil {
		t.Fatalf("startPlay first: %v", err)
	}
	if _, err := startPlay(db, base.Add(time.Hour), "second", "Second", "", "", 0); err != nil {
		t.Fatalf("startPlay second: %v", err)
	}

	t.Run("basic case", func(t *testing.T) {
		p, ok, err := mostRecentPlay(db, "")
		if err != nil {
			t.Fatalf("mostRecentPlay: %v", err)
		}
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if p.URI != "second" {
			t.Errorf("mostRecentPlay URI = %q, want %q", p.URI, "second")
		}
	})

	t.Run("excludeURI skips the most recent matching play", func(t *testing.T) {
		p, ok, err := mostRecentPlay(db, "second")
		if err != nil {
			t.Fatalf("mostRecentPlay: %v", err)
		}
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if p.URI != "first" {
			t.Errorf("mostRecentPlay(excludeURI=%q) URI = %q, want %q", "second", p.URI, "first")
		}
	})
}
