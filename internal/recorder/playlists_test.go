package recorder

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wcbn-recorder/internal/id3"
	"wcbn-recorder/internal/spinitron"
	"wcbn-recorder/internal/store"
)

func TestPlaylistSongs(t *testing.T) {
	loc, _ := time.LoadLocation("America/Detroit")
	slot := time.Date(2026, 10, 13, 19, 0, 0, 0, loc)
	rec := store.Recording{ID: 7, StartedAt: slot.Add(-time.Minute), DurationMs: int64(63 * time.Minute / time.Millisecond)}
	// The recording runs 6:59 to 8:02. A song logged before it started is
	// the one playing at the start (the one before that is dropped), and one
	// logged after it ended is left out.
	pl := spinitron.Playlist{Start: slot, End: slot.Add(time.Hour), Spins: []spinitron.Spin{
		{Artist: "z", Song: "long before", Minute: 18*60 + 47},
		{Artist: "a", Song: "before", Minute: 18*60 + 50},
		{Artist: "b", Song: "first", Album: "First", Label: "Studio One", Year: 1966, Minute: 19 * 60},
		{Artist: "c", Song: "second", Minute: 19*60 + 12},
		{Artist: "d", Song: "too late", Minute: 20*60 + 32},
	}}
	songs := playlistSongs([]spinitron.Playlist{pl}, rec, loc)
	var got []string
	for _, s := range songs {
		got = append(got, fmt.Sprintf("%d %s/%s %d+%d", s.Number, s.Artist, s.Title, s.StartMs/1000, s.DurationMs/1000))
	}
	want := []string{"1 a/before 0+60", "2 b/first 60+720", "3 c/second 780+3000"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("songs = %v, want %v", got, want)
	}
	if songs[1].RecordingID != 7 || !songs[1].PlayedAt.Equal(slot) || songs[1].Album != "First" || songs[1].Label != "Studio One" || songs[1].Year != 1966 {
		t.Errorf("song = %+v", songs[1])
	}
	if playlistSongs(nil, rec, loc) != nil {
		t.Error("songs without a playlist")
	}
}

