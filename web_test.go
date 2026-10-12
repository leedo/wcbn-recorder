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
	if !strings.Contains(page, "Train To Skaville") || !strings.Contains(page, "Nothing selected airs") {
		t.Fatalf("unexpected page:\n%s", page)
	}
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Error("show title was not escaped")
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
		Record   bool
		Selected int
		Upcoming string
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || !out.Record || out.Selected != 1 || !strings.Contains(out.Upcoming, "Train To Skaville") {
		t.Fatalf("toggle: %s %+v", resp.Status, out)
	}
	if sel, _ := app.store.Selected(context.Background()); len(sel) != 1 {
		t.Errorf("selected = %d", len(sel))
	}

	// No-JS path: a plain form post redirects back to the row.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = client.PostForm(srv.URL+"/shows/"+id+"/record", url.Values{"record": {"0"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/#show-"+id {
		t.Errorf("form post: %s %s", resp.Status, resp.Header.Get("Location"))
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
