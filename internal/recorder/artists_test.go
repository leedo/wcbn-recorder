package recorder

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wcbn-recorder/internal/musicbrainz"
	"wcbn-recorder/internal/store"
)

func TestCreditParts(t *testing.T) {
	links := map[string]string{"roy shirley": "https://w/Roy", "glen adams": "https://w/Glen", "x feat. y": "https://w/XY"}
	for credit, want := range map[string]string{
		"Roy Shirley/Glen Adams":      "[{Roy Shirley https://w/Roy true} {/  false} {Glen Adams https://w/Glen true}]",
		"roy shirley / glen adams":    "[{roy shirley https://w/Roy true} { /   false} {glen adams https://w/Glen true}]",
		"justin hinds & the dominoes": "[{justin hinds & the dominoes  true}]", // & doesn't split
		"x feat. y":                   "[{x feat. y https://w/XY true}]",       // linked as a whole
		"a featuring b":               "[{a  true} { featuring   false} {b  true}]",
	} {
		if got := fmt.Sprint(creditParts(credit, links)); got != want {
			t.Errorf("creditParts(%q) = %s, want %s", credit, got, want)
		}
	}
}

// fakeMusicBrainz serves MusicBrainz and Wikidata for the artists given, by
// exact name: each one's Wikipedia page title, or "" for one with no page. It
// counts the searches it gets.
func fakeMusicBrainz(t *testing.T, artists map[string]string) *atomic.Int32 {
	t.Helper()
	var searches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/ws/2/artist/":
			searches.Add(1)
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Query().Get("query"), `artist:"`), `"`)
			if _, ok := artists[name]; ok {
				fmt.Fprintf(w, `{"artists":[{"id":%q,"name":%q,"score":100}]}`, name, name)
			} else {
				fmt.Fprint(w, `{"artists":[]}`)
			}
		case strings.HasPrefix(r.URL.Path, "/ws/2/artist/"):
			if title := artists[strings.TrimPrefix(r.URL.Path, "/ws/2/artist/")]; title != "" {
				fmt.Fprintf(w, `{"relations":[{"type":"wikidata","url":{"resource":"https://www.wikidata.org/wiki/%s"}}]}`, title)
			} else {
				fmt.Fprint(w, `{"relations":[]}`)
			}
		default: // Wikidata, by the page's title as its ID
			title := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/wiki/Special:EntityData/"), ".json")
			fmt.Fprintf(w, `{"entities":{"Q1":{"sitelinks":{"enwiki":{"title":%q}}}}}`, title)
		}
	}))
	t.Cleanup(srv.Close)
	oldMB, oldWD, oldInterval := musicbrainz.MusicBrainzURL, musicbrainz.WikidataURL, musicbrainz.Interval
	musicbrainz.MusicBrainzURL, musicbrainz.WikidataURL, musicbrainz.Interval = srv.URL+"/ws/2", srv.URL, 0
	t.Cleanup(func() {
		musicbrainz.MusicBrainzURL, musicbrainz.WikidataURL, musicbrainz.Interval = oldMB, oldWD, oldInterval
	})
	return &searches
}

// The songs' artists are looked up once, and linked to their Wikipedia pages
// on the recording's details page.
func TestArtistLinks(t *testing.T) {
	app, srv := testApp(t)
	ctx := t.Context()
	searches := fakeMusicBrainz(t, map[string]string{"third world": "Third World (band)", "roy shirley": "Roy Shirley", "glen adams": "Glen Adams", "federals": ""})
	at := time.Date(2026, 10, 6, 19, 0, 0, 0, app.cfg.Location)
	id, _ := app.store.AddRecording(ctx, store.Recording{Airing: "a", Title: "Train To Skaville", Start: at, End: at.Add(time.Hour), StartedAt: at, Dir: "Train To Skaville"})
	app.store.SetSongs(ctx, id, []store.Song{
		{Number: 1, Title: "journey to addis", Artist: "third world", PlayedAt: at},
		{Number: 2, Title: "rocksteady train", Artist: "roy shirley/glen adams", PlayedAt: at},
		{Number: 3, Title: "wailing festival", Artist: "federals", PlayedAt: at},
		{Number: 4, Title: "over the river", Artist: "justin hinds & the dominoes", PlayedAt: at},
	}, "")

	now := time.Now()
	app.lookUpArtists(ctx, now)
	// Each credit, and the two names in "roy shirley/glen adams".
	if n := searches.Load(); n != 6 {
		t.Errorf("%d searches, want 6", n)
	}
	page := get(t, fmt.Sprintf("%s/recordings/%d", srv.URL, id))
	for _, want := range []string{
		`<td><a href="https://en.wikipedia.org/wiki/Third_World_%28band%29" target="_blank" rel="noopener" title="third world on Wikipedia">third world</a></td>`,
		`<td><a href="https://en.wikipedia.org/wiki/Roy_Shirley" target="_blank" rel="noopener" title="roy shirley on Wikipedia">roy shirley</a>/<a href="https://en.wikipedia.org/wiki/Glen_Adams" target="_blank" rel="noopener" title="glen adams on Wikipedia">glen adams</a></td>`,
		`<td>federals</td>`, `<td>justin hinds &amp; the dominoes</td>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("details page missing %q", want)
		}
	}

	// Looked up once; those with no page, again after a while.
	app.lookUpArtists(ctx, now.Add(time.Hour))
	if n := searches.Load(); n != 6 {
		t.Errorf("%d searches after looking again, want 6", n)
	}
	app.lookUpArtists(ctx, now.Add(artistRecheck+time.Hour))
	if n := searches.Load(); n != 9 { // the three credits with no page of their own
		t.Errorf("%d searches after %s, want 9", n, artistRecheck)
	}
}

// The live card's artist is looked up as soon as it's shown, and linked once
// it's found.
func TestLiveCardArtistLink(t *testing.T) {
	app, srv := testApp(t)
	searches := fakeMusicBrainz(t, map[string]string{"Toots & the Maytals": "Toots and the Maytals"})
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); app.wg.Wait() }()
	app.mu.Lock()
	app.runCtx = ctx // as Run sets it
	app.mu.Unlock()

	f := newFakeRecording(t, testConfig())
	f.job.Event.Start, f.job.Event.End = time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	f.title("Pressure Drop by Toots & the Maytals on Train To Skaville with brian tomsic")
	f.frames(1, 3)
	f.Write(nil)
	app.mu.Lock()
	app.active[f.job.Event.Key()] = f.recording
	app.mu.Unlock()

	get(t, srv.URL+"/recordings") // shows the artist, and starts its lookup
	for deadline := time.Now().Add(5 * time.Second); app.artistsRunning.Load() || searches.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the artist wasn't looked up")
		}
	}
	want := `<span class="artist"><a href="https://en.wikipedia.org/wiki/Toots_and_the_Maytals" target="_blank" rel="noopener" title="Toots &amp; the Maytals on Wikipedia">Toots &amp; the Maytals</a></span>`
	if page := get(t, srv.URL+"/recordings"); !strings.Contains(page, want) {
		t.Errorf("live card missing %q", want)
	}
	// Looked up once.
	get(t, srv.URL+"/recordings")
	if n := searches.Load(); n != 1 {
		t.Errorf("%d searches", n)
	}
}
