package main

import (
	"encoding/json"
	"net/http"
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
	for _, want := range []string{`<section class="card live-card"`, "REC", "Now playing", "Pressure Drop", "Toots &amp; the Maytals", "Songs recorded so far (1)"} {
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
