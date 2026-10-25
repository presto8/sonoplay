// sonoplay - minimal Sonos control tool, pure Go stdlib, no cgo.
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
	Title  string `xml:"title"`
	Artist string `xml:"artist"`
	Album  string `xml:"album"`
	URI    string `xml:"res"`
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

// resolvePatternHit records how many tracks a single "@..." selector
// matched, so callers can decide whether multiple matches need extra
// confirmation (e.g. remove's -y flag).
type resolvePatternHit struct {
	pattern   string
	positions []int
}

// resolveSelectors parses a mix of position/range selectors (see
// parseTrackRange: "5", "1-9", "1..9", "9-") and "@word..." search
// selectors against tracks, returning every matching 1-based position,
// sorted ascending with duplicates removed. patternHits reports, for each
// "@..." selector given (in the order given), which positions it matched;
// callers that want to require confirmation for a pattern matching more
// than one track can inspect this. An "@..." selector matching no tracks,
// or any selector with invalid syntax or an out-of-range position, is an
// error - in neither case is a partial result returned.
func resolveSelectors(args []string, tracks []queueTrack) (positions []int, patternHits []resolvePatternHit, err error) {
	total := len(tracks)
	selected := map[int]bool{}

	for _, arg := range args {
		if pattern, ok := strings.CutPrefix(arg, selectorPatternPrefix); ok {
			words := strings.Fields(pattern)
			if len(words) == 0 {
				return nil, nil, fmt.Errorf("empty search pattern %q", arg)
			}
			hits := matchQueue(tracks, words)
			if len(hits) == 0 {
				return nil, nil, fmt.Errorf("%q matched no tracks in the queue", arg)
			}
			patternHits = append(patternHits, resolvePatternHit{pattern: pattern, positions: hits})
			for _, i := range hits {
				selected[i] = true
			}
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

	positions = make([]int, 0, len(selected))
	for i := range selected {
		positions = append(positions, i)
	}
	sort.Ints(positions)
	return positions, patternHits, nil
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

func runPlay(args []string) {
	fs := flag.NewFlagSet("play", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s play [--ip <speaker-ip>] [--base-url <url>] [--dest-dir <dir>] [--now] <file-path-or-url-or-queue-position> [<file-path-or-url> ...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip        SONOS_SPEAKER_IP   ip\n")
		fmt.Fprintf(os.Stderr, "  --base-url  SONOS_BASE_URL     base_url\n")
		fmt.Fprintf(os.Stderr, "  --dest-dir  SONOS_DEST_DIR     dest_dir\n")
		fmt.Fprintf(os.Stderr, "\nIf a single argument is a plain positive number (e.g. \"3\"), it's\n")
		fmt.Fprintf(os.Stderr, "treated as a 1-based position in the current queue (see '%s list'):\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "the speaker switches to its queue, jumps to that item, and plays it\n")
		fmt.Fprintf(os.Stderr, "immediately. (Only applies with exactly one numeric argument.)\n")
		fmt.Fprintf(os.Stderr, "\nA single argument starting with \"@\" (e.g. \"@amazing grace\") is treated\n")
		fmt.Fprintf(os.Stderr, "the same way, but resolved by searching the queue (see '%s find') rather\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "than by position. If it matches more than one track, that's an error\n")
		fmt.Fprintf(os.Stderr, "unless --first is given, in which case the first (lowest-positioned)\n")
		fmt.Fprintf(os.Stderr, "match is played.\n")
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
		fmt.Fprintf(os.Stderr, "instead of aborting the rest.\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
	baseURL := fs.String("base-url", firstNonEmpty(os.Getenv("SONOS_BASE_URL"), cfg.BaseURL), `base URL to prefix the file name with, e.g. "http://myhost/"`)
	destDir := fs.String("dest-dir", firstNonEmpty(os.Getenv("SONOS_DEST_DIR"), cfg.DestDir), "local directory to copy the source file into before playing it")
	now := fs.Bool("now", false, "play immediately, interrupting current playback, instead of enqueuing")
	first := fs.Bool("first", false, "with an @pattern argument matching multiple tracks, play the first match instead of erroring")
	fs.Parse(args)

	requireIP(fs, *ip)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}

	// Single argument that's either a plain queue position (e.g. "3") or
	// an "@pattern" search selector (e.g. "@amazing grace"): resolve it
	// against the current queue and jump straight to the result, rather
	// than treating it as a file/URL argument. Both forms go through
	// resolveSelectors so there's one code path instead of two divergent
	// ones; a bare number only ever resolves to itself (already
	// range-checked by resolveSelectors), while "@pattern" may resolve
	// to more than one position.
	if fs.NArg() == 1 && (isPositiveInteger(fs.Arg(0)) || strings.HasPrefix(fs.Arg(0), selectorPatternPrefix)) {
		tracks, _, err := browseQueue(*ip, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		positions, _, err := resolveSelectors([]string{fs.Arg(0)}, tracks)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}

		index := positions[0] // lowest-positioned match; the only one unless an @pattern matched several
		if len(positions) > 1 && !*first {
			fmt.Fprintf(os.Stderr, "error: %q matches more than one track:\n", fs.Arg(0))
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
	fmt.Printf("queued %d tracks and started playing from item %d\n", queuedCount, firstTrackNumber)
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
		fmt.Fprintf(os.Stderr, "usage: %s remove [--ip <speaker-ip>] <selector> [<selector> ...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nRemoves one or more tracks from the current play queue. Positions are\n")
		fmt.Fprintf(os.Stderr, "1-based, matching '%s list' output.\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach <selector> is one of:\n")
		fmt.Fprintf(os.Stderr, "  N      a single queue position, e.g. \"5\"\n")
		fmt.Fprintf(os.Stderr, "  A-B    an inclusive range, e.g. \"1-9\"\n")
		fmt.Fprintf(os.Stderr, "  A..B   same as A-B, e.g. \"1..9\"\n")
		fmt.Fprintf(os.Stderr, "  A-     from A to the end of the queue, e.g. \"9-\"\n")
		fmt.Fprintf(os.Stderr, "\nMultiple selectors may be given at once, e.g.:\n")
		fmt.Fprintf(os.Stderr, "  %s remove 1 5 9\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s remove 1-9\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
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

	selected := map[int]bool{}
	for _, arg := range fs.Args() {
		start, end, err := parseTrackRange(arg, total)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid selector %q: %v\n", arg, err)
			os.Exit(2)
		}
		if start < 1 || end > total || start > end {
			fmt.Fprintf(os.Stderr, "error: selector %q is out of range (queue has %d track(s))\n", arg, total)
			os.Exit(2)
		}
		for i := start; i <= end; i++ {
			selected[i] = true
		}
	}

	indices := make([]int, 0, len(selected))
	for i := range selected {
		indices = append(indices, i)
	}
	sort.Ints(indices)

	// Show what's about to be removed before touching the queue.
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

func printTopUsage() {
	fmt.Fprintf(os.Stderr, "usage: %s <command> [flags] [args]\n\n", os.Args[0])
	fmt.Fprintln(os.Stderr, "Commands:")
	for _, c := range [][2]string{
		{"play <file-path-or-url>", "play a file/URL on the speaker (default if no command matches)"},
		{"volume-up, up", "increase volume (--step, default 2)"},
		{"volume-down, down", "decrease volume (--step, default 2)"},
		{"mute-toggle, mute", "toggle mute state"},
		{"mute-on", "mute the speaker"},
		{"mute-off", "unmute the speaker"},
		{"list, ls", "list the current play queue (-a/--all for the full list)"},
		{"remove, rm", "remove tracks from the play queue (e.g. \"rm 1 5 9\", \"rm 1-9\")"},
		{"find, f", "search the queue by title/artist/album/file name"},
		{"status", "show transport state, volume, and now playing"},
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
	case "volume-up", "up":
		runVolume(os.Args[1], os.Args[2:], +1)
	case "volume-down", "down":
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
	case "status":
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
