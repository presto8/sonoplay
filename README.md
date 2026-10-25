# sonoplay

A minimal Sonos control CLI. Pure Go stdlib, no cgo. It talks directly to
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

## Commands

| Command | Description |
| --- | --- |
| `play <file-or-url>` | play a file/URL on the speaker (default if no command matches) |
| `volume-up`, `up` | increase volume (`--step`, default 2) |
| `volume-down`, `down` | decrease volume (`--step`, default 2) |
| `mute-toggle`, `mute` | toggle mute state |
| `mute-on` | mute the speaker |
| `mute-off` | unmute the speaker |
| `list`, `ls` | list the current play queue (`-a`/`--all` for the full list) |
| `find`, `f` | search the queue by title/artist/album/file name |
| `remove`, `rm` | remove tracks from the play queue (e.g. `rm 1 5 9`, `rm 1-9`, `rm @amazing`) |
| `bump`, `up-next` | move matched/selected tracks to play right after the current track |
| `status` | show transport state, volume, and now playing |
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
