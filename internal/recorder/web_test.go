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
	"slices"
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
	// Your shows, empty for now, then Add shows, with the search and every
	// other show.
	if !strings.Contains(page, `<h2>Your shows</h2>`) || !strings.Contains(page, `<p class="empty muted">You're not recording any shows yet. Add some below.</p>`) ||
		!strings.Contains(page, `<h2>Add shows</h2>`) || !strings.Contains(page, `<section id="browse" aria-label="Shows on the calendar to add">`) || !strings.Contains(page, `id="q"`) {
		t.Error("the shows page should have Your shows, empty, and Add shows")
	}
	if strings.Index(page, `id="mine"`) > strings.Index(page, `id="browse"`) {
		t.Error("Your shows should come first")
	}
	// Both sort by either column, starting by next airing.
	if n := strings.Count(page, `<th aria-sort="ascending"><button type="button" class="sort" data-col="1" data-type="number">Next airing</button></th>`); n != 2 {
		t.Errorf("%d lists start sorted by next airing, want 2", n)
	}
	if !strings.Contains(page, `<input type="hidden" name="record" value="1">`) || !strings.Contains(page, `aria-label="Record Train To Skaville">Add</button>`) {
		t.Error("a show that isn't selected should have Add")
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

	// With a show selected, it's in Your shows, with Remove, and not in Add
	// shows. The Recordings page has it coming up, linked to its page.
	page = get(t, srv.URL+"/shows")
	_, afterMine, _ := strings.Cut(page, `<section id="mine"`)
	yours, others, _ := strings.Cut(afterMine, `<section id="browse"`)
	if !strings.Contains(yours, `<tr id="show-`+id+`" class="selected `) || !strings.Contains(yours, `<input type="hidden" name="record" value="0">`) ||
		!strings.Contains(yours, `aria-label="Stop recording Train To Skaville">Remove</button>`) || strings.Contains(yours, `<p class="empty muted">You're`) {
		t.Error("a selected show should be in Your shows, with Remove")
	}
	if strings.Contains(others, `id="show-`+id+`"`) {
		t.Error("a selected show shouldn't be in Add shows")
	}
	if page := get(t, srv.URL+"/recordings"); !strings.Contains(page, `<a class="rec-link" href="/shows/`+id+`">`) || !strings.Contains(page, `<span class="show"><strong>Train To Skaville</strong> <span class="muted">with brian tomsic</span></span>`) {
		t.Error("the Recordings page should list the show's next airing, like the recordings, linked to the show's page")
	}
	if sel, _ := app.store.Selected(context.Background()); len(sel) != 1 {
		t.Errorf("selected = %d", len(sel))
	}

	// No-JS path: a plain form post redirects back to the show in the list.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, tc := range []struct{ record, want string }{
		{"1", "/shows#show-" + id},
		{"0", "/shows#show-" + id},
	} {
		resp, err = client.PostForm(srv.URL+"/shows/"+id+"/record", url.Values{"record": {tc.record}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != tc.want {
			t.Errorf("form post record=%s: %s %s", tc.record, resp.Status, resp.Header.Get("Location"))
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

	// The home page is the Recordings page; links to the show list from when
	// it was on the home page still work.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := noFollow.Get(srv.URL + "/"); err != nil || resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/recordings" {
		t.Errorf("/ should redirect to /recordings: %v %v", resp.Header.Get("Location"), err)
	} else {
		resp.Body.Close()
	}
	for _, old := range []string{"/?add=1", "/shows?add=1"} {
		if resp, err := noFollow.Get(srv.URL + old); err != nil || resp.Header.Get("Location") != "/shows#add-shows" {
			t.Errorf("%s should redirect to /shows#add-shows: %v %v", old, resp.Header.Get("Location"), err)
		} else {
			resp.Body.Close()
		}
	}

	// The Recordings page, first in the navigation, has the upcoming
	// recordings above the ones recorded.
	page := get(t, srv.URL+"/recordings")
	for _, want := range []string{`<main id="page" data-page="recordings"`, `<div id="live" aria-live="polite">`, `<div id="player">`,
		`<nav class="site-nav" aria-label="Site">
  <a href="/recordings" aria-current="page">Recordings</a>
  <a href="/shows">Shows</a>`, `class="card live-card onair"`, `class="listen"`,
		"<title>Recordings · WCBN Recorder</title>", "Nothing has been recorded yet"} {
		if !strings.Contains(page, want) {
			t.Errorf("recordings page missing %q", want)
		}
	}
	if up, rec := strings.Index(page, `<h2>Upcoming</h2></div>
<div class="card" id="upcoming">`), strings.Index(page, `<h2>Recorded</h2></div>
<div id="recordings"`); up < 0 || rec < up {
		t.Error("the upcoming list should be above the recordings")
	}
	if strings.Contains(page, ">Upcoming</a>") {
		t.Error("Upcoming shouldn't be in the navigation")
	}

	// The recordings page lists the database's recordings, newest first, each
	// with its date, show and DJ linked to its details page.
	ctx := t.Context()
	start := time.Date(2026, 10, 6, 19, 0, 0, 0, app.cfg.Location)
	for i, title := range []string{"Train To Skaville", "Yazoo City Calling"} {
		at := start.Add(time.Duration(i) * 24 * time.Hour)
		id, err := app.store.AddRecording(ctx, store.Recording{Airing: title, Title: title, Start: at, End: at.Add(time.Hour), StartedAt: at, Dir: title + "/" + title + " - 2026-10-0" + fmt.Sprint(6+i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			app.store.UpdateRecording(ctx, store.Recording{ID: id, DJ: "brian tomsic", URL: "https://spinitron.com/WCBN/show/264320/Train-To-Skaville", FinishedAt: at.Add(time.Hour), DurationMs: 3_600_000, Bytes: 144_000_000})
			app.store.SetSongs(ctx, id, []store.Song{{Number: 1, Title: "Pressure Drop", Artist: "Toots & the Maytals", PlayedAt: at, DurationMs: 185_000}}, "https://spinitron.com/WCBN/pl/23086806/Train-To-Skaville")
		}
	}
	page = get(t, srv.URL+"/recordings")
	skaville := strings.Index(page, `<a class="rec-link" href="/recordings/1">
      <span class="time">Tue Oct 6, 7:00 PM</span>
      <span class="show"><strong>Train To Skaville</strong> <span class="muted">with brian tomsic</span></span>
    </a>`)
	yazoo := strings.Index(page, `<a class="rec-link" href="/recordings/2">`)
	if skaville < 0 || yazoo < 0 || yazoo > skaville {
		t.Errorf("recordings out of order or missing:\n%s", page)
	}
	for _, want := range []string{"with brian tomsic", "unfinished"} {
		if !strings.Contains(page, want) {
			t.Errorf("recordings page missing %q", want)
		}
	}
	if strings.Contains(page, `class="sub muted">Train To Skaville/`) || strings.Contains(page, "<details") || strings.Contains(page, "144.0 MB") || strings.Contains(page, "1 song") {
		t.Error("the recordings page shouldn't show the folder, size, song count or a song list; they're on the details page")
	}
}

// Upcoming lists only the next two days of recordings.
// TestWebHomeScreen: the page links the icon and manifest an iPhone or
// Android uses to add the app to its home screen, and they're served.
func TestWebHomeScreen(t *testing.T) {
	_, srv := testApp(t)
	page := get(t, srv.URL+"/recordings")
	for _, want := range []string{`viewport-fit=cover`, `<meta name="apple-mobile-web-app-capable" content="yes">`,
		`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`, `<link rel="manifest" href="/manifest.webmanifest">`} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %s", want)
		}
	}
	var m struct {
		StartURL string `json:"start_url"`
		Display  string
		Icons    []struct{ Src string }
	}
	if err := json.Unmarshal([]byte(get(t, srv.URL+"/manifest.webmanifest")), &m); err != nil {
		t.Fatal(err)
	}
	if m.StartURL != "/recordings" || m.Display != "standalone" || len(m.Icons) != 2 {
		t.Errorf("manifest = %+v", m)
	}
	for _, icon := range m.Icons {
		if b := get(t, srv.URL+icon.Src); !strings.HasPrefix(b, "\x89PNG") {
			t.Errorf("%s isn't a PNG", icon.Src)
		}
	}
}

func TestWebUpcomingWindow(t *testing.T) {
	app, srv := testApp(t)
	ctx := t.Context()
	soon := app.events[0] // Train To Skaville, starting within the hour
	later := soon
	later.Start, later.End = soon.Start.Add(3*24*time.Hour), soon.End.Add(3*24*time.Hour)
	app.mu.Lock()
	app.events = append(app.events, later)
	app.mu.Unlock()
	shows, _ := app.store.Shows(ctx)
	for _, sh := range shows {
		if sh.Title == soon.Title {
			app.store.SetRecord(ctx, sh.ID, true)
		}
	}

	page := get(t, srv.URL+"/")
	when := func(ev spinitron.Event) string { return ev.Start.In(app.cfg.Location).Format("Mon Jan 2, 3:04 PM") }
	if !strings.Contains(page, when(soon)) {
		t.Errorf("the airing in the next two days is missing (%s)", when(soon))
	}
	if strings.Contains(page, when(later)) {
		t.Errorf("the airing three days out is listed (%s)", when(later))
	}

	// With nothing in the next two days, the next airing is listed anyway.
	app.mu.Lock()
	app.events = slices.DeleteFunc(app.events, func(ev spinitron.Event) bool { return ev.Start.Equal(soon.Start) })
	app.mu.Unlock()
	page = get(t, srv.URL+"/")
	if !strings.Contains(page, when(later)) {
		t.Errorf("with nothing sooner, the airing three days out is missing (%s)", when(later))
	}
	if strings.Contains(page, "None of your shows") {
		t.Error("the list says it's empty")
	}
}

// The Recorded list shows ten recordings a page, newest first, with links to
// the newer and older pages.
func TestWebRecordingsPages(t *testing.T) {
	app, srv := testApp(t)
	ctx := t.Context()
	start := time.Date(2026, 9, 1, 19, 0, 0, 0, app.cfg.Location)
	for i := range 23 {
		at := start.Add(time.Duration(i) * 24 * time.Hour)
		title := fmt.Sprintf("Show %02d", i+1)
		if _, err := app.store.AddRecording(ctx, store.Recording{Airing: title, Title: title, Start: at, End: at.Add(time.Hour), StartedAt: at, Dir: title}); err != nil {
			t.Fatal(err)
		}
	}
	rows := func(page string) []string {
		var titles []string
		for _, part := range strings.Split(page, `<span class="show"><strong>`)[1:] {
			title, _, _ := strings.Cut(part, "<")
			if strings.HasPrefix(title, "Show ") {
				titles = append(titles, title)
			}
		}
		return titles
	}
	for _, c := range []struct {
		query       string
		first, last string
		n           int
		pager       []string
		notInPager  []string
	}{
		{"", "Show 23", "Show 14", 10, []string{"Page 1 of 3", `<a href="/recordings?page=2#recorded" rel="next">Older →</a>`}, []string{`rel="prev"`}},
		{"?page=2", "Show 13", "Show 04", 10, []string{"Page 2 of 3", `<a href="/recordings#recorded" rel="prev">← Newer</a>`, `<a href="/recordings?page=3#recorded" rel="next">`}, nil},
		{"?page=3", "Show 03", "Show 01", 3, []string{"Page 3 of 3", `<a href="/recordings?page=2#recorded" rel="prev">`}, []string{`rel="next"`}},
		{"?page=99", "Show 03", "Show 01", 3, []string{"Page 3 of 3"}, nil},
		{"?page=x", "Show 23", "Show 14", 10, []string{"Page 1 of 3"}, nil},
	} {
		page := mainOf(get(t, srv.URL+"/recordings"+c.query))
		got := rows(page)
		if len(got) != c.n || got[0] != c.first || got[len(got)-1] != c.last {
			t.Errorf("%q: %v", c.query, got)
		}
		for _, want := range c.pager {
			if !strings.Contains(page, want) {
				t.Errorf("%q: missing %q", c.query, want)
			}
		}
		for _, unwanted := range c.notInPager {
			if strings.Contains(page, unwanted) {
				t.Errorf("%q: has %q", c.query, unwanted)
			}
		}
	}
}

// A show's page has its details, Add or Remove, and its recordings; the Shows
// page links to it.
func TestWebShowPage(t *testing.T) {
	app, srv := testApp(t)
	ctx := t.Context()
	shows, _ := app.store.Shows(ctx)
	var sh store.Show
	for _, s := range shows {
		if s.Title == "Train To Skaville" {
			sh = s
		}
	}
	app.store.UpdateDetails(ctx, sh.ID, spinitron.ShowInfo{Title: sh.Title, URL: "https://spinitron.com/WCBN/show/264320/Train-To-Skaville", DJ: "brian tomsic", Category: "Specialty",
		Schedule: []string{"Every week on Tue at 7:00 PM"}, Description: "Ska, rocksteady and early reggae.", Image: "https://example.org/skaville.jpg"}, time.Now())
	at := time.Date(2026, 10, 6, 19, 0, 0, 0, app.cfg.Location)
	for i := range 12 {
		start := at.Add(time.Duration(-i) * 7 * 24 * time.Hour)
		if _, err := app.store.AddRecording(ctx, store.Recording{Airing: fmt.Sprint(i), Title: "Train To Skaville", Start: start, End: start.Add(time.Hour), StartedAt: start, Dir: "Train To Skaville"}); err != nil {
			t.Fatal(err)
		}
	}
	app.store.AddRecording(ctx, store.Recording{Airing: "other", Title: "Yazoo City Calling", Start: at, End: at, StartedAt: at, Dir: "Yazoo"})

	if page := get(t, srv.URL+"/shows"); !strings.Contains(page, fmt.Sprintf(`<a href="/shows/%d">Train To Skaville</a>`, sh.ID)) {
		t.Error("the Shows page doesn't link to the show's page")
	}
	page := get(t, fmt.Sprintf("%s/shows/%d", srv.URL, sh.ID))
	for _, want := range []string{
		`<main id="page" data-page="show"`, `<a href="/shows" aria-current="page">Shows</a>`, "<title>Train To Skaville · WCBN Recorder</title>",
		`<img class="art art-lg" src="https://example.org/skaville.jpg"`, "<h2>Train To Skaville</h2>", "with brian tomsic",
		fmt.Sprintf(`id="show-%d" data-title="Train To Skaville"`, sh.ID), `<button type="submit" class="toggle" aria-label="Record Train To Skaville">Add</button>`,
		`<input type="hidden" name="from" value="show">`, "<dt>Next airing</dt><dd>", "Every week on Tue at 7:00 PM", "Ska, rocksteady and early reggae.",
		`<a href="https://spinitron.com/WCBN/show/264320/Train-To-Skaville" target="_blank" rel="noopener">Show page</a>`,
		fmt.Sprintf(`<a href="/shows/%d?page=2#recorded" rel="next">Older →</a>`, sh.ID), "Page 1 of 2",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("show page missing %q", want)
		}
	}
	if n := strings.Count(mainOf(page), `class="rec-link"`); n != 10 || strings.Contains(page, "Yazoo City Calling</strong>") {
		t.Errorf("%d recordings listed, or another show's", n)
	}
	if n := strings.Count(mainOf(get(t, fmt.Sprintf("%s/shows/%d?page=2", srv.URL, sh.ID))), `class="rec-link"`); n != 2 {
		t.Errorf("%d recordings on page 2", n)
	}

	// Without JS, Add on the show's page comes back to it.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.PostForm(fmt.Sprintf("%s/shows/%d/record", srv.URL, sh.ID), url.Values{"record": {"1"}, "from": {"show"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Location") != fmt.Sprintf("/shows/%d", sh.ID) {
		t.Errorf("redirected to %q", resp.Header.Get("Location"))
	}
	if page := get(t, fmt.Sprintf("%s/shows/%d", srv.URL, sh.ID)); !strings.Contains(page, `class="card rec-detail show-detail selected"`) || !strings.Contains(page, ">Remove</button>") {
		t.Error("an added show's page should show it selected, with Remove")
	}
	for _, path := range []string{"/shows/999", "/shows/x"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %s", path, resp.Status)
		}
	}
}

