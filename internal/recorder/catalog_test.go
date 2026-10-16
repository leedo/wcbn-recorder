package recorder

import (
	"context"
	"os"
	"strings"
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
	if r.Airing != job.Event.Key() || r.Title != "Club H" || r.DJ != "dj h" || r.Dir != "Club H" ||
		r.ShowFile != "Club H/"+base+".mp3" || r.CueFile != "Club H/"+base+".cue" ||
		r.FinishedAt.IsZero() || r.Bytes != rec.audio.Written() || r.DurationMs == 0 || r.Songs != 0 {
		t.Errorf("recording: %+v", r)
	}

	// A restart resumes the airing in the same row, adding its size and length.
	// A run that gets no audio leaves it be. Its songs are still to come from
	// Spinitron.
	record := func(min int64) *recording {
		rec, err := startRecording(cfg, db, job)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- rec.Run(ctx) }()
		for deadline := time.Now().Add(5 * time.Second); rec.bytesWritten() < min; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("no audio recorded")
			}
		}
		rec.Stop()
		<-done
		return rec
	}
	resumed := record(20 * 1044)
	if !resumed.resumed || resumed.catalogID != r.ID {
		t.Fatalf("resumed = %v, catalogID = %d, want %d", resumed.resumed, resumed.catalogID, r.ID)
	}
	record(0) // stopped before any audio
	recs, _ = db.Recordings(ctx)
	if len(recs) != 1 || recs[0].ID != r.ID || recs[0].Songs != 0 || recs[0].ShowFile != r.ShowFile ||
		recs[0].Bytes != resumed.prefixBytes+resumed.audio.Written() || resumed.prefixBytes < r.Bytes || recs[0].DurationMs <= r.DurationMs {
		t.Fatalf("after resuming: %+v", recs)
	}

	rec2, _ := startRecording(cfg, db, Job{Program: "Club H", Event: spinitron.Event{Title: "Club H 2", Start: now, End: now.Add(time.Hour)}, Until: now.Add(time.Hour)})
	rec2.Stop()
	rec2.Run(ctx) // "no audio received" is expected
	if recs, _ := db.Recordings(ctx); len(recs) != 1 {
		t.Errorf("a recording with no audio was kept: %+v", recs)
	}
}

// The stream's song titles aren't listed: every recording waits for its
// songs from Spinitron, including a resumed run, which carries on in the
// same row.
func TestCatalogSongsPending(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	f := newFakeRecording(t, testConfig())
	f.db = db
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
	if len(recs) != 1 || recs[0].Songs != 0 || !recs[0].SongsPending || recs[0].ShowFile != "Train To Skaville - 2026-10-13.mp3" {
		t.Fatalf("recordings: %+v", recs)
	}
	// The CUE sheet written at the end still comes from the stream.
	if cue, _ := os.ReadFile(cuePath(f.showOut)); !strings.Contains(string(cue), `TITLE "A Message to You, Rudy"`) {
		t.Errorf("CUE sheet:\n%s", cue)
	}

	g := newFakeRecording(t, testConfig())
	g.db, g.out, g.resumed = db, f.out, true
	g.cfg.Destination = f.cfg.Destination
	g.catalogStart(ctx)
	if g.catalogID != f.catalogID {
		t.Fatalf("resumed into %d, want %d", g.catalogID, f.catalogID)
	}
	g.title("Monkey Man by Toots & the Maytals")
	g.frames(3, 2)
	if err := g.finalize(ctx, g.part, g.out); err != nil {
		t.Fatal(err)
	}
	g.catalogFinish(ctx, g.showMetadata(ctx), g.showOut, g.cueOut)
	if recs, _ := db.Recordings(ctx); len(recs) != 1 || recs[0].Songs != 0 || !recs[0].SongsPending {
		t.Errorf("after resuming: %+v", recs)
	}
}
