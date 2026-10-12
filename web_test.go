package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	loc, _ := time.LoadLocation("America/Detroit")
	cfg := &Config{location: loc, PostRoll: Duration{2 * time.Minute}}
	app := newApp(cfg, store)

	start := time.Now().Add(time.Hour).Truncate(time.Hour)
	app.events = []Event{
		{Title: "Train To Skaville", DJ: "brian tomsic", Category: "Specialty", URL: "https://spinitron.com/WCBN/show/1/x", Start: start, End: start.Add(time.Hour)},
		{Title: "<script>alert(1)</script>", Category: "Freeform", URL: "https://spinitron.com/WCBN/show/2/y", Start: start.Add(time.Hour), End: start.Add(2 * time.Hour)},
	}
	app.lastRefresh = time.Now()
	if err := store.UpsertEvents(ctx, app.events, time.Now()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.handler())
	t.Cleanup(srv.Close)
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
	page := get(t, srv.URL+"/")
	if !strings.Contains(page, "Train To Skaville") || !strings.Contains(page, "None of your shows air") ||
		!strings.Contains(page, "not recording any shows yet") {
		t.Fatalf("unexpected page:\n%s", page)
	}
	// With nothing selected, the full list opens on its own.
	if !strings.Contains(page, `<section id="browse" >`) || !strings.Contains(page, `aria-expanded="true"`) {
		t.Error("full list should be open when nothing is selected")
	}
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Error("show title was not escaped")
	}
	if !strings.Contains(page, `<td class="title" data-sort="train to skaville">`) || !strings.Contains(page, `class="sort" data-col="5" data-type="number"`) {
		t.Error("missing sort attributes")
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
		Selected int
		Upcoming string
		Mine     string
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || !out.Record || out.Selected != 1 || strconv.FormatInt(out.ID, 10) != id ||
		!strings.Contains(out.Upcoming, "Train To Skaville") || !strings.Contains(out.Mine, `id="mine-`+id+`"`) {
		t.Fatalf("toggle: %s %+v", resp.Status, out)
	}

	// With a show selected, the main page lists it and hides the full list
	// until "Add shows" (?add=1) is used.
	page = get(t, srv.URL+"/")
	if !strings.Contains(page, `<li id="mine-`+id+`">`) || !strings.Contains(page, `<section id="browse" hidden>`) ||
		!strings.Contains(page, `href="/?add=1"`) {
		t.Error("main page should list the selection and hide the full list")
	}
	if page := get(t, srv.URL+"/?add=1"); !strings.Contains(page, `<section id="browse" >`) || !strings.Contains(page, ">Done<") {
		t.Error("?add=1 should open the full list")
	}
	if sel, _ := app.store.Selected(context.Background()); len(sel) != 1 {
		t.Errorf("selected = %d", len(sel))
	}

	// No-JS path: a plain form post redirects back to where it came from.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, tc := range []struct{ from, record, want string }{
		{"add", "1", "/?add=1#show-" + id},
		{"mine", "0", "/#your-shows"},
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

func TestScheduleSortKey(t *testing.T) {
	const day = 24 * 60
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"Every week on Mon at 7:00 PM", 0*day + 19*60},
		{"Every  week on Tue at 11:00 AM", 1*day + 11*60},
		{"Every week on Sun at 12:00 AM", 6 * day},
		{"Every week on Sat at 12:30 PM", 5*day + 12*60 + 30},
		{"Every week on Mon, Tue, Wed, Thu at 6:00 PM", 18 * 60},
		{"Every week on Sun at 1:00 AM; Every week on Mon at 4:00 AM", 4 * 60}, // earliest slot wins
		{"", -1},
		{"Every other week", -1},
	} {
		if got := scheduleSortKey(tc.in); got != tc.want {
			t.Errorf("scheduleSortKey(%q) = %d, want %d", tc.in, got, tc.want)
		}
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
		app.store.UpdateDetails(ctx, sh.ID, ShowInfo{Image: img}, time.Now())
		app.store.SetRecord(ctx, sh.ID, true)
	}
	app.store.SetSetting(ctx, "station_logo", "https://spinitron.com/images/Station/22/2263-img_logo.225x225.jpg")

	page := get(t, srv.URL+"/?add=1")
	want := `<img class="art art-md" src="https://farm6.staticflickr.com/5017/5476585780_087d4791ae_m.jpg" alt="" data-fallback="https://spinitron.com/images/Station/22/2263-img_logo.225x225.jpg" loading="lazy"`
	if !strings.Contains(page, want) {
		t.Errorf("Your shows artwork missing:\n%s", want)
	}
	if !strings.Contains(page, `class="art art-sm" src="https://farm6.staticflickr.com/5017/5476585780_087d4791ae_m.jpg"`) {
		t.Error("full list artwork missing")
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