func TestTimeRange(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 14, h, m, 0, 0, time.UTC) }
	for _, c := range []struct {
		start, end time.Time
		want       string
	}{
		{at(19, 0), at(20, 0), "7 – 8 PM"},
		{at(19, 30), at(21, 0), "7:30 – 9 PM"},
		{at(11, 0), at(13, 0), "11 AM – 1 PM"},
		{at(6, 0), at(7, 15), "6 – 7:15 AM"},
		{at(23, 30), at(1, 0), "11:30 PM – 1 AM"},
	} {
		if got := timeRange(c.start, c.end); got != c.want {
			t.Errorf("timeRange(%s, %s) = %q, want %q", c.start.Format("15:04"), c.end.Format("15:04"), got, c.want)
		}
	}
}

func TestAiredRange(t *testing.T) {
	start := time.Date(2026, 10, 13, 19, 0, 0, 0, time.UTC)
	if got := airedRange(start, start.Add(time.Hour), start.Add(3*time.Hour)); got != "7 – 8 PM" {
		t.Errorf("today: %q", got)
	}
	if got := airedRange(start, start.Add(90*time.Minute), start.Add(24*time.Hour)); got != "Tue Oct 13, 7 – 8:30 PM" {
		t.Errorf("another day: %q", got)
	}
}

func TestFileSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 bytes", 1: "1 byte", 999: "999 bytes", 2044: "2.0 KB", 99_950: "100.0 KB", 140_299_808: "140.3 MB"} {
		if got := fileSize(n); got != want {
			t.Errorf("fileSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// Both lists start by next airing, with those off the schedule last.
func TestNewShowsData(t *testing.T) {
	at := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	data := pageData{Shows: []showView{
		{Show: store.Show{Title: "A", Record: true}}, // off the schedule
		{Show: store.Show{Title: "B", Record: true}, OnCalendar: true, NextAiring: at.Add(time.Hour)},
		{Show: store.Show{Title: "C"}, OnCalendar: true, NextAiring: at},
		{Show: store.Show{Title: "D", Record: true}, OnCalendar: true, NextAiring: at},
		{Show: store.Show{Title: "E"}},
		{Show: store.Show{Title: "F"}, OnCalendar: true, NextAiring: at.Add(-time.Hour)},
	}}
	d := newShowsData(data)
	var mine, others []string
	for _, sh := range d.Mine {
		mine = append(mine, sh.Title)
	}
	for _, sh := range d.Others {
		others = append(others, sh.Title)
	}
	if fmt.Sprint(mine) != "[D B A]" || fmt.Sprint(others) != "[F C E]" {
		t.Errorf("mine %v, others %v", mine, others)
	}
}
