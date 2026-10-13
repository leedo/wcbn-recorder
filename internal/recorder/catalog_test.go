package recorder

import (
	"context"
	"testing"
	"time"

	"wcbn-recorder/internal/spinitron"
)

// A recording and its songs are listed in the database, with paths relative
// to the destination; one that gets no audio is removed again.
func TestCatalog(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	cfg := testConfig()
	cfg.Destination, cfg.StreamURL = t.TempDir(), fakeStation(t).URL
	now := time.Now()
	job := Job{Program: "Club H", Event: spinitron.Event{Title: "Club H", DJ: "dj h", Start: now, End: now.Add(time.Hour)}, Until: now.Add(time.Hour)}
	rec, err := startRecording(cfg, db, job)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()
	for deadline := time.Now().Add(5 * time.Second); rec.bytesWritten() < 20*1044; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no audio recorded")
		}
	}
	if recs, _ := db.Recordings(ctx); len(recs) != 1 || !recs[0].FinishedAt.IsZero() {
		t.Fatalf("while recording: %+v", recs)
	}
	rec.Stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	recs, err := db.Recordings(ctx)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recordings: %+v, %v", recs, err)
	}
	r := recs[0]
	base := "Club H - " + now.In(cfg.Location).Format("2006-01-02")
	dir := "Club H/" + base
	if r.Airing != job.Event.Key() || r.Title != "Club H" || r.DJ != "dj h" || r.Dir != dir ||
		r.ShowFile != dir+"/."+base+".mp3" || r.CueFile != dir+"/."+base+".cue" || r.Playlist != dir+"/"+base+".m3u8" ||
		r.FinishedAt.IsZero() || r.Bytes != rec.audio.Written() || r.DurationMs == 0 || r.Songs != 1 {
		t.Errorf("recording: %+v", r)
	}
	songs, err := db.Songs(ctx, r.ID)
	if err != nil || len(songs) != 1 {
		t.Fatalf("songs: %+v, %v", songs, err)
	}
	if s := songs[0]; s.Number != 1 || s.Title != "Club H" || s.File != dir+"/01 - Club H.mp3" || s.StartMs != 0 || s.DurationMs != r.DurationMs {
		t.Errorf("song: %+v", s)
	}

	rec2, _ := startRecording(cfg, db, Job{Program: "Club H", Event: spinitron.Event{Title: "Club H 2", Start: now, End: now.Add(time.Hour)}, Until: now.Add(time.Hour)})
	rec2.Stop()
	rec2.Run(ctx) // "no audio received" is expected
	if recs, _ := db.Recordings(ctx); len(recs) != 1 {
		t.Errorf("a recording with no audio was kept: %+v", recs)
	}
}

// With song files off, the songs are still listed, as the show file's chapters.
func TestCatalogShowFileOnly(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	cfg := testConfig()
	cfg.TrackFiles = false
	f := newFakeRecording(t, cfg)
	f.db = db
	f.out = outputPaths(cfg, cfg.Destination, "Train To Skaville - 2026-10-13")
	f.catalogStart(ctx)
	f.title("Pressure Drop by Toots & the Maytals on Train To Skaville with brian tomsic")
	f.frames(1, 5)
	f.title("A Message to You, Rudy by The Specials on Train To Skaville with brian tomsic")
	f.frames(2, 3)
	if err := f.finalize(ctx, f.part, f.out); err != nil {
		t.Fatal(err)
	}
	f.catalogFinish(ctx, f.showMetadata(ctx), f.showOut, f.cueOut)

	recs, _ := db.Recordings(ctx)
	if len(recs) != 1 || recs[0].Dir != "." || recs[0].ShowFile != "Train To Skaville - 2026-10-13.mp3" || recs[0].Playlist != "" {
		t.Fatalf("recordings: %+v", recs)
	}
	songs, _ := db.Songs(ctx, recs[0].ID)
	if len(songs) != 2 || songs[1].Number != 2 || songs[1].Artist != "The Specials" || songs[1].File != "" || songs[1].StartMs != 5*26 {
		t.Errorf("songs: %+v", songs)
	}
}
