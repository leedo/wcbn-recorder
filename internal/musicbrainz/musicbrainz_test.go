package musicbrainz

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// FakeServer serves MusicBrainz and Wikidata for the artists given, by exact
// name: each one's Wikipedia page title, or "" for an artist MusicBrainz knows
// with no page. Others aren't found. It points the package at itself.
func fakeServer(t *testing.T, artists map[string]string) {
	t.Helper()
	ids := map[string]string{}
	for name := range artists {
		ids[name] = fmt.Sprintf("id%d", len(ids)+1)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/ws/2/artist/":
			for name, id := range ids {
				if r.URL.Query().Get("query") == `artist:"`+name+`"` {
					fmt.Fprintf(w, `{"artists":[{"id":%q,"name":%q,"score":100}]}`, id, name)
					return
				}
			}
			if r.URL.Query().Get("query") == `artist:"fay"` {
				fmt.Fprint(w, `{"artists":[{"id":"id1","name":"Bill Fay","score":100}]}`) // by an alias
				return
			}
			fmt.Fprint(w, `{"artists":[{"id":"nope","name":"Nope","score":60}]}`) // a poor match
		case strings.HasPrefix(r.URL.Path, "/ws/2/artist/"):
			id := strings.TrimPrefix(r.URL.Path, "/ws/2/artist/")
			for name, i := range ids {
				if i == id && artists[name] != "" {
					fmt.Fprintf(w, `{"relations":[{"type":"wikidata","url":{"resource":"https://www.wikidata.org/wiki/Q%s"}}]}`, id)
					return
				}
			}
			fmt.Fprint(w, `{"relations":[]}`)
		default:
			for name, id := range ids {
				if r.URL.Path == "/wiki/Special:EntityData/Q"+id+".json" {
					fmt.Fprintf(w, `{"entities":{"Q%s":{"sitelinks":{"enwiki":{"title":%q}}}}}`, id, artists[name])
					return
				}
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldMB, oldWD, oldInterval := MusicBrainzURL, WikidataURL, Interval
	MusicBrainzURL, WikidataURL, Interval = srv.URL+"/ws/2", srv.URL, 0
	t.Cleanup(func() { MusicBrainzURL, WikidataURL, Interval = oldMB, oldWD, oldInterval })
}

func TestSameName(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{{"The Tennors", "tennors", true}, {"Third  World", "third world", true}, {"Bill Fay", "fay", false}} {
		if sameName(c.a, c.b) != c.same {
			t.Errorf("sameName(%q, %q) = %v", c.a, c.b, !c.same)
		}
	}
}

func TestWikipediaPage(t *testing.T) {
	fakeServer(t, map[string]string{"third world": "Third World (band)", "federals": ""})
	for artist, want := range map[string]string{
		"third world": "https://en.wikipedia.org/wiki/Third_World_%28band%29",
		"federals":    "", // known to MusicBrainz, with no page
		"nobody":      "", // not known
		"fay":         "", // only an alias of Bill Fay
	} {
		got, err := WikipediaPage(t.Context(), artist)
		if err != nil || got != want {
			t.Errorf("WikipediaPage(%q) = %q, %v; want %q", artist, got, err, want)
		}
	}
}

// A busy response is retried.
func TestBusy(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls++; calls < 3 {
			http.Error(w, "slow down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"artists":[]}`)
	}))
	defer srv.Close()
	oldMB, oldInterval, oldBackoff := MusicBrainzURL, Interval, Backoff
	MusicBrainzURL, Interval, Backoff = srv.URL+"/ws/2", 0, []time.Duration{0, 0}
	defer func() { MusicBrainzURL, Interval, Backoff = oldMB, oldInterval, oldBackoff }()
	if page, err := WikipediaPage(t.Context(), "x"); err != nil || page != "" || calls != 3 {
		t.Errorf("page %q, err %v, after %d calls", page, err, calls)
	}
	calls = -10 // busy for good
	if _, err := WikipediaPage(t.Context(), "x"); err == nil {
		t.Error("no error when it stays busy")
	}
}
