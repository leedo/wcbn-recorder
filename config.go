package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
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
	// Programs are show names to record, matched case-insensitively against
	// the show titles on the schedule.
	Programs []string `json:"programs"`
	// MatchSubstring matches programs anywhere in the show title instead of
	// requiring an exact (case-insensitive) match.
	MatchSubstring bool `json:"match_substring"`

	StreamURL string `json:"stream_url"`
	// ScheduleURL is the Spinitron HTML program calendar.
	ScheduleURL string `json:"schedule_url"`

	PollInterval Duration `json:"poll_interval"`
	// PreRoll / PostRoll pad the recording on either side of the scheduled slot.
	PreRoll  Duration `json:"pre_roll"`
	PostRoll Duration `json:"post_roll"`

	Timezone   string `json:"timezone"`
	FFmpegPath string `json:"ffmpeg_path"`

	location *time.Location
}

func loadConfig(path string) (*Config, error) {
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
		FFmpegPath:   "ffmpeg",
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if cfg.Destination == "" {
		return nil, fmt.Errorf("config: destination is required")
	}
	if len(cfg.Programs) == 0 {
		return nil, fmt.Errorf("config: at least one program is required")
	}
	if cfg.location, err = time.LoadLocation(cfg.Timezone); err != nil {
		return nil, fmt.Errorf("config: timezone: %w", err)
	}
	return cfg, nil
}

// matches reports which configured program (if any) a schedule summary matches.
func (c *Config) matches(summary string) (string, bool) {
	s := normalize(summary)
	for _, p := range c.Programs {
		n := normalize(p)
		if s == n || (c.MatchSubstring && strings.Contains(s, n)) {
			return p, true
		}
	}
	return "", false
}

func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
