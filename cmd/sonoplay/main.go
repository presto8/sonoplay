// sonoplay - minimal Sonos control tool, pure Go, no cgo.
//
// Subcommands:
//
//	sonoplay play [--ip <ip>] [--base-url <url>] [--dest-dir <dir>] <file-path-or-url>
//	sonoplay volume-up   [--ip <ip>] [--step <n>]
//	sonoplay volume-down [--ip <ip>] [--step <n>]
//	sonoplay mute        [--ip <ip>]
//	sonoplay unmute      [--ip <ip>]
//	sonoplay toggle      [--ip <ip>]
//	sonoplay list        [--ip <ip>]
//	sonoplay remove      [--ip <ip>] <selector> [<selector> ...]
//	sonoplay find        [--ip <ip>] <word> [<word> ...]
//	sonoplay bump        [--ip <ip>] <selector> [<selector> ...]
//	sonoplay like        [--ip <ip>] [--db <path>] [<selector> ...]
//	sonoplay unlike      [--ip <ip>] [--db <path>] [<selector> ...]
//	sonoplay likes       [--db <path>]
//
// For backward compatibility, if the first argument isn't a known
// subcommand, it's treated as `play <args...>`, e.g.:
//
//	sonoplay red.mp3
//
// is equivalent to:
//
//	sonoplay play red.mp3
//
// Options (ip, base-url, dest-dir, step) can be set, in order of
// precedence (highest first):
//
//  1. command-line flags (--ip, --base-url, --dest-dir, --step)
//
//  2. environment variables (SONOS_SPEAKER_IP, SONOS_BASE_URL,
//     SONOS_DEST_DIR, SONOS_STEP)
//
//  3. a config file at $XDG_CONFIG_HOME/sonoplay/config (or
//     ~/.config/sonoplay/config if $XDG_CONFIG_HOME is unset), using
//     simple "key = value" lines, e.g.:
//
//     ip = 192.168.1.50
//     base_url = http://myhost/
//     dest_dir = /srv/www/sonos
//     step = 2
//
// like/unlike/likes (and watch/history) share a SQLite database, whose
// path can likewise be set via --db, SONOS_DB, or a db config key
// (SONOS_LIKES_DB/likes_db also still work, for backward compatibility
// with versions of sonoplay that only had likes); see dbPath for the
// default location.
//
// Playing a Spotify share link (see below) additionally needs
// spotify_account and spotify_sn (or SONOS_SPOTIFY_ACCOUNT/SONOS_SPOTIFY_SN),
// but only if sonoplay can't auto-detect them (see discoverSpotifyAccount).
//
// For `play`: the file to play is normally just a name (e.g. "red.mp3"),
// which gets appended to the base URL, e.g. "http://myhost/". A full URL
// (containing "://") can still be passed directly, bypassing the base URL
// entirely.
//
// A Spotify share link (e.g. "https://open.spotify.com/track/...") or
// spotify: URI (e.g. "spotify:album:...") is recognized and queued
// directly on the speaker; this is independent of --base-url/--dest-dir.
// Track, album, playlist, episode, show, and artist (radio) links are
// supported. NOTE: as of this writing, current Sonos S2 firmware rejects
// third-party/non-Sonos-app playback of linked streaming-service content
// outright (UPnP error 800), so this may not work regardless of setup;
// see https://github.com/SoCo/SoCo/issues/969.
//
// If dest-dir is set and the argument is a local file path (not a full
// URL), the file is first copied into that directory before the base-url +
// filename is sent to the speaker. This is for setups where dest-dir is
// the local path backing the web server that base-url points at. If the
// file's extension isn't a format Sonos natively supports and ffmpeg is
// available on PATH, it's transcoded to mp3 instead of copied as-is.
//
// If the speaker is currently playing something, `play` adds the track to
// the end of the queue instead of interrupting it. Pass --now to always
// play immediately, replacing whatever is currently playing.
//
// `mute` always mutes, `unmute` always unmutes, `toggle` flips the current
// mute state.
//
// Build a fully static binary with:
//
//	CGO_ENABLED=0 go build -ldflags="-s -w" -o sonoplay ./cmd/sonoplay
package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	avTransportControlPath = "/MediaRenderer/AVTransport/Control"
	avTransportServiceType = "urn:schemas-upnp-org:service:AVTransport:1"

	renderingControlPath        = "/MediaRenderer/RenderingControl/Control"
	renderingControlServiceType = "urn:schemas-upnp-org:service:RenderingControl:1"

	// ContentDirectory lives under /MediaServer, not /MediaRenderer, unlike
	// AVTransport and RenderingControl.
	contentDirectoryControlPath = "/MediaServer/ContentDirectory/Control"
	contentDirectoryServiceType = "urn:schemas-upnp-org:service:ContentDirectory:1"

	// MusicServices lives at the top level (not under /MediaRenderer or
	// /MediaServer). We only use it to look up which music-service
	// "types" (see spotifyServiceTypeCandidates) are available on the
	// household, to build correct Spotify share-link metadata.
	musicServicesControlPath = "/MusicServices/Control"
	musicServicesServiceType = "urn:schemas-upnp-org:service:MusicServices:1"
)

// appConfig holds options loaded from the config file. Zero values mean
// "not set".
type appConfig struct {
	IP      string
	BaseURL string
	DestDir string
	Step    string

	// SpotifyAccount and SpotifySN override auto-detection of the
	// Sonos-internal Spotify account identifiers needed to play Spotify
	// share links (see discoverSpotifyAccount). Only needed if
	// auto-detection fails (e.g. no Spotify item has ever been saved to
	// Sonos Favorites or played on this system).
	SpotifyAccount string
	SpotifySN      string

	// DB overrides the path to sonoplay's SQLite database, used by
	// like/unlike/likes and watch/history (see dbPath). LikesDB is the
	// older, likes-only name for the same setting, kept as a fallback
	// for existing config files from before the database also held play
	// history.
	DB      string
	LikesDB string
}

// cfg is populated once at startup from the config file, then used as a
// fallback for flag defaults in the run* functions below.
var cfg appConfig

// configFilePath returns $XDG_CONFIG_HOME/sonoplay/config (honoring the XDG
// spec via os.UserConfigDir, which also covers the ~/.config fallback).
func configFilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sonoplay", "config"), nil
}

// loadConfig reads the config file, if present. A missing file is not an
// error; any other read/parse error is returned.
func loadConfig() (appConfig, error) {
	var cfg appConfig

	path, err := configFilePath()
	if err != nil {
		return cfg, nil // no config dir available; proceed with no config
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("open config file %q: %w", path, err)
	}
	defer f.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return cfg, fmt.Errorf("read config file %q: %w", path, err)
	}

	cfg.IP = values["ip"]
	cfg.BaseURL = values["base_url"]
	cfg.DestDir = values["dest_dir"]
	cfg.Step = values["step"]
	cfg.SpotifyAccount = values["spotify_account"]
	cfg.SpotifySN = values["spotify_sn"]
	cfg.DB = values["db"]
	cfg.LikesDB = values["likes_db"]
	return cfg, nil
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// soapEnvelope builds the minimal SOAP body for a UPnP action.
func soapEnvelope(serviceType, action, body string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
  <s:Body>
    <u:%s xmlns:u="%s">
      %s
    </u:%s>
  </s:Body>
</s:Envelope>`, action, serviceType, body, action)
}

// soapRequest POSTs a SOAP action to the speaker and returns the raw
// response body on success.
func soapRequest(ip, path, serviceType, action, body string) ([]byte, error) {
	reqURL := fmt.Sprintf("http://%s:1400%s", ip, path)
	payload := soapEnvelope(serviceType, action, body)

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewBufferString(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", fmt.Sprintf(`"%s#%s"`, serviceType, action))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s failed: HTTP %d: %s", action, resp.StatusCode, respBody)
	}
	return respBody, nil
}

func avTransportCall(ip, action, body string) error {
	_, err := soapRequest(ip, avTransportControlPath, avTransportServiceType, action, body)
	return err
}

func avTransportCallWithResponse(ip, action, body string) ([]byte, error) {
	return soapRequest(ip, avTransportControlPath, avTransportServiceType, action, body)
}

func renderingControlCall(ip, action, body string) ([]byte, error) {
	return soapRequest(ip, renderingControlPath, renderingControlServiceType, action, body)
}

func contentDirectoryCall(ip, action, body string) ([]byte, error) {
	return soapRequest(ip, contentDirectoryControlPath, contentDirectoryServiceType, action, body)
}

func musicServicesCall(ip, action, body string) ([]byte, error) {
	return soapRequest(ip, musicServicesControlPath, musicServicesServiceType, action, body)
}

// xmlEscape escapes s for safe embedding as XML character data (e.g. in a
// URI that may contain "&", "<", etc.).
func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// setAVTransportURI tells the speaker what URL to play (metadata left empty,
// same as soco's play_uri default).
func setAVTransportURI(ip, uri string) error {
	body := fmt.Sprintf(`<InstanceID>0</InstanceID>
      <CurrentURI>%s</CurrentURI>
      <CurrentURIMetaData></CurrentURIMetaData>`, xmlEscape(uri))
	return avTransportCall(ip, "SetAVTransportURI", body)
}

// startPlayback starts playback on the speaker.
func startPlayback(ip string) error {
	body := `<InstanceID>0</InstanceID>
      <Speed>1</Speed>`
	return avTransportCall(ip, "Play", body)
}

// stopPlayback stops playback on the speaker.
func stopPlayback(ip string) error {
	return avTransportCall(ip, "Stop", `<InstanceID>0</InstanceID>`)
}

// getTransportState returns the speaker's current transport state, e.g.
// "PLAYING", "PAUSED_PLAYBACK", "STOPPED", "TRANSITIONING".
func getTransportState(ip string) (string, error) {
	body := `<InstanceID>0</InstanceID>`
	respBody, err := avTransportCallWithResponse(ip, "GetTransportInfo", body)
	if err != nil {
		return "", err
	}

	var parsed struct {
		Body struct {
			Resp struct {
				CurrentTransportState string `xml:"CurrentTransportState"`
			} `xml:"GetTransportInfoResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parse GetTransportInfo response: %w", err)
	}
	return parsed.Body.Resp.CurrentTransportState, nil
}

// positionInfo describes what's currently loaded/playing on the speaker.
type positionInfo struct {
	Track         int
	TrackDuration string
	TrackMetaData string // raw DIDL-Lite XML for the current track (single <item>)
	TrackURI      string
	RelTime       string

	// paused is set by callers (not by getPositionInfo itself, which has
	// no notion of transport state) when the track is loaded but
	// PAUSED_PLAYBACK rather than actively PLAYING.
	paused bool
}

// getPositionInfo returns info about the speaker's current track.
func getPositionInfo(ip string) (positionInfo, error) {
	body := `<InstanceID>0</InstanceID>`
	respBody, err := avTransportCallWithResponse(ip, "GetPositionInfo", body)
	if err != nil {
		return positionInfo{}, err
	}

	var parsed struct {
		Body struct {
			Resp struct {
				Track         int    `xml:"Track"`
				TrackDuration string `xml:"TrackDuration"`
				TrackMetaData string `xml:"TrackMetaData"`
				TrackURI      string `xml:"TrackURI"`
				RelTime       string `xml:"RelTime"`
			} `xml:"GetPositionInfoResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(respBody, &parsed); err != nil {
		return positionInfo{}, fmt.Errorf("parse GetPositionInfo response: %w", err)
	}
	return positionInfo{
		Track:         parsed.Body.Resp.Track,
		TrackDuration: parsed.Body.Resp.TrackDuration,
		TrackMetaData: parsed.Body.Resp.TrackMetaData,
		TrackURI:      parsed.Body.Resp.TrackURI,
		RelTime:       parsed.Body.Resp.RelTime,
	}, nil
}

// nextTrack skips to the next track.
func nextTrack(ip string) error {
	return avTransportCall(ip, "Next", `<InstanceID>0</InstanceID>`)
}

// previousTrack skips to the previous track.
func previousTrack(ip string) error {
	return avTransportCall(ip, "Previous", `<InstanceID>0</InstanceID>`)
}

func playURI(ip, uri string) error {
	if err := setAVTransportURI(ip, uri); err != nil {
		return err
	}
	return startPlayback(ip)
}

// addURIToQueue appends (or, if asNext is true, inserts right after the
// current track) uri to the speaker's play queue, returning the 1-based
// position it landed at and the resulting queue length.
func addURIToQueue(ip, uri string, asNext bool) (firstTrackNumberEnqueued, newQueueLength int, err error) {
	return addURIToQueueWithMetadata(ip, uri, "", asNext)
}

// addURIToQueueWithMetadata is like addURIToQueue, but also lets the
// caller supply DIDL-Lite metadata for the enqueued URI (needed for
// music-service share links, e.g. Spotify; see resolveSpotifyItem).
func addURIToQueueWithMetadata(ip, uri, metadata string, asNext bool) (firstTrackNumberEnqueued, newQueueLength int, err error) {
	next := 0
	if asNext {
		next = 1
	}
	body := fmt.Sprintf(`<InstanceID>0</InstanceID>
      <EnqueuedURI>%s</EnqueuedURI>
      <EnqueuedURIMetaData>%s</EnqueuedURIMetaData>
      <DesiredFirstTrackNumberEnqueued>0</DesiredFirstTrackNumberEnqueued>
      <EnqueueAsNext>%d</EnqueueAsNext>`, xmlEscape(uri), xmlEscape(metadata), next)

	respBody, err := avTransportCallWithResponse(ip, "AddURIToQueue", body)
	if err != nil {
		return 0, 0, err
	}

	var parsed struct {
		Body struct {
			Resp struct {
				FirstTrackNumberEnqueued int `xml:"FirstTrackNumberEnqueued"`
				NewQueueLength           int `xml:"NewQueueLength"`
			} `xml:"AddURIToQueueResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(respBody, &parsed); err != nil {
		return 0, 0, fmt.Errorf("parse AddURIToQueue response: %w", err)
	}
	return parsed.Body.Resp.FirstTrackNumberEnqueued, parsed.Body.Resp.NewQueueLength, nil
}

// removeTrackRangeFromQueue removes numberOfTracks tracks from the queue,
// starting at the given 1-based startingIndex (inclusive).
func removeTrackRangeFromQueue(ip string, startingIndex, numberOfTracks int) error {
	body := fmt.Sprintf(`<InstanceID>0</InstanceID>
      <UpdateID></UpdateID>
      <StartingIndex>%d</StartingIndex>
      <NumberOfTracks>%d</NumberOfTracks>`, startingIndex, numberOfTracks)
	return avTransportCall(ip, "RemoveTrackRangeFromQueue", body)
}

// reorderTrackRangeInQueue moves a contiguous range of numberOfTracks
// tracks, starting at the given 1-based startingIndex, to immediately
// before whatever is at 1-based position insertBefore - evaluated
// against the queue's current (pre-this-call) state. Used by "bump" to
// move tracks to play next. Unlike RemoveTrackRangeFromQueue,
// ReorderTracksInQueue needs an explicit UpdateID (0 here, meaning "any
// revision"), per every reference implementation checked.
func reorderTrackRangeInQueue(ip string, startingIndex, numberOfTracks, insertBefore int) error {
	body := fmt.Sprintf(`<InstanceID>0</InstanceID>
      <StartingIndex>%d</StartingIndex>
      <NumberOfTracks>%d</NumberOfTracks>
      <InsertBefore>%d</InsertBefore>
      <UpdateID>0</UpdateID>`, startingIndex, numberOfTracks, insertBefore)
	return avTransportCall(ip, "ReorderTracksInQueue", body)
}

// getUDN fetches the speaker's device description and returns its UDN
// (without the "uuid:" prefix), needed to address its play queue.
func getUDN(ip string) (string, error) {
	resp, err := http.Get(fmt.Sprintf("http://%s:1400/xml/device_description.xml", ip))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch device description: HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Device struct {
			UDN string `xml:"UDN"`
		} `xml:"device"`
	}
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse device description: %w", err)
	}
	udn := strings.TrimPrefix(parsed.Device.UDN, "uuid:")
	if udn == "" {
		return "", fmt.Errorf("device description has no UDN")
	}
	return udn, nil
}

