// This file implements path resolution, opening, and schema migration
// for sonoplay's SQLite database - shared storage for both likes
// (likes.go) and play history (history.go). It used to be likes-only
// (hence the historical "likes.db" default file name and the
// SONOS_LIKES_DB/likes_db fallbacks dbPath still honors below), but as
// of schema version 2 it also holds play history, so the file and the
// functions in this file are no longer likes-specific.

package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

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

// dbPath resolves the path to sonoplay's SQLite database (shared by
// likes and play history), in order of precedence: flagValue (the --db
// flag, empty if not given), SONOS_DB, the db config key, and - for
// backward compatibility with versions of sonoplay that only had likes,
// before the database held play history too - SONOS_LIKES_DB and the
// likes_db config key. Finally, $XDG_DATA_HOME/sonoplay/sonoplay.db (see
// userDataDir).
//
// isDefault reports whether the returned path came from that final
// fallback (true) rather than being explicitly set via a flag, env var,
// or config key (false) - openDB uses this to decide whether attempting
// the one-time likes.db -> sonoplay.db rename (see openDB) is
// appropriate; it never touches an explicitly-configured path.
func dbPath(flagValue string) (path string, isDefault bool, err error) {
	if path := firstNonEmpty(flagValue, os.Getenv("SONOS_DB"), cfg.DB, os.Getenv("SONOS_LIKES_DB"), cfg.LikesDB); path != "" {
		return path, false, nil
	}
	dir, err := userDataDir()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(dir, "sonoplay", "sonoplay.db"), true, nil
}

// openDB opens (creating parent directories, and the file, if
// necessary) the SQLite database at path, migrating its schema to the
// current version if needed, and returns it. Callers must Close() the
// returned *sql.DB.
//
// If isDefault is true (path came from dbPath's fallback, not an
// explicit flag/env/config value) and no file exists yet at path, but a
// file exists at likes.db in the same directory (the old default name,
// from before play history existed), that file is renamed to path
// first, with a one-time notice printed to stderr. This never happens
// for an explicitly-configured path (isDefault == false) - a user who's
// set --db/SONOS_DB/etc explicitly is assumed to know what they're
// doing, and auto-renaming a file they pointed at explicitly would be
// surprising.
func openDB(path string, isDefault bool) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory %q: %w", dir, err)
		}
	}

	if isDefault {
		if err := renameOldLikesDBIfNeeded(path); err != nil {
			return nil, err
		}
	}

	// busy_timeout: if another sonoplay invocation (or anything else)
	// holds the database locked for a write, wait up to 5s for it to
	// finish rather than immediately failing with SQLITE_BUSY - likes
	// and play-history writes are small/quick operations, so a short
	// wait is enough and avoids spurious errors from e.g. two commands
	// run back-to-back in a script.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database %q: %w", path, err)
	}

	if err := migrateDB(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("set up database %q: %w", path, err)
	}

	return db, nil
}

// renameOldLikesDBIfNeeded implements the one-time likes.db ->
// sonoplay.db rename described in openDB's doc comment: if path doesn't
// exist yet but a likes.db does, alongside it, rename likes.db to path
// and print a brief notice to stderr. A no-op if path already exists
// (nothing to rename into) or if there's no likes.db to rename (nothing
// to do).
func renameOldLikesDBIfNeeded(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // path already exists; nothing to rename
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check for database %q: %w", path, err)
	}

	oldPath := filepath.Join(filepath.Dir(path), "likes.db")
	if _, err := os.Stat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return nil // no old likes.db to migrate
		}
		return fmt.Errorf("check for old likes database %q: %w", oldPath, err)
	}

	if err := os.Rename(oldPath, path); err != nil {
		return fmt.Errorf("rename old likes database %q to %q: %w", oldPath, path, err)
	}
	fmt.Fprintf(os.Stderr, "renamed %s to %s: it now also holds play history\n", oldPath, path)
	return nil
}

// currentSchemaVersion is recorded via PRAGMA user_version so future
// changes to the schema can detect and migrate older databases.
const currentSchemaVersion = 2

// schemaMigrations are applied in order, starting from the database's
// current PRAGMA user_version, up to currentSchemaVersion. migrations[i]
// upgrades a database from schema version i to version i+1 - e.g.
// migrations[0] takes a brand-new (version 0) database to version 1,
// migrations[1] takes version 1 to version 2. Each must be safe to run
// against a database that's already at a LATER version than its own
// target - migrateDB only ever calls migrations[i] when the database's
// current version is exactly i, so this is naturally satisfied as long
// as each migration doesn't get called out of order, but double-check
// migrateDB's loop bounds carefully (off-by-one errors here are easy to
// make and nasty to debug against a real user's database).
var schemaMigrations = []func(*sql.DB) error{
	migrateToV1,
	migrateToV2,
}

// migrateDB brings db's schema up to currentSchemaVersion, applying
// whichever of schemaMigrations haven't run yet (per PRAGMA
// user_version). A no-op if the database is already current. Returns an
// error, without modifying anything further, if the database's recorded
// version is newer than this version of sonoplay understands (e.g. the
// database was created by a newer sonoplay and then opened with an
// older one).
func migrateDB(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database has schema version %d, newer than this version of sonoplay supports (%d) - upgrade sonoplay", version, currentSchemaVersion)
	}
	for v := version; v < currentSchemaVersion; v++ {
		if err := schemaMigrations[v](db); err != nil {
			return fmt.Errorf("migrate schema from version %d to %d: %w", v, v+1, err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			return fmt.Errorf("set schema version to %d: %w", v+1, err)
		}
	}
	return nil
}

// migrateToV1 creates the likes table (the schema's original/only table,
// from before play history existed). Safe to run against a totally
// empty/brand-new database.
func migrateToV1(db *sql.DB) error {
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
	_, err := db.Exec(schema)
	return err
}

// migrateToV2 creates the plays table (play history, added alongside
// the sonoplay.db rename). Existing likes data is untouched.
func migrateToV2(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS plays (
	id               INTEGER PRIMARY KEY,
	started_at       TEXT NOT NULL,
	last_seen_at     TEXT NOT NULL,
	listened_seconds INTEGER NOT NULL DEFAULT 0,
	duration_seconds INTEGER,
	uri              TEXT NOT NULL,
	title            TEXT NOT NULL DEFAULT '',
	artist           TEXT NOT NULL DEFAULT '',
	album            TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS plays_started_at ON plays(started_at);
CREATE INDEX IF NOT EXISTS plays_uri ON plays(uri);
`
	_, err := db.Exec(schema)
	return err
}
