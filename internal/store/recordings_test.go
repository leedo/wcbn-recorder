package store

import (
	"context"
	"database/sql"
	"fmt"
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

// Resumed airings used to get a row per run; the migration merges them.
func TestMergeResumedRecordings(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	start := time.Unix(1_790_000_000, 0)
	add := func(airing, dir, show string, started time.Time, songs ...int) int64 {
		id, err := s.AddRecording(ctx, Recording{Airing: airing, Title: "Club H", Start: start, End: start.Add(time.Hour), StartedAt: started, Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range songs {
			s.AddSong(ctx, Song{RecordingID: id, Number: n, Title: fmt.Sprint(n), PlayedAt: started})
		}
		s.UpdateRecording(ctx, Recording{ID: id, FinishedAt: started.Add(time.Minute), ShowFile: show, Bytes: 100, DurationMs: 1000})
		return id
	}
	// Song files on: the runs' songs are numbered on already.
	first := add("h|1", "Club H/Club H - 1", "Club H/Club H - 1/.Club H - 1.mp3", start, 1, 2)
	add("h|1", "Club H/Club H - 1", "Club H/Club H - 1/.Club H - 1 2.mp3", start.Add(10*time.Minute), 3)
	// Song files off: each run numbered from 1.
	off := add("h|2", "Club H", "Club H/Club H - 2.mp3", start, 1, 2)
	add("h|2", "Club H", "Club H/Club H - 2 2.mp3", start.Add(10*time.Minute), 1)
	add("h|2", "Club H", "Club H/Club H - 2 3.mp3", start.Add(20*time.Minute), 1, 2)
	other := add("h|3", "Club H/Club H - 3", "x.mp3", start, 1)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := mergeResumedRecordings(tx); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	recs, _ := s.Recordings(ctx)
	got := map[int64]Recording{}
	for _, r := range recs {
		got[r.ID] = r
	}
	if len(recs) != 3 || got[other].Songs != 1 {
		t.Fatalf("recordings: %+v", recs)
	}
	for id, want := range map[int64][]int{first: {1, 2, 3}, off: {1, 2, 3, 4, 5}} {
		songs, _ := s.Songs(ctx, id)
		var nums []int
		for _, s := range songs {
			nums = append(nums, s.Number)
		}
		if fmt.Sprint(nums) != fmt.Sprint(want) {
			t.Errorf("recording %d songs %v, want %v", id, nums, want)
		}
	}
	if r := got[first]; r.Bytes != 200 || r.DurationMs != 2000 || r.ShowFile != "Club H/Club H - 1/.Club H - 1.mp3" || !r.FinishedAt.Equal(start.Add(11*time.Minute)) {
		t.Errorf("merged: %+v", r)
	}
	if r := got[off]; r.Bytes != 300 || r.ShowFile != "Club H/Club H - 2.mp3" {
		t.Errorf("merged with song files off: %+v", r)
	}
}

func TestRecordingsPage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	start := time.Unix(1_790_000_000, 0)
	for i := range 5 {
		at := start.Add(time.Duration(i) * time.Hour)
		if _, err := s.AddRecording(ctx, Recording{Airing: fmt.Sprint(i), Title: fmt.Sprint(i), Start: at, End: at, StartedAt: at, Dir: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	recs, total, err := s.RecordingsPage(ctx, 0, 2, 2)
	if err != nil || total != 5 || len(recs) != 2 || recs[0].Title != "2" || recs[1].Title != "1" {
		t.Errorf("page: %+v, %d, %v", recs, total, err)
	}
	if recs, total, _ := s.RecordingsPage(ctx, 0, 2, 10); len(recs) != 0 || total != 5 {
		t.Errorf("past the end: %+v, %d", recs, total)
	}
	// One show's: none here, since none of these titles is a show.
	if recs, total, _ := s.RecordingsPage(ctx, 1, 2, 0); len(recs) != 0 || total != 0 {
		t.Errorf("one show's: %+v, %d", recs, total)
	}
}

// Songs so far leave a recording waiting for the rest; the final list doesn't.
func TestSetSongsSoFar(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	start := time.Unix(1_790_000_000, 0)
	id, _ := s.AddRecording(ctx, Recording{Airing: "a", Title: "Club H", Start: start, End: start, StartedAt: start, Dir: "Club H"})
	s.SetSongsPending(ctx, id, true)
	if err := s.SetSongsSoFar(ctx, id, []Song{{Number: 1, Title: "a", PlayedAt: start}}, "https://example.org/pl/1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Recording(ctx, id); !r.SongsPending || r.Songs != 1 || r.PlaylistURL != "https://example.org/pl/1" {
		t.Errorf("so far: %+v", r)
	}
	s.SetSongs(ctx, id, []Song{{Number: 1, Title: "a", PlayedAt: start}, {Number: 2, Title: "b", PlayedAt: start}}, "https://example.org/pl/1")
	if r, _ := s.Recording(ctx, id); r.SongsPending || r.Songs != 2 {
		t.Errorf("final: %+v", r)
	}
}

// A song's release, label and year are saved with it.
func TestSongRelease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	start := time.Unix(1_790_000_000, 0)
	id, _ := s.AddRecording(ctx, Recording{Airing: "a", Title: "Animal Haus", Start: start, End: start, StartedAt: start, Dir: "Animal Haus"})
	s.SetSongs(ctx, id, []Song{{Number: 1, Title: "The Caterpillar", Artist: "The Cure", Album: "The Top", Label: "Rhino/Elektra", Year: 1984, PlayedAt: start}}, "")
	s.AddSong(ctx, Song{RecordingID: id, Number: 2, Title: "Butterfly", Artist: "Jamiroquai", Album: "Synkronized", Label: "Sony Music CG", Year: 1999, PlayedAt: start})
	songs, err := s.Songs(ctx, id)
	if err != nil || len(songs) != 2 || songs[0].Album != "The Top" || songs[0].Label != "Rhino/Elektra" || songs[0].Year != 1984 || songs[1].Year != 1999 {
		t.Errorf("songs = %+v, %v", songs, err)
	}
}