// switchToQueueAndPlay points the speaker's transport at its own play queue
// and starts playback.
func switchToQueueAndPlay(ip string) error {
	udn, err := getUDN(ip)
	if err != nil {
		return fmt.Errorf("get device UDN: %w", err)
	}
	if err := setAVTransportURI(ip, fmt.Sprintf("x-rincon-queue:%s#0", udn)); err != nil {
		return err
	}
	return startPlayback(ip)
}

// seekTrack jumps to the given 1-based track number within whatever's
// currently loaded (e.g. the queue).
func seekTrack(ip string, trackNumber int) error {
	body := fmt.Sprintf(`<InstanceID>0</InstanceID>
      <Unit>TRACK_NR</Unit>
      <Target>%d</Target>`, trackNumber)
	return avTransportCall(ip, "Seek", body)
}

// playQueueIndex switches the speaker to its own play queue, jumps to the
// given 1-based position, and starts playback.
func playQueueIndex(ip string, index int) error {
	udn, err := getUDN(ip)
	if err != nil {
		return fmt.Errorf("get device UDN: %w", err)
	}
	if err := setAVTransportURI(ip, fmt.Sprintf("x-rincon-queue:%s#0", udn)); err != nil {
		return err
	}
	if err := seekTrack(ip, index); err != nil {
		return fmt.Errorf("seek to queue item %d: %w", index, err)
	}
	return startPlayback(ip)
}

// isPositiveInteger reports whether s consists only of ASCII digits and is
// non-empty (so "3" is, but "", "-3", "3.5", "03x" are not).
func isPositiveInteger(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseTrackRange parses a single remove-command selector into an
// inclusive [start, end] range of 1-based queue positions. Accepted forms:
//
//	"5"      a single position
//	"1-9"    an inclusive range
//	"1..9"   same, alternate syntax
//	"9-"     from 9 to the end of the queue (maxIndex)
func parseTrackRange(selector string, maxIndex int) (start, end int, err error) {
	switch {
	case strings.Contains(selector, ".."):
		startStr, endStr, _ := strings.Cut(selector, "..")
		start, err = strconv.Atoi(strings.TrimSpace(startStr))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid start %q", startStr)
		}
		end, err = strconv.Atoi(strings.TrimSpace(endStr))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid end %q", endStr)
		}
		return start, end, nil

	case strings.Contains(selector, "-"):
		startStr, endStr, _ := strings.Cut(selector, "-")
		start, err = strconv.Atoi(strings.TrimSpace(startStr))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid start %q", startStr)
		}
		endStr = strings.TrimSpace(endStr)
		if endStr == "" {
			// Open-ended range, e.g. "9-": run to the end of the queue.
			end = maxIndex
		} else {
			end, err = strconv.Atoi(endStr)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid end %q", endStr)
			}
		}
		return start, end, nil

	default:
		n, err := strconv.Atoi(strings.TrimSpace(selector))
		if err != nil {
			return 0, 0, fmt.Errorf("not a number, range (A-B, A..B), or open range (A-)")
		}
		return n, n, nil
	}
}

// queueTrack is one entry returned by browseQueue.
type queueTrack struct {
	// ID is the DIDL-Lite item's "id" attribute (e.g. "Q:0/5"), a stable
	// per-slot identifier within the queue. It's used (in preference to
	// URI) to re-locate a specific queue entry after the queue has been
	// reordered, since URI alone can't disambiguate the same track
	// appearing more than once in the queue. Empty for metadata parsed
	// from a context with no DIDL item wrapper (e.g. parseDIDLItem on
	// TrackMetaData, which never carries queue-slot identity anyway).
	ID     string `xml:"id,attr"`
	Title  string `xml:"title"`
	Artist string `xml:"artist"`
	Album  string `xml:"album"`
	URI    string `xml:"res"`
}

// trackKey returns the best available stable identifier for a specific
// queue slot: its DIDL item ID if known, falling back to its URI. Used to
// re-locate the same queue entry across successive queue re-reads (e.g.
// between the single-track moves bump makes), where positions shift but
// identity doesn't. Note that URI-only fallback can't distinguish two
// occurrences of the same track in the queue - ID is unambiguous (it's
// the specific slot), so is always preferred when available.
func trackKey(t queueTrack) string {
	if t.ID != "" {
		return t.ID
	}
	return t.URI
}

// findTrackPosition returns the 1-based position of the first track in
// tracks whose trackKey matches key, or (-1, false) if none does (e.g.
// the track was removed from the queue by something else between reads).
func findTrackPosition(tracks []queueTrack, key string) (int, bool) {
	for i, t := range tracks {
		if trackKey(t) == key {
			return i + 1, true
		}
	}
	return -1, false
}

// queueBrowsePageSize is how many queue entries are requested per
// ContentDirectory Browse call. Sonos (and UPnP media servers generally)
// may silently cap a single Browse response well below any count asked
// for - 100 items is a commonly seen limit - so browseQueue pages through
// StartingIndex rather than trusting one request to return everything.
const queueBrowsePageSize = 100

// browseQueuePage runs a single ContentDirectory Browse call against the
// play queue (object "Q:0"), starting at the given 0-based index and
// asking for up to count items. It returns the tracks on that page, the
// number of items the server actually returned, and the total number of
// tracks in the queue (which may exceed both count and the number
// returned, if the server capped the page smaller than requested).
func browseQueuePage(ip string, startingIndex, count int) (tracks []queueTrack, numberReturned, totalMatches int, err error) {
	body := fmt.Sprintf(`<ObjectID>Q:0</ObjectID>
      <BrowseFlag>BrowseDirectChildren</BrowseFlag>
      <Filter>*</Filter>
      <StartingIndex>%d</StartingIndex>
      <RequestedCount>%d</RequestedCount>
      <SortCriteria></SortCriteria>`, startingIndex, count)

	respBody, err := contentDirectoryCall(ip, "Browse", body)
	if err != nil {
		return nil, 0, 0, err
	}

	// The outer SOAP envelope carries the inner DIDL-Lite XML as ordinary
	// (XML-entity-escaped) character data in <Result>; a normal
	// xml.Unmarshal of the envelope already un-escapes it, so parsed.Result
	// ends up holding literal, well-formed DIDL-Lite XML ready for a
	// second xml.Unmarshal pass below.
	var parsed struct {
		Body struct {
			Resp struct {
				Result         string `xml:"Result"`
				NumberReturned int    `xml:"NumberReturned"`
				TotalMatches   int    `xml:"TotalMatches"`
			} `xml:"BrowseResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(respBody, &parsed); err != nil {
		return nil, 0, 0, fmt.Errorf("parse Browse response: %w", err)
	}

	var didl struct {
		Items []queueTrack `xml:"item"`
	}
	if parsed.Body.Resp.Result != "" {
		if err := xml.Unmarshal([]byte(parsed.Body.Resp.Result), &didl); err != nil {
			return nil, 0, 0, fmt.Errorf("parse DIDL-Lite queue contents: %w", err)
		}
	}

	return didl.Items, parsed.Body.Resp.NumberReturned, parsed.Body.Resp.TotalMatches, nil
}

// browseQueue lists the speaker's current play queue (object "Q:0") via
// the ContentDirectory service's Browse action, paging through
// browseQueuePage as needed so the result always covers the whole queue
// regardless of any per-request cap the server imposes. If count is
// positive, at most that many tracks are returned (still potentially
// requiring multiple page requests to reach); if count is 0, every track
// in the queue is returned. The returned totalMatches is the queue's full
// length, even when fewer tracks than that are returned because of count.
func browseQueue(ip string, count int) (tracks []queueTrack, totalMatches int, err error) {
	want := count
	for {
		pageSize := queueBrowsePageSize
		if want > 0 && want-len(tracks) < pageSize {
			pageSize = want - len(tracks)
		}
		page, numberReturned, total, err := browseQueuePage(ip, len(tracks), pageSize)
		if err != nil {
			return nil, 0, err
		}
		tracks = append(tracks, page...)
		totalMatches = total

		if numberReturned == 0 {
			// Nothing more to fetch (either the queue is exhausted, or the
			// server returned an empty page for some other reason) - stop
			// here rather than looping forever.
			break
		}
		if len(tracks) >= total {
			break
		}
		if want > 0 && len(tracks) >= want {
			break
		}
	}
	return tracks, totalMatches, nil
}

// parseDIDLItem parses a single-item DIDL-Lite XML blob, as returned in
// AVTransport's TrackMetaData field, into a queueTrack. An empty or
// "NOT_IMPLEMENTED" input (both of which the speaker may return) yields a
// zero-value queueTrack with no error.
func parseDIDLItem(didlXML string) (queueTrack, error) {
	if didlXML == "" || didlXML == "NOT_IMPLEMENTED" {
		return queueTrack{}, nil
	}
	var didl struct {
		Items []queueTrack `xml:"item"`
	}
	if err := xml.Unmarshal([]byte(didlXML), &didl); err != nil {
		return queueTrack{}, fmt.Errorf("parse DIDL-Lite track metadata: %w", err)
	}
	if len(didl.Items) == 0 {
		return queueTrack{}, nil
	}
	return didl.Items[0], nil
}

// formatTrack renders a track for display, preferring "Artist - Title
// (Album)" and falling back progressively to whatever fields are present,
// finally to the raw URI if there's no metadata at all.
func formatTrack(t queueTrack) string {
	title := t.Title
	if title == "" {
		title = t.URI
	}
	switch {
	case t.Artist != "" && t.Album != "":
		return fmt.Sprintf("%s - %s (%s)", t.Artist, title, t.Album)
	case t.Artist != "":
		return fmt.Sprintf("%s - %s", t.Artist, title)
	default:
		return title
	}
}

// trackFileName extracts the file name portion of a track's URI, for
// matching against tracks whose only identifying text is their file name
// (common for local files played with no, or minimal, metadata). The URI
// is treated as a slash-separated path (true of both local file paths and
// HTTP(S) URLs) and the last segment is percent-decoded, e.g.
// "http://host/Amazing%20Grace.mp3" yields "Amazing Grace.mp3". If decoding
// fails, the raw (still-encoded) segment is returned rather than an error,
// since this is only ever used for a case-insensitive substring match.
func trackFileName(uri string) string {
	name := uri
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	return name
}

// trackMatches reports whether every word in words is found, as a
// case-insensitive substring, somewhere in t's title, artist, album, or
// the file name portion of its URI (see trackFileName). Word order and
// which field each word matches don't matter: "amazing grace" matches a
// track titled "Grace, Amazing" just as readily as "Amazing Grace". An empty
// words list matches nothing (callers should treat that as a usage
// error, not "match everything").
func trackMatches(t queueTrack, words []string) bool {
	if len(words) == 0 {
		return false
	}
	haystack := strings.ToLower(strings.Join([]string{t.Title, t.Artist, t.Album, trackFileName(t.URI)}, "\x00"))
	for _, w := range words {
		if w == "" {
			continue
		}
		if !strings.Contains(haystack, strings.ToLower(w)) {
			return false
		}
	}
	return true
}

// matchQueue returns the 1-based positions, in queue order, of every
// track in tracks matching trackMatches(_, words).
func matchQueue(tracks []queueTrack, words []string) []int {
	var positions []int
	for i, t := range tracks {
		if trackMatches(t, words) {
			positions = append(positions, i+1)
		}
	}
	return positions
}

// selectorPatternPrefix marks a queue-search selector, e.g. "@amazing
// zero", as accepted by resolveSelectors alongside plain-number and range
// selectors (see parseTrackRange). The word(s) after the prefix are
// matched per trackMatches.
const selectorPatternPrefix = "@"

// resolvePatternHit records which tracks the combined "@..." search
// resolveSelectors performed matched, so callers can decide whether
// multiple matches need extra confirmation (e.g. remove's -y flag).
// pattern is every "@..." selector given, space-joined, as typed
// (including the "@"), e.g. "@foo @bar".
type resolvePatternHit struct {
	pattern   string
	positions []int
}

// resolveSelectors parses a mix of position/range selectors (see
// parseTrackRange: "5", "1-9", "1..9", "9-") and "@word..." search
// selectors against tracks, returning every matching 1-based position,
// sorted ascending with duplicates removed.
//
// Every "@..." selector given is combined into a single search: all of
// their words together must match (AND), the same as if they'd been
// written as one "@word1 word2 ..." selector. This means
// "@foo @bar" and "@bar foo" and "@foo bar" all narrow to the same
// tracks - matching ALL of foo and bar - rather than matching foo OR
// bar. If that combined search matches no tracks, or any selector has
// invalid syntax or an out-of-range position, resolveSelectors returns
// an error and no partial result.
//
// patternHits reports the single combined "@..." search actually
// performed (nil if no "@..." selector was given), and which positions
// it matched, so callers can require extra confirmation (e.g. remove's
// -y flag) when it matched more than one track. pattern is the "@..."
// selector(s) exactly as given, space-joined, e.g. "@foo @bar".
func resolveSelectors(args []string, tracks []queueTrack) (positions []int, patternHits []resolvePatternHit, err error) {
	total := len(tracks)
	selected := map[int]bool{}

	var patternArgs []string
	var words []string
	for _, arg := range args {
		if pattern, ok := strings.CutPrefix(arg, selectorPatternPrefix); ok {
			w := strings.Fields(pattern)
			if len(w) == 0 {
				return nil, nil, fmt.Errorf("empty search pattern %q", arg)
			}
			patternArgs = append(patternArgs, arg)
			words = append(words, w...)
			continue
		}

		start, end, err := parseTrackRange(arg, total)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid selector %q: %w", arg, err)
		}
		if start < 1 || end > total || start > end {
			return nil, nil, fmt.Errorf("selector %q is out of range (queue has %d track(s))", arg, total)
		}
		for i := start; i <= end; i++ {
			selected[i] = true
		}
	}

	if len(patternArgs) > 0 {
		combined := strings.Join(patternArgs, " ")
		hits := matchQueue(tracks, words)
		if len(hits) == 0 {
			return nil, nil, fmt.Errorf("%q matched no tracks in the queue", combined)
		}
		patternHits = append(patternHits, resolvePatternHit{pattern: combined, positions: hits})
		for _, i := range hits {
			selected[i] = true
		}
	}

	positions = make([]int, 0, len(selected))
	for i := range selected {
		positions = append(positions, i)
	}
	sort.Ints(positions)
	return positions, patternHits, nil
}

// ambiguousPatternHits returns the subset of patternHits (as returned by
// resolveSelectors) whose "@pattern" selector matched more than one
// track, preserving order. This is the pure decision logic behind
// confirmMultiMatchOrExit, pulled out so it's testable without a live
// speaker or the os.Exit side effect.
func ambiguousPatternHits(patternHits []resolvePatternHit) []resolvePatternHit {
	var ambiguous []resolvePatternHit
	for _, hit := range patternHits {
		if len(hit.positions) > 1 {
			ambiguous = append(ambiguous, hit)
		}
	}
	return ambiguous
}

// confirmMultiMatchOrExit checks patternHits (as returned by
// resolveSelectors) for any "@pattern" selector that matched more than
// one track. Plain number/range selectors never need confirmation, no
// matter how many are given - only a search pattern can silently expand
// to more tracks than the user may have expected. If yes is false and
// any pattern matched multiple tracks, every such pattern's matches are
// listed (via formatQueueRow, so the currently-playing row - if among
// them - gets its usual marker) and the process exits 2 without the
// caller's action (e.g. remove, bump) having touched the queue. verb
// (e.g. "remove", "bump") is used in the printed instructions.
func confirmMultiMatchOrExit(patternHits []resolvePatternHit, yes bool, tracks []queueTrack, matched int, pos positionInfo, verb string) {
	if yes {
		return
	}
	ambiguous := ambiguousPatternHits(patternHits)
	if len(ambiguous) == 0 {
		return
	}
	for _, hit := range ambiguous {
		fmt.Fprintf(os.Stderr, "error: %q matched more than one track:\n", hit.pattern)
		for _, p := range hit.positions {
			fmt.Fprintln(os.Stderr, formatQueueRow(p, tracks[p-1], p-1 == matched, pos))
		}
	}
	fmt.Fprintf(os.Stderr, "pass -y/--yes to %s all of the above\n", verb)
	os.Exit(2)
}

// setRelativeVolume adjusts the volume by adjustment (can be negative) and
// returns the speaker's new volume (0-100).
func setRelativeVolume(ip string, adjustment int) (int, error) {
	body := fmt.Sprintf(`<InstanceID>0</InstanceID>
      <Channel>Master</Channel>
      <Adjustment>%d</Adjustment>`, adjustment)
	respBody, err := renderingControlCall(ip, "SetRelativeVolume", body)
	if err != nil {
		return 0, err
	}

	var parsed struct {
		Body struct {
			Resp struct {
				NewVolume int `xml:"NewVolume"`
			} `xml:"SetRelativeVolumeResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(respBody, &parsed); err != nil {
		return 0, fmt.Errorf("parse SetRelativeVolume response: %w", err)
	}
	return parsed.Body.Resp.NewVolume, nil
}

// getVolume returns the speaker's current volume (0-100).
func getVolume(ip string) (int, error) {
	body := `<InstanceID>0</InstanceID>
      <Channel>Master</Channel>`
	respBody, err := renderingControlCall(ip, "GetVolume", body)
	if err != nil {
		return 0, err
	}

	var parsed struct {
		Body struct {
			Resp struct {
				CurrentVolume int `xml:"CurrentVolume"`
			} `xml:"GetVolumeResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(respBody, &parsed); err != nil {
		return 0, fmt.Errorf("parse GetVolume response: %w", err)
	}
	return parsed.Body.Resp.CurrentVolume, nil
}

// getMute returns the speaker's current mute state.
func getMute(ip string) (bool, error) {
	body := `<InstanceID>0</InstanceID>
      <Channel>Master</Channel>`
	respBody, err := renderingControlCall(ip, "GetMute", body)
	if err != nil {
		return false, err
	}

	var parsed struct {
		Body struct {
			Resp struct {
				CurrentMute int `xml:"CurrentMute"`
			} `xml:"GetMuteResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(respBody, &parsed); err != nil {
		return false, fmt.Errorf("parse GetMute response: %w", err)
	}
	return parsed.Body.Resp.CurrentMute != 0, nil
}

// setMute sets the speaker's mute state.
func setMute(ip string, mute bool) error {
	val := 0
	if mute {
		val = 1
	}
	body := fmt.Sprintf(`<InstanceID>0</InstanceID>
      <Channel>Master</Channel>
      <DesiredMute>%d</DesiredMute>`, val)
	_, err := renderingControlCall(ip, "SetMute", body)
	return err
}

// resolveURI returns the URL to play. If name already looks like a full URL
// (contains "://"), it's used as-is. Otherwise it's appended to baseURL.
func resolveURI(baseURL, name string) (string, error) {
	if strings.Contains(name, "://") {
		return name, nil
	}
	if baseURL == "" {
		return "", fmt.Errorf("no base URL set; pass --base-url, set SONOS_BASE_URL, or supply a full URL")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base URL %q: %w", baseURL, err)
	}
	return u.JoinPath(name).String(), nil
}

// copyFile copies src to dstDir, preserving its base name, creating dstDir
// if needed. It returns the base name of the copied file.
func copyFile(src, dstDir string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open source file: %w", err)
	}
	defer in.Close()

	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", fmt.Errorf("create destination directory: %w", err)
	}

	name := filepath.Base(src)
	dst := filepath.Join(dstDir, name)

	tmp, err := os.CreateTemp(dstDir, name+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp file in destination: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("copy file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("close temp file: %w", err)
	}
	if info, err := os.Stat(src); err == nil {
		_ = os.Chmod(tmpPath, info.Mode())
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("move file into place: %w", err)
	}

	return name, nil
}

// transcodeToMP3 uses ffmpeg to convert src into an mp3 file in dstDir,
// creating dstDir if needed. It returns the base name of the resulting
// file (src's name with its extension replaced by ".mp3").

// transcodingHeartbeat is how often we print a "still working" message
// while ffmpeg runs, so the user doesn't think the tool has hung on large
// files.
const transcodingHeartbeat = 5 * time.Second

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// mp3BaseName returns src's base name with its extension replaced by
// ".mp3".
func mp3BaseName(src string) string {
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	return base + ".mp3"
}

// hasAudioStream uses ffprobe (part of the ffmpeg suite) to check whether
// src contains at least one audio stream, so we can reject obviously
// unplayable files (e.g. a .jpg) before wasting time copying/transcoding
// them or queuing something Sonos will just refuse to play. If ffprobe
// isn't available or fails to run, we can't tell either way, so we're
// permissive and assume it might be audio rather than blocking it.
func hasAudioStream(src string) bool {
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		return true
	}
	out, err := exec.Command(ffprobePath,
		"-v", "error",
		"-select_streams", "a",
		"-show_entries", "stream=codec_type",
		"-of", "csv=p=0",
		src,
	).Output()
	if err != nil {
		return true
	}
	return strings.Contains(string(out), "audio")
}

