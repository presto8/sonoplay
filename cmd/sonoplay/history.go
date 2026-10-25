// This file implements storage for play history: one row per song the
// user explicitly started playing. See runPlay's recordPlay calls (in
// main.go) for what actually gets recorded and why - in short, only a
// single song started deliberately (by file/URL, queue position or
// search selector, or a single Spotify track link), never a batch of
// several files, a wildcard expansion, a whole directory, likes --play,
// skip/bump, or anything that happened to start playing on its own
// (e.g. the queue auto-advancing, or something started from the Sonos
// app) - there's no way to distinguish those cases from a speaker's
// perspective after the fact, and recording them anyway would flood
// history with noise the user never explicitly chose. This file is the
// storage layer, mirroring the structure of likes.go.

package main

import (
	"database/sql"
	"fmt"
	"time"
)

// play is a single row in the plays table: one song the user explicitly
// started playing, and when.
type play struct {
	ID                   int64
	PlayedAt             time.Time // UTC
	URI                  string
	Title, Artist, Album string
}

// recordPlay records that uri (with the given metadata, best-effort -
// any of title/artist/album may be empty if unknown) was explicitly
// played at playedAt (normally time.Now(), but accepted as a parameter
// for testability).
func recordPlay(db *sql.DB, playedAt time.Time, uri, title, artist, album string) error {
	_, err := db.Exec(
		`INSERT INTO plays (played_at, uri, title, artist, album) VALUES (?, ?, ?, ?, ?)`,
		playedAt.UTC().Format(time.RFC3339Nano),
		uri, title, artist, album,
	)
	if err != nil {
		return fmt.Errorf("insert play: %w", err)
	}
	return nil
}

// listPlays returns the most recent plays, newest (most recently
// played) first. limit <= 0 means no limit (return all). If since is
// non-zero, only plays with PlayedAt >= since are included.
func listPlays(db *sql.DB, limit int, since time.Time) ([]play, error) {
	q := `SELECT id, played_at, uri, title, artist, album FROM plays`
	var args []any
	if !since.IsZero() {
		q += ` WHERE played_at >= ?`
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += ` ORDER BY played_at DESC, id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list plays: %w", err)
	}
	defer rows.Close()

	var plays []play
	for rows.Next() {
		p, err := scanPlay(rows)
		if err != nil {
			return nil, fmt.Errorf("scan play: %w", err)
		}
		plays = append(plays, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list plays: %w", err)
	}
	return plays, nil
}

// playScanner is satisfied by both *sql.Row and *sql.Rows, letting
// scanPlay be shared between listPlays (which uses Query/Rows) and
// mostRecentPlay (which uses QueryRow/Row).
type playScanner interface {
	Scan(dest ...any) error
}

// scanPlay scans a single plays row (in the column order produced by the
// SELECTs in this file: id, played_at, uri, title, artist, album) into
// a play, parsing its timestamp column.
func scanPlay(row playScanner) (play, error) {
	var p play
	var playedAt string
	if err := row.Scan(&p.ID, &playedAt, &p.URI, &p.Title, &p.Artist, &p.Album); err != nil {
		return play{}, err
	}
	var err error
	p.PlayedAt, err = time.Parse(time.RFC3339Nano, playedAt)
	if err != nil {
		return play{}, fmt.Errorf("parse played_at %q: %w", playedAt, err)
	}
	return p, nil
}

// playCount is one row of the "most played" ranking from topPlayed.
type playCount struct {
	URI, Title, Artist, Album string
	PlayCount                 int
	LastPlayed                time.Time // UTC
}

// topPlayed ranks songs by how many times they've been explicitly
// played, most-played first (ties broken by most recently played).
// Grouped by URI. limit <= 0 means no limit.
func topPlayed(db *sql.DB, limit int) ([]playCount, error) {
	q := `
SELECT
	uri,
	(SELECT title FROM plays p2 WHERE p2.uri = plays.uri ORDER BY p2.id DESC LIMIT 1) AS title,
	(SELECT artist FROM plays p2 WHERE p2.uri = plays.uri ORDER BY p2.id DESC LIMIT 1) AS artist,
	(SELECT album FROM plays p2 WHERE p2.uri = plays.uri ORDER BY p2.id DESC LIMIT 1) AS album,
	COUNT(*) AS play_count,
	MAX(played_at) AS last_played
FROM plays
GROUP BY uri
ORDER BY play_count DESC, last_played DESC
`
	var args []any
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("top played: %w", err)
	}
	defer rows.Close()

	var counts []playCount
	for rows.Next() {
		var c playCount
		var lastPlayed string
		if err := rows.Scan(&c.URI, &c.Title, &c.Artist, &c.Album, &c.PlayCount, &lastPlayed); err != nil {
			return nil, fmt.Errorf("scan top played row: %w", err)
		}
		c.LastPlayed, err = time.Parse(time.RFC3339Nano, lastPlayed)
		if err != nil {
			return nil, fmt.Errorf("parse last played %q: %w", lastPlayed, err)
		}
		counts = append(counts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("top played: %w", err)
	}
	return counts, nil
}

// mostRecentPlay returns the single most recent play (by PlayedAt, then
// id, descending), or ok=false if there are no plays at all. excludeURI,
// if non-empty, skips plays with that URI - used by "like --last" to
// find the previous song when the current one (already known to the
// caller, e.g. from positionInfo) shouldn't be re-suggested as "the last
// song".
func mostRecentPlay(db *sql.DB, excludeURI string) (p play, ok bool, err error) {
	q := `SELECT id, played_at, uri, title, artist, album FROM plays`
	var args []any
	if excludeURI != "" {
		q += ` WHERE uri != ?`
		args = append(args, excludeURI)
	}
	q += ` ORDER BY played_at DESC, id DESC LIMIT 1`

	p, err = scanPlay(db.QueryRow(q, args...))
	if err != nil {
		if err == sql.ErrNoRows {
			return play{}, false, nil
		}
		return play{}, false, fmt.Errorf("most recent play: %w", err)
	}
	return p, true, nil
}
