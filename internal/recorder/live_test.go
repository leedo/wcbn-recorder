package recorder

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"wcbn-recorder/internal/spinitron"
)

func TestLiveStatus(t *testing.T) {
	f := newFakeRecording(t, testConfig())
	f.job.Until = f.job.Event.End.Add(2 * time.Minute)
	f.title("Pressure Drop by Toots & the Maytals on Train To Skaville with brian tomsic")
	f.frames(1, 3)
	f.title("Stand by Me by Ben E. King on Train To Skaville with brian tomsic") // replaced before any audio
	f.title("A Message to You, Rudy by The Specials on Train To Skaville with brian tomsic")
	f.frames(2, 2)

	st := f.status(f.job.Event.Start.Add(15 * time.Minute))
	if st.Title != "Train To Skaville" || st.DJ != "brian tomsic" || st.Progress != 25 || st.Bytes != int64(len(f.audio)) {
		t.Errorf("status = %+v", st)
	}
	if len(st.Tracks) != 2 {
		t.Fatalf("tracks = %+v", st.Tracks)
	}
	if tr := st.Tracks[0]; tr.N != 1 || tr.Title != "Pressure Drop" || tr.Artist != "Toots & the Maytals" || tr.Current || tr.DurationMs != 3*1152*1000/44100 {
		t.Errorf("track 1 = %+v", tr)
	}
	if st.Current == nil || st.Current.N != 2 || st.Current.Artist != "The Specials" || !st.Current.Current {
		t.Errorf("current = %+v", st.Current)
	}
	// Progress is clamped to the scheduled slot.
	if st := f.status(f.job.Event.End.Add(time.Minute)); st.Progress != 100 {
		t.Errorf("progress after end = %d", st.Progress)
	}
}

// The UI reads status while the stream goroutine writes; run with -race.
func TestLiveStatusConcurrent(t *testing.T) {
	f := newFakeRecording(t, testConfig())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 200 {
			f.Write(mp3Frame(byte(i)))
			if i%20 == 0 {
				f.onMetadata("StreamTitle='Song " + string(rune('A'+i/20)) + " by Band';")
			}
		}
	}()
	for range 200 {
		f.status(time.Now())
	}
	wg.Wait()
	if st := f.status(time.Now()); len(st.Tracks) != 10 || !st.Connected {
		t.Errorf("tracks = %d, connected = %v", len(st.Tracks), st.Connected)
	}
}

func TestWebLive(t *testing.T) {
	app, srv := testApp(t)
	if page := get(t, srv.URL+"/"); strings.Contains(page, `<section class="card live-card"`) {
		t.Error("live panel shown with nothing recording")
	}

	f := newFakeRecording(t, testConfig())
	f.job.Event.Start, f.job.Event.End = time.Now().Add(-time.Minute), time.Now().Add(time.Hour) // on the air now
	f.title("Pressure Drop by Toots & the Maytals on Train To Skaville with brian tomsic")
	f.frames(1, 3)
	f.Write(nil) // marks the stream connected
	app.mu.Lock()
	app.active[f.job.Event.Key()] = f.recording
	app.mu.Unlock()

	page := get(t, srv.URL+"/")
	for _, want := range []string{`<section class="card live-card"`, `<span class="dot"></span>LIVE REC</span>`, "Now playing", "Pressure Drop", "Toots &amp; the Maytals", " · 320 kbps\n"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(page, `class="stop-form"`) {
		t.Error("the live card shouldn't have End recording; that's on the Recordings page")
	}
	if strings.Contains(page, `class="songs"`) || strings.Contains(page, `<span class="meta">Started`) {
		t.Error("the live card shouldn't list the songs recorded so far, or how long the current one has played")
	}

	var out struct {
		Live, Upcoming string
		Recording      int
	}
	header := firstEvent(t, srv.URL, &out)
	if out.Recording != 1 || !strings.Contains(out.Live, "Pressure Drop") || header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("/events = %+v", out)
	}
}