func transcodeToMP3(src, dstDir string) (string, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", fmt.Errorf("create destination directory: %w", err)
	}

	outName := mp3BaseName(src)

	tmp, err := os.CreateTemp(dstDir, outName+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp output file: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	// ffmpeg refuses to write to an existing non-empty file without -y;
	// remove the placeholder so it can create it fresh.
	os.Remove(tmpPath)

	fmt.Fprintf(os.Stderr, "transcoding %q to mp3 via ffmpeg...\n", filepath.Base(src))
	start := time.Now()

	cmd := exec.Command(ffmpegPath,
		"-y",
		"-loglevel", "error",
		"-i", src,
		"-vn",
		"-ar", "44100",
		"-ac", "2",
		"-b:a", "192k",
		"-f", "mp3", // force mp3 muxer; tmpPath's suffix isn't ".mp3"
		tmpPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("start ffmpeg: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ticker := time.NewTicker(transcodingHeartbeat)
	defer ticker.Stop()

	var waitErr error
waitLoop:
	for {
		select {
		case waitErr = <-done:
			break waitLoop
		case <-ticker.C:
			fmt.Fprintf(os.Stderr, "  ... still transcoding %q (%s elapsed)\n", filepath.Base(src), time.Since(start).Round(time.Second))
		}
	}

	if waitErr != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("ffmpeg: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}

	dst := filepath.Join(dstDir, outName)
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("move transcoded file into place: %w", err)
	}

	fmt.Fprintf(os.Stderr, "transcoded %q to %q in %s\n", filepath.Base(src), outName, time.Since(start).Round(10*time.Millisecond))

	return outName, nil
}

// resolvePlayURL figures out the final URL to send to the speaker.
//
//   - If arg is already a full URL (contains "://"), it's used as-is.
//   - Else if destDir is set, arg is treated as a local file path: it's
//     copied into destDir, and baseURL + its base name is returned. If
//     arg's extension isn't a format Sonos natively supports and ffmpeg is
//     available, it's transcoded to mp3 instead of copied as-is. Either
//     way, if the destination file already exists, the copy/transcode
//     step is skipped and the existing file is reused.
//   - Else, arg is treated as a bare file name and simply appended to
//     baseURL (no copy performed).
func resolvePlayURL(baseURL, destDir, arg string) (string, error) {
	if strings.Contains(arg, "://") {
		return arg, nil
	}

	name := arg
	if destDir != "" {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(arg), "."))
		if ext != "" && !supportedAudioExtensions[ext] {
			transcodedName := mp3BaseName(arg)
			if fileExists(filepath.Join(destDir, transcodedName)) {
				fmt.Fprintf(os.Stderr, "using existing %q (already transcoded)\n", transcodedName)
				return resolveURI(baseURL, transcodedName)
			}
			if !hasAudioStream(arg) {
				return "", fmt.Errorf("no audio stream found; %q doesn't look like a playable audio file", filepath.Base(arg))
			}
			if transcodedName, err := transcodeToMP3(arg, destDir); err == nil {
				return resolveURI(baseURL, transcodedName)
			} else {
				fmt.Fprintf(os.Stderr, "warning: could not transcode %q (%v); copying as-is\n", arg, err)
			}
		}

		if fileExists(filepath.Join(destDir, filepath.Base(arg))) {
			fmt.Fprintf(os.Stderr, "using existing %q (already copied)\n", filepath.Base(arg))
			return resolveURI(baseURL, filepath.Base(arg))
		}

		copiedName, err := copyFile(arg, destDir)
		if err != nil {
			return "", fmt.Errorf("copy %q to %q: %w", arg, destDir, err)
		}
		name = copiedName
	}

	return resolveURI(baseURL, name)
}

// supportedAudioExtensions lists file extensions (without the leading dot,
// lowercased) that Sonos officially supports for music library / network
// streaming playback, per:
// https://support.sonos.com/en-us/article/supported-audio-formats-for-sonos-music-library
var supportedAudioExtensions = map[string]bool{
	"mp3":  true,
	"mp4":  true,
	"m4a":  true,
	"wma":  true,
	"aac":  true,
	"ogg":  true,
	"flac": true,
	"alac": true,
	"aiff": true,
	"aif":  true,
	"wav":  true,
}

// warnIfUnsupportedFormat prints a warning to stderr if uriOrPath's file
// extension isn't a format Sonos is known to support natively.
func warnIfUnsupportedFormat(uriOrPath string) {
	p := uriOrPath
	if u, err := url.Parse(uriOrPath); err == nil && u.Path != "" {
		p = u.Path
	}

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))
	if ext == "" || supportedAudioExtensions[ext] {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: %q has an unrecognized file extension; Sonos may not be able to play it\n", filepath.Base(p))
}

// playItem is a fully-resolved thing to send to the speaker, either a
// plain audio URI (Metadata == "") or a music-service share link (e.g.
// Spotify) that needs DIDL-Lite metadata alongside it so the speaker
// knows which linked service account to play it with.
type playItem struct {
	URI string

	// Metadata, if non-empty, is the DIDL-Lite XML to send as
	// EnqueuedURIMetaData/CurrentURIMetaData alongside URI.
	Metadata string

	// Label, if set, is used in place of URI in user-facing messages.
	Label string
}

// label returns a human-friendly description of the item for messages.
func (it playItem) label() string {
	if it.Label != "" {
		return it.Label
	}
	return it.URI
}

// addItemToQueue adds item to the play queue.
func addItemToQueue(ip string, item playItem, asNext bool) (firstTrackNumberEnqueued, newQueueLength int, err error) {
	firstTrackNumberEnqueued, newQueueLength, err = addURIToQueueWithMetadata(ip, item.URI, item.Metadata, asNext)
	if err != nil && item.Metadata != "" {
		// item.Metadata non-empty means this is a music-service share
		// link (currently only Spotify). A UPnP error 800 here is a
		// known Sonos firmware restriction (not something sonoplay can
		// construct its way around): confirmed on a real Sonos One
		// (S2, 83.1-61240) by replaying the exact URI/metadata from a
		// Sonos-app-created Favorite for the same item, which was
		// rejected identically. Third-party/raw UPnP control of linked
		// streaming-service content requires an authenticated cloud
		// session that only the Sonos app itself has; see
		// https://github.com/SoCo/SoCo/issues/969 (one report says a
		// later Sonos software update resolved it for them, so it may
		// be worth checking for a system update).
		if strings.Contains(err.Error(), "errorCode>800<") {
			err = fmt.Errorf("%w (Sonos firmware is rejecting third-party playback of linked streaming-service content; this isn't a sonoplay bug or a missing service link - see https://github.com/SoCo/SoCo/issues/969. Try checking for a Sonos system update)", err)
		} else {
			err = fmt.Errorf("%w (is this service linked under Settings > Services & Voice in the Sonos app? casting via Spotify Connect alone isn't enough)", err)
		}
	}
	return firstTrackNumberEnqueued, newQueueLength, err
}

// spotifyShareRe recognizes Spotify URLs (open.spotify.com, with an
// optional "intl-xx" locale path segment) and spotify:-scheme URIs,
// capturing the share type and ID, e.g. "album", "6JvmSy...".
var spotifyShareRe = regexp.MustCompile(`(?i)spotify(?:\.com)?(?:/intl-[a-z0-9-]+)?[:/](track|album|playlist|episode|show|artist)[:/]([A-Za-z0-9]+)`)

// spotifyShareMagic describes how to build an AddURIToQueue request for a
// given Spotify share type, matching what the Sonos app itself sends
// (confirmed against a Sonos-saved Favorite's DIDL-Lite on a real
// speaker: "artist" share links produce a playlistContainer with the
// 1006206c prefix/key, used here for "radio" playback of that artist).
type spotifyShareMagic struct {
	prefix string
	key    string
	class  string
}

var spotifyShareMagicByType = map[string]spotifyShareMagic{
	"track":    {prefix: "", key: "00032020", class: "object.item.audioItem.musicTrack"},
	"episode":  {prefix: "", key: "00032020", class: "object.item.audioItem.musicTrack"},
	"album":    {prefix: "x-rincon-cpcontainer:1004206c", key: "00040000", class: "object.container.album.musicAlbum"},
	"playlist": {prefix: "x-rincon-cpcontainer:1006206c", key: "1006206c", class: "object.container.playlistContainer"},
	"show":     {prefix: "x-rincon-cpcontainer:1006206c", key: "1006206c", class: "object.container.playlistContainer"},
	"artist":   {prefix: "x-rincon-cpcontainer:1006206c", key: "1006206c", class: "object.container.playlistContainer"},
}

// spotifyTrackURIFlags and spotifyContainerURIFlags are the "flags="
// values Sonos uses in the query string of Spotify share-link URIs. They
// encode generic container/track capabilities and are the same across
// households; only sid/sn (see spotifyAccount) are household-specific.
const (
	spotifyTrackURIFlags     = 32
	spotifyContainerURIFlags = 8300
)

// spotifyMetadataTemplate is the DIDL-Lite metadata Sonos expects
// alongside a Spotify share-link URI. The "desc" element's token tells
// the speaker which linked service account to use; see spotifyAccount.
const spotifyMetadataTemplate = `<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns:r="urn:schemas-rinconnetworks-com:metadata-1-0/" xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/"><item id="%s" parentID="-1" restricted="true"><dc:title></dc:title><upnp:class>%s</upnp:class><desc id="cdudn" nameSpace="urn:schemas-rinconnetworks-com:metadata-1-0/">%s</desc></item></DIDL-Lite>`

