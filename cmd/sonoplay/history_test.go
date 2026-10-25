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

func TestRecordPlayAndListPlaysRoundTrip(t *testing.T) {
	db := mustOpenTestDB(t)

	playedAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := recordPlay(db, playedAt, "spotify:track:a", "Title A", "Artist A", "Album A"); err != nil {
		t.Fatalf("recordPlay a: %v", err)
	}
	if err := recordPlay(db, playedAt.Add(time.Minute), "spotify:track:b", "Title B", "Artist B", "Album B"); err != nil {
		t.Fatalf("recordPlay b: %v", err)
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
	if !a.PlayedAt.Equal(playedAt) {
		t.Errorf("plays[1].PlayedAt = %v, want %v", a.PlayedAt, playedAt)
	}
	if b.URI != "spotify:track:b" {
		t.Errorf("plays[0].URI = %q, want %q", b.URI, "spotify:track:b")
	}
}

func TestListPlaysOrderingAndLimit(t *testing.T) {
	db := mustOpenTestDB(t)

	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	var uris []string
	for i := 0; i < 5; i++ {
		uri := "spotify:track:" + string(rune('a'+i))
		uris = append(uris, uri)
		if err := recordPlay(db, base.Add(time.Duration(i)*time.Hour), uri, "", "", ""); err != nil {
			t.Fatalf("recordPlay #%d: %v", i, err)
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
		if err := recordPlay(db, base.Add(time.Duration(i)*time.Hour), uri, "", "", ""); err != nil {
			t.Fatalf("recordPlay(%q): %v", uri, err)
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

func TestTopPlayedCountsAndOrdering(t *testing.T) {
	db := mustOpenTestDB(t)

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Song "popular": played 3 times.
	for i := 0; i < 3; i++ {
		if err := recordPlay(db, base.Add(time.Duration(i)*time.Hour), "popular", "Popular", "Art", "Alb"); err != nil {
			t.Fatalf("recordPlay popular #%d: %v", i, err)
		}
	}

	// Song "rare": played once.
	if err := recordPlay(db, base.Add(10*time.Hour), "rare", "Rare", "Art2", "Alb2"); err != nil {
		t.Fatalf("recordPlay rare: %v", err)
	}

	ranked, err := topPlayed(db, 0)
	if err != nil {
		t.Fatalf("topPlayed: %v", err)
	}
	if len(ranked) != 2 {
		t.Fatalf("topPlayed = %d rows, want 2", len(ranked))
	}
	if ranked[0].URI != "popular" {
		t.Errorf("ranked[0].URI = %q, want %q (most play count first)", ranked[0].URI, "popular")
	}
	if ranked[0].PlayCount != 3 {
		t.Errorf("ranked[0].PlayCount = %d, want 3", ranked[0].PlayCount)
	}
	if ranked[1].URI != "rare" || ranked[1].PlayCount != 1 {
		t.Errorf("ranked[1] = %+v, want rare with PlayCount 1", ranked[1])
	}

	limited, err := topPlayed(db, 1)
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
	if err := recordPlay(db, base, "first", "First", "", ""); err != nil {
		t.Fatalf("recordPlay first: %v", err)
	}
	if err := recordPlay(db, base.Add(time.Hour), "second", "Second", "", ""); err != nil {
		t.Fatalf("recordPlay second: %v", err)
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
