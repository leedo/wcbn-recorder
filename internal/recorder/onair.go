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
	Bitrate    int // kbps, from the station's status page
}

// stationStatusMaxAge is how long a fetched station status is reused, so
// open pages polling every 5 seconds don't each hit the station.
const stationStatusMaxAge = 10 * time.Second

// stationStatus is what the Icecast status page says about our stream.
type stationStatus struct {
	Title   string // "<song> by <artist> on <show> with <dj>"
	Bitrate int    // kbps
}

// currentEvent returns the program on the air at now, if any.
func currentEvent(events []spinitron.Event, now time.Time) (spinitron.Event, bool) {
	for _, ev := range events {
		if !now.Before(ev.Start) && now.Before(ev.End) {
			return ev, true
		}
	}
	return spinitron.Event{}, false
}

// stationStatus returns the stream's status from the Icecast status page
// next to the stream (or cached).
func (a *App) stationStatus(ctx context.Context) stationStatus {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if time.Since(a.statusAt) < stationStatusMaxAge {
		return a.status
	}
	a.statusAt = time.Now()
	st, err := fetchStationStatus(ctx, a.cfg.StreamURL)
	if err != nil {
		log.Printf("station status: %v", err)
	}
	a.status = st
	return st
}

// fetchStationStatus reads Icecast's status-json.xsl on the stream's server
// and returns the status of the source serving the stream's mount point.
func fetchStationStatus(ctx context.Context, streamURL string) (stationStatus, error) {
	stream, err := url.Parse(streamURL)
	if err != nil {
		return stationStatus{}, err
	}
	status := *stream
	status.Path, status.RawQuery = "/status-json.xsl", ""
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := httpx.Get(ctx, status.String())
	if err != nil {
		return stationStatus{}, err
	}
	var st struct {
		Icestats struct {
			Source json.RawMessage `json:"source"` // an object, or a list of them
		} `json:"icestats"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return stationStatus{}, err
	}
	type source struct {
		ListenURL string `json:"listenurl"`
		Title     string `json:"title"`
		Bitrate   int    `json:"bitrate"`
	}
	var sources []source
	if err := json.Unmarshal(st.Icestats.Source, &sources); err != nil {
		var one source
		if err := json.Unmarshal(st.Icestats.Source, &one); err != nil {
			return stationStatus{}, err
		}
		sources = []source{one}
	}
	// The listen URL can differ in host and port from ours; match the mount.
	for _, s := range sources {
		if u, err := url.Parse(s.ListenURL); err == nil && u.Path == stream.Path {
			return stationStatus{Title: strings.TrimSpace(s.Title), Bitrate: s.Bitrate}, nil
		}
	}
	return stationStatus{}, nil
}