// parseSpotifyShareLink recognizes a Spotify share URL or spotify: URI
// and extracts its share type ("track", "album", etc.) and ID.
//
// If arg has a "highlight" query parameter referencing a specific track
// (as seen on links like ".../album/<id>?highlight=spotify:track:<id>",
// generated when sharing a single track from within an album view),
// that's preferred over the URL's own path, since it's almost always the
// user's actual intent.
func parseSpotifyShareLink(arg string) (shareType, id string, ok bool) {
	if u, err := url.Parse(arg); err == nil {
		if highlight := u.Query().Get("highlight"); highlight != "" {
			if m := spotifyShareRe.FindStringSubmatch(highlight); m != nil {
				return strings.ToLower(m[1]), m[2], true
			}
		}
	}
	if m := spotifyShareRe.FindStringSubmatch(arg); m != nil {
		return strings.ToLower(m[1]), m[2], true
	}
	return "", "", false
}

// spotifyEncodeURI returns "spotify:<shareType>:<id>" with ":" replaced
// by "%3A" (uppercase, matching what the Sonos app itself generates).
func spotifyEncodeURI(shareType, id string) string {
	return strings.ReplaceAll(fmt.Sprintf("spotify:%s:%s", shareType, id), ":", "%3A")
}

// musicService is one entry from MusicServices.ListAvailableServices:
// a household-specific "sid" plus the fixed, region-dependent "service
// type" number embedded in DIDL-Lite "desc" tokens for that service.
type musicService struct {
	ID          int // household-specific service id (the "sid" query param)
	Name        string
	ServiceType int // fixed catalog/region number used in "SA_RINCON<n>" desc tokens
}

// listMusicServices returns the household's available music services,
// with each one's "service type" number resolved.
//
// MusicServices.ListAvailableServices returns two parallel lists: a
// descriptor list (one <Service> per service, with its household-
// specific "sid") and a flat, comma-separated AvailableServiceTypeList
// of "service type" numbers. The two are positionally aligned, except
// that ContainerType="Preload" entries (e.g. "Sonos Backgrounds") have
// no corresponding entry in the type list and must be skipped; this was
// confirmed empirically against a real Sonos speaker (108 services, 107
// types, with exactly one Preload entry).
func listMusicServices(ip string) ([]musicService, error) {
	respBody, err := musicServicesCall(ip, "ListAvailableServices", "")
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Body struct {
			Resp struct {
				AvailableServiceDescriptorList string `xml:"AvailableServiceDescriptorList"`
				AvailableServiceTypeList       string `xml:"AvailableServiceTypeList"`
			} `xml:"ListAvailableServicesResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse ListAvailableServices response: %w", err)
	}

	var descriptors struct {
		Services []struct {
			ID            int    `xml:"Id,attr"`
			Name          string `xml:"Name,attr"`
			ContainerType string `xml:"ContainerType,attr"`
		} `xml:"Service"`
	}
	if err := xml.Unmarshal([]byte(parsed.Body.Resp.AvailableServiceDescriptorList), &descriptors); err != nil {
		return nil, fmt.Errorf("parse AvailableServiceDescriptorList: %w", err)
	}

	var types []int
	for _, s := range strings.Split(parsed.Body.Resp.AvailableServiceTypeList, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if n, err := strconv.Atoi(s); err == nil {
			types = append(types, n)
		}
	}

	services := make([]musicService, 0, len(descriptors.Services))
	typeIdx := 0
	for _, s := range descriptors.Services {
		if s.ContainerType == "Preload" {
			continue // no corresponding AvailableServiceTypeList entry
		}
		svc := musicService{ID: s.ID, Name: s.Name}
		if typeIdx < len(types) {
			svc.ServiceType = types[typeIdx]
		}
		typeIdx++
		services = append(services, svc)
	}
	return services, nil
}

// browseRaw runs a ContentDirectory Browse and returns the raw,
// unescaped DIDL-Lite XML from the response's <Result>.
func browseRaw(ip, objectID string, count int) (string, error) {
	body := fmt.Sprintf(`<ObjectID>%s</ObjectID>
      <BrowseFlag>BrowseDirectChildren</BrowseFlag>
      <Filter>*</Filter>
      <StartingIndex>0</StartingIndex>
      <RequestedCount>%d</RequestedCount>
      <SortCriteria></SortCriteria>`, xmlEscape(objectID), count)

	respBody, err := contentDirectoryCall(ip, "Browse", body)
	if err != nil {
		return "", err
	}

	var parsed struct {
		Body struct {
			Resp struct {
				Result string `xml:"Result"`
			} `xml:"BrowseResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parse Browse response: %w", err)
	}
	return parsed.Body.Resp.Result, nil
}

// spotifyAccount holds the Sonos-internal identifiers needed to build a
// working Spotify share-link URI/metadata pair on a given household:
// SID/ServiceType come from ListAvailableServices; AccountID/SN identify
// which linked Spotify account to use and can't be queried directly (no
// API exposes them), so they're read from an existing Spotify item
// elsewhere on the system (see discoverSpotifyAccount).
type spotifyAccount struct {
	SID         int
	ServiceType int
	AccountID   string
	SN          int
}

// spotifyAccountRe extracts an account ID and matching "sn" from a blob
// of DIDL-Lite XML (as returned by browseRaw), given the household's
// Spotify sid/serviceType (so other linked services' entries, and other
// services' sid/sn pairs, aren't mistaken for Spotify's).
func spotifyAccountRe(sid, serviceType int) (*regexp.Regexp, *regexp.Regexp) {
	token := regexp.MustCompile(fmt.Sprintf(`SA_RINCON%d_X_#Svc%d-([0-9A-Za-z]+)-Token`, serviceType, serviceType))
	sn := regexp.MustCompile(fmt.Sprintf(`[?&]sid=%d(?:&amp;|&)flags=\d+(?:&amp;|&)sn=(\d+)`, sid))
	return token, sn
}

// findSpotifyAccountIn scans a ContentDirectory container (e.g. "FV:2"
// for Sonos Favorites, "Q:0" for the play queue) for any Spotify item,
// returning its account ID and sn if found.
func findSpotifyAccountIn(ip, objectID string, sid, serviceType int) (accountID string, sn int, ok bool) {
	raw, err := browseRaw(ip, objectID, 200)
	if err != nil || raw == "" {
		return "", 0, false
	}
	tokenRe, snRe := spotifyAccountRe(sid, serviceType)
	tm := tokenRe.FindStringSubmatch(raw)
	if tm == nil {
		return "", 0, false
	}
	sm := snRe.FindStringSubmatch(raw)
	if sm == nil {
		return "", 0, false
	}
	snVal, err := strconv.Atoi(sm[1])
	if err != nil {
		return "", 0, false
	}
	return tm[1], snVal, true
}

// discoverSpotifyAccount figures out the spotifyAccount needed to build
// working Spotify share-link requests on this household.
//
// SID/ServiceType are always looked up live via ListAvailableServices.
// AccountID/SN come from, in order: the spotify_account/spotify_sn
// config file settings (or SONOS_SPOTIFY_ACCOUNT/SONOS_SPOTIFY_SN env
// vars); otherwise by scanning Sonos Favorites, then the play queue, for
// an existing Spotify item to read them from.
func discoverSpotifyAccount(ip string) (spotifyAccount, error) {
	services, err := listMusicServices(ip)
	if err != nil {
		return spotifyAccount{}, fmt.Errorf("list music services: %w", err)
	}
	var acct spotifyAccount
	found := false
	for _, s := range services {
		if s.Name == "Spotify" {
			acct.SID, acct.ServiceType = s.ID, s.ServiceType
			found = true
			break
		}
	}
	if !found {
		return spotifyAccount{}, fmt.Errorf("Spotify doesn't appear to be available as a music service on this Sonos system")
	}

	accountOverride := firstNonEmpty(os.Getenv("SONOS_SPOTIFY_ACCOUNT"), cfg.SpotifyAccount)
	snOverride := firstNonEmpty(os.Getenv("SONOS_SPOTIFY_SN"), cfg.SpotifySN)
	if accountOverride != "" && snOverride != "" {
		sn, err := strconv.Atoi(snOverride)
		if err != nil {
			return spotifyAccount{}, fmt.Errorf("invalid spotify_sn %q: must be a number", snOverride)
		}
		acct.AccountID, acct.SN = accountOverride, sn
		return acct, nil
	}

	for _, objectID := range []string{"FV:2", "Q:0"} {
		if accountID, sn, ok := findSpotifyAccountIn(ip, objectID, acct.SID, acct.ServiceType); ok {
			acct.AccountID, acct.SN = accountID, sn
			return acct, nil
		}
	}

	return spotifyAccount{}, fmt.Errorf("couldn't determine your Spotify account on this Sonos system (no Spotify item found in Sonos Favorites or the play queue); either save any Spotify item to Sonos Favorites once, or set spotify_account/spotify_sn in the config file (or SONOS_SPOTIFY_ACCOUNT/SONOS_SPOTIFY_SN)")
}

// resolveSpotifyItem recognizes arg as a Spotify share link/URI and, if
// so, builds the playItem needed to queue it. matched is true whenever
// arg looked like a Spotify link at all, even if it couldn't ultimately
// be resolved, so callers can distinguish "not Spotify" from "Spotify,
// but unplayable" (e.g. acctErr is non-nil, or an unsupported type).
func resolveSpotifyItem(arg string, acct spotifyAccount, acctErr error) (item playItem, matched bool, err error) {
	shareType, id, ok := parseSpotifyShareLink(arg)
	if !ok {
		return playItem{}, false, nil
	}
	if acctErr != nil {
		return playItem{}, true, acctErr
	}

	magic, ok := spotifyShareMagicByType[shareType]
	if !ok {
		return playItem{}, true, fmt.Errorf("spotify %q links aren't supported for playback (try a track, album, playlist, episode, show, or artist link)", shareType)
	}

	encoded := spotifyEncodeURI(shareType, id)
	itemID := magic.key + encoded
	descToken := fmt.Sprintf("SA_RINCON%d_X_#Svc%d-%s-Token", acct.ServiceType, acct.ServiceType, acct.AccountID)

	var enqueueURI string
	if magic.prefix == "" {
		enqueueURI = fmt.Sprintf("x-sonos-spotify:%s?sid=%d&flags=%d&sn=%d", encoded, acct.SID, spotifyTrackURIFlags, acct.SN)
	} else {
		enqueueURI = fmt.Sprintf("%s%s?sid=%d&flags=%d&sn=%d", magic.prefix, encoded, acct.SID, spotifyContainerURIFlags, acct.SN)
	}

	return playItem{
		URI:      enqueueURI,
		Metadata: fmt.Sprintf(spotifyMetadataTemplate, itemID, magic.class, descToken),
		Label:    fmt.Sprintf("spotify %s %s", shareType, id),
	}, true, nil
}

func requireIP(fs *flag.FlagSet, ip string) {
	if ip == "" {
		fmt.Fprintln(os.Stderr, "error: speaker IP not set; pass --ip, set SONOS_SPEAKER_IP, or add it to the config file")
		fs.Usage()
		os.Exit(2)
	}
}

// classifyPlayArgs decides whether runPlay's arguments should be resolved
// as queue selectors (see resolveSelectors) rather than as files/URLs/
// Spotify links. An argument is "selector-like" if it's a bare positive
// integer (a queue position) or starts with "@" (a search pattern); any
// mix of those, in any combination and however many, counts.
//
//   - If every argument is selector-like, selector is true: this covers
//     both the historical single-plain-number case ("play 3") and any
//     combination of numbers and/or "@pattern"s ("play @foo",
//     "play 3 @foo @bar").
//   - If at least one argument starts with "@" but at least one other
//     argument is NOT selector-like (e.g. a file path, URL, or Spotify
//     link), mixed is true: there's no sensible way to both search the
//     queue and queue a file in one command, so this is reported as an
//     error rather than silently treating the "@..." argument as a file
//     name (which is what happened before this check existed).
//   - Otherwise (no "@" arguments at all, i.e. this is the pre-existing
//     "possibly several file/URL arguments, with plain numbers among
//     them treated as literal file names" case) both are false, and
//     runPlay's existing file/URL/Spotify-link handling is unchanged.
func classifyPlayArgs(args []string) (selector, mixed bool) {
	if len(args) == 0 {
		return false, false
	}
	allSelectorLike := true
	anyPattern := false
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, selectorPatternPrefix):
			anyPattern = true
		case isPositiveInteger(a):
			// Also selector-like; allSelectorLike unchanged.
		default:
			allSelectorLike = false
		}
	}
	if allSelectorLike {
		return true, false
	}
	return false, anyPattern
}

