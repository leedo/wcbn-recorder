package recorder

import (
	"context"
	"encoding/json"
	"log"
	"net/url"
	"strings"
	"time"

	"wcbn-recorder/internal/httpx"
	"wcbn-recorder/internal/spinitron"
)

// When nothing is being recorded, the UI shows what's on the air: the
// current program from the schedule, and the current song from the station's
// Icecast status page (the same "<song> by <artist> on <show> with <dj>"
// title the stream carries).

// onAirView is the "On air" panel shown when nothing is recording.
type onAirView struct {
	ShowID     int64 // 0 if the program isn't a known show
	Title      string
	URL        string // the show's Spinitron page
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
func currentEvent(events []spinitron.Event, now time.Time) (spinitron.Event, bool) {
	for _, ev := range events {
		if !now.Before(ev.Start) && now.Before(ev.End) {
			return ev, true
		}
	}
	return spinitron.Event{}, false
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
	data, err := httpx.Get(ctx, status.String())
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
