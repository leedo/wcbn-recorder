package recorder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"wcbn-recorder/internal/spinitron"
)

func TestCurrentEvent(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 12, h, 0, 0, 0, time.UTC) }
	events := []spinitron.Event{{Title: "A", Start: at(14), End: at(15)}, {Title: "B", Start: at(15), End: at(16)}}
	if ev, ok := currentEvent(events, at(15)); !ok || ev.Title != "B" {
		t.Errorf("at 15:00 got %q, %v", ev.Title, ok)
	}
	if _, ok := currentEvent(events, at(16)); ok {
		t.Error("nothing should be on at 16:00")
	}
}

// fakeIcecast serves an Icecast status page and an endless stream.
func fakeIcecast(t *testing.T, status string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status-json.xsl":
			w.Write([]byte(status))
		case "/wcbn-hd.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			rc := http.NewResponseController(w)
			for i := 0; r.Context().Err() == nil; i++ {
				if _, err := w.Write(mp3Frame(byte(i))); err != nil {
					return
				}
				rc.Flush()
				time.Sleep(10 * time.Millisecond)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return srv
}

const icecastStatus = `{"icestats":{"source":[
 {"listenurl":"http://floyd.wcbn.org:8000/wcbn-hd.mp3","bitrate":320,"title":"West Coast Blues by Blind Blake on Yazoo City Calling with Zekkie"},
 {"listenurl":"http://floyd.wcbn.org:8000/wcbn3.mp3","title":"Arwulf - Something else"}]}}`

func TestFetchStationStatus(t *testing.T) {
	srv := fakeIcecast(t, icecastStatus)
	if got, err := fetchStationStatus(t.Context(), srv.URL+"/wcbn-hd.mp3"); err != nil ||
		got.Title != "West Coast Blues by Blind Blake on Yazoo City Calling with Zekkie" || got.Bitrate != 320 {
		t.Errorf("list: %+v, %v", got, err)
	}
	// Icecast sends a single object, not a list, when there's one source.
	one := fakeIcecast(t, `{"icestats":{"source":{"listenurl":"http://x:8000/wcbn-hd.mp3","title":"Solo"}}}`)
	if got, err := fetchStationStatus(t.Context(), one.URL+"/wcbn-hd.mp3"); err != nil || got.Title != "Solo" || got.Bitrate != 0 {
		t.Errorf("single: %+v, %v", got, err)
	}
	if got, _ := fetchStationStatus(t.Context(), srv.URL+"/other.mp3"); got != (stationStatus{}) {
		t.Errorf("unknown mount: %+v", got)
	}
}

func TestWebOnAir(t *testing.T) {
	app, srv := testApp(t)
	station := fakeIcecast(t, strings.Replace(icecastStatus, "Yazoo City Calling with Zekkie", "Train To Skaville with brian tomsic", 1))
	app.cfg.StreamURL, app.cfg.Destination = station.URL+"/wcbn-hd.mp3", t.TempDir()
	app.cfg.ShowFile, app.cfg.TrackFiles = true, true
	now := time.Now()
	app.mu.Lock()
	app.events[0].Start, app.events[0].End = now.Add(-10*time.Minute), now.Add(50*time.Minute) // Train To Skaville is on
	app.mu.Unlock()

	page := get(t, srv.URL+"/")
	for _, want := range []string{`class="card live-card onair"`, "ON AIR", `<h2><a href="https://spinitron.com/WCBN/show/1/x" target="_blank" rel="noopener">Train To Skaville</a></h2>`, "with brian tomsic",
		"West Coast Blues", "Blind Blake", "Not recording · 320 kbps"} {
		if !strings.Contains(page, want) {
			t.Errorf("On air card missing %q", want)
		}
	}
	if strings.Contains(page, "Record this show") || strings.Contains(page, `class="toggle-form"`) {
		t.Error("the On air card shouldn't have a Record button; shows are added on the Shows page")
	}
	// The player bar, the card's compact form, has the show and song too.
	for _, want := range []string{`<div id="player">`, `<strong>Train To Skaville</strong> <span class="muted">with brian tomsic</span>`,
		"West Coast Blues — Blind Blake", `class="listen"`} {
		if !strings.Contains(page, want) {
			t.Errorf("player bar missing %q", want)
		}
	}

	// Listen relays the station's audio.
	resp, err := http.Get(srv.URL + "/listen")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	io.ReadFull(resp.Body, buf)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "audio/mpeg" || buf[0] != 0xFF {
		t.Errorf("live stream: %s, starts % x", resp.Header.Get("Content-Type"), buf)
	}

	// Adding the show that's on the air starts recording it before the
	// response, so the next refresh shows the recording instead of the On air
	// card.
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); app.wg.Wait() }()
	app.mu.Lock()
	app.runCtx = ctx
	app.mu.Unlock()
	shows, _ := app.store.Shows(ctx)
	var id int64
	for _, sh := range shows {
		if sh.Title == "Train To Skaville" {
			id = sh.ID
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = client.PostForm(fmt.Sprintf("%s/shows/%d/record", srv.URL, id), url.Values{"record": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("adding the show on the air: %s", resp.Status)
	}
	app.mu.Lock()
	recording := len(app.active)
	app.mu.Unlock()
	if recording != 1 {
		t.Fatalf("%d recordings started", recording)
	}
	if page := get(t, srv.URL+"/"); strings.Contains(page, `live-card onair"`) || !strings.Contains(page, `<span class="badge rec">`) {
		t.Error("page should show the recording, not the On air card")
	}
}