func runPlay(args []string) {
	fs := flag.NewFlagSet("play", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s play [--ip <speaker-ip>] [--base-url <url>] [--dest-dir <dir>] [--db <path>] [--now] <file-path-or-url-or-queue-position> [<file-path-or-url> ...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip        SONOS_SPEAKER_IP   ip\n")
		fmt.Fprintf(os.Stderr, "  --base-url  SONOS_BASE_URL     base_url\n")
		fmt.Fprintf(os.Stderr, "  --dest-dir  SONOS_DEST_DIR     dest_dir\n")
		fmt.Fprintf(os.Stderr, "  --db        SONOS_DB           db\n")
		fmt.Fprintf(os.Stderr, "\nIf a single argument is a plain positive number (e.g. \"3\"), it's\n")
		fmt.Fprintf(os.Stderr, "treated as a 1-based position in the current queue (see '%s list'):\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "the speaker switches to its queue, jumps to that item, and plays it\n")
		fmt.Fprintf(os.Stderr, "immediately.\n")
		fmt.Fprintf(os.Stderr, "\nArguments starting with \"@\" (e.g. \"@amazing grace\") are resolved the\n")
		fmt.Fprintf(os.Stderr, "same way, but by searching the queue (see '%s find') rather than by\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "position. Several \"@...\" arguments narrow the search further rather\n")
		fmt.Fprintf(os.Stderr, "than searching separately: \"play @foo @bar\" means every word of foo\n")
		fmt.Fprintf(os.Stderr, "AND bar must match (the same as \"play @\\\"foo bar\\\"\"), not foo OR bar.\n")
		fmt.Fprintf(os.Stderr, "A plain number may be combined with \"@...\" arguments too, adding that\n")
		fmt.Fprintf(os.Stderr, "position to the search results.\n")
		fmt.Fprintf(os.Stderr, "\nThese selector forms only apply when EVERY argument is one of them -\n")
		fmt.Fprintf(os.Stderr, "mixing an \"@...\" argument with a file/URL/Spotify-link argument is an\n")
		fmt.Fprintf(os.Stderr, "error, not a guess. If the (possibly combined) selectors match more\n")
		fmt.Fprintf(os.Stderr, "than one track, that's an error unless --first is given, in which\n")
		fmt.Fprintf(os.Stderr, "case the first (lowest-positioned) match is played.\n")
		fmt.Fprintf(os.Stderr, "\nOtherwise, each argument is resolved independently: if it contains\n")
		fmt.Fprintf(os.Stderr, "\"://\" it's played as-is; else if --dest-dir is set it's treated as a\n")
		fmt.Fprintf(os.Stderr, "local file path that gets copied into --dest-dir (transcoded to mp3\n")
		fmt.Fprintf(os.Stderr, "via ffmpeg first if its extension isn't natively supported), and\n")
		fmt.Fprintf(os.Stderr, "base-url + its file name is used; else it's a bare file name appended\n")
		fmt.Fprintf(os.Stderr, "to base-url.\n")
		fmt.Fprintf(os.Stderr, "\nA Spotify share link or URI (e.g. \"https://open.spotify.com/track/...\"\n")
		fmt.Fprintf(os.Stderr, "or \"spotify:album:...\") is recognized and queued directly. A link to\n")
		fmt.Fprintf(os.Stderr, "a track highlighted within an album (\"...?highlight=spotify:track:...\")\n")
		fmt.Fprintf(os.Stderr, "plays that track, not the whole album. NOTE: current Sonos S2 firmware\n")
		fmt.Fprintf(os.Stderr, "may reject this outright (UPnP error 800) regardless of setup; see\n")
		fmt.Fprintf(os.Stderr, "https://github.com/SoCo/SoCo/issues/969.\n")
		fmt.Fprintf(os.Stderr, "\nWith multiple arguments, all are queued in the order given. If the\n")
		fmt.Fprintf(os.Stderr, "speaker is currently PLAYING, they're simply appended to the end of\n")
		fmt.Fprintf(os.Stderr, "the queue without interrupting playback. Otherwise (or with --now),\n")
		fmt.Fprintf(os.Stderr, "the speaker switches to its queue and starts playing from the first\n")
		fmt.Fprintf(os.Stderr, "of the newly queued tracks.\n")
		fmt.Fprintf(os.Stderr, "\nAny argument that can't be resolved into something playable (e.g. a\n")
		fmt.Fprintf(os.Stderr, ".jpg, or a local file with no audio stream) is skipped with a message\n")
		fmt.Fprintf(os.Stderr, "instead of aborting the rest.\n")
		fmt.Fprintf(os.Stderr, "\nA single song that actually starts playing now - by file/URL, queue\n")
		fmt.Fprintf(os.Stderr, "position/selector, or a single Spotify track link - is recorded in\n")
		fmt.Fprintf(os.Stderr, "play history (see '%s history -h'). Several files/URLs, a Spotify\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "album/playlist/show/artist link, and anything merely queued (not\n")
		fmt.Fprintf(os.Stderr, "played immediately) are NOT recorded - only a single song you\n")
		fmt.Fprintf(os.Stderr, "deliberately start playing right now counts.\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	baseURL := fs.String("base-url", firstNonEmpty(os.Getenv("SONOS_BASE_URL"), cfg.BaseURL), `base URL to prefix the file name with, e.g. "http://myhost/"`)
	destDir := fs.String("dest-dir", firstNonEmpty(os.Getenv("SONOS_DEST_DIR"), cfg.DestDir), "local directory to copy the source file into before playing it")
	now := fs.Bool("now", false, "play immediately, interrupting current playback, instead of enqueuing")
	first := fs.Bool("first", false, "with an @pattern argument matching multiple tracks, play the first match instead of erroring")
	dbFlag := fs.String("db", "", "override the play-history database path")
	fs.Parse(args)

	requireIP(fs, *ip)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}

	// Arguments that are all queue selectors (plain queue position(s)
	// and/or "@pattern" search(es), see classifyPlayArgs): resolve them
	// against the current queue and jump straight to the result, rather
	// than treating them as file/URL arguments. Several "@..." arguments
	// combine into a single search requiring all of their words to match
	// (see resolveSelectors), e.g. "play @foo @bar" narrows to tracks
	// matching both foo and bar, the same as "play @\"foo bar\"".
	selectorArgs, mixedArgs := classifyPlayArgs(fs.Args())
	if mixedArgs {
		fmt.Fprintln(os.Stderr, "error: can't mix @patterns with files/URLs")
		os.Exit(2)
	}
	if selectorArgs {
		label := strings.Join(fs.Args(), " ")

		tracks, _, err := browseQueue(*ip, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		positions, _, err := resolveSelectors(fs.Args(), tracks)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}

		index := positions[0] // lowest-positioned match; the only one unless an @pattern matched several
		if len(positions) > 1 && !*first {
			fmt.Fprintf(os.Stderr, "error: %q matches more than one track:\n", label)
			matched, pos, _ := currentQueuePosition(*ip, tracks)
			for _, p := range positions {
				fmt.Fprintln(os.Stderr, formatQueueRow(p, tracks[p-1], p-1 == matched, pos))
			}
			os.Exit(2)
		}

		if err := playQueueIndex(*ip, index); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		t := tracks[index-1]
		recordExplicitPlay(*dbFlag, t.URI, t.Title, t.Artist, t.Album)
		if pos, err := getPositionInfo(*ip); err == nil {
			track, _ := parseDIDLItem(pos.TrackMetaData)
			if track.Title == "" && track.URI == "" {
				track.URI = pos.TrackURI
			}
			if formatted := formatTrack(track); formatted != "" {
				fmt.Printf("playing queue item %d: %s\n", index, formatted)
				return
			}
		}
		fmt.Printf("playing queue item %d\n", index)
		return
	}

	// Resolve every argument (copy/transcode as needed) up front, before
	// touching the speaker's queue at all. Invalid/unplayable arguments
	// (e.g. a .jpg) are skipped with a message rather than aborting the
	// whole batch.
	//
	// If any argument looks like a Spotify share link, do a one-time,
	// read-only lookup of the account info needed to play it (see
	// discoverSpotifyAccount); otherwise skip it, since it needs several
	// round-trips to the speaker that plain file/URL args don't.
	needSpotifyLookup := false
	for _, arg := range fs.Args() {
		if _, _, ok := parseSpotifyShareLink(arg); ok {
			needSpotifyLookup = true
			break
		}
	}
	var spotifyAcct spotifyAccount
	var spotifyAcctErr error
	if needSpotifyLookup {
		spotifyAcct, spotifyAcctErr = discoverSpotifyAccount(*ip)
	}

	items := make([]playItem, 0, fs.NArg())
	for _, arg := range fs.Args() {
		if item, matched, err := resolveSpotifyItem(arg, spotifyAcct, spotifyAcctErr); matched {
			if err != nil {
				fmt.Fprintf(os.Stderr, "skipping %q: %v\n", arg, err)
				continue
			}
			items = append(items, item)
			continue
		}

		uri, err := resolvePlayURL(*baseURL, *destDir, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping %q: %v\n", arg, err)
			continue
		}
		warnIfUnsupportedFormat(uri)
		items = append(items, playItem{URI: uri})
	}

	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "error: no valid files to play")
		os.Exit(1)
	}

	// A single plain file/URL (no metadata) keeps the original direct-play
	// behavior. Anything with metadata (e.g. a Spotify share link) always
	// goes through the queue below, even alone, since SetAVTransportURI
	// can't play a music-service container/track directly without it.
	if len(items) == 1 && items[0].Metadata == "" {
		uri := items[0].URI
		if !*now {
			// If something is already playing, don't interrupt it: queue
			// the track instead. If we can't determine the state for any
			// reason, fall through to the normal immediate-play behavior.
			if state, err := getTransportState(*ip); err == nil && state == "PLAYING" {
				_, newLength, err := addURIToQueue(*ip, uri, false)
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
					os.Exit(1)
				}
				fmt.Printf("speaker is already playing; queued %q (queue length: %d)\n", uri, newLength)
				return
			}
		}

		if err := playURI(*ip, uri); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		title, artist, album := nowPlayingMetadata(*ip)
		recordExplicitPlay(*dbFlag, uri, title, artist, album)
		return
	}

	// Multiple files (or a single share link): always go through the
	// queue, since there's no "play this in place, right now" action
	// other than the queue for those.
	playImmediately := *now
	if !playImmediately {
		state, err := getTransportState(*ip)
		playImmediately = err != nil || state != "PLAYING"
	}

	firstTrackNumber := 0
	queueLength := 0
	queuedCount := 0
	for _, item := range items {
		firstNum, newLength, err := addItemToQueue(*ip, item, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping %q: %v\n", item.label(), err)
			continue
		}
		if queuedCount == 0 {
			firstTrackNumber = firstNum
		}
		queueLength = newLength
		queuedCount++
	}

	if queuedCount == 0 {
		fmt.Fprintln(os.Stderr, "error: failed to queue any tracks")
		os.Exit(1)
	}

	if !playImmediately {
		fmt.Printf("speaker is already playing; queued %d tracks (queue length: %d)\n", queuedCount, queueLength)
		return
	}

	if err := playQueueIndex(*ip, firstTrackNumber); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if isSingleSpotifyTrack(items) {
		title, artist, album := nowPlayingMetadata(*ip)
		recordExplicitPlay(*dbFlag, items[0].URI, title, artist, album)
	}
	fmt.Printf("queued %d tracks and started playing from item %d\n", queuedCount, firstTrackNumber)
}

// isSingleSpotifyTrack reports whether items is exactly one playItem
// resolved from a Spotify TRACK share link, as opposed to an album,
// playlist, episode, show, or artist link - see resolveSpotifyItem's
// Label, which always has the form "spotify <shareType> <id>", so a
// track link's Label always starts with "spotify track ".
func isSingleSpotifyTrack(items []playItem) bool {
	return len(items) == 1 && strings.HasPrefix(items[0].Label, "spotify track ")
}

// nowPlayingMetadata best-effort reads the title/artist/album of
// whatever the speaker is currently playing, for recordExplicitPlay to
// attach to a play history row right after starting something whose
// metadata wasn't already known locally (e.g. a plain file/URL, which
// resolvePlayURL never attaches tags to). Returns zero values (not an
// error) if this can't be determined - a play with just a URI and no
// metadata is still useful history, so this is never treated as fatal.
func nowPlayingMetadata(ip string) (title, artist, album string) {
	pos, err := getPositionInfo(ip)
	if err != nil {
		return "", "", ""
	}
	track, _ := parseDIDLItem(pos.TrackMetaData)
	return track.Title, track.Artist, track.Album
}

// recordExplicitPlay best-effort records uri (with title/artist/album,
// any of which may be empty if unknown) as an explicitly-started play
// in the play-history database (see recordPlay in history.go), using
// dbFlagValue (a command's --db flag). Called only at the specific
// points in runPlay where a single song has just started playing right
// now (not merely been added to the queue) - see runPlay's usage text
// for exactly which cases that covers.
//
// Failures here are reported as a warning, not a fatal error:
// playback has already started successfully by the time this is
// called, so losing a history row is far less bad than failing an
// otherwise-successful play command over it.
func recordExplicitPlay(dbFlagValue, uri, title, artist, album string) {
	if uri == "" {
		return
	}
	path, isDefault, err := dbPath(dbFlagValue)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: couldn't record play history:", err)
		return
	}
	db, err := openDB(path, isDefault)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: couldn't record play history:", err)
		return
	}
	defer db.Close()
	if err := recordPlay(db, time.Now(), uri, title, artist, album); err != nil {
		fmt.Fprintln(os.Stderr, "warning: couldn't record play history:", err)
	}
}

func runVolume(name string, args []string, sign int) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s %s [--ip <speaker-ip>] [--step <n>]\n", os.Args[0], name)
		fmt.Fprintf(os.Stderr, "\nIf the speaker is currently muted, it's unmuted first.\n")
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip    SONOS_SPEAKER_IP   ip\n")
		fmt.Fprintf(os.Stderr, "  --step  SONOS_STEP         step\n\n")
		fs.PrintDefaults()
	}

	defaultStep := 2
	if s := firstNonEmpty(os.Getenv("SONOS_STEP"), cfg.Step); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			defaultStep = n
		}
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	step := fs.Int("step", defaultStep, "volume step size")
	fs.Parse(args)

	requireIP(fs, *ip)

	// If the speaker is muted, unmute it first so the volume change is
	// actually audible.
	if currentlyMuted, err := getMute(*ip); err == nil && currentlyMuted {
		if err := setMute(*ip, false); err != nil {
			fmt.Fprintln(os.Stderr, "error: failed to unmute:", err)
			os.Exit(1)
		}
		fmt.Println("unmuted")
	}

	newVolume, err := setRelativeVolume(*ip, sign*(*step))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println("volume:", newVolume)
}

// muteAction is one of "mute", "unmute", or "toggle".
func runMute(name string, args []string, action string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s %s [--ip <speaker-ip>]\n", os.Args[0], name)
		switch action {
		case "mute":
			fmt.Fprintf(os.Stderr, "\nMutes the speaker.\n")
		case "unmute":
			fmt.Fprintf(os.Stderr, "\nUnmutes the speaker.\n")
		case "toggle":
			fmt.Fprintf(os.Stderr, "\nToggles the speaker's current mute state.\n")
		}
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	fs.Parse(args)

	requireIP(fs, *ip)

	var desired bool
	switch action {
	case "mute":
		desired = true
	case "unmute":
		desired = false
	case "toggle":
		currentlyMuted, err := getMute(*ip)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		desired = !currentlyMuted
	default:
		panic("unknown mute action: " + action)
	}

	if err := setMute(*ip, desired); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if desired {
		fmt.Println("muted")
	} else {
		fmt.Println("unmuted")
	}
}

// terminalMargin is how much vertical space runList leaves unused below
// its output when truncating to fit the terminal, so the result doesn't
// run flush against the bottom of the screen (e.g. a shell prompt below
// it). On top of this fixed margin, a further 10% of the detected window
// height is reserved (see terminalSafetyMargin) to absorb any discrepancy
// between the reported size and what's actually visible/usable (wrapped
// prompts, tmux/multiplexer chrome, off-by-a-bit terminal reporting, etc).
const terminalMargin = 2

// terminalSafetyMargin returns an additional safety margin equal to ~10%
// of rows (rounded up, at least 1), reserved on top of terminalMargin.
func terminalSafetyMargin(rows int) int {
	return (rows + 9) / 10
}

// visibleWindow picks which [start, end) slice of a 0..total list to show
// given a budget of maxLines printed rows, keeping anchor (e.g. the
// currently-playing track's index, or -1 if there is none) visible.
// topEllipsis/bottomEllipsis report whether a "..." row is needed above
// and/or below the window to indicate hidden items.
//
// If everything fits (total <= maxLines), or maxLines is too small to
// usefully truncate (less than one row for an item plus both ellipsis
// rows), the whole list is returned untruncated.
func visibleWindow(total, anchor, maxLines int) (start, end int, topEllipsis, bottomEllipsis bool) {
	if maxLines < 3 || total <= maxLines {
		return 0, total, false, false
	}
	if anchor < 0 {
		anchor = 0
	}

	// Anchor falls within the first maxLines-1 items: show from the top,
	// with just a bottom ellipsis for the rest.
	if anchor < maxLines-1 {
		end = maxLines - 1
		if end > total {
			end = total
		}
		return 0, end, false, true
	}

	// Anchor falls within the last maxLines-1 items: show through the
	// end, with just a top ellipsis for the rest.
	if anchor >= total-(maxLines-1) {
		start = total - (maxLines - 1)
		if start < 0 {
			start = 0
		}
		return start, total, true, false
	}

	// Anchor is in the middle: center the window on it, with an ellipsis
	// on both sides.
	inner := maxLines - 2
	start = anchor - inner/2
	end = start + inner
	if start < 1 {
		start, end = 1, 1+inner
	}
	if end > total-1 {
		end = total - 1
		start = end - inner
	}
	return start, end, true, true
}

// currentQueuePosition looks up the speaker's transport state and, if
// something is actively loaded (PLAYING/PAUSED_PLAYBACK/TRANSITIONING),
// matches its TrackURI against tracks to find which 0-based index (if
// any) it corresponds to in the queue - the same "what's playing, and
// where is it in the queue" check runList uses to place its "▶" marker,
// shared here for play's multi-match listing and bump. matched is -1 if
// nothing is playing, or what's playing isn't part of the queue (e.g. a
// one-off direct-played URL).
func currentQueuePosition(ip string, tracks []queueTrack) (matched int, pos positionInfo, havePos bool) {
	matched = -1
	state, err := getTransportState(ip)
	if err != nil {
		return matched, pos, false
	}
	switch state {
	case "PLAYING", "PAUSED_PLAYBACK", "TRANSITIONING":
	default:
		return matched, pos, false
	}
	p, err := getPositionInfo(ip)
	if err != nil {
		return matched, pos, false
	}
	pos = p
	havePos = true
	if state == "PAUSED_PLAYBACK" {
		pos.paused = true
	}
	if pos.TrackURI != "" {
		for i, t := range tracks {
			if t.URI == pos.TrackURI {
				matched = i
				break
			}
		}
	}
	return matched, pos, true
}

