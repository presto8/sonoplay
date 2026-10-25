package main

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDBPathPrecedence(t *testing.T) {
	t.Run("flag value wins over everything", func(t *testing.T) {
		savedCfg := cfg
		t.Cleanup(func() { cfg = savedCfg })
		cfg.DB = "/cfg/sonoplay.db"
		cfg.LikesDB = "/cfg/likes.db"
		t.Setenv("SONOS_DB", "/env/sonoplay.db")
		t.Setenv("SONOS_LIKES_DB", "/env/likes.db")

		got, isDefault, err := dbPath("/flag/sonoplay.db")
		if err != nil {
			t.Fatalf("dbPath: %v", err)
		}
		if got != "/flag/sonoplay.db" {
			t.Errorf("dbPath = %q, want the flag value", got)
		}
		if isDefault {
			t.Error("isDefault = true, want false for an explicit flag value")
		}
	})

	t.Run("SONOS_DB wins over everything but the flag", func(t *testing.T) {
		savedCfg := cfg
		t.Cleanup(func() { cfg = savedCfg })
		cfg.DB = "/cfg/sonoplay.db"
		cfg.LikesDB = "/cfg/likes.db"
		t.Setenv("SONOS_DB", "/env/sonoplay.db")
		t.Setenv("SONOS_LIKES_DB", "/env/likes.db")

		got, isDefault, err := dbPath("")
		if err != nil {
			t.Fatalf("dbPath: %v", err)
		}
		if got != "/env/sonoplay.db" {
			t.Errorf("dbPath = %q, want the SONOS_DB value", got)
		}
		if isDefault {
			t.Error("isDefault = true, want false")
		}
	})

	t.Run("db config key wins over the legacy likes fallbacks", func(t *testing.T) {
		savedCfg := cfg
		t.Cleanup(func() { cfg = savedCfg })
		cfg.DB = "/cfg/sonoplay.db"
		cfg.LikesDB = "/cfg/likes.db"
		t.Setenv("SONOS_DB", "")
		t.Setenv("SONOS_LIKES_DB", "/env/likes.db")

		got, isDefault, err := dbPath("")
		if err != nil {
			t.Fatalf("dbPath: %v", err)
		}
		if got != "/cfg/sonoplay.db" {
			t.Errorf("dbPath = %q, want the db config value", got)
		}
		if isDefault {
			t.Error("isDefault = true, want false")
		}
	})

	t.Run("SONOS_LIKES_DB is honored for backward compatibility", func(t *testing.T) {
		savedCfg := cfg
		t.Cleanup(func() { cfg = savedCfg })
		cfg.DB = ""
		cfg.LikesDB = "/cfg/likes.db"
		t.Setenv("SONOS_DB", "")
		t.Setenv("SONOS_LIKES_DB", "/env/likes.db")

		got, isDefault, err := dbPath("")
		if err != nil {
			t.Fatalf("dbPath: %v", err)
		}
		if got != "/env/likes.db" {
			t.Errorf("dbPath = %q, want the legacy SONOS_LIKES_DB value", got)
		}
		if isDefault {
			t.Error("isDefault = true, want false")
		}
	})

	t.Run("likes_db config key is the last explicit fallback", func(t *testing.T) {
		savedCfg := cfg
		t.Cleanup(func() { cfg = savedCfg })
		cfg.DB = ""
		cfg.LikesDB = "/cfg/likes.db"
		t.Setenv("SONOS_DB", "")
		t.Setenv("SONOS_LIKES_DB", "")

		got, isDefault, err := dbPath("")
		if err != nil {
			t.Fatalf("dbPath: %v", err)
		}
		if got != "/cfg/likes.db" {
			t.Errorf("dbPath = %q, want the legacy likes_db config value", got)
		}
		if isDefault {
			t.Error("isDefault = true, want false")
		}
	})

	t.Run("falls back to XDG data dir when nothing else is set", func(t *testing.T) {
		savedCfg := cfg
		t.Cleanup(func() { cfg = savedCfg })
		cfg.DB = ""
		cfg.LikesDB = ""
		t.Setenv("SONOS_DB", "")
		t.Setenv("SONOS_LIKES_DB", "")
		t.Setenv("XDG_DATA_HOME", "/xdg-data")

		got, isDefault, err := dbPath("")
		if err != nil {
			t.Fatalf("dbPath: %v", err)
		}
		want := filepath.Join("/xdg-data", "sonoplay", "sonoplay.db")
		if got != want {
			t.Errorf("dbPath = %q, want %q", got, want)
		}
		if !isDefault {
			t.Error("isDefault = false, want true for the fallback default path")
		}
	})
}

func TestUserDataDirFallsBackToHomeLocalShare(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/testuser")

	got, err := userDataDir()
	if err != nil {
		t.Fatalf("userDataDir: %v", err)
	}
	want := filepath.Join("/home/testuser", ".local", "share")
	if got != want {
		t.Errorf("userDataDir() = %q, want %q", got, want)
	}
}

