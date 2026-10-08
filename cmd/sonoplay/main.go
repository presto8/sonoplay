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
// For `play`: the file to play is normally just a name (e.g. "red.mp3"),
// which gets appended to the base URL, e.g. "http://myhost/". A full URL
// (containing "://") can still be passed directly, bypassing the base URL
// entirely.
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
)

// appConfig holds options loaded from the config file. Zero values mean
// "not set".
type appConfig struct {
	IP      string
	BaseURL string
	DestDir string
	Step    string
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
	next := 0
	if asNext {
		next = 1
	}
	body := fmt.Sprintf(`<InstanceID>0</InstanceID>
      <EnqueuedURI>%s</EnqueuedURI>
      <EnqueuedURIMetaData></EnqueuedURIMetaData>
      <DesiredFirstTrackNumberEnqueued>0</DesiredFirstTrackNumberEnqueued>
      <EnqueueAsNext>%d</EnqueueAsNext>`, xmlEscape(uri), next)

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

// queueTrack is one entry returned by browseQueue.
type queueTrack struct {
	Title  string `xml:"title"`
	Artist string `xml:"artist"`
	Album  string `xml:"album"`
	URI    string `xml:"res"`
}

// browseQueue lists the speaker's current play queue (object "Q:0") via the
// ContentDirectory service's Browse action. It returns the tracks along
// with the total number of tracks in the queue (useful if len(tracks) was
// capped by count).
func browseQueue(ip string, count int) (tracks []queueTrack, totalMatches int, err error) {
	body := fmt.Sprintf(`<ObjectID>Q:0</ObjectID>
      <BrowseFlag>BrowseDirectChildren</BrowseFlag>
      <Filter>*</Filter>
      <StartingIndex>0</StartingIndex>
      <RequestedCount>%d</RequestedCount>
      <SortCriteria></SortCriteria>`, count)

	respBody, err := contentDirectoryCall(ip, "Browse", body)
	if err != nil {
		return nil, 0, err
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
		return nil, 0, fmt.Errorf("parse Browse response: %w", err)
	}

	var didl struct {
		Items []queueTrack `xml:"item"`
	}
	if parsed.Body.Resp.Result != "" {
		if err := xml.Unmarshal([]byte(parsed.Body.Resp.Result), &didl); err != nil {
			return nil, 0, fmt.Errorf("parse DIDL-Lite queue contents: %w", err)
		}
	}

	return didl.Items, parsed.Body.Resp.TotalMatches, nil
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
		fmt.Fprintf(os.Stderr, "\nOtherwise, each argument is resolved independently: if it contains\n")
		fmt.Fprintf(os.Stderr, "\"://\" it's played as-is; else if --dest-dir is set it's treated as a\n")
		fmt.Fprintf(os.Stderr, "local file path that gets copied into --dest-dir (transcoded to mp3\n")
		fmt.Fprintf(os.Stderr, "via ffmpeg first if its extension isn't natively supported), and\n")
		fmt.Fprintf(os.Stderr, "base-url + its file name is used; else it's a bare file name appended\n")
		fmt.Fprintf(os.Stderr, "to base-url.\n")
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
	fs.Parse(args)

	requireIP(fs, *ip)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}

	// Single plain-number argument: jump straight to that queue position.
	if fs.NArg() == 1 && isPositiveInteger(fs.Arg(0)) {
		index, _ := strconv.Atoi(fs.Arg(0)) // safe: isPositiveInteger validated digits-only
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
	uris := make([]string, 0, fs.NArg())
	for _, arg := range fs.Args() {
		uri, err := resolvePlayURL(*baseURL, *destDir, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping %q: %v\n", arg, err)
			continue
		}
		warnIfUnsupportedFormat(uri)
		uris = append(uris, uri)
	}

	if len(uris) == 0 {
		fmt.Fprintln(os.Stderr, "error: no valid files to play")
		os.Exit(1)
	}

	if len(uris) == 1 {
		uri := uris[0]
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

	// Multiple files: always go through the queue, since there's no
	// "play these N URLs in sequence" action other than the queue.
	playImmediately := *now
	if !playImmediately {
		state, err := getTransportState(*ip)
		playImmediately = err != nil || state != "PLAYING"
	}

	firstTrackNumber := 0
	queueLength := 0
	queuedCount := 0
	for _, uri := range uris {
		firstNum, newLength, err := addURIToQueue(*ip, uri, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping %q: %v\n", uri, err)
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

func runList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s list [--ip <speaker-ip>]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nLists the speaker's play queue, marking the currently playing item.\n")
		fmt.Fprintf(os.Stderr, "\nEach option falls back, in order: flag > env var > config file:\n")
		fmt.Fprintf(os.Stderr, "  --ip  SONOS_SPEAKER_IP  ip\n\n")
		fs.PrintDefaults()
	}

	ip := fs.String("ip", firstNonEmpty(os.Getenv("SONOS_SPEAKER_IP"), cfg.IP), "speaker IP address")
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
	if havePos && matched == -1 && nowPlaying.URI != "" {
		fmt.Println(nowPlayingLine(nowPlaying, pos) + "  (not queued)")
	}

	for i, t := range tracks {
		marker := "  "
		suffix := ""
		if i == matched {
			marker = "▶ "
			suffix = nowPlayingSuffix(pos)
		}
		fmt.Printf("%s%3d. %s%s\n", marker, i+1, formatTrack(t), suffix)
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

// runSkip handles "next" and "prev"/"previous": it skips to the next or
// previous track, then prints what's now playing (best-effort).
func runSkip(name string, args []string, skip func(ip string) error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		verb := "next"
		if name == "prev" || name == "previous" {
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
		{"list, ls", "list the current play queue"},
		{"status", "show transport state, volume, and now playing"},
		{"next", "skip to the next track"},
		{"prev, previous", "skip to the previous track"},
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
	case "status":
		runStatus(os.Args[2:])
	case "next":
		runSkip(os.Args[1], os.Args[2:], nextTrack)
	case "prev", "previous":
		runSkip(os.Args[1], os.Args[2:], previousTrack)
	case "stop":
		runStop(os.Args[2:])
	default:
		// Backward compatibility: no recognized subcommand means "play".
		runPlay(os.Args[1:])
	}
}