// formatQueueRow renders a single queue listing row, 1-based position i,
// in the shared format used by both "list" and "find": "  12. Artist -
// Title (Album)", or "▶  12. ..." plus the elapsed/duration suffix (see
// nowPlayingSuffix) when isPlaying is true.
func formatQueueRow(i int, t queueTrack, isPlaying bool, pos positionInfo) string {
	marker := "  "
	suffix := ""
	if isPlaying {
		marker = "▶ "
		suffix = nowPlayingSuffix(pos)
	}
	return fmt.Sprintf("%s%3d. %s%s", marker, i, formatTrack(t), suffix)
}

func runList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s list [--ip <speaker-ip>] [-a|--all]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nLists the speaker's play queue, marking the currently playing item.\n")
		fmt.Fprintf(os.Stderr, "\nWhen run in a terminal, the list is shortened to fit the screen\n")
		fmt.Fprintf(os.Stderr, "(with \"...\" rows marking hidden items). Piping or redirecting the\n")
		fmt.Fprintf(os.Stderr, "output, or passing --all, always prints the entire queue.\n")
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	var all bool
	fs.BoolVar(&all, "all", false, "show the entire queue, even if it doesn't fit the terminal")
	fs.BoolVar(&all, "a", false, "shorthand for --all")
	fs.Parse(args)

	requireIP(fs, *ip)

	// See what's currently loaded, if anything, so we can mark it below.
	var pos positionInfo
	var nowPlaying queueTrack
	var havePos bool
	if state, err := getTransportState(*ip); err == nil {
		switch state {
		case "PLAYING", "PAUSED_PLAYBACK", "TRANSITIONING":
			if p, err := getPositionInfo(*ip); err == nil {
				pos = p
				havePos = true
				nowPlaying, _ = parseDIDLItem(pos.TrackMetaData)
				if nowPlaying.Title == "" && nowPlaying.URI == "" {
					nowPlaying.URI = pos.TrackURI
				}
				if state == "PAUSED_PLAYBACK" {
					pos.paused = true
				}
			}
		}
	}

	tracks, total, err := browseQueue(*ip, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if total == 0 {
		if havePos && nowPlaying.URI != "" {
			fmt.Println(nowPlayingLine(nowPlaying, pos))
		} else {
			fmt.Println("queue is empty")
		}
		return
	}

	matched := -1
	if havePos && pos.TrackURI != "" {
		for i, t := range tracks {
			if t.URI == pos.TrackURI {
				matched = i
				break
			}
		}
	}

	// Playing something that isn't part of the queue (e.g. a one-off URL)?
	// There's no row to attach the marker to, so call it out separately.
	extraLines := 0
	if havePos && matched == -1 && nowPlaying.URI != "" {
		fmt.Println(nowPlayingLine(nowPlaying, pos) + "  (not queued)")
		extraLines++
	}

	// If stdout is a terminal and we can determine its size, truncate the
	// list to fit the screen (leaving a little room to spare), rather
	// than dumping potentially hundreds of lines. The ellipsis rows below
	// mark whichever ends got cut off. --all (or stdout not being a
	// terminal, e.g. piped or redirected) always shows everything.
	maxLines := len(tracks)
	if !all {
		if _, rows, ok := terminalSize(os.Stdout.Fd()); ok {
			if budget := rows - terminalMargin - terminalSafetyMargin(rows) - extraLines; budget < maxLines {
				maxLines = budget
			}
		}
	}

	start, end, topEllipsis, bottomEllipsis := visibleWindow(len(tracks), matched, maxLines)

	if topEllipsis {
		fmt.Printf("... %d before\n", start)
	}
	for i := start; i < end; i++ {
		fmt.Println(formatQueueRow(i+1, tracks[i], i == matched, pos))
	}
	if bottomEllipsis {
		fmt.Printf("... %d more\n", len(tracks)-end)
	}
}

