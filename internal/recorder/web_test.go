package recorder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"wcbn-recorder/internal/config"
	"wcbn-recorder/internal/spinitron"
	"wcbn-recorder/internal/store"
)

func testApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	loc, _ := time.LoadLocation("America/Detroit")
	cfg := &config.Config{Location: loc, PostRoll: config.Duration{Duration: 2 * time.Minute}}
	app := NewApp(cfg, store, NewLogs(100, nil, false))

	start := time.Now().Add(time.Hour).Truncate(time.Hour)
	app.events = []spinitron.Event{
		{Title: "Train To Skaville", DJ: "brian tomsic", Category: "Specialty", URL: "https://spinitron.com/WCBN/show/1/x", Start: start, End: start.Add(time.Hour)},
		{Title: "<script>alert(1)</script>", Category: "Freeform", URL: "https://spinitron.com/WCBN/show/2/y", Start: start.Add(time.Hour), End: start.Add(2 * time.Hour)},
	}
	if err := store.UpsertEvents(ctx, app.events, time.Now()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { app.tap.close() }) // runs first: ends /listen streams, as Run does on shutdown
	return app, srv
}

func get(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %s\n%s", u, resp.Status, b)
	}
	return string(b)
}

func TestWebToggle(t *testing.T) {
	app, srv := testApp(t)
	if home := get(t, srv.URL+"/"); !strings.Contains(home, `Pick some on <a href="/shows">Shows</a>`) || strings.Contains(home, `id="browse"`) {
		t.Errorf("home page should point to Shows, not list them:\n%s", home)
	}
	page := get(t, srv.URL+"/shows")
	if !strings.Contains(page, "Train To Skaville") || !strings.Contains(page, `<main id="page" data-page="shows"`) {
		t.Fatalf("unexpected page:\n%s", page)
	}
	// Just the search and the list of every show: no Your shows, no heading.
	if !strings.Contains(page, `<section id="browse" aria-label="Every show on the calendar">`) || !strings.Contains(page, `id="q"`) ||
		strings.Contains(page, `id="mine"`) || strings.Contains(page, "Your shows") || strings.Contains(page, "<h2>Add shows</h2>") {
		t.Error("the shows page should be just the search and the show list")
	}
	if !strings.Contains(page, `<input type="hidden" name="record" value="1">`) || !strings.Contains(page, `aria-label="Record Train To Skaville">+ Add</button>`) {
		t.Error("a show that isn't selected should have + Add")
	}
	if strings.Contains(page, "Refresh now") || strings.Contains(page, "Schedule updated") {
		t.Error("the page shouldn't show when the schedule was updated, or a Refresh button")
	}
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Error("show title was not escaped")
	}
	if !strings.Contains(page, `<td class="title" data-sort="train to skaville" data-title="Train To Skaville">`) || !strings.Contains(page, `class="sort" data-col="1" data-type="number">Next airing`) {
		t.Error("missing sort attributes")
	}
	// The DJ and category are under the show's title, not in columns of their
	// own, and there's no description.
	if !strings.Contains(page, `<div class="sub muted">with brian tomsic · Specialty</div>`) || strings.Contains(page, ">DJ</button>") ||
		strings.Contains(page, ">Category</button>") || strings.Contains(page, "<summary>Description</summary>") {
		t.Error("the show list should have the DJ and category under the title, and no DJ or Category column or description")
	}

	shows, _ := app.store.Shows(context.Background())
	var id string
	for _, sh := range shows {
		if sh.Title == "Train To Skaville" {
			id = strconv.FormatInt(sh.ID, 10)
		}
	}

	// JS path: JSON response with the re-rendered upcoming list.
	req, _ := http.NewRequest("POST", srv.URL+"/shows/"+id+"/record", strings.NewReader("record=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ID       int64
		Record   bool
		Upcoming string
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || !out.Record || strconv.FormatInt(out.ID, 10) != id || !strings.Contains(out.Upcoming, "Train To Skaville") {
		t.Fatalf("toggle: %s %+v", resp.Status, out)
	}

	// With a show selected, its row in the list is highlighted, with Remove.
	// The home page has it coming up, linked to its row.
	page = get(t, srv.URL+"/shows")
	if !strings.Contains(page, `<tr id="show-`+id+`" class="selected `) || !strings.Contains(page, `<input type="hidden" name="record" value="0">`) ||
		!strings.Contains(page, `aria-label="Stop recording Train To Skaville">Remove</button>`) {
		t.Error("a selected show should be highlighted, with Remove")
	}
	if home := get(t, srv.URL+"/"); !strings.Contains(home, `<a href="/shows#show-`+id+`">Train To Skaville</a>`) {
		t.Error("home page should list the show's next airing, linked to it on the Shows page")
	}
	if sel, _ := app.store.Selected(context.Background()); len(sel) != 1 {
		t.Errorf("selected = %d", len(sel))
	}

	// No-JS path: a plain form post redirects back to where it came from.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, tc := range []struct{ from, record, want string }{
		{"onair", "1", "/"},
		{"shows", "1", "/shows#show-" + id},
		{"shows", "0", "/shows#show-" + id},
	} {
		resp, err = client.PostForm(srv.URL+"/shows/"+id+"/record", url.Values{"record": {tc.record}, "from": {tc.from}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != tc.want {
			t.Errorf("form post from %s: %s %s", tc.from, resp.Status, resp.Header.Get("Location"))
		}
	}
	if sel, _ := app.store.Selected(context.Background()); len(sel) != 0 {
		t.Errorf("selected after unselect = %d", len(sel))
	}

	resp, _ = client.PostForm(srv.URL+"/shows/999/record", url.Values{"record": {"1"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown show: %s", resp.Status)
	}
}

func TestWebRejectsCrossOrigin(t *testing.T) {
	app, srv := testApp(t)
	req, _ := http.NewRequest("POST", srv.URL+"/shows/1/record", strings.NewReader("record=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site post: %s", resp.Status)
	}
	if sel, _ := app.store.Selected(context.Background()); len(sel) != 0 {
		t.Error("cross-site post changed selections")
	}
}

func TestWebArtwork(t *testing.T) {
	app, srv := testApp(t)
	ctx := context.Background()
	shows, _ := app.store.Shows(ctx)
	for _, sh := range shows {
		img := "https://farm6.staticflickr.com/5017/5476585780_087d4791ae_m.jpg"
		if sh.Title != "Train To Skaville" {
			img = "javascript:alert(1)" // a hostile page; must not reach the HTML
		}
		app.store.UpdateDetails(ctx, sh.ID, spinitron.ShowInfo{Image: img}, time.Now())
		app.store.SetRecord(ctx, sh.ID, true)
	}
	app.store.SetSetting(ctx, "station_logo", "https://spinitron.com/images/Station/22/2263-img_logo.225x225.jpg")

	page := get(t, srv.URL+"/shows")
	want := `<img class="art art-sm" src="https://farm6.staticflickr.com/5017/5476585780_087d4791ae_m.jpg" alt="" data-fallback="https://spinitron.com/images/Station/22/2263-img_logo.225x225.jpg" loading="lazy"`
	if !strings.Contains(page, want) {
		t.Errorf("show list artwork missing:\n%s", want)
	}
	if strings.Contains(page, "javascript:") {
		t.Error("javascript: URL reached the page")
	}
	if got := artImage("", "logo.jpg", "art-sm", true); got.Src != "logo.jpg" || got.Fallback != "" {
		t.Errorf("no image should use the logo: %+v", got)
	}
	if got := artImage("", "", "art-sm", true); got.Src != "" {
		t.Errorf("no image and no logo: %+v", got)
	}
}

// Remove answers the page's script without a redirect, so the page doesn't
// reload (which would stop any audio).
func TestWebInPlaceActions(t *testing.T) {
	app, srv := testApp(t)
	post := func(path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, nil)
		req.Header.Set("Accept", "application/json")
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	shows, _ := app.store.Shows(context.Background())
	if resp := post(fmt.Sprintf("/shows/%d/delete", shows[0].ID)); resp.StatusCode != http.StatusOK {
		t.Errorf("delete: %s", resp.Status)
	}
	if after, _ := app.store.Shows(context.Background()); len(after) != len(shows)-1 {
		t.Errorf("delete didn't remove the show: %d -> %d", len(shows), len(after))
	}
}

// The event stream sends a change as soon as it's made, not at the next
// check for progress, and ends when the app shuts down.
func TestWebEvents(t *testing.T) {
	app, srv := testApp(t)
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	type event struct{ SelectedShows []int64 }
	events := make(chan event)
	go func() {
		defer close(events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var e event
				json.Unmarshal([]byte(data), &e)
				events <- e
			}
		}
	}()
	next := func() (event, bool) {
		t.Helper()
		select {
		case e, ok := <-events:
			return e, ok
		case <-time.After(liveInterval / 2):
			t.Fatal("no event")
			return event{}, false
		}
	}
	if e, _ := next(); len(e.SelectedShows) != 0 {
		t.Fatalf("shows selected before any were added: %v", e.SelectedShows)
	}

	shows, _ := app.store.Shows(context.Background())
	var id int64
	for _, sh := range shows {
		if sh.Title == "Train To Skaville" {
			id = sh.ID
			app.store.SetRecord(context.Background(), sh.ID, true)
		}
	}
	app.notify()
	if e, _ := next(); len(e.SelectedShows) != 1 || e.SelectedShows[0] != id {
		t.Errorf("selected shows = %v, want [%d]", e.SelectedShows, id)
	}

	close(app.done)
	if _, ok := next(); ok {
		t.Error("stream still open after shutdown")
	}
}

// Every page has the navigation, the live card and the player bar (which the
// page script shows once the card is scrolled away) around its contents.
func TestWebLayout(t *testing.T) {
	app, srv := testApp(t)
	home := get(t, srv.URL+"/")
	for _, want := range []string{`<main id="page" data-page="home"`, `<a href="/" aria-current="page">Upcoming</a>`, `<div id="live" aria-live="polite">`, `<div id="player">`, "<title>WCBN Recorder</title>"} {
		if !strings.Contains(home, want) {
			t.Errorf("home page missing %q", want)
		}
	}

	// Links to the show list from when it was on the home page still work.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, old := range []string{"/?add=1", "/shows?add=1"} {
		if resp, err := noFollow.Get(srv.URL + old); err != nil || resp.Header.Get("Location") != "/shows#add-shows" {
			t.Errorf("%s should redirect to /shows#add-shows: %v %v", old, resp.Header.Get("Location"), err)
		} else {
			resp.Body.Close()
		}
	}

	page := get(t, srv.URL+"/recordings")
	for _, want := range []string{`<main id="page" data-page="recordings"`, `<a href="/recordings" aria-current="page">Recordings</a>`, `class="card live-card onair"`, `class="listen"`,
		"<title>Recordings · WCBN Recorder</title>", "Nothing has been recorded yet"} {
		if !strings.Contains(page, want) {
			t.Errorf("recordings page missing %q", want)
		}
	}

	// The recordings page lists the database's recordings, newest first.
	ctx := t.Context()
	start := time.Date(2026, 10, 6, 19, 0, 0, 0, app.cfg.Location)
	for i, title := range []string{"Train To Skaville", "Yazoo City Calling"} {
		at := start.Add(time.Duration(i) * 24 * time.Hour)
		id, err := app.store.AddRecording(ctx, store.Recording{Airing: title, Title: title, Start: at, End: at.Add(time.Hour), StartedAt: at, Dir: title + "/" + title + " - 2026-10-0" + fmt.Sprint(6+i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			app.store.UpdateRecording(ctx, store.Recording{ID: id, DJ: "brian tomsic", FinishedAt: at.Add(time.Hour), DurationMs: 3_600_000, Bytes: 144_000_000})
			app.store.AddSong(ctx, store.Song{RecordingID: id, Number: 1, Title: "Pressure Drop", PlayedAt: at})
		}
	}
	page = get(t, srv.URL+"/recordings")
	skaville, yazoo := strings.Index(page, "<strong>Train To Skaville</strong>"), strings.Index(page, "<strong>Yazoo City Calling</strong>")
	if skaville < 0 || yazoo < 0 || yazoo > skaville {
		t.Errorf("recordings out of order or missing:\n%s", page)
	}
	for _, want := range []string{"with brian tomsic", "1:00:00 · 144.0 MB · 1 song", "unfinished", "Train To Skaville/Train To Skaville - 2026-10-06"} {
		if !strings.Contains(page, want) {
			t.Errorf("recordings page missing %q", want)
		}
	}
}
