# sonoplay

A minimal Sonos control CLI. Pure Go, no cgo. It talks directly to
a Sonos speaker's local UPnP/SOAP API, so basic playback and queue
control work without a cloud account.

## Install

```
go install github.com/presto8/sonoplay/cmd/sonoplay@latest
```

The repo also includes a [devenv](https://devenv.sh/)-based dev shell
(`devenv.nix` / `devenv.yaml`) for contributors who use Nix. It provides a
`build` script equivalent to:

```
CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/sonoplay ./cmd/sonoplay
```

## Configuration

Options (`ip`, `base-url`, `dest-dir`, `step`) are resolved in order of
precedence, highest first:

1. command-line flags (`--ip`, `--base-url`, `--dest-dir`, `--step`)
2. environment variables (`SONOS_SPEAKER_IP`, `SONOS_BASE_URL`,
   `SONOS_DEST_DIR`, `SONOS_STEP`)
3. a config file at `$XDG_CONFIG_HOME/sonoplay/config` (falling back to
   `~/.config/sonoplay/config` if `$XDG_CONFIG_HOME` is unset)

The config file uses simple `key = value` lines:

```
ip = 192.168.1.50
base_url = http://myhost/
dest_dir = /srv/www/sonos
step = 2
```

- `ip` - the speaker's IP address
- `base_url` - prefix used to turn a bare file name into a playable URL,
  e.g. `http://myhost/`
- `dest_dir` - local directory `play` copies (or transcodes) files into
  before handing them to the speaker, if set
- `step` - default volume step size used by `volume-up`/`volume-down`

`like`/`unlike`/`likes` (and `history`) share a separate SQLite database
(see [Liked songs](#liked-songs) and [Play history](#play-history)
below), whose path follows the same precedence: `--db`, then `SONOS_DB`,
then a `db` config key, falling back to
`$XDG_DATA_HOME/sonoplay/sonoplay.db`
(`~/.local/share/sonoplay/sonoplay.db` if `$XDG_DATA_HOME` is unset).
`SONOS_LIKES_DB`/`likes_db` still work too, for compatibility with
versions of sonoplay that only had likes - an existing `likes.db` at the
default location is renamed to `sonoplay.db` automatically, once, the
first time it's opened after upgrading.

## Commands

| Command | Description |
| --- | --- |
| `play <file-or-url>` | play a file/URL on the speaker (default if no command matches) |
| `volume-up`, `up`, `u` | increase volume (`--step`, default 2) |
| `volume-down`, `down`, `d` | decrease volume (`--step`, default 2) |
| `mute-toggle`, `mute` | toggle mute state |
| `mute-on` | mute the speaker |
| `mute-off` | unmute the speaker |
| `list`, `ls` | list the current play queue (`-a`/`--all` for the full list) |
| `find`, `f` | search the queue by title/artist/album/file name |
| `remove`, `rm` | remove tracks from the play queue (e.g. `rm 1 5 9`, `rm 1-9`, `rm @amazing`) |
| `bump`, `up-next` | move matched/selected tracks to play right after the current track |
| `like` | like the current track, matched/selected queue tracks, or `--last` played |
| `unlike` | remove the current or matched/selected tracks from likes |
| `likes` | list liked songs (`--play` to queue/play them all) |
| `history` | show play history (`--top` for most-played) |
| `status`, `s` | show transport state, volume, and now playing |
| `next`, `n` | skip to the next track |
| `prev`, `previous`, `p` | skip to the previous track |
| `stop` | stop playback |

Run `sonoplay <command> -h` for full per-command help, including flags
and selector syntax. A bare `sonoplay <file-or-url>` is shorthand for
`sonoplay play <file-or-url>`.

### Searching the queue

`find` searches the queue by title, artist, album, or file name (every
given word must match, case-insensitively, in any field):

```
sonoplay find amazing grace
```

`play` and `remove`/`rm` (and `bump`) also accept an `@pattern` selector
in place of a numeric position, to act on searched tracks directly:

```
sonoplay play @amazing
sonoplay rm @amazing
sonoplay bump @amazing
```

Several `@pattern` arguments narrow the search further rather than
searching separately: `@foo @bar` matches tracks containing both foo and
bar (the same as `@"foo bar"`), not foo or bar:

```
sonoplay play @newton @grace
```

If `@pattern` matches more than one track, `play` requires `--first` to
pick the first match, and `remove`/`bump` require `-y`/`--yes` to confirm
acting on all of them; otherwise the matches are listed and nothing is
changed.

## Liked songs

`like` records a song in a persistent SQLite database, separate from
(and surviving) the speaker's play queue. With no arguments, it likes
whatever is currently playing; with a selector (same syntax as
`remove`/`bump`), it likes matched/selected tracks in the queue instead:

```
sonoplay like
sonoplay like 5
sonoplay like @amazing
```

Liking an already-liked song adds another like event rather than
erroring or no-op'ing - it's a history, not a toggle:

```
sonoplay likes            # one row per song, with its total like count
sonoplay likes --history  # every like event, newest first
```

`unlike` removes a song from likes entirely (all of its like history, not
just the most recent event), using the same no-args/selector rules as
`like`. `likes --play` queues every liked song and starts playing them
(appending without interrupting playback unless `--now` is given, or
nothing is currently playing).

Liked tracks are replayed by their bare URL/file; this works for ordinary
files and streams, but special per-service metadata (e.g. Spotify) isn't
preserved, so such liked tracks may not replay correctly with
`likes --play`.

## Play history

`play` records a song to a persistent play-history database whenever a
single song actually starts playing right now - by file/URL, queue
position/selector, or a single Spotify track link:

```
sonoplay song.mp3
sonoplay play 5
sonoplay play @amazing
```

Deliberately **not** recorded: several files/URLs in one command
(including a shell wildcard like `*.mp3`, or a whole directory), a
Spotify album/playlist/episode/show/artist link, `likes --play`,
`next`/`prev`/`bump`, or anything merely added to the queue without
playing immediately (e.g. `play song.mp3` while something else is
already playing). The goal is a history of songs you deliberately picked
one at a time, not a log of everything that ever passed through the
queue.

```
sonoplay history             # the last 20 plays, newest first
sonoplay history --since today
sonoplay history --top       # most-played songs, by play count
```

A ♥ marks a song in the list that's also in your likes. `like --last`
likes the most recent play in history that isn't the song currently
playing - handy right after skipping a track you want to keep:

```
sonoplay next
sonoplay like --last
```

## Spotify share links

`play` accepts a Spotify share URL (e.g.
`https://open.spotify.com/track/...`) or `spotify:` URI directly, and
queues it on the speaker. Track, album, playlist, episode, show, and
artist (radio) links are supported.

Note: as of this writing, current Sonos S2 firmware may reject
third-party/non-Sonos-app playback of linked streaming-service content
outright (UPnP error 800), regardless of setup; see
[SoCo/SoCo#969](https://github.com/SoCo/SoCo/issues/969).

## License

GPLv3. See [LICENSE](LICENSE) for the full text.