func runRemove(args []string) {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s remove [--ip <speaker-ip>] [-y|--yes] <selector> [<selector> ...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nRemoves one or more tracks from the current play queue. Positions are\n")
		fmt.Fprintf(os.Stderr, "1-based, matching '%s list' output.\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach <selector> is one of:\n")
		fmt.Fprintf(os.Stderr, "  N         a single queue position, e.g. \"5\"\n")
		fmt.Fprintf(os.Stderr, "  A-B       an inclusive range, e.g. \"1-9\"\n")
		fmt.Fprintf(os.Stderr, "  A..B      same as A-B, e.g. \"1..9\"\n")
		fmt.Fprintf(os.Stderr, "  A-        from A to the end of the queue, e.g. \"9-\"\n")
		fmt.Fprintf(os.Stderr, "  @word...  every track matching a '%s find'-style search, e.g. \"@amazing grace\"\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nSeveral @-selectors narrow the search rather than searching\n")
		fmt.Fprintf(os.Stderr, "separately: \"@foo @bar\" matches tracks containing both foo and bar\n")
		fmt.Fprintf(os.Stderr, "(the same as \"@\\\"foo bar\\\"\"), not foo or bar. Number/range selectors\n")
		fmt.Fprintf(os.Stderr, "add to the result instead of narrowing it.\n")
		fmt.Fprintf(os.Stderr, "\nAn @pattern selector matching more than one track requires -y/--yes\n")
		fmt.Fprintf(os.Stderr, "to confirm removing all of them; the matches are listed so you can\n")
		fmt.Fprintf(os.Stderr, "check before re-running with -y. Plain number/range selectors never\n")
		fmt.Fprintf(os.Stderr, "require -y, however many are given.\n")
		fmt.Fprintf(os.Stderr, "\nMultiple selectors may be given at once, e.g.:\n")
		fmt.Fprintf(os.Stderr, "  %s remove 1 5 9\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s remove 1-9\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s remove -y @amazing\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	var yes bool
	fs.BoolVar(&yes, "yes", false, "don't require confirmation when an @pattern selector matches multiple tracks")
	fs.BoolVar(&yes, "y", false, "shorthand for --yes")
	fs.Parse(args)

	requireIP(fs, *ip)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}

	tracks, total, err := browseQueue(*ip, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if total == 0 {
		fmt.Fprintln(os.Stderr, "error: queue is empty")
		os.Exit(1)
	}

	indices, patternHits, err := resolveSelectors(fs.Args(), tracks)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	matched, pos, _ := currentQueuePosition(*ip, tracks)
	confirmMultiMatchOrExit(patternHits, yes, tracks, matched, pos, "remove")

	// Show what's about to be removed before touching the queue.
	// indices is already sorted ascending by resolveSelectors.
	for _, i := range indices {
		fmt.Printf("removing %3d. %s\n", i, formatTrack(tracks[i-1]))
	}

	// Group the sorted, 1-based indices into maximal contiguous ranges, so
	// each range can be removed with a single RemoveTrackRangeFromQueue
	// call instead of one call per track.
	type trackRange struct{ start, count int }
	var ranges []trackRange
	for _, i := range indices {
		if n := len(ranges); n > 0 && ranges[n-1].start+ranges[n-1].count == i {
			ranges[n-1].count++
		} else {
			ranges = append(ranges, trackRange{start: i, count: 1})
		}
	}

	// Remove ranges from highest starting index to lowest: removing a
	// higher range doesn't shift the position of lower, not-yet-removed
	// tracks, so later calls in the loop still target the right indices.
	removed := 0
	for j := len(ranges) - 1; j >= 0; j-- {
		r := ranges[j]
		if err := removeTrackRangeFromQueue(*ip, r.start, r.count); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		removed += r.count
	}

	fmt.Printf("removed %d track(s)\n", removed)
}

// bumpTargets resolves which tracks runBump needs to move: every
// position in selected except currentPosition itself (which is already
// "now", not "next", so it's silently dropped rather than moved),
// returned as a list of trackKey identifiers in the same relative order
// as selected (which resolveSelectors returns sorted ascending - i.e.
// queue order), for use with findTrackPosition against subsequent,
// freshly re-read queue snapshots. tracks must correspond 1:1 with
// selected/currentPosition's positions (i.e. the same snapshot they were
// computed against).
func bumpTargets(selected []int, currentPosition int, tracks []queueTrack) (targets []string, droppedCurrent int) {
	for _, p := range selected {
		if p == currentPosition {
			droppedCurrent++
			continue
		}
		targets = append(targets, trackKey(tracks[p-1]))
	}
	return targets, droppedCurrent
}

func runBump(args []string) {
	fs := flag.NewFlagSet("bump", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s bump [--ip <speaker-ip>] [-y|--yes] <selector> [<selector> ...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nMoves the matched/selected tracks to play immediately after whatever\n")
		fmt.Fprintf(os.Stderr, "is currently playing, without otherwise changing the queue's order.\n")
		fmt.Fprintf(os.Stderr, "Positions are 1-based, matching '%s list' output.\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach <selector> is one of:\n")
		fmt.Fprintf(os.Stderr, "  N         a single queue position, e.g. \"5\"\n")
		fmt.Fprintf(os.Stderr, "  A-B       an inclusive range, e.g. \"1-9\"\n")
		fmt.Fprintf(os.Stderr, "  A..B      same as A-B, e.g. \"1..9\"\n")
		fmt.Fprintf(os.Stderr, "  A-        from A to the end of the queue, e.g. \"9-\"\n")
		fmt.Fprintf(os.Stderr, "  @word...  every track matching a '%s find'-style search, e.g. \"@amazing grace\"\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nSeveral @-selectors narrow the search rather than searching\n")
		fmt.Fprintf(os.Stderr, "separately: \"@foo @bar\" matches tracks containing both foo and bar\n")
		fmt.Fprintf(os.Stderr, "(the same as \"@\\\"foo bar\\\"\"), not foo or bar. Number/range selectors\n")
		fmt.Fprintf(os.Stderr, "add to the result instead of narrowing it.\n")
		fmt.Fprintf(os.Stderr, "\nAn @pattern selector matching more than one track requires -y/--yes\n")
		fmt.Fprintf(os.Stderr, "to confirm bumping all of them; the matches are listed so you can\n")
		fmt.Fprintf(os.Stderr, "check before re-running with -y. Plain number/range selectors never\n")
		fmt.Fprintf(os.Stderr, "require -y, however many are given.\n")
		fmt.Fprintf(os.Stderr, "\nSomething must currently be playing from the queue - there's otherwise\n")
		fmt.Fprintf(os.Stderr, "no well-defined \"next\" position to move tracks to. If a selected\n")
		fmt.Fprintf(os.Stderr, "track is the one currently playing, it's silently left alone (it's\n")
		fmt.Fprintf(os.Stderr, "already \"now\", not \"next\").\n")
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s bump @amazing\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s bump 42 50-52\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	var yes bool
	fs.BoolVar(&yes, "yes", false, "don't require confirmation when an @pattern selector matches multiple tracks")
	fs.BoolVar(&yes, "y", false, "shorthand for --yes")
	fs.Parse(args)

	requireIP(fs, *ip)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}

	tracks, total, err := browseQueue(*ip, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if total == 0 {
		fmt.Fprintln(os.Stderr, "error: queue is empty")
		os.Exit(1)
	}

	positions, patternHits, err := resolveSelectors(fs.Args(), tracks)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	// Look up what's currently playing (if anything) before the -y
	// guard below, purely so an ambiguous pattern's matches can be
	// displayed with the usual "▶" marker if the now-playing track
	// happens to be among them; the guard itself only depends on
	// patternHits. Whether something usable is actually playing is
	// checked separately, right after, since an @pattern match needing
	// -y/--yes is a more specific problem than "nothing is playing" and
	// should be reported first if both apply.
	matched, pos, havePos := currentQueuePosition(*ip, tracks)

	confirmMultiMatchOrExit(patternHits, yes, tracks, matched, pos, "bump")

	if !havePos || matched == -1 {
		fmt.Fprintln(os.Stderr, "error: nothing is currently playing from the queue to bump tracks after")
		os.Exit(1)
	}
	currentPosition := matched + 1 // currentQueuePosition's matched is 0-based; selectors/positions are 1-based
	anchorKey := trackKey(tracks[matched])

	targets, droppedCurrent := bumpTargets(positions, currentPosition, tracks)
	if droppedCurrent > 0 {
		fmt.Printf("skipping %d selected track(s) already at the current position\n", droppedCurrent)
	}
	if len(targets) == 0 {
		fmt.Println("nothing to do")
		os.Exit(0)
	}

	// Move one target at a time, immediately after whatever was placed
	// right before it (the current track, for the first target; the
	// previous target, thereafter). Each iteration re-reads the live
	// queue and re-locates both the anchor and the target by trackKey
	// rather than trusting position arithmetic across multiple calls:
	// a single ReorderTracksInQueue call's effect on *other* tracks'
	// positions depends on whether the moved track started before or
	// after the insertion point (see reorderTrackRangeInQueue), and that
	// relationship can flip between one target and the next (e.g. a
	// target upstream of the current track shifts the current track's
	// own position) - so static bookkeeping across calls is fragile.
	// Re-reading is simply correct by construction, at the cost of one
	// extra Browse call per moved track, which is cheap relative to the
	// interactive, low-volume nature of this command.
	moved := 0
	for _, targetKey := range targets {
		liveTracks, _, err := browseQueue(*ip, 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: moved %d track(s) before failing: %v\n", moved, err)
			os.Exit(1)
		}
		anchorPos, ok := findTrackPosition(liveTracks, anchorKey)
		if !ok {
			fmt.Fprintf(os.Stderr, "error: moved %d track(s) before failing: lost track of the current queue position\n", moved)
			os.Exit(1)
		}
		targetPos, ok := findTrackPosition(liveTracks, targetKey)
		if !ok {
			// No longer in the queue (e.g. removed concurrently by
			// something else) - nothing to move, skip it rather than
			// aborting the rest of the batch.
			continue
		}
		if targetPos != anchorPos+1 {
			if err := reorderTrackRangeInQueue(*ip, targetPos, 1, anchorPos+1); err != nil {
				fmt.Fprintf(os.Stderr, "error: moved %d track(s) before failing: %v\n", moved, err)
				os.Exit(1)
			}
		}
		anchorKey = targetKey
		moved++
	}
	fmt.Printf("moved %d track(s) to play next\n", moved)

	// Show the result: re-browse (positions have shifted) and print the
	// now-playing row plus the newly bumped tracks right after it, so
	// the user can see where things landed without running a separate
	// 'list'.
	newTracks, newTotal, err := browseQueue(*ip, 0)
	if err != nil {
		return // best-effort; the move itself already succeeded
	}
	newMatched, newPos, _ := currentQueuePosition(*ip, newTracks)
	start := newMatched
	if start < 0 {
		start = 0
	}
	end := start + 1 + moved
	if end > newTotal {
		end = newTotal
	}
	for i := start; i < end; i++ {
		fmt.Println(formatQueueRow(i+1, newTracks[i], i == newMatched, newPos))
	}
}

// nowPlayingLine renders the "▶ Title [elapsed/duration]" line used when
// the currently playing track has no corresponding queue row to mark.
func nowPlayingLine(t queueTrack, pos positionInfo) string {
	return "▶ " + formatTrack(t) + nowPlayingSuffix(pos)
}

// nowPlayingSuffix renders "  [elapsed/duration]" (plus "  (paused)" if
// applicable), or "" if there's no useful timing info.
func nowPlayingSuffix(pos positionInfo) string {
	suffix := ""
	if pos.RelTime != "" && pos.TrackDuration != "" && pos.TrackDuration != "0:00:00" {
		suffix = fmt.Sprintf("  [%s/%s]", pos.RelTime, pos.TrackDuration)
	}
	if pos.paused {
		suffix += "  (paused)"
	}
	return suffix
}

// runFind implements "find"/"f": search the queue by title/artist/album/
// file name and print the matching rows, in queue order.
func runFind(args []string) {
	fs := flag.NewFlagSet("find", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s find [--ip <speaker-ip>] [-n|--numbers-only] <word> [<word> ...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nSearches the current play queue for tracks whose title, artist,\n")
		fmt.Fprintf(os.Stderr, "album, or file name contains every given word, case-insensitively\n")
		fmt.Fprintf(os.Stderr, "(an AND match; word order and which field each word matches don't\n")
		fmt.Fprintf(os.Stderr, "matter). Matching rows are printed in queue order, e.g.:\n")
		fmt.Fprintf(os.Stderr, "  %s find amazing grace\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nWith -n/--numbers-only, only the matching 1-based queue positions\n")
		fmt.Fprintf(os.Stderr, "are printed, one per line, with no other output - handy for scripting,\n")
		fmt.Fprintf(os.Stderr, "e.g.:\n")
		fmt.Fprintf(os.Stderr, "  %s rm $(%s find -n foo)\n", os.Args[0], os.Args[0])
		fmt.Fprintf(os.Stderr, "\nExits 1 if nothing matches (like grep), so the result is scriptable.\n")
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	var numbersOnly bool
	fs.BoolVar(&numbersOnly, "numbers-only", false, "print only the matching 1-based positions, one per line")
	fs.BoolVar(&numbersOnly, "n", false, "shorthand for --numbers-only")
	fs.Parse(args)

	requireIP(fs, *ip)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}
	words := fs.Args()

	// See what's currently loaded, if anything, so a match that happens
	// to also be the now-playing track gets the "▶" marker and elapsed
	// time suffix (same pattern as runList).
	var pos positionInfo
	var havePos bool
	if state, err := getTransportState(*ip); err == nil {
		switch state {
		case "PLAYING", "PAUSED_PLAYBACK", "TRANSITIONING":
			if p, err := getPositionInfo(*ip); err == nil {
				pos = p
				havePos = true
				if state == "PAUSED_PLAYBACK" {
					pos.paused = true
				}
			}
		}
	}

	tracks, _, err := browseQueue(*ip, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	matched := -1
	if havePos && pos.TrackURI != "" {
		for i, t := range tracks {
			if t.URI == pos.TrackURI {
				matched = i
				break
			}
		}
	}

	positions := matchQueue(tracks, words)
	if len(positions) == 0 {
		if !numbersOnly {
			fmt.Println("no tracks match:", strings.Join(words, " "))
		}
		os.Exit(1)
	}

	for _, p := range positions {
		if numbersOnly {
			fmt.Println(p)
			continue
		}
		fmt.Println(formatQueueRow(p, tracks[p-1], p-1 == matched, pos))
	}
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s status [--ip <speaker-ip>]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nShows the speaker's transport state, volume, mute state, and what's\n")
		fmt.Fprintf(os.Stderr, "currently playing (if anything). See also '%s list' for the full queue.\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	fs.Parse(args)

	requireIP(fs, *ip)

	state, err := getTransportState(*ip)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println("state:", state)

	vol, volErr := getVolume(*ip)
	muted, muteErr := getMute(*ip)
	switch {
	case volErr == nil && muteErr == nil && muted:
		fmt.Printf("volume: %d (muted)\n", vol)
	case volErr == nil:
		fmt.Printf("volume: %d\n", vol)
	case muteErr == nil && muted:
		fmt.Println("volume: unknown (muted)")
	default:
		fmt.Println("volume: unknown")
	}

	switch state {
	case "PLAYING", "PAUSED_PLAYBACK", "TRANSITIONING":
		pos, err := getPositionInfo(*ip)
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not get now-playing details:", err)
			return
		}
		track, _ := parseDIDLItem(pos.TrackMetaData)
		if track.Title == "" && track.URI == "" {
			track.URI = pos.TrackURI
		}
		pos.paused = state == "PAUSED_PLAYBACK"
		fmt.Println("now playing:", formatTrack(track)+nowPlayingSuffix(pos))
	}
}

// runSkip handles "next"/"n" and "prev"/"previous"/"p": it skips to the
// next or previous track, then prints what's now playing (best-effort).
func runSkip(name string, args []string, skip func(ip string) error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		verb := "next"
		if name == "prev" || name == "previous" || name == "p" {
			verb = "previous"
		}
		fmt.Fprintf(os.Stderr, "usage: %s %s [--ip <speaker-ip>]\n", os.Args[0], name)
		fmt.Fprintf(os.Stderr, "\nSkips to the %s track.\n", verb)
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	fs.Parse(args)

	requireIP(fs, *ip)

	if err := skip(*ip); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	pos, err := getPositionInfo(*ip)
	if err != nil {
		// Not fatal: the skip itself succeeded.
		return
	}
	track, _ := parseDIDLItem(pos.TrackMetaData)
	if track.Title == "" && track.URI == "" {
		track.URI = pos.TrackURI
	}
	if formatted := formatTrack(track); formatted != "" {
		fmt.Println("now playing:", formatted)
	}
}

func runStop(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s stop [--ip <speaker-ip>]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nStops playback on the speaker.\n")
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	fs.Parse(args)

	requireIP(fs, *ip)

	if err := stopPlayback(*ip); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println("stopped")
}

// likeTarget is one thing for runLike/runUnlike to act on: either the
// currently playing track (the no-selector-args case; position == 0,
// since there's no queue position involved - it may not even be part of
// the saved queue) or a specific 1-based queue position and its track
// (the selector-args case).
type likeTarget struct {
	position int
	track    queueTrack
}

// buildLikeTargets converts resolved 1-based queue positions (as
// returned by resolveSelectors, already sorted ascending) into
// likeTargets by looking up each one's track in tracks. Pulled out of
// resolveLikeTargets purely so this part is unit-testable without a
// live speaker.
func buildLikeTargets(positions []int, tracks []queueTrack) []likeTarget {
	targets := make([]likeTarget, 0, len(positions))
	for _, p := range positions {
		targets = append(targets, likeTarget{position: p, track: tracks[p-1]})
	}
	return targets
}

// resolveLikeTargets implements the target-selection logic shared by
// runLike and runUnlike:
//
//   - With no selector args, the single currently playing track,
//     wherever it came from (not necessarily part of the saved queue -
//     this also works right after a one-off "play <url>", since only
//     title/artist/album/URI from positionInfo are needed, not a queue
//     position). Returned as a single likeTarget with position == 0.
//   - With selector args, the matching queue position(s) (via
//     browseQueue + resolveSelectors), subject to the same -y/--yes
//     multi-match-@pattern guard remove/bump use
//     (confirmMultiMatchOrExit).
//
// Like every other run* function, this prints its own error message and
// exits rather than returning an error: the right exit code differs by
// failure (1 for "nothing is playing"/"queue is empty", 2 for bad
// selector syntax or an unconfirmed multi-match - the last of which
// confirmMultiMatchOrExit itself handles), so there's no single error
// value that would let callers decide uniformly anyway. verb (e.g.
// "like", "unlike") is used in the "nothing is currently playing"
// message and passed through to confirmMultiMatchOrExit.
func resolveLikeTargets(ip string, args []string, yes bool, verb string) []likeTarget {
	if len(args) == 0 {
		var pos positionInfo
		var havePos bool
		if state, err := getTransportState(ip); err == nil {
			switch state {
			case "PLAYING", "PAUSED_PLAYBACK", "TRANSITIONING":
				if p, err := getPositionInfo(ip); err == nil {
					pos = p
					havePos = true
					if state == "PAUSED_PLAYBACK" {
						pos.paused = true
					}
				}
			}
		}
		track, _ := parseDIDLItem(pos.TrackMetaData)
		if track.Title == "" && track.URI == "" {
			track.URI = pos.TrackURI
		}
		if !havePos || track.URI == "" {
			fmt.Fprintf(os.Stderr, "error: nothing is currently playing to %s\n", verb)
			os.Exit(1)
		}
		return []likeTarget{{track: track}}
	}

	tracks, total, err := browseQueue(ip, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if total == 0 {
		fmt.Fprintln(os.Stderr, "error: queue is empty")
		os.Exit(1)
	}

	positions, patternHits, err := resolveSelectors(args, tracks)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	matched, pos, _ := currentQueuePosition(ip, tracks)
	confirmMultiMatchOrExit(patternHits, yes, tracks, matched, pos, verb)

	return buildLikeTargets(positions, tracks)
}

// openDBOrExit resolves dbFlagValue (the --db flag) to a path (see
// dbPath) and opens it (see openDB), printing a clear error and exiting
// 1 on failure at either step. Shared by runLike, runUnlike, runLikes,
// and (once wired up) runWatch/runHistory - anything touching the
// shared likes/play-history database.
func openDBOrExit(dbFlagValue string) *sql.DB {
	path, isDefault, err := dbPath(dbFlagValue)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: open database:", err)
		os.Exit(1)
	}
	db, err := openDB(path, isDefault)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: open database:", err)
		os.Exit(1)
	}
	return db
}

func runLike(args []string) {
	fs := flag.NewFlagSet("like", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s like [--ip <speaker-ip>] [--db <path>] [-y|--yes] [--last|<selector> ...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nRecords a song in a persistent \"likes\" database (separate from, and\n")
		fmt.Fprintf(os.Stderr, "surviving, the speaker's play queue), for later listing or replay\n")
		fmt.Fprintf(os.Stderr, "with '%s likes'.\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nWith no <selector> arguments, likes whatever is currently playing\n")
		fmt.Fprintf(os.Stderr, "(whether or not it's part of the saved queue - this also works right\n")
		fmt.Fprintf(os.Stderr, "after a one-off '%s play <url>').\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nWith <selector> arguments, likes the matched/selected tracks in the\n")
		fmt.Fprintf(os.Stderr, "current play queue instead. Positions are 1-based, matching\n")
		fmt.Fprintf(os.Stderr, "'%s list' output. Each <selector> is one of:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  N         a single queue position, e.g. \"5\"\n")
		fmt.Fprintf(os.Stderr, "  A-B       an inclusive range, e.g. \"1-9\"\n")
		fmt.Fprintf(os.Stderr, "  A..B      same as A-B, e.g. \"1..9\"\n")
		fmt.Fprintf(os.Stderr, "  A-        from A to the end of the queue, e.g. \"9-\"\n")
		fmt.Fprintf(os.Stderr, "  @word...  every track matching a '%s find'-style search, e.g. \"@amazing grace\"\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nSeveral @-selectors narrow the search rather than searching\n")
		fmt.Fprintf(os.Stderr, "separately, the same AND semantics as remove/bump; an @pattern\n")
		fmt.Fprintf(os.Stderr, "matching more than one track requires -y/--yes to confirm liking all\n")
		fmt.Fprintf(os.Stderr, "of them.\n")
		fmt.Fprintf(os.Stderr, "\nLiking an already-liked song just adds another like event rather\n")
		fmt.Fprintf(os.Stderr, "than erroring or no-op'ing - it's a history, not a toggle, so\n")
		fmt.Fprintf(os.Stderr, "'%s likes --history' shows every time you liked something, and\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "'%s likes' shows each song's total like count.\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nNote: liked tracks are replayed by their bare URL/file; this works\n")
		fmt.Fprintf(os.Stderr, "fine for ordinary files and streams, but special per-service metadata\n")
		fmt.Fprintf(os.Stderr, "(e.g. Spotify) isn't preserved, so such liked tracks may not replay\n")
		fmt.Fprintf(os.Stderr, "correctly with '%s likes --play'.\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\n--last likes the previous song from play history (see '%s history')\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "- the most recent play that isn't the current track - instead of\n")
		fmt.Fprintf(os.Stderr, "what's currently playing or a selector; can't be combined with\n")
		fmt.Fprintf(os.Stderr, "selector arguments.\n")
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n")
		fmt.Fprintf(os.Stderr, "  --db  SONOS_DB          db\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	dbFlag := fs.String("db", "", "override the likes database path")
	var yes, last bool
	fs.BoolVar(&yes, "yes", false, "don't require confirmation when an @pattern selector matches multiple tracks")
	fs.BoolVar(&yes, "y", false, "shorthand for --yes")
	fs.BoolVar(&last, "last", false, "like the previous song from play history, instead of what's currently playing or a selector")
	fs.Parse(args)

	requireIP(fs, *ip)

	if last && fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "error: --last can't be combined with a selector")
		fs.Usage()
		os.Exit(2)
	}

	db := openDBOrExit(*dbFlag)
	defer db.Close()

	if last {
		// Best-effort: find out what's currently playing (if anything)
		// so it can be excluded from "the previous song" - if this
		// fails for any reason, just proceed without excluding
		// anything, rather than failing --last entirely over it.
		currentURI := ""
		if pos, err := getPositionInfo(*ip); err == nil {
			track, _ := parseDIDLItem(pos.TrackMetaData)
			if track.URI == "" {
				track.URI = pos.TrackURI
			}
			currentURI = track.URI
		}
		p, ok, err := mostRecentPlay(db, currentURI)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "error: no play history yet")
			os.Exit(1)
		}
		if err := addLike(db, p.URI, p.Title, p.Artist, p.Album); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println("liked:", formatTrack(queueTrack{Title: p.Title, Artist: p.Artist, Album: p.Album}))
		return
	}

	targets := resolveLikeTargets(*ip, fs.Args(), yes, "like")

	if len(targets) == 1 && targets[0].position == 0 {
		t := targets[0].track
		if err := addLike(db, t.URI, t.Title, t.Artist, t.Album); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println("liked:", formatTrack(t))
		return
	}

	liked := 0
	for _, tgt := range targets {
		t := tgt.track
		if err := addLike(db, t.URI, t.Title, t.Artist, t.Album); err != nil {
			fmt.Fprintf(os.Stderr, "error: liked %d track(s) before failing: %v\n", liked, err)
			os.Exit(1)
		}
		fmt.Printf("liked %3d. %s\n", tgt.position, formatTrack(t))
		liked++
	}
	fmt.Printf("liked %d track(s)\n", liked)
}

func runUnlike(args []string) {
	fs := flag.NewFlagSet("unlike", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s unlike [--ip <speaker-ip>] [--db <path>] [-y|--yes] [<selector> ...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nRemoves a song from the \"likes\" database (see '%s like -h'). This\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "removes ALL of that song's like history, not just its most recent\n")
		fmt.Fprintf(os.Stderr, "like - a song liked three times is fully unliked in one 'unlike', not\n")
		fmt.Fprintf(os.Stderr, "left with two likes remaining.\n")
		fmt.Fprintf(os.Stderr, "\nWith no <selector> arguments, unlikes whatever is currently playing\n")
		fmt.Fprintf(os.Stderr, "(whether or not it's part of the saved queue).\n")
		fmt.Fprintf(os.Stderr, "\nWith <selector> arguments, unlikes the matched/selected tracks in the\n")
		fmt.Fprintf(os.Stderr, "current play queue instead. Positions are 1-based, matching\n")
		fmt.Fprintf(os.Stderr, "'%s list' output. Each <selector> is one of:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  N         a single queue position, e.g. \"5\"\n")
		fmt.Fprintf(os.Stderr, "  A-B       an inclusive range, e.g. \"1-9\"\n")
		fmt.Fprintf(os.Stderr, "  A..B      same as A-B, e.g. \"1..9\"\n")
		fmt.Fprintf(os.Stderr, "  A-        from A to the end of the queue, e.g. \"9-\"\n")
		fmt.Fprintf(os.Stderr, "  @word...  every track matching a '%s find'-style search, e.g. \"@amazing grace\"\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nSeveral @-selectors narrow the search rather than searching\n")
		fmt.Fprintf(os.Stderr, "separately, the same AND semantics as remove/bump; an @pattern\n")
		fmt.Fprintf(os.Stderr, "matching more than one track requires -y/--yes to confirm unliking\n")
		fmt.Fprintf(os.Stderr, "all of them.\n")
		fmt.Fprintf(os.Stderr, "\nUnliking a song that was never liked is reported, not an error.\n")
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n")
		fmt.Fprintf(os.Stderr, "  --db  SONOS_DB          db\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	dbFlag := fs.String("db", "", "override the likes database path")
	var yes bool
	fs.BoolVar(&yes, "yes", false, "don't require confirmation when an @pattern selector matches multiple tracks")
	fs.BoolVar(&yes, "y", false, "shorthand for --yes")
	fs.Parse(args)

	requireIP(fs, *ip)

	db := openDBOrExit(*dbFlag)
	defer db.Close()

	targets := resolveLikeTargets(*ip, fs.Args(), yes, "unlike")

	if len(targets) == 1 && targets[0].position == 0 {
		t := targets[0].track
		removed, err := deleteLikes(db, t.URI)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if removed > 0 {
			fmt.Println("unliked:", formatTrack(t))
		} else {
			fmt.Println("not liked:", formatTrack(t))
		}
		return
	}

	unliked, notLiked := 0, 0
	for _, tgt := range targets {
		t := tgt.track
		removed, err := deleteLikes(db, t.URI)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: unliked %d track(s) before failing: %v\n", unliked, err)
			os.Exit(1)
		}
		if removed > 0 {
			fmt.Printf("unliked %3d. %s\n", tgt.position, formatTrack(t))
			unliked++
		} else {
			fmt.Printf("not liked %3d. %s\n", tgt.position, formatTrack(t))
			notLiked++
		}
	}
	if notLiked == 0 {
		fmt.Printf("unliked %d track(s)\n", unliked)
	} else {
		fmt.Printf("unliked %d track(s) (%d weren't liked)\n", unliked, notLiked)
	}
}

// likedSongLine renders one listLikedSongs row for "likes": its total
// like count, when it was last liked, and its track info.
func likedSongLine(s likedSong) string {
	return fmt.Sprintf("%2dx  %s  %s", s.LikeCount, s.LastLiked.Local().Format("2006-01-02 15:04"),
		formatTrack(queueTrack{Title: s.Title, Artist: s.Artist, Album: s.Album}))
}

// likeEntryLine renders one listLikeHistory row for "likes --history":
// its own timestamp (not aggregated, unlike likedSongLine) and track
// info.
func likeEntryLine(e likeEntry) string {
	return fmt.Sprintf("%s  %s", e.LikedAt.Local().Format("2006-01-02 15:04:05"),
		formatTrack(queueTrack{Title: e.Title, Artist: e.Artist, Album: e.Album}))
}

func runLikes(args []string) {
	fs := flag.NewFlagSet("likes", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s likes [--db <path>] [-n|--numbers-only] [--history] [--play [--now]] [--ip <ip>]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nLists songs recorded by '%s like' (most recently liked first), each\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "with its total like count and when it was last liked.\n")
		fmt.Fprintf(os.Stderr, "\n-n/--history/--play are mutually exclusive (pick at most one):\n")
		fmt.Fprintf(os.Stderr, "  -n, --numbers-only  print only each song's URI, one per line (for\n")
		fmt.Fprintf(os.Stderr, "                      scripting, e.g. piping into another command);\n")
		fmt.Fprintf(os.Stderr, "                      exits 1 if there are no liked songs, like find -n\n")
		fmt.Fprintf(os.Stderr, "  --history           show every like event (not deduplicated by\n")
		fmt.Fprintf(os.Stderr, "                      song - a song liked 3 times shows 3 times),\n")
		fmt.Fprintf(os.Stderr, "                      newest first, each with its own timestamp\n")
		fmt.Fprintf(os.Stderr, "  --play              queue every liked song and play them (most\n")
		fmt.Fprintf(os.Stderr, "                      recently liked first); appends to the queue\n")
		fmt.Fprintf(os.Stderr, "                      without interrupting playback unless --now (or\n")
		fmt.Fprintf(os.Stderr, "                      nothing is currently playing) is given; a song\n")
		fmt.Fprintf(os.Stderr, "                      that fails to queue is skipped with a message\n")
		fmt.Fprintf(os.Stderr, "                      rather than aborting the rest\n")
		fmt.Fprintf(os.Stderr, "\nNote: liked tracks are replayed by their bare URL/file, so special\n")
		fmt.Fprintf(os.Stderr, "per-service metadata (e.g. Spotify) isn't preserved - see '%s like -h'.\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s likes\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s likes --history\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s likes --play\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip        (only used with --play)\n")
		fmt.Fprintf(os.Stderr, "  --db  SONOS_DB          db\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address (only used with --play)")
	dbFlag := fs.String("db", "", "override the likes database path")
	var numbersOnly, history, play, now bool
	fs.BoolVar(&numbersOnly, "numbers-only", false, "print only each song's URI, one per line")
	fs.BoolVar(&numbersOnly, "n", false, "shorthand for --numbers-only")
	fs.BoolVar(&history, "history", false, "show every like event instead of one row per song")
	fs.BoolVar(&play, "play", false, "queue and play every liked song")
	fs.BoolVar(&now, "now", false, "with --play, play immediately instead of appending to the queue")
	fs.Parse(args)

	if history && play {
		fmt.Fprintln(os.Stderr, "error: --history and --play can't be combined")
		fs.Usage()
		os.Exit(2)
	}
	if numbersOnly && (history || play) {
		fmt.Fprintln(os.Stderr, "error: -n/--numbers-only can't be combined with --history or --play")
		fs.Usage()
		os.Exit(2)
	}

	db := openDBOrExit(*dbFlag)
	defer db.Close()

	switch {
	case history:
		entries, err := listLikeHistory(db)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if len(entries) == 0 {
			fmt.Println("no liked songs yet")
			return
		}
		for _, e := range entries {
			fmt.Println(likeEntryLine(e))
		}
		return

	case play:
		requireIP(fs, *ip)

		songs, err := listLikedSongs(db)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if len(songs) == 0 {
			fmt.Println("no liked songs yet")
			return
		}

		firstTrackNumber := 0
		queueLength := 0
		queuedCount := 0
		for _, s := range songs {
			firstNum, newLength, err := addURIToQueue(*ip, s.URI, false)
			if err != nil {
				fmt.Fprintf(os.Stderr, "skipping %q: %v\n", formatTrack(queueTrack{Title: s.Title, Artist: s.Artist, Album: s.Album}), err)
				continue
			}
			if queuedCount == 0 {
				firstTrackNumber = firstNum
			}
			queueLength = newLength
			queuedCount++
		}

		if queuedCount == 0 {
			fmt.Fprintln(os.Stderr, "error: failed to queue any liked songs")
			os.Exit(1)
		}

		playImmediately := now
		if !playImmediately {
			state, err := getTransportState(*ip)
			playImmediately = err != nil || state != "PLAYING"
		}

		if !playImmediately {
			fmt.Printf("speaker is already playing; queued %d liked song(s) (queue length: %d)\n", queuedCount, queueLength)
			return
		}

		if err := playQueueIndex(*ip, firstTrackNumber); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Printf("queued %d liked song(s) and started playing from item %d\n", queuedCount, firstTrackNumber)
		return
	}

	// Default listing mode (also handles -n/--numbers-only).
	songs, err := listLikedSongs(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if numbersOnly {
		if len(songs) == 0 {
			os.Exit(1)
		}
		for _, s := range songs {
			fmt.Println(s.URI)
		}
		return
	}

	if len(songs) == 0 {
		fmt.Println("no liked songs yet")
		return
	}
	for _, s := range songs {
		fmt.Println(likedSongLine(s))
	}
}

// likedURISet returns the set of every liked song's URI, for ♥-marking
// play-history rows (see historyPlayLine/topPlayedLine). Built once per
// command invocation rather than querying per-row.
func likedURISet(db *sql.DB) (map[string]bool, error) {
	songs, err := listLikedSongs(db)
	if err != nil {
		return nil, err
	}
	liked := make(map[string]bool, len(songs))
	for _, s := range songs {
		liked[s.URI] = true
	}
	return liked, nil
}

// historyPlayLine renders one listPlays row for "history": a ♥ marker if
// the song is liked, when it was played, and the track itself.
func historyPlayLine(p play, liked bool) string {
	marker := "  "
	if liked {
		marker = "♥ "
	}
	return fmt.Sprintf("%s%s  %s", marker, p.PlayedAt.Local().Format("2006-01-02 15:04:05"),
		formatTrack(queueTrack{Title: p.Title, Artist: p.Artist, Album: p.Album}))
}

// topPlayedLine renders one topPlayed row for "history --top": a ♥
// marker if liked, the play count, and the track itself.
func topPlayedLine(c playCount, liked bool) string {
	marker := "  "
	if liked {
		marker = "♥ "
	}
	return fmt.Sprintf("%s%2dx  %s", marker, c.PlayCount,
		formatTrack(queueTrack{Title: c.Title, Artist: c.Artist, Album: c.Album}))
}

// parseSinceFlag parses the --since flag for "history": "" (no filter,
// returns the zero time.Time), "today" (local midnight today), a bare
// "YYYY-MM-DD" date (local midnight that day), or a Go duration (e.g.
// "24h", "2h30m" - ago from now). Go's time.ParseDuration doesn't
// understand "d"/"w" units, so those aren't accepted here either; use
// "24h"/"168h" or a specific date instead.
func parseSinceFlag(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if s == "today" {
		now := time.Now()
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf(`invalid --since %q: want "today", a date like "2026-01-02", or a duration like "24h"`, s)
}

func runHistory(args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s history [--db <path>] [-n <count>|--all] [--since <when>] [--top]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nShows songs recorded by '%s play' (most recently played first) - only\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "a single song explicitly started playing right now is ever recorded;\n")
		fmt.Fprintf(os.Stderr, "see '%s play -h' for exactly what that covers and what's excluded.\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "A ♥ marks a song that's also in your likes (see '%s likes').\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\n  -n, --count <n>  how many plays to show (default 20)\n")
		fmt.Fprintf(os.Stderr, "  --all            show all plays, ignoring --count\n")
		fmt.Fprintf(os.Stderr, "  --since <when>   only plays at/after <when>: \"today\", a date like\n")
		fmt.Fprintf(os.Stderr, "                   \"2026-01-02\", or a duration like \"24h\" (ago)\n")
		fmt.Fprintf(os.Stderr, "  --top            show the most-played songs (with play counts)\n")
		fmt.Fprintf(os.Stderr, "                   instead of a chronological list; --count/--all\n")
		fmt.Fprintf(os.Stderr, "                   still apply, --since does not\n")
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s history\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s history --since today\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s history --top\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --db  SONOS_DB  db\n\n")
		fs.PrintDefaults()
	}

	dbFlag := fs.String("db", "", "override the play-history database path")
	var count int
	var all, top bool
	var since string
	fs.IntVar(&count, "count", 20, "how many plays to show")
	fs.IntVar(&count, "n", 20, "shorthand for --count")
	fs.BoolVar(&all, "all", false, "show all plays, ignoring --count")
	fs.BoolVar(&top, "top", false, "show the most-played songs instead of a chronological list")
	fs.StringVar(&since, "since", "", `only plays at/after this time: "today", a date, or a duration like "24h"`)
	fs.Parse(args)

	sinceTime, err := parseSinceFlag(since)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	limit := count
	if all {
		limit = 0
	}

	db := openDBOrExit(*dbFlag)
	defer db.Close()

	liked, err := likedURISet(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if top {
		ranked, err := topPlayed(db, limit)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if len(ranked) == 0 {
			fmt.Println("no plays recorded yet")
			return
		}
		for _, c := range ranked {
			fmt.Println(topPlayedLine(c, liked[c.URI]))
		}
		return
	}

	plays, err := listPlays(db, limit, sinceTime)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(plays) == 0 {
		fmt.Println("no plays recorded yet")
		return
	}
	for _, p := range plays {
		fmt.Println(historyPlayLine(p, liked[p.URI]))
	}
}

func printTopUsage() {
	fmt.Fprintf(os.Stderr, "usage: %s <command> [flags] [args]\n\n", os.Args[0])
	fmt.Fprintln(os.Stderr, "Commands:")
	for _, c := range [][2]string{
		{"play <file-path-or-url>", "play a file/URL on the speaker (default if no command matches)"},
		{"volume-up, up, u", "increase volume (--step, default 2)"},
		{"volume-down, down, d", "decrease volume (--step, default 2)"},
		{"mute-toggle, mute", "toggle mute state"},
		{"mute-on", "mute the speaker"},
		{"mute-off", "unmute the speaker"},
		{"list, ls", "list the current play queue (-a/--all for the full list)"},
		{"remove, rm", "remove tracks from the play queue (e.g. \"rm 1 5 9\", \"rm 1-9\")"},
		{"find, f", "search the queue by title/artist/album/file name"},
		{"bump, up-next", "move matched/selected tracks to play right after the current track"},
		{"like", "like the current track, or matched/selected queue tracks"},
		{"unlike", "remove the current or matched/selected tracks from likes"},
		{"likes", "list liked songs (--play to queue/play them all)"},
		{"history", "show play history (--top for most-played)"},
		{"status, s", "show transport state, volume, and now playing"},
		{"next, n", "skip to the next track"},
		{"prev, previous, p", "skip to the previous track"},
		{"stop", "stop playback"},
	} {
		fmt.Fprintf(os.Stderr, "  %-26s %s\n", c[0], c[1])
	}
	fmt.Fprintf(os.Stderr, "\nRun '%s <command> -h' for command-specific help.\n", os.Args[0])
}

func main() {
	loaded, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: failed to load config file:", err)
	}
	cfg = loaded

	if len(os.Args) < 2 {
		printTopUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "-h", "--help", "help":
		printTopUsage()
		os.Exit(0)
	case "play":
		runPlay(os.Args[2:])
	case "volume-up", "up", "u":
		runVolume(os.Args[1], os.Args[2:], +1)
	case "volume-down", "down", "d":
		runVolume(os.Args[1], os.Args[2:], -1)
	case "mute-on":
		runMute(os.Args[1], os.Args[2:], "mute")
	case "mute-off":
		runMute(os.Args[1], os.Args[2:], "unmute")
	case "mute-toggle", "mute":
		runMute(os.Args[1], os.Args[2:], "toggle")
	case "list", "ls":
		runList(os.Args[2:])
	case "remove", "rm":
		runRemove(os.Args[2:])
	case "find", "f":
		runFind(os.Args[2:])
	case "bump", "up-next":
		runBump(os.Args[2:])
	case "like":
		runLike(os.Args[2:])
	case "unlike":
		runUnlike(os.Args[2:])
	case "likes":
		runLikes(os.Args[2:])
	case "history":
		runHistory(os.Args[2:])
	case "status", "s":
		runStatus(os.Args[2:])
	case "next", "n":
		runSkip(os.Args[1], os.Args[2:], nextTrack)
	case "prev", "previous", "p":
		runSkip(os.Args[1], os.Args[2:], previousTrack)
	case "stop":
		runStop(os.Args[2:])
	default:
		// Backward compatibility: no recognized subcommand means "play".
		runPlay(os.Args[1:])
	}
}
