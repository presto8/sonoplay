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
	"time"
)

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
