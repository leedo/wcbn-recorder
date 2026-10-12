package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// When nothing is being recorded, the UI shows what's on the air: the
// current program from the schedule, and the current song from the station's
// Icecast status page (the same "<song> by <artist> on <show> with <dj>"
// title the stream carries).

// onAirView is the "On air" panel shown when nothing is recording.
type onAirView struct {
	ShowID     int64 // 0 if the program isn't a known show
	Title      string
	DJ         string
	Image      string
	Start, End time.Time
	Progress   int
	Selected   bool // already selected for recording
	Song       string
	Artist     string
}

// stationTitleMaxAge is how long a fetched stream title is reused, so open
// pages polling every 5 seconds don't each hit the station.
const stationTitleMaxAge = 10 * time.Second

// currentEvent returns the program on the air at now, if any.
func currentEvent(events []Event, now time.Time) (Event, bool) {
	for _, ev := range events {
		if !now.Before(ev.Start) && now.Before(ev.End) {
			return ev, true
		}
	}
	return Event{}, false
}

// stationTitle returns the stream's current title, fetched from the Icecast
// status page next to the stream (or cached).
func (a *App) stationTitle(ctx context.Context) string {
	a.titleMu.Lock()
	defer a.titleMu.Unlock()
	if time.Since(a.titleAt) < stationTitleMaxAge {
		return a.title
	}
	a.titleAt = time.Now()
	title, err := fetchStationTitle(ctx, a.cfg.StreamURL)
	if err != nil {
		log.Printf("station status: %v", err)
	}
	a.title = title
	return title
}

// fetchStationTitle reads Icecast's status-json.xsl on the stream's server
// and returns the title of the source serving the stream's mount point.
func fetchStationTitle(ctx context.Context, streamURL string) (string, error) {
	stream, err := url.Parse(streamURL)
	if err != nil {
		return "", err
	}
	status := *stream
	status.Path, status.RawQuery = "/status-json.xsl", ""
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := httpGet(ctx, status.String())
	if err != nil {
		return "", err
	}
	var st struct {
		Icestats struct {
			Source json.RawMessage `json:"source"` // an object, or a list of them
		} `json:"icestats"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "", err
	}
	type source struct {
		ListenURL string `json:"listenurl"`
		Title     string `json:"title"`
	}
	var sources []source
	if err := json.Unmarshal(st.Icestats.Source, &sources); err != nil {
		var one source
		if err := json.Unmarshal(st.Icestats.Source, &one); err != nil {
			return "", err
		}
		sources = []source{one}
	}
	// The listen URL can differ in host and port from ours; match the mount.
	for _, s := range sources {
		if u, err := url.Parse(s.ListenURL); err == nil && u.Path == stream.Path {
			return strings.TrimSpace(s.Title), nil
		}
	}
	return "", nil
}

// serveLiveStream relays the station's stream to a listener, through the
// recorder like recordings are, so it works wherever the UI does.
func (a *App) serveLiveStream(w http.ResponseWriter, r *http.Request) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.cfg.StreamURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", "wcbn-recorder/1.0") // no Icy-MetaData: plain MP3
	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, "couldn't reach the station", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "the station returned "+resp.Status, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	io.Copy(flushWriter{w, http.NewResponseController(w)}, resp.Body)
}

// flushWriter sends each chunk to the listener as soon as it arrives.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.rc.Flush()
	}
	return n, err
}
