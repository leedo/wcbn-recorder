package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	// The finished song is saved in the background; the current one isn't.
	waitForFiles(t, f.out.trackDir, "01 - Toots & the Maytals - Pressure Drop.mp3")
	for deadline := time.Now().Add(5 * time.Second); f.status(time.Now()).Saved < 1 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if st := f.status(time.Now()); st.Saved != 1 || !st.Tracks[0].Saved || st.Tracks[1].Saved || !st.SongFiles {
		t.Errorf("saved = %d, tracks %+v", st.Saved, st.Tracks)
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
	f.title("Pressure Drop by Toots & the Maytals on Train To Skaville with brian tomsic")
	f.frames(1, 3)
	f.Write(nil) // marks the stream connected
	app.mu.Lock()
	app.active[f.job.Event.key()] = f.recording
	app.mu.Unlock()

	page := get(t, srv.URL+"/")
	for _, want := range []string{`<section class="card live-card"`, "REC", "Now playing", "Pressure Drop", "Toots &amp; the Maytals", "Songs recorded so far (1)", `class="stop-form"`, "End recording"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}

	resp, err := http.Get(srv.URL + "/live")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Live, Upcoming string
		Recording      int
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Recording != 1 || !strings.Contains(out.Live, "Pressure Drop") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("/live = %+v", out)
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

func TestWebListen(t *testing.T) {
	app, srv := testApp(t)
	f := newFakeRecording(t, testConfig())
	f.id = 42
	f.title("Pressure Drop by Toots & the Maytals")
	f.frames(1, 400) // ~10.4 s of audio
	app.mu.Lock()
	app.active[f.job.Event.key()] = f.recording
	app.mu.Unlock()

	if resp, _ := http.Get(srv.URL + "/listen/999"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown recording: %s", resp.Status)
	}

	resp, err := http.Get(srv.URL + "/listen/42")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "audio/mpeg" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers: %v", resp.Header)
	}

	frameLen := len(mp3Frame(0))
	readN := func(n int) []byte {
		t.Helper()
		buf := make([]byte, n)
		done := make(chan error, 1)
		go func() { _, err := io.ReadFull(resp.Body, buf); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("reading %d bytes: %v", n, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out reading %d bytes", n)
		}
		return buf
	}

	// It starts about 5 s back from the live edge, on a frame boundary.
	lead := len(f.audio) * 5000 / int(f.recording.audio.durationMs())
	lead = (lead/frameLen + 1) * frameLen // first frame boundary at or after 5 s back
	first := readN(lead - frameLen)
	if _, ok := parseFrameHeader(first); !ok {
		t.Fatalf("stream doesn't start on a frame: % x", first[:4])
	}
	start := len(f.audio) - (lead - frameLen)
	if !bytes.Equal(first, f.audio[start:]) {
		t.Errorf("first chunk isn't the last %d bytes of the recording", len(first))
	}

	// New audio follows as it's recorded.
	f.frames(2, 10)
	if got := readN(10 * frameLen); !bytes.Equal(got, f.audio[len(f.audio)-10*frameLen:]) {
		t.Error("new frames didn't follow")
	}

	// When recording stops, the stream ends after the last bytes.
	f.frames(3, 5)
	f.mu.Lock()
	f.finishing = true
	f.mu.Unlock()
	rest, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(rest, f.audio[len(f.audio)-5*frameLen:]) {
		t.Errorf("tail: %d bytes, err %v", len(rest), err)
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
	t.Cleanup(station.Close)
	return station
}

func TestStopRecording(t *testing.T) {
	station := fakeStation(t)

	app, srv := testApp(t)
	cfg := testConfig()
	cfg.Destination, cfg.StreamURL = t.TempDir(), station.URL
	now := time.Now()
	job := Job{Program: "Club H", Event: Event{Title: "Club H", Start: now, End: now.Add(time.Hour)}, Until: now.Add(time.Hour)}
	rec, err := startRecording(cfg, job)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(rec.part) != rec.out.trackDir || !strings.HasPrefix(filepath.Base(rec.part), ".") {
		t.Errorf("part file %s should be hidden in the show folder %s", rec.part, rec.out.trackDir)
	}
	app.mu.Lock()
	app.active[job.Event.key()] = rec
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
	if resp := post(fmt.Sprintf("/recordings/%d/stop", rec.id)); resp.StatusCode != http.StatusOK {
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
	// What was recorded is saved: the whole show, and its one (untitled) song.
	if _, err := os.Stat(rec.out.showFile); err != nil {
		t.Error(err)
	}
	waitForFiles(t, rec.out.trackDir, "01 - Club H.mp3")
	if _, err := os.Stat(rec.part); !os.IsNotExist(err) {
		t.Error("part file left behind")
	}

	// Ending a recording before Run has started stops it as soon as it does.
	rec2, _ := startRecording(cfg, Job{Program: "Club H", Event: Event{Title: "Club H 2", Start: now, End: now.Add(time.Hour)}, Until: now.Add(time.Hour)})
	rec2.Stop()
	go func() { done <- rec2.Run(t.Context()) }()
	select {
	case <-done: // "no audio received" is expected
	case <-time.After(5 * time.Second):
		t.Fatal("stopped-before-start recording ran")
	}
	if _, err := os.Stat(rec2.out.trackDir); !os.IsNotExist(err) {
		t.Error("a recording with no audio left an empty show folder")
	}
}

// Ending a recording, removing the show and adding it back records the
// current airing again (it used to be skipped as already recorded).
func TestReaddAfterEnd(t *testing.T) {
	app, srv := testApp(t)
	app.cfg.Destination, app.cfg.StreamURL = t.TempDir(), fakeStation(t).URL
	app.cfg.ShowFile, app.cfg.TrackFiles = true, true
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); app.wg.Wait() }()

	// Put "Train To Skaville" on the air now.
	now := time.Now()
	app.mu.Lock()
	app.events[0].Start, app.events[0].End = now.Add(-time.Minute), now.Add(time.Hour)
	key := app.events[0].key()
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
	if second == first || second.out.base == first.out.base {
		t.Errorf("second recording should get its own files: %q vs %q", second.out.base, first.out.base)
	}
	if _, err := os.Stat(first.out.showFile); err != nil {
		t.Errorf("first recording's file: %v", err)
	}
	waitFor("audio", func() bool { return second.bytesWritten() > 0 })
	second.Stop()
	waitFor("the second recording to finish", func() bool { return active() == nil })
}
