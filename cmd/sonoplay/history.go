// This file implements storage for play history: one row per song
// actually listened to, recorded by the (separately-wired-up) "watch"
// command as it observes the speaker's transport state. See runWatch
// and runHistory in main.go for the command-line side; this file is the
// storage layer, mirroring the structure of likes.go.
//
// Unlike likes (which are instantaneous, fire-and-forget events), a play
// has a lifecycle: it starts (startPlay) when a track begins, and is
// updated (updatePlayProgress) as the watcher continues to observe it
// actually playing, accumulating listened_seconds and refreshing
// last_seen_at. This lets the watcher distinguish a brief skip from a
// full listen after the fact (see topPlayed's minListenedSeconds
// threshold), without needing to know in advance how long a track will
// be listened to.

package main

import (
	"database/sql"
	"fmt"
	"time"
)

// play is a single row in the plays table: one listen of a track,
// started when it began playing and updated as it continues to be
// listened to (see updatePlayProgress). DurationSeconds is 0 if the
// track's duration wasn't known when the play was recorded.
type play struct {
	ID                   int64
	StartedAt            time.Time // UTC
	LastSeenAt           time.Time // UTC
	ListenedSeconds      int
	DurationSeconds      int
	URI                  string
	Title, Artist, Album string
}

// startPlay records the start of a new play: startedAt (not necessarily
// "now" - see runWatch, which back-calculates this when the watcher
// starts up mid-track), the track's identity/metadata, and its known
// duration (0 if unknown). listened_seconds starts at 0; last_seen_at is
// set equal to startedAt initially. Returns the new row's id, for later
// updatePlayProgress calls.
func startPlay(db *sql.DB, startedAt time.Time, uri, title, artist, album string, durationSeconds int) (id int64, err error) {
	res, err := db.Exec(
		`INSERT INTO plays (started_at, last_seen_at, listened_seconds, duration_seconds, uri, title, artist, album) VALUES (?, ?, 0, ?, ?, ?, ?, ?)`,
		startedAt.UTC().Format(time.RFC3339Nano),
		startedAt.UTC().Format(time.RFC3339Nano),
		durationSeconds,
		uri, title, artist, album,
	)
	if err != nil {
		return 0, fmt.Errorf("insert play: %w", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get new play id: %w", err)
	}
	return id, nil
}

// updatePlayProgress adds additionalListenedSeconds to the play
// identified by id's listened_seconds and bumps its last_seen_at to now
// (UTC). If id doesn't exist (e.g. a stale id from before the database
// was reset), this is a no-op, not an error: the caller (the watcher
// loop) tracks progress incrementally and has no reasonable recovery
// path for a vanished row other than to keep going, so treating it as a
// hard error would just add noise without giving the caller anything
// useful to do about it.
func updatePlayProgress(db *sql.DB, id int64, additionalListenedSeconds int) error {
	_, err := db.Exec(
		`UPDATE plays SET listened_seconds = listened_seconds + ?, last_seen_at = ? WHERE id = ?`,
		additionalListenedSeconds,
		time.Now().UTC().Format(time.RFC3339Nano),
		id,
	)
	if err != nil {
		return fmt.Errorf("update play progress for id %d: %w", id, err)
	}
	return nil
}

// listPlays returns the most recent plays, newest (most recently
// started) first. limit <= 0 means no limit (return all). If since is
// non-zero, only plays with StartedAt >= since are included.
func listPlays(db *sql.DB, limit int, since time.Time) ([]play, error) {
	q := `SELECT id, started_at, last_seen_at, listened_seconds, duration_seconds, uri, title, artist, album FROM plays`
	var args []any
	if !since.IsZero() {
		q += ` WHERE started_at >= ?`
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += ` ORDER BY started_at DESC, id DESC`
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
// SELECTs in this file: id, started_at, last_seen_at, listened_seconds,
// duration_seconds, uri, title, artist, album) into a play, parsing its
// two timestamp columns.
func scanPlay(row playScanner) (play, error) {
	var p play
	var startedAt, lastSeenAt string
	if err := row.Scan(&p.ID, &startedAt, &lastSeenAt, &p.ListenedSeconds, &p.DurationSeconds, &p.URI, &p.Title, &p.Artist, &p.Album); err != nil {
		return play{}, err
	}
	var err error
	p.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return play{}, fmt.Errorf("parse started_at %q: %w", startedAt, err)
	}
	p.LastSeenAt, err = time.Parse(time.RFC3339Nano, lastSeenAt)
	if err != nil {
		return play{}, fmt.Errorf("parse last_seen_at %q: %w", lastSeenAt, err)
	}
	return p, nil
}

// playCount is one row of the "most played" ranking from topPlayed.
type playCount struct {
	URI, Title, Artist, Album string
	PlayCount                 int
	TotalListenedSeconds      int
	LastPlayed                time.Time // UTC
}

// topPlayed ranks songs by how many times they've been played,
// most-played first (ties broken by most total listened time, then most
// recently played). A play only counts toward PlayCount if its
// ListenedSeconds >= minListenedSeconds (excluding brief skips from the
// ranking - see runHistory's --top, which is expected to pass something
// like 30 here); TotalListenedSeconds sums ALL of that song's listened
// time regardless of this threshold (so a mix of skips and full listens
// still reports accurate total listening time, only the COUNT excludes
// skips). Grouped by URI. limit <= 0 means no limit.
func topPlayed(db *sql.DB, minListenedSeconds, limit int) ([]playCount, error) {
	q := `
SELECT
	uri,
	(SELECT title FROM plays p2 WHERE p2.uri = plays.uri ORDER BY p2.id DESC LIMIT 1) AS title,
	(SELECT artist FROM plays p2 WHERE p2.uri = plays.uri ORDER BY p2.id DESC LIMIT 1) AS artist,
	(SELECT album FROM plays p2 WHERE p2.uri = plays.uri ORDER BY p2.id DESC LIMIT 1) AS album,
	SUM(CASE WHEN listened_seconds >= ? THEN 1 ELSE 0 END) AS play_count,
	SUM(listened_seconds) AS total_listened_seconds,
	MAX(started_at) AS last_played
FROM plays
GROUP BY uri
ORDER BY play_count DESC, total_listened_seconds DESC, last_played DESC
`
	args := []any{minListenedSeconds}
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
		if err := rows.Scan(&c.URI, &c.Title, &c.Artist, &c.Album, &c.PlayCount, &c.TotalListenedSeconds, &lastPlayed); err != nil {
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

// mostRecentPlay returns the single most recent play (by StartedAt,
// then id, descending), or ok=false if there are no plays at all.
// excludeURI, if non-empty, skips plays with that URI - used by
// "like --last" to find the previous song when the current one (already
// known to the caller, e.g. from positionInfo) has already been
// recorded as a play and shouldn't be re-suggested as "the last song".
func mostRecentPlay(db *sql.DB, excludeURI string) (p play, ok bool, err error) {
	q := `SELECT id, started_at, last_seen_at, listened_seconds, duration_seconds, uri, title, artist, album FROM plays`
	var args []any
	if excludeURI != "" {
		q += ` WHERE uri != ?`
		args = append(args, excludeURI)
	}
	q += ` ORDER BY started_at DESC, id DESC LIMIT 1`

	p, err = scanPlay(db.QueryRow(q, args...))
	if err != nil {
		if err == sql.ErrNoRows {
			return play{}, false, nil
		}
		return play{}, false, fmt.Errorf("most recent play: %w", err)
	}
	return p, true, nil
}
