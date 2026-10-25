// This file implements the "like"/"unlike"/"likes" commands: a small
// SQLite-backed history of songs the user has liked, independent of the
// speaker's play queue (which is transient and gets cleared/reordered
// all the time). See runLike, runUnlike and runLikes in main.go for the
// command-line side; this file is the storage layer.
//
// Every like is recorded as its own row (liking the same song twice adds
// a second row rather than updating one), so the table doubles as a
// like history, not just a deduped set. listLikedSongs collapses that
// history into one row per distinct song (by URI) for display and for
// "likes --play".

package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// likesSchemaVersion is recorded via PRAGMA user_version so future
// changes to the schema can detect and migrate older databases. There's
// only ever been version 1 so far; bump this and add a migration step in
// openLikesDB if the schema changes.
const likesSchemaVersion = 1

// likeEntry is a single row in the likes table: one like event.
type likeEntry struct {
	ID      int64
	LikedAt time.Time // UTC
	URI     string
	Title   string
	Artist  string
	Album   string
}

// likedSong is one distinct liked song (grouped by URI) for display and
// playback, with aggregate info about how it's been liked over time.
type likedSong struct {
	URI       string
	Title     string
	Artist    string
	Album     string
	LikeCount int
	LastLiked time.Time // UTC; the most recent like of this URI
}

// userDataDir returns $XDG_DATA_HOME, falling back to ~/.local/share if
// unset, per the XDG base directory spec. Unlike os.UserConfigDir (used
// by configFilePath), the standard library has no built-in equivalent
// for the data directory, so this is done by hand; sonoplay's data
// directory use is Linux-first, like the rest of its XDG handling.
func userDataDir() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}

// likesDBPath resolves the path to the likes database, in order of
// precedence: flagValue (the --db flag, empty if not given),
// SONOS_LIKES_DB, the likes_db config key, and finally
// $XDG_DATA_HOME/sonoplay/likes.db (see userDataDir).
func likesDBPath(flagValue string) (string, error) {
	if path := firstNonEmpty(flagValue, os.Getenv("SONOS_LIKES_DB"), cfg.LikesDB); path != "" {
		return path, nil
	}
	dir, err := userDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sonoplay", "likes.db"), nil
}

// openLikesDB opens (creating if necessary, including parent
// directories) the SQLite database at path and ensures its schema is up
// to date. Callers must Close() the returned *sql.DB.
func openLikesDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create likes database directory %q: %w", dir, err)
		}
	}

	// busy_timeout: if another sonoplay invocation (or anything else)
	// holds the database locked for a write, wait up to 5s for it to
	// finish rather than immediately failing with SQLITE_BUSY - likes
	// are small/quick operations, so a short wait is enough and avoids
	// spurious errors from e.g. two commands run back-to-back in a
	// script.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open likes database %q: %w", path, err)
	}

	if err := migrateLikesDB(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("set up likes database %q: %w", path, err)
	}

	return db, nil
}

// migrateLikesDB creates the likes table if it doesn't exist yet. There's
// only ever been one schema version so far, so this just creates it
// outright and stamps user_version; a future schema change would check
// the existing user_version here and migrate forward from it instead.
func migrateLikesDB(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= likesSchemaVersion {
		return nil
	}
	if version != 0 {
		return fmt.Errorf("likes database has schema version %d, newer than this version of sonoplay supports (%d) - upgrade sonoplay", version, likesSchemaVersion)
	}

	const schema = `
CREATE TABLE IF NOT EXISTS likes (
	id       INTEGER PRIMARY KEY,
	liked_at TEXT NOT NULL,
	uri      TEXT NOT NULL,
	title    TEXT NOT NULL DEFAULT '',
	artist   TEXT NOT NULL DEFAULT '',
	album    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS likes_uri ON likes(uri);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, likesSchemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return nil
}

// addLike records a new like of the track identified by uri (required;
// addLike returns an error if uri is empty, since a like with nothing to
// play back is useless) with the given metadata, timestamped now (UTC).
// Liking the same uri again (even immediately) adds another row rather
// than erroring or updating one - see the package doc comment at the top
// of this file for why.
func addLike(db *sql.DB, uri, title, artist, album string) error {
	if uri == "" {
		return fmt.Errorf("can't like a track with no URI")
	}
	_, err := db.Exec(
		`INSERT INTO likes (liked_at, uri, title, artist, album) VALUES (?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
		uri, title, artist, album,
	)
	if err != nil {
		return fmt.Errorf("insert like: %w", err)
	}
	return nil
}

// deleteLikes removes every like of the track identified by uri,
// returning how many rows were removed (0, nil if it was never liked).
func deleteLikes(db *sql.DB, uri string) (removed int, err error) {
	res, err := db.Exec(`DELETE FROM likes WHERE uri = ?`, uri)
	if err != nil {
		return 0, fmt.Errorf("delete likes for %q: %w", uri, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted likes for %q: %w", uri, err)
	}
	return int(n), nil
}

// listLikedSongs returns one entry per distinct liked uri (most recently
// liked first), with LikeCount/LastLiked aggregated across that song's
// full like history. Title/Artist/Album come from whichever like of
// that uri has the highest id (i.e. was inserted last - ids are
// monotonically increasing, unlike timestamps, which could in principle
// tie if two likes land in the same instant); a song's metadata could in
// principle differ between likes (e.g. if retagged), and the most recent
// is the most likely to still be accurate.
func listLikedSongs(db *sql.DB) ([]likedSong, error) {
	const q = `
SELECT uri, title, artist, album, like_count, last_liked
FROM (
	SELECT
		uri, title, artist, album,
		COUNT(*) OVER (PARTITION BY uri) AS like_count,
		MAX(liked_at) OVER (PARTITION BY uri) AS last_liked,
		ROW_NUMBER() OVER (PARTITION BY uri ORDER BY id DESC) AS rn
	FROM likes
)
WHERE rn = 1
ORDER BY last_liked DESC
`
	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("list liked songs: %w", err)
	}
	defer rows.Close()

	var songs []likedSong
	for rows.Next() {
		var s likedSong
		var lastLiked string
		if err := rows.Scan(&s.URI, &s.Title, &s.Artist, &s.Album, &s.LikeCount, &lastLiked); err != nil {
			return nil, fmt.Errorf("scan liked song: %w", err)
		}
		s.LastLiked, err = time.Parse(time.RFC3339Nano, lastLiked)
		if err != nil {
			return nil, fmt.Errorf("parse liked_at %q: %w", lastLiked, err)
		}
		songs = append(songs, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list liked songs: %w", err)
	}
	return songs, nil
}

// listLikeHistory returns every like event, newest first, for
// "likes --history".
func listLikeHistory(db *sql.DB) ([]likeEntry, error) {
	rows, err := db.Query(`SELECT id, liked_at, uri, title, artist, album FROM likes ORDER BY liked_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list like history: %w", err)
	}
	defer rows.Close()

	var entries []likeEntry
	for rows.Next() {
		var e likeEntry
		var likedAt string
		if err := rows.Scan(&e.ID, &likedAt, &e.URI, &e.Title, &e.Artist, &e.Album); err != nil {
			return nil, fmt.Errorf("scan like entry: %w", err)
		}
		e.LikedAt, err = time.Parse(time.RFC3339Nano, likedAt)
		if err != nil {
			return nil, fmt.Errorf("parse liked_at %q: %w", likedAt, err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list like history: %w", err)
	}
	return entries, nil
}