// --- schema migration ---

func TestMigrateDBFreshDatabaseReachesCurrentVersionWithBothTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonoplay.db")
	db, err := openDB(path, false)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", version, currentSchemaVersion)
	}

	if err := addLike(db, "http://host/a.mp3", "A", "Artist", "Album"); err != nil {
		t.Errorf("addLike on a fresh db: %v", err)
	}
	if err := recordPlay(db, time.Now().UTC(), "http://host/a.mp3", "A", "Artist", "Album"); err != nil {
		t.Errorf("recordPlay on a fresh db: %v", err)
	}
}

func TestMigrateDBFromV1PreservesExistingLikes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonoplay.db")

	// Simulate a pre-upgrade (version 1, likes-only) database by hand,
	// WITHOUT going through migrateDB/openDB.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := migrateToV1(db); err != nil {
		t.Fatalf("migrateToV1: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := addLike(db, "http://host/a.mp3", "A", "Artist", "Album"); err != nil {
		t.Fatalf("addLike into v1 db: %v", err)
	}
	if err := addLike(db, "http://host/b.mp3", "B", "Artist2", "Album2"); err != nil {
		t.Fatalf("addLike into v1 db: %v", err)
	}
	db.Close()

	// Now run the real migration path.
	db, err = openDB(path, false)
	if err != nil {
		t.Fatalf("openDB after simulated v1: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("user_version after migration = %d, want %d", version, currentSchemaVersion)
	}

	// The most important check: the pre-existing likes must still be
	// there, untouched.
	songs, err := listLikedSongs(db)
	if err != nil {
		t.Fatalf("listLikedSongs after migration: %v", err)
	}
	if len(songs) != 2 {
		t.Fatalf("listLikedSongs after migration = %d songs, want 2", len(songs))
	}

	history, err := listLikeHistory(db)
	if err != nil {
		t.Fatalf("listLikeHistory after migration: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("listLikeHistory after migration = %d entries, want 2", len(history))
	}
	if history[0].URI != "http://host/b.mp3" || history[1].URI != "http://host/a.mp3" {
		t.Errorf("history after migration = %+v, want a.mp3 and b.mp3 preserved", history)
	}

	// And the plays table must now exist and be usable.
	if err := recordPlay(db, time.Now().UTC(), "http://host/c.mp3", "C", "", ""); err != nil {
		t.Errorf("recordPlay after migration: %v", err)
	}
}

func TestMigrateDBAtCurrentVersionIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonoplay.db")
	db, err := openDB(path, false)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if err := addLike(db, "http://host/a.mp3", "A", "", ""); err != nil {
		t.Fatalf("addLike: %v", err)
	}

	if err := migrateDB(db); err != nil {
		t.Fatalf("migrateDB on an already-current db: %v", err)
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d (unchanged)", version, currentSchemaVersion)
	}
	songs, err := listLikedSongs(db)
	if err != nil {
		t.Fatalf("listLikedSongs: %v", err)
	}
	if len(songs) != 1 {
		t.Errorf("listLikedSongs = %d songs, want 1 (unchanged)", len(songs))
	}
}

func TestMigrateDBRejectsFutureSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonoplay.db")
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	if err := migrateDB(db); err == nil {
		t.Error("migrateDB with user_version=99: expected an error, got nil")
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 99 {
		t.Errorf("user_version after failed migrateDB = %d, want unchanged 99", version)
	}
}

func TestMigrateDBIsSafeToRunTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonoplay.db")

	// Simulate two separate sonoplay invocations against the same file.
	db1, err := openDB(path, false)
	if err != nil {
		t.Fatalf("first openDB: %v", err)
	}
	if err := addLike(db1, "http://host/a.mp3", "A", "", ""); err != nil {
		t.Fatalf("addLike: %v", err)
	}
	db1.Close()

	db2, err := openDB(path, false)
	if err != nil {
		t.Fatalf("second openDB: %v", err)
	}
	defer db2.Close()

	songs, err := listLikedSongs(db2)
	if err != nil {
		t.Fatalf("listLikedSongs: %v", err)
	}
	if len(songs) != 1 {
		t.Errorf("listLikedSongs after reopening = %d songs, want 1", len(songs))
	}
}

// --- likes.db -> sonoplay.db rename ---

// captureStderr temporarily redirects os.Stderr to a pipe for the
// duration of f, returning everything written to it. Used to verify
// openDB's one-time rename notice (and its absence, when no rename
// happens).
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()

	f()

	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(out)
}

func TestOpenDBNoRenameWhenNoLikesDBExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sonoplay.db")

	var db *sql.DB
	var err error
	stderr := captureStderr(t, func() {
		db, err = openDB(path, true)
	})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if stderr != "" {
		t.Errorf("stderr = %q, want no rename notice", stderr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %q to exist: %v", path, err)
	}
}