// fakeSpinitron serves a calendar, its feed, and one playlist page with the
// spins given, at /WCBN/pl/1/Club-H; nil spins leaves it off the calendar.
func fakeSpinitron(t *testing.T, slot time.Time, spins *string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/WCBN/calendar":
			fmt.Fprint(w, `<script>jQuery('#calendar-w0').calendar({"events":"/WCBN/calendar-feed"});</script>`)
		case "/WCBN/calendar-feed":
			if *spins == "" {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprintf(w, `[{"id":1,"title":"Club H","start":%q,"end":%q,"className":"spin-cal-pl","url":"/WCBN/pl/1/Club-H"}]`,
				slot.Format("2006-01-02T15:04:05-0700"), slot.Add(time.Hour).Format("2006-01-02T15:04:05-0700"))
		case "/WCBN/pl/1/Club-H":
			fmt.Fprintf(w, `<table>%s</table>`, *spins)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func spinRow(at, artist, song string) string {
	return fmt.Sprintf(`<tr class="spin-item"><td class="spin-time"><a>%s</a></td><td class="spin-text"><span class="artist">%s</span> <span class="song">%s</span></td></tr>`, at, artist, song)
}

// A recording with no songs gets them from its Spinitron playlist once it's
// up, and its CUE sheets are rewritten with them.
func TestFillPendingSongs(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	app.cfg.Destination = t.TempDir()
	slot := time.Date(2026, 10, 13, 19, 0, 0, 0, app.cfg.Location)
	spins := ""
	app.cfg.ScheduleURL = fakeSpinitron(t, slot, &spins).URL + "/WCBN/calendar"

	// A resumed recording: two show files of 1,000 frames (26.1 s) each, and
	// their CUE sheets from when it was recorded, with one track.
	dir := filepath.Join(app.cfg.Destination, "Club H")
	os.MkdirAll(dir, 0o755)
	frames := bytes.Repeat(mp3Frame(1), 1000)
	for _, name := range []string{"Club H - 2026-10-13.mp3", "Club H - 2026-10-13 2.mp3"} {
		var tag id3.Tag
		tag.Text("TIT2", "Club H")
		if err := writeTagged(filepath.Join(dir, name), &tag, bytes.NewReader(frames)); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(cuePath(filepath.Join(dir, name)), []byte("old"), 0o644)
	}
	started := slot.Add(-2 * time.Second)
	id, err := app.store.AddRecording(ctx, store.Recording{Airing: "club h|1", Title: "Club H", DJ: "dj h", Category: "Rock", Start: slot, End: slot.Add(time.Hour), StartedAt: started, Dir: "Club H"})
	if err != nil {
		t.Fatal(err)
	}
	app.store.UpdateRecording(ctx, store.Recording{ID: id, DJ: "dj h", FinishedAt: slot.Add(time.Hour), ShowFile: "Club H/Club H - 2026-10-13.mp3", DurationMs: 52_244})
	app.store.SetSongsPending(ctx, id, true)
	pending := func() bool {
		r, _ := app.store.Recording(ctx, id)
		return r.SongsPending
	}

	// Not looked for until a little after the airing; then not there yet.
	app.fillPendingSongs(ctx, slot.Add(time.Hour+time.Minute))
	app.fillPendingSongs(ctx, slot.Add(2*time.Hour))
	if !pending() {
		t.Fatal("no longer pending without a playlist")
	}

	// Up now: two songs logged at 7:00, 2 s into the recording (which is
	// shorter than a real one).
	spins = spinRow("7:00 PM", "Toots &amp; the Maytals", "Pressure Drop") + spinRow("7:00 PM", "The Specials", "Rudy")
	app.fillPendingSongs(ctx, slot.Add(2*time.Hour))
	if r, _ := app.store.Recording(ctx, id); r.SongsPending || r.PlaylistURL != strings.TrimSuffix(app.cfg.ScheduleURL, "/WCBN/calendar")+"/WCBN/pl/1/Club-H" {
		t.Fatalf("after finding the playlist: %+v", r)
	}
	songs, _ := app.store.Songs(ctx, id)
	if len(songs) != 2 || songs[0].Title != "Pressure Drop" || songs[0].StartMs != 2000 || songs[1].Artist != "The Specials" || songs[1].DurationMs != 52_244-2000 {
		t.Fatalf("songs = %+v", songs)
	}
	cue, _ := os.ReadFile(filepath.Join(dir, "Club H - 2026-10-13.cue"))
	for _, want := range []string{`PERFORMER "dj h"`, `TITLE "Club H (2026-10-13)"`, `REM GENRE "Rock"`, `FILE "Club H - 2026-10-13.mp3" MP3`,
		"TRACK 01 AUDIO\n    TITLE \"Pressure Drop\"\n    PERFORMER \"Toots & the Maytals\"\n    INDEX 01 00:02:00",
		"TRACK 02 AUDIO\n    TITLE \"Rudy\"\n    PERFORMER \"The Specials\"\n    INDEX 01 00:02:00"} {
		if !strings.Contains(string(cue), want) {
			t.Errorf("CUE sheet missing %q:\n%s", want, cue)
		}
	}
	// The second file starts 26.1 s in, with Rudy still playing.
	cue2, _ := os.ReadFile(filepath.Join(dir, "Club H - 2026-10-13 2.cue"))
	if !strings.Contains(string(cue2), `FILE "Club H - 2026-10-13 2.mp3" MP3`) || !strings.Contains(string(cue2), "TRACK 01 AUDIO\n    TITLE \"Rudy\"\n    PERFORMER \"The Specials\"\n    INDEX 01 00:00:00") || strings.Contains(string(cue2), "TRACK 02") {
		t.Errorf("second CUE sheet:\n%s", cue2)
	}

	// One whose playlist never turns up is given up on after a week.
	gone, _ := app.store.AddRecording(ctx, store.Recording{Airing: "gone", Title: "Gone", Start: slot, End: slot.Add(time.Hour), StartedAt: slot, Dir: "Gone"})
	app.store.SetSongsPending(ctx, gone, true)
	app.fillPendingSongs(ctx, slot.Add(8*24*time.Hour))
	if r, _ := app.store.Recording(ctx, gone); r.SongsPending {
		t.Error("still looking after a week")
	}
}

func TestAudioDurationMs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.mp3")
	var tag id3.Tag
	tag.Text("TIT2", "x")
	writeTagged(path, &tag, bytes.NewReader(bytes.Repeat(mp3Frame(1), 100)))
	if ms, err := audioDurationMs(path); err != nil || ms != 100*1152*1000/44100 {
		t.Errorf("duration = %d, %v", ms, err)
	}
}

// While recording, the songs logged on Spinitron so far are listed, timed in
// the audio so far; the recording still waits for the full playlist.
func TestUpdateLiveSongs(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	slot := time.Date(2026, 10, 13, 19, 0, 0, 0, app.cfg.Location)
	spins := ""
	app.cfg.ScheduleURL = fakeSpinitron(t, slot, &spins).URL + "/WCBN/calendar"

	f := newFakeRecording(t, testConfig())
	f.db = app.store
	f.job.Event.Title, f.job.Event.Start, f.job.Event.End = "Club H", slot, slot.Add(time.Hour)
	f.started = slot.Add(-2 * time.Second)
	f.catalogStart(ctx)
	app.store.SetSongsPending(ctx, f.catalogID, true) // as an earlier run would have left it
	f.frames(1, 100)                                  // 2.6 s
	w := &liveSongs{rec: f.recording}

	// Nothing logged yet.
	app.updateLiveSongs(ctx, w)
	if songs, _ := app.store.Songs(ctx, f.catalogID); len(songs) != 0 {
		t.Fatalf("songs = %+v", songs)
	}
	spins = spinRow("7:00 PM", "Toots &amp; the Maytals", "Pressure Drop")
	app.updateLiveSongs(ctx, w)
	songs, _ := app.store.Songs(ctx, f.catalogID)
	if len(songs) != 1 || songs[0].Title != "Pressure Drop" || songs[0].StartMs != 2000 || songs[0].DurationMs != f.audioDurationMs()-2000 {
		t.Fatalf("songs = %+v", songs)
	}
	r, _ := app.store.Recording(ctx, f.catalogID)
	if !r.SongsPending || !strings.HasSuffix(r.PlaylistURL, "/WCBN/pl/1/Club-H") {
		t.Errorf("recording = %+v", r)
	}
	// Another song logged, and more audio recorded.
	spins += spinRow("7:00 PM", "The Specials", "Rudy")
	f.frames(2, 50)
	app.updateLiveSongs(ctx, w)
	if songs, _ := app.store.Songs(ctx, f.catalogID); len(songs) != 2 || songs[1].Title != "Rudy" || songs[1].DurationMs != f.audioDurationMs()-2000 || w.listed != 2 {
		t.Errorf("songs = %+v", songs)
	}
	// Each change tells the pages to redraw.
	if n := app.songsChanged.Load(); n != 2 {
		t.Errorf("songsChanged = %d, want 2", n)
	}
	if state := recordingsState(nil, app.songsChanged.Load()); state != " songs:2" {
		t.Errorf("state = %q", state)
	}
}

func (f *fakeRecording) audioDurationMs() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recording.audio.DurationMs()
}
