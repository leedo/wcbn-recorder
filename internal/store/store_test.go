package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"wcbn-recorder/internal/spinitron"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
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
	events := []spinitron.Event{
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
	info := spinitron.ShowInfo{Description: "Ska.", Schedule: []string{"Every week on Tue at 7:00 PM"}}
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
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.ImportPrograms(ctx, []string{"A Show"})
	s.Close()
	s, err = Open(path) // migrations must not re-run
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if sel, _ := s.Selected(ctx); len(sel) != 1 {
		t.Errorf("selected after reopen = %d", len(sel))
	}
}

func TestStoreArtwork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.UpsertEvents(ctx, []spinitron.Event{{Title: "Club H", URL: "https://spinitron.com/WCBN/show/1/Club-H"}}, time.Now())
	shows, _ := s.Shows(ctx)
	info := spinitron.ShowInfo{Image: "https://farm7.staticflickr.com/6154/6150710010_08df74f1ee_m.jpg"}
	if err := s.UpdateDetails(ctx, shows[0].ID, info, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Show(ctx, shows[0].ID); got.Image != info.Image {
		t.Errorf("image = %q", got.Image)
	}
	if err := s.SetSetting(ctx, "station_logo", "https://spinitron.com/logo.jpg"); err != nil {
		t.Fatal(err)
	}
	s.SetSetting(ctx, "station_logo", "https://spinitron.com/logo2.jpg") // updates in place
	if v, _ := s.Setting(ctx, "station_logo"); v != "https://spinitron.com/logo2.jpg" {
		t.Errorf("setting = %q", v)
	}
	if v, err := s.Setting(ctx, "missing"); v != "" || err != nil {
		t.Errorf("missing setting = %q, %v", v, err)
	}
}

// Upgrading a database from before artwork refetches every show page.
func TestMigrationRefetchesDetails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	old := migrations
	migrations = old[:1]
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// An old database: a show whose details were fetched just now.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO shows (key, title, url, details_at) VALUES ('club h', 'Club H', ?, ?)`,
		"https://spinitron.com/WCBN/show/1/Club-H", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	migrations = old

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if stale, _ := s.StaleDetails(ctx, time.Now().Add(-time.Hour)); len(stale) != 1 {
		t.Errorf("after upgrade, %d shows need details; want 1", len(stale))
	}
}
