package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenDBCreatesParentDirAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "sonoplay.db")

	db1, err := openDB(path, false)
	if err != nil {
		t.Fatalf("first openDB(%q): %v", path, err)
	}
	db1.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected database file to exist at %q: %v", path, err)
	}

	// Opening again (as a second invocation of sonoplay would) must not
	// error, and must not wipe existing data.
	db2, err := openDB(path, false)
	if err != nil {
		t.Fatalf("second openDB(%q): %v", path, err)
	}
	defer db2.Close()

	if err := addLike(db2, "http://host/a.mp3", "A", "Artist", "Album"); err != nil {
		t.Fatalf("addLike after reopen: %v", err)
	}
	songs, err := listLikedSongs(db2)
	if err != nil {
		t.Fatalf("listLikedSongs: %v", err)
	}
	if len(songs) != 1 {
		t.Fatalf("listLikedSongs after reopen = %d songs, want 1", len(songs))
	}
}

func TestAddLikeRejectsEmptyURI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonoplay.db")
	db, err := openDB(path, false)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if err := addLike(db, "", "Title", "Artist", "Album"); err == nil {
		t.Error("addLike with empty URI: expected an error, got nil")
	}
}

func TestAddLikeRecordsEveryLikeAsANewRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonoplay.db")
	db, err := openDB(path, false)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Like the same URI three times - this should NOT dedupe or update
	// in place; it's a history, so three rows should exist.
	for i := 0; i < 3; i++ {
		if err := addLike(db, "http://host/a.mp3", "A", "Artist", "Album"); err != nil {
			t.Fatalf("addLike #%d: %v", i, err)
		}
	}

	history, err := listLikeHistory(db)
	if err != nil {
		t.Fatalf("listLikeHistory: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("listLikeHistory returned %d entries, want 3", len(history))
	}

	songs, err := listLikedSongs(db)
	if err != nil {
		t.Fatalf("listLikedSongs: %v", err)
	}
	if len(songs) != 1 {
		t.Fatalf("listLikedSongs returned %d distinct songs, want 1", len(songs))
	}
	if songs[0].LikeCount != 3 {
		t.Errorf("LikeCount = %d, want 3", songs[0].LikeCount)
	}
}

func TestListLikedSongsOrderingAndMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "likes.db")
	db, err := openDB(path, false)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Like "a" first, then "b" - "b" should sort first (most recently
	// liked first). Then re-like "a" with different metadata (as if
	// retagged) - listLikedSongs should report "a" first now (its most
	// recent like moved it to the front) and use the NEW metadata, not
	// the old.
	mustLike := func(uri, title, artist, album string) {
		t.Helper()
		if err := addLike(db, uri, title, artist, album); err != nil {
			t.Fatalf("addLike(%q): %v", uri, err)
		}
	}
	mustLike("http://host/a.mp3", "Old Title A", "Artist", "Album")
	mustLike("http://host/b.mp3", "Title B", "Artist", "Album")
	mustLike("http://host/a.mp3", "New Title A", "Artist", "Album")

	songs, err := listLikedSongs(db)
	if err != nil {
		t.Fatalf("listLikedSongs: %v", err)
	}
	if len(songs) != 2 {
		t.Fatalf("listLikedSongs returned %d songs, want 2", len(songs))
	}
	if songs[0].URI != "http://host/a.mp3" {
		t.Errorf("songs[0].URI = %q, want the most-recently-liked (a.mp3)", songs[0].URI)
	}
	if songs[0].Title != "New Title A" {
		t.Errorf("songs[0].Title = %q, want the metadata from the most recent like", songs[0].Title)
	}
	if songs[0].LikeCount != 2 {
		t.Errorf("songs[0].LikeCount = %d, want 2", songs[0].LikeCount)
	}
	if songs[1].URI != "http://host/b.mp3" {
		t.Errorf("songs[1].URI = %q, want b.mp3", songs[1].URI)
	}
	if songs[1].LikeCount != 1 {
		t.Errorf("songs[1].LikeCount = %d, want 1", songs[1].LikeCount)
	}
}

func TestListLikeHistoryNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "likes.db")
	db, err := openDB(path, false)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	for _, uri := range []string{"u1", "u2", "u3"} {
		if err := addLike(db, uri, "", "", ""); err != nil {
			t.Fatalf("addLike(%q): %v", uri, err)
		}
	}

	history, err := listLikeHistory(db)
	if err != nil {
		t.Fatalf("listLikeHistory: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("len(history) = %d, want 3", len(history))
	}
	wantOrder := []string{"u3", "u2", "u1"}
	for i, want := range wantOrder {
		if history[i].URI != want {
			t.Errorf("history[%d].URI = %q, want %q (newest first)", i, history[i].URI, want)
		}
	}
	// Sanity check LikedAt actually got set to something recent/parseable.
	if time.Since(history[0].LikedAt) > time.Minute || time.Since(history[0].LikedAt) < 0 {
		t.Errorf("history[0].LikedAt = %v, want roughly now", history[0].LikedAt)
	}
}

func TestDeleteLikesRemovesAllRowsForURI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "likes.db")
	db, err := openDB(path, false)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	for i := 0; i < 2; i++ {
		if err := addLike(db, "http://host/a.mp3", "A", "", ""); err != nil {
			t.Fatalf("addLike a #%d: %v", i, err)
		}
	}
	if err := addLike(db, "http://host/b.mp3", "B", "", ""); err != nil {
		t.Fatalf("addLike b: %v", err)
	}

	removed, err := deleteLikes(db, "http://host/a.mp3")
	if err != nil {
		t.Fatalf("deleteLikes: %v", err)
	}
	if removed != 2 {
		t.Errorf("deleteLikes removed = %d, want 2", removed)
	}

	songs, err := listLikedSongs(db)
	if err != nil {
		t.Fatalf("listLikedSongs: %v", err)
	}
	if len(songs) != 1 || songs[0].URI != "http://host/b.mp3" {
		t.Errorf("listLikedSongs after delete = %+v, want only b.mp3 remaining", songs)
	}
}

func TestDeleteLikesOfUnlikedURIRemovesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "likes.db")
	db, err := openDB(path, false)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	removed, err := deleteLikes(db, "http://host/never-liked.mp3")
	if err != nil {
		t.Fatalf("deleteLikes: %v", err)
	}
	if removed != 0 {
		t.Errorf("deleteLikes of never-liked URI removed = %d, want 0", removed)
	}
}
