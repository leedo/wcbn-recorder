package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Unix(1_790_000_000, 0)

	// Legacy config programs are imported once, with config spelling.
	if err := s.ImportPrograms(ctx, []string{"Train to Skaville", "Dancehall Reggae"}); err != nil {
		t.Fatal(err)
	}
	events := []Event{
		{Title: "Train To Skaville", DJ: "brian tomsic", Category: "Specialty", URL: "https://spinitron.com/WCBN/pl/1/Train"},
		{Title: "Train To Skaville", URL: "https://spinitron.com/WCBN/show/264320/Train-To-Skaville"},
		{Title: "Yazoo City Calling", DJ: "rotating hosts", Category: "Specialty", URL: "https://spinitron.com/WCBN/show/248524/Yazoo"},
	}
	if err := s.UpsertEvents(ctx, events, now); err != nil {
		t.Fatal(err)
	}

	shows, err := s.Shows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) != 3 {
		t.Fatalf("got %d shows: %+v", len(shows), shows)
	}
	byKey := map[string]Show{}
	for _, sh := range shows {
		byKey[sh.Key] = sh
	}
	ska := byKey["train to skaville"]
	if ska.Title != "Train To Skaville" || !ska.Record || ska.DJ != "brian tomsic" ||
		ska.URL != "https://spinitron.com/WCBN/show/264320/Train-To-Skaville" || !ska.LastSeen.Equal(now) {
		t.Errorf("skaville = %+v", ska)
	}
	if dh := byKey["dancehall reggae"]; !dh.Record || !dh.LastSeen.IsZero() {
		t.Errorf("dancehall = %+v", dh)
	}
	if yz := byKey["yazoo city calling"]; yz.Record {
		t.Errorf("yazoo should not be selected")
	}

	// Unselecting survives another import (e.g. a restart with the same config).
	if err := s.SetRecord(ctx, ska.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportPrograms(ctx, []string{"Train to Skaville"}); err != nil {
		t.Fatal(err)
	}
	sel, err := s.Selected(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel) != 1 || !sel["dancehall reggae"].Record {
		t.Errorf("selected = %+v", sel)
	}

	// Details: only shows with a URL and stale details are returned.
	stale, err := s.StaleDetails(ctx, now)
	if err != nil || len(stale) != 2 {
		t.Fatalf("stale = %+v, %v", stale, err)
	}
	info := ShowInfo{Description: "Ska.", Schedule: []string{"Every week on Tue at 7:00 PM"}}
	if err := s.UpdateDetails(ctx, ska.ID, info, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Show(ctx, ska.ID); got.Description != "Ska." || got.Schedule != "Every week on Tue at 7:00 PM" || got.DJ != "brian tomsic" {
		t.Errorf("after details: %+v", got)
	}
	if stale, _ := s.StaleDetails(ctx, now); len(stale) != 1 {
		t.Errorf("stale after update = %d", len(stale))
	}

	if err := s.SetRecord(ctx, 9999, true); err == nil {
		t.Error("expected error for unknown show")
	}
}

func TestStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.ImportPrograms(ctx, []string{"A Show"})
	s.Close()
	s, err = openStore(path) // migrations must not re-run
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if sel, _ := s.Selected(ctx); len(sel) != 1 {
		t.Errorf("selected after reopen = %d", len(sel))
	}
}

func TestUpcomingJobs(t *testing.T) {
	cfg := &Config{PostRoll: Duration{2 * time.Minute}}
	now := time.Date(2026, 10, 12, 19, 30, 0, 0, time.UTC)
	at := func(h int) time.Time { return time.Date(2026, 10, 12, h, 0, 0, 0, time.UTC) }
	events := []Event{
		{Title: "Yazoo City Calling", Start: at(21), End: at(22)},
		{Title: "train to skaville", Start: at(19), End: at(20)}, // on now
		{Title: "Train To Skaville", Start: at(17), End: at(18)}, // over
		{Title: "Other", Start: at(20), End: at(21)},
	}
	selected := map[string]Show{
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