func TestLiveRecording(t *testing.T) {
	ten := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	// Back to back, each with pre-roll and post-roll, so both are recording
	// around 10:00.
	active := []liveStatus{
		{Title: "Nine", Start: ten.Add(-time.Hour), End: ten},
		{Title: "Ten", Start: ten, End: ten.Add(time.Hour)},
	}
	for at, want := range map[time.Duration]string{-time.Minute: "Nine", 0: "Ten", time.Minute: "Ten", -2 * time.Hour: "", 2 * time.Hour: ""} {
		got := ""
		if st := liveRecording(active, ten.Add(at)); st != nil {
			got = st.Title
		}
		if got != want {
			t.Errorf("at 10:00%+v: live card shows %q, want %q", at, got, want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	for ms, want := range map[int64]string{0: "0:00", 59_999: "0:59", 61_000: "1:01", 3_725_000: "1:02:05", -5: "0:00"} {
		if got := formatDuration(ms); got != want {
			t.Errorf("formatDuration(%d) = %q, want %q", ms, got, want)
		}
	}
}

func TestStopAtWallTime(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// A deadline that has already passed on the wall clock is enforced within
	// a tick, even though no monotonic timer is involved.
	start := time.Now()
	go stopAtWallTime(ctx, time.Now().Round(0).Add(-time.Second), cancel)
	select {
	case <-ctx.Done():
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("took %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not cancelled")
	}
}

// fakeStation serves an endless stream, a frame every 10 ms, until the
// client hangs up.
func fakeStation(t *testing.T) *httptest.Server {
	station := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		rc := http.NewResponseController(w)
		for i := 0; ; i++ {
			if _, err := w.Write(mp3Frame(byte(i))); err != nil {
				return
			}
			rc.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(func() { station.CloseClientConnections(); station.Close() })
	return station
}

func TestStopRecording(t *testing.T) {
	station := fakeStation(t)

	app, srv := testApp(t)
	cfg := testConfig()
	cfg.Destination, cfg.StreamURL = t.TempDir(), station.URL
	now := time.Now()
	job := Job{Program: "Club H", Event: spinitron.Event{Title: "Club H", Start: now, End: now.Add(time.Hour)}, Until: now.Add(time.Hour)}
	rec, err := startRecording(cfg, app.store, job)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(rec.part) != filepath.Join(cfg.Destination, "Club H") || !strings.HasPrefix(filepath.Base(rec.part), ".") {
		t.Errorf("part file %s should be hidden in the program folder", rec.part)
	}
	app.mu.Lock()
	app.active[job.Event.Key()] = rec
	app.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- rec.Run(t.Context()) }()
	for deadline := time.Now().Add(5 * time.Second); rec.bytesWritten() < 20*1044; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no audio recorded")
		}
	}

	post := func(path string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+path, nil)
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := post("/recordings/999/stop"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown recording: %s", resp.Status)
	}
	if resp := post(fmt.Sprintf("/recordings/%d/stop", rec.catalogID)); resp.StatusCode != http.StatusOK {
		t.Fatalf("stop: %s", resp.Status)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recording didn't stop")
	}
	// What was recorded is saved.
	if _, err := os.Stat(rec.out.showFile); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(rec.part); !os.IsNotExist(err) {
		t.Error("part file left behind")
	}

	// Ending a recording before Run has started stops it as soon as it does.
	rec2, _ := startRecording(cfg, nil, Job{Program: "Club H", Event: spinitron.Event{Title: "Club H 2", Start: now, End: now.Add(time.Hour)}, Until: now.Add(time.Hour)})
	rec2.Stop()
	go func() { done <- rec2.Run(t.Context()) }()
	select {
	case <-done: // "no audio received" is expected
	case <-time.After(5 * time.Second):
		t.Fatal("stopped-before-start recording ran")
	}
	if left, _ := filepath.Glob(filepath.Join(cfg.Destination, "Club H", "*Club H 2*")); len(left) != 0 {
		t.Errorf("a recording with no audio left %v", left)
	}
}

// Ending a recording, removing the show and adding it back records the
// current airing again (it used to be skipped as already recorded).
func TestReaddAfterEnd(t *testing.T) {
	app, srv := testApp(t)
	app.cfg.Destination, app.cfg.StreamURL = t.TempDir(), fakeStation(t).URL
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); app.wg.Wait() }()

	// Put "Train To Skaville" on the air now.
	now := time.Now()
	app.mu.Lock()
	app.events[0].Start, app.events[0].End = now.Add(-time.Minute), now.Add(time.Hour)
	key := app.events[0].Key()
	app.mu.Unlock()
	shows, _ := app.store.Shows(t.Context())
	var id int64
	for _, sh := range shows {
		if sh.Title == "Train To Skaville" {
			id = sh.ID
		}
	}
	toggle := func(record string) {
		t.Helper()
		resp, err := http.PostForm(fmt.Sprintf("%s/shows/%d/record", srv.URL, id), url.Values{"record": {record}, "from": {"mine"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	active := func() *recording {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.active[key]
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	toggle("1")
	app.startDue(ctx, time.Now())
	first := active()
	if first == nil {
		t.Fatal("selecting an on-air show didn't start recording")
	}
	waitFor("audio", func() bool { return first.bytesWritten() > 0 })

	// End it; it is not restarted while the show stays selected.
	first.Stop()
	waitFor("the recording to finish", func() bool { return active() == nil })
	app.startDue(ctx, time.Now())
	if active() != nil {
		t.Fatal("an ended recording restarted by itself")
	}

	// Remove the show and add it back: the current airing records again.
	toggle("0")
	toggle("1")
	app.startDue(ctx, time.Now())
	second := active()
	if second == nil {
		t.Fatal("re-adding the show didn't start recording")
	}
	// It's the same airing, so it resumes beside the same files.
	if second == first || second.out != first.out || !second.resumed {
		t.Errorf("second recording should resume %+v, got %+v (resumed %v)", first.out, second.out, second.resumed)
	}
	if _, err := os.Stat(first.out.showFile); err != nil {
		t.Errorf("first recording's file: %v", err)
	}
	waitFor("audio", func() bool { return second.bytesWritten() > 0 })
	second.Stop()
	waitFor("the second recording to finish", func() bool { return active() == nil })

	// The second recording's audio is added to the first's show file, after
	// silence for the gap, rather than written to a file of its own, and the
	// airing's length in the database is the file's.
	entries, _ := os.ReadDir(filepath.Dir(first.out.showFile))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	base := first.out.base
	if want := []string{"." + base + ".airing", base + ".cue", base + ".mp3"}; !slices.Equal(names, want) {
		t.Errorf("files %v, want %v", names, want)
	}
	if !second.merged {
		t.Error("the second recording wasn't added to the first's file")
	}
	recs, _ := app.store.Recordings(t.Context())
	fileMs, _ := audioDurationMs(first.out.showFile)
	// (Give or take a millisecond a part, from rounding each part's length.)
	if len(recs) != 1 || recs[0].DurationMs < fileMs-2 || recs[0].DurationMs > fileMs || recs[0].ShowFile != first.relPath(first.out.showFile) {
		t.Errorf("recordings %+v; the file lasts %d ms", recs, fileMs)
	}
}

// After End recording, the next refresh drops the ● recording tag from Your
// shows and the full list, and the ended airing doesn't show as upcoming.
func TestRecordingTagClearsAfterEnd(t *testing.T) {
	app, srv := testApp(t)
	ctx := context.Background()
	now := time.Now()
	app.mu.Lock()
	app.events[0].Start, app.events[0].End = now.Add(-time.Minute), now.Add(time.Hour) // Train To Skaville, on now
	ev := app.events[0]
	app.mu.Unlock()
	shows, _ := app.store.Shows(ctx)
	var id int64
	for _, sh := range shows {
		if sh.Title == ev.Title {
			id = sh.ID
		}
	}
	app.store.SetRecord(ctx, id, true)

	f := newFakeRecording(t, testConfig())
	f.job = Job{Program: ev.Title, Event: ev, Until: ev.End}
	f.frames(1, 3)
	app.mu.Lock()
	app.started[ev.Key()], app.active[ev.Key()] = true, f.recording
	app.mu.Unlock()

	live := func() (upcoming string, selected, tagged []int64) {
		t.Helper()
		var out struct {
			Upcoming                      string
			SelectedShows, RecordingShows []int64
		}
		firstEvent(t, srv.URL, &out)
		return out.Upcoming, out.SelectedShows, out.RecordingShows
	}
	if _, _, tagged := live(); len(tagged) != 1 || tagged[0] != id {
		t.Fatalf("while recording: tagged %v", tagged)
	}

	// End recording: Stop marks it stopped right away, while files are saved.
	f.Stop()
	upcoming, selected, tagged := live()
	if len(tagged) != 0 {
		t.Errorf("after ending: tagged %v", tagged)
	}
	if strings.Contains(upcoming, ev.Title) {
		t.Errorf("the ended airing is listed as upcoming:\n%s", upcoming)
	}
	if len(selected) != 1 || selected[0] != id {
		t.Errorf("the show should still be selected: %v", selected)
	}
}
