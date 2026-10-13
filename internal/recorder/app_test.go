package recorder

import (
	"testing"
	"time"

	"wcbn-recorder/internal/config"
	"wcbn-recorder/internal/spinitron"
	"wcbn-recorder/internal/store"
)

func TestSplitStreamTitle(t *testing.T) {
	const title = "Black Magic Woman / Gypsy Queen (Live) by Santana on Open slot with eddienow"
	track, dj, ours := splitStreamTitle(title, "Open Slot")
	if track != "Black Magic Woman / Gypsy Queen (Live) by Santana" || dj != "eddienow" || !ours {
		t.Errorf("split = %q, %q, %v", track, dj, ours)
	}
	track, _, ours = splitStreamTitle(title, "Some Other Show")
	if track != "Black Magic Woman / Gypsy Queen (Live) by Santana" || ours {
		t.Errorf("generic split = %q, %v", track, ours)
	}
}

func TestUpcomingJobs(t *testing.T) {
	cfg := &config.Config{PostRoll: config.Duration{Duration: 2 * time.Minute}}
	now := time.Date(2026, 10, 12, 19, 30, 0, 0, time.UTC)
	at := func(h int) time.Time { return time.Date(2026, 10, 12, h, 0, 0, 0, time.UTC) }
	events := []spinitron.Event{
		{Title: "Yazoo City Calling", Start: at(21), End: at(22)},
		{Title: "train to skaville", Start: at(19), End: at(20)}, // on now
		{Title: "Train To Skaville", Start: at(17), End: at(18)}, // over
		{Title: "Other", Start: at(20), End: at(21)},
	}
	selected := map[string]store.Show{
		"train to skaville":  {Title: "Train To Skaville"},
		"yazoo city calling": {Title: "Yazoo City Calling"},
	}
	jobs := upcomingJobs(cfg, events, selected, now)
	if len(jobs) != 2 || jobs[0].Program != "Train To Skaville" || jobs[1].Event.Title != "Yazoo City Calling" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if !jobs[0].Until.Equal(at(20).Add(2 * time.Minute)) {
		t.Errorf("until = %v", jobs[0].Until)
	}
}
