package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"wcbn-recorder/internal/spinitron"
)

func TestRecordings(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	start := time.Unix(1_790_000_000, 0)
	if err := s.UpsertEvents(ctx, []spinitron.Event{{Title: "Train To Skaville"}}, start); err != nil {
		t.Fatal(err)
	}
	shows, _ := s.Shows(ctx)

	id, err := s.AddRecording(ctx, Recording{
		Airing: "train to skaville|1", Title: "Train To Skaville", Start: start, End: start.Add(time.Hour),
		StartedAt: start, Dir: "Train To Skaville/Train To Skaville - 2026-09-21",
	})
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		if err := s.AddSong(ctx, Song{RecordingID: id, Number: n, Title: "Pressure Drop", PlayedAt: start, StartMs: int64(n-1) * 1000, DurationMs: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	// Adding a number again replaces that song.
	if err := s.AddSong(ctx, Song{RecordingID: id, Number: 2, Title: "Rudy", Artist: "The Specials", PlayedAt: start, StartMs: 1000, DurationMs: 2000, File: "x/02 - The Specials - Rudy.mp3"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRecording(ctx, Recording{ID: id, DJ: "brian tomsic", FinishedAt: start.Add(time.Hour), ShowFile: "x/.show.mp3", Bytes: 123, DurationMs: 3000}); err != nil {
		t.Fatal(err)
	}

	recs, err := s.Recordings(ctx)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recordings: %+v, %v", recs, err)
	}
	r := recs[0]
	if r.ShowID != shows[0].ID || r.DJ != "brian tomsic" || r.ShowFile != "x/.show.mp3" || r.Songs != 2 ||
		!r.Start.Equal(start) || !r.FinishedAt.Equal(start.Add(time.Hour)) || r.Dir != "Train To Skaville/Train To Skaville - 2026-09-21" {
		t.Errorf("recording: %+v", r)
	}
	songs, _ := s.Songs(ctx, id)
	if len(songs) != 2 || songs[1].Title != "Rudy" || songs[1].File != "x/02 - The Specials - Rudy.mp3" {
		t.Errorf("songs: %+v", songs)
	}

	if got, err := s.Recording(ctx, id); err != nil || got.ShowFile != "x/.show.mp3" || got.Songs != 2 {
		t.Errorf("recording %d: %+v, %v", id, got, err)
	}
	if _, err := s.Recording(ctx, id+100); err != sql.ErrNoRows {
		t.Errorf("unknown recording: %v", err)
	}

	// Removing the show keeps its recordings, unlinked.
	if err := s.Delete(ctx, shows[0].ID); err != nil {
		t.Fatal(err)
	}
	if recs, _ := s.Recordings(ctx); len(recs) != 1 || recs[0].ShowID != 0 {
		t.Errorf("after removing the show: %+v", recs)
	}
	// Removing a recording removes its songs. Recordings are removed by
	// folder, which a resumed airing's recordings share.
	other, _ := s.AddRecording(ctx, Recording{Airing: "x", Title: "Yazoo City Calling", Start: start, End: start, StartedAt: start, Dir: "Yazoo/Yazoo - 2026-09-22"})
	if n, err := s.DeleteRecordingsIn(ctx, "Train To Skaville/Train To Skaville - 2026-09-21"); err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if recs, _ := s.Recordings(ctx); len(recs) != 1 || recs[0].ID != other {
		t.Errorf("left: %+v", recs)
	}
	if songs, _ := s.Songs(ctx, id); len(songs) != 0 {
		t.Errorf("songs left: %+v", songs)
	}
}