func TestOpenDBRenamesOldLikesDBAndPreservesData(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "likes.db")
	newPath := filepath.Join(dir, "sonoplay.db")

	// Simulate a pre-upgrade user's real likes.db: version 1 schema,
	// with an existing like.
	oldDB, err := sql.Open("sqlite", oldPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := migrateToV1(oldDB); err != nil {
		t.Fatalf("migrateToV1: %v", err)
	}
	if _, err := oldDB.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := addLike(oldDB, "http://host/a.mp3", "A", "Artist", "Album"); err != nil {
		t.Fatalf("addLike: %v", err)
	}
	oldDB.Close()

	var db *sql.DB
	stderr := captureStderr(t, func() {
		db, err = openDB(newPath, true)
	})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if stderr == "" {
		t.Error("stderr = \"\", want a rename notice")
	}
	if !strings.Contains(stderr, oldPath) || !strings.Contains(stderr, newPath) {
		t.Errorf("stderr = %q, want it to mention both %q and %q", stderr, oldPath, newPath)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old likes.db still exists (or stat errored unexpectedly): %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("expected %q to exist after rename: %v", newPath, err)
	}

	songs, err := listLikedSongs(db)
	if err != nil {
		t.Fatalf("listLikedSongs after rename: %v", err)
	}
	if len(songs) != 1 || songs[0].URI != "http://host/a.mp3" {
		t.Errorf("listLikedSongs after rename = %+v, want the pre-existing like preserved", songs)
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("user_version after rename+migration = %d, want %d", version, currentSchemaVersion)
	}
	if err := recordPlay(db, time.Now().UTC(), "http://host/b.mp3", "B", "", ""); err != nil {
		t.Errorf("recordPlay after rename+migration: %v", err)
	}
}

func TestOpenDBDoesNotRenameWhenPathIsExplicit(t *testing.T) {
	dir := t.TempDir()
	// An old likes.db exists in the directory, but the user has
	// explicitly pointed --db at a different, not-yet-existing path in
	// the same directory. isDefault=false means openDB must never look
	// for (or touch) any likes.db here.
	likesDBFile := filepath.Join(dir, "likes.db")
	explicitPath := filepath.Join(dir, "explicit.db")

	oldDB, err := sql.Open("sqlite", likesDBFile+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := migrateToV1(oldDB); err != nil {
		t.Fatalf("migrateToV1: %v", err)
	}
	if _, err := oldDB.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := addLike(oldDB, "http://host/a.mp3", "A", "", ""); err != nil {
		t.Fatalf("addLike: %v", err)
	}
	oldDB.Close()

	var db *sql.DB
	stderr := captureStderr(t, func() {
		db, err = openDB(explicitPath, false)
	})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if stderr != "" {
		t.Errorf("stderr = %q, want no rename notice for an explicit path", stderr)
	}
	if _, err := os.Stat(likesDBFile); err != nil {
		t.Errorf("likes.db should be left untouched: %v", err)
	}
	songs, err := listLikedSongs(db)
	if err != nil {
		t.Fatalf("listLikedSongs: %v", err)
	}
	if len(songs) != 0 {
		t.Errorf("explicit.db should start empty, got %d liked songs", len(songs))
	}
}

func TestOpenDBDoesNotRenameWhenDestinationAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "likes.db")
	newPath := filepath.Join(dir, "sonoplay.db")

	// sonoplay.db already exists (e.g. created by a prior run), created
	// BEFORE any likes.db shows up - note isDefault=false here purely so
	// this setup step itself can't trigger a rename (there's nothing to
	// rename yet anyway, but this keeps the setup unambiguous).
	newDB, err := openDB(newPath, false)
	if err != nil {
		t.Fatalf("create sonoplay.db up front: %v", err)
	}
	if err := addLike(newDB, "http://host/new.mp3", "New", "", ""); err != nil {
		t.Fatalf("addLike into sonoplay.db: %v", err)
	}
	newDB.Close()

	// Now an old likes.db also happens to exist alongside it (left over
	// from before the upgrade, never cleaned up).
	oldDB, err := sql.Open("sqlite", oldPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := migrateToV1(oldDB); err != nil {
		t.Fatalf("migrateToV1: %v", err)
	}
	if _, err := oldDB.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := addLike(oldDB, "http://host/old.mp3", "Old", "", ""); err != nil {
		t.Fatalf("addLike: %v", err)
	}
	oldDB.Close()

	// sonoplay.db already exists, so no rename should be attempted, even
	// though a likes.db also exists.
	var db *sql.DB

	stderr := captureStderr(t, func() {
		db, err = openDB(newPath, true)
	})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if stderr != "" {
		t.Errorf("stderr = %q, want no rename notice when sonoplay.db already exists", stderr)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("likes.db should be left untouched: %v", err)
	}

	songs, err := listLikedSongs(db)
	if err != nil {
		t.Fatalf("listLikedSongs: %v", err)
	}
	if len(songs) != 1 || songs[0].URI != "http://host/new.mp3" {
		t.Errorf("listLikedSongs = %+v, want only the pre-existing sonoplay.db content (new.mp3)", songs)
	}
}
