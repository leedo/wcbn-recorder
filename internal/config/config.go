// Package config loads the recorder's JSON configuration file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Duration is a time.Duration that unmarshals from strings like "90s" or "15m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

type Config struct {
	// Destination is the root folder; each program gets a subfolder.
	Destination string `json:"destination"`
	// ShowFile writes the whole show as one tagged MP3 with chapters.
	ShowFile bool `json:"show_file"`
	// TrackFiles also writes each song as its own MP3, with a playlist, in a
	// folder named after the show and date.
	TrackFiles bool `json:"track_files"`
	// Gapless makes song files play back seamlessly in players that support
	// it, by starting each file a couple of frames early and marking those
	// samples for the player to skip (LAME/Xing "Info" header and iTunSMPB).
	Gapless bool `json:"gapless"`
	// Programs is an optional list of show titles to select for recording. It
	// is imported into the database once; after that, use the web UI.
	Programs []string `json:"programs"`

	// Listen is the address of the web UI, e.g. "127.0.0.1:8080".
	Listen string `json:"listen"`
	// Database is the SQLite file holding show metadata and selections.
	Database string `json:"database"`

	StreamURL string `json:"stream_url"`
	// ScheduleURL is the Spinitron HTML program calendar.
	ScheduleURL string `json:"schedule_url"`

	PollInterval Duration `json:"poll_interval"`
	// PreRoll / PostRoll pad the recording on either side of the scheduled slot.
	PreRoll  Duration `json:"pre_roll"`
	PostRoll Duration `json:"post_roll"`

	Timezone string `json:"timezone"`

	Location *time.Location `json:"-"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		StreamURL:    "https://floyd.wcbn.org:8443/wcbn-hd.mp3",
		ScheduleURL:  "https://spinitron.com/WCBN/calendar",
		PollInterval: Duration{15 * time.Minute},
		PreRoll:      Duration{1 * time.Minute},
		PostRoll:     Duration{2 * time.Minute},
		Timezone:     "America/Detroit",
		Listen:       "127.0.0.1:8080",
		ShowFile:     true,
		TrackFiles:   true,
		Gapless:      true,
		Database:     "wcbn-recorder.db",
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if cfg.Destination == "" {
		return nil, fmt.Errorf("config: destination is required")
	}
	if !cfg.ShowFile && !cfg.TrackFiles {
		return nil, fmt.Errorf("config: at least one of show_file and track_files must be true")
	}
	if cfg.Location, err = time.LoadLocation(cfg.Timezone); err != nil {
		return nil, fmt.Errorf("config: timezone: %w", err)
	}
	return cfg, nil
}
