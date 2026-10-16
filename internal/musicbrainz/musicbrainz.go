// Package musicbrainz finds an artist's English Wikipedia page: it searches
// MusicBrainz for the artist by name, follows the artist's link to Wikidata,
// and takes the page Wikidata links to. MusicBrainz only lists musicians, and
// Wikidata names the exact page ("Third World (band)"), so this rarely links
// the wrong page, unlike searching Wikipedia for the name.
package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"wcbn-recorder/internal/httpx"
)

// The services' addresses; tests point them elsewhere.
var (
	MusicBrainzURL = "https://musicbrainz.org/ws/2"
	WikidataURL    = "https://www.wikidata.org"
)

// minScore is the lowest MusicBrainz search score taken as a match. An exact
// name scores 100.
const minScore = 95

// Interval is the least time between requests: MusicBrainz allows one a
// second. Tests set it to 0.
var Interval = time.Second

var (
	pace     sync.Mutex
	lastCall time.Time
)

// sameName reports whether two artist names are the same but for case,
// spacing and a leading "The" ("tennors" is The Tennors).
func sameName(a, b string) bool {
	norm := func(s string) string {
		return strings.TrimPrefix(strings.ToLower(strings.Join(strings.Fields(s), " ")), "the ")
	}
	return norm(a) == norm(b)
}

// get requests u and decodes its JSON into v, a request a second at most.
// MusicBrainz answers 503 when it's busy or the rate is exceeded; those are
// retried, waiting longer each time.
func get(ctx context.Context, u string, v any) error {
	for attempt := 0; ; attempt++ {
		pace.Lock()
		if wait := Interval - time.Since(lastCall); wait > 0 {
			time.Sleep(wait)
		}
		lastCall = time.Now()
		pace.Unlock()
		err := fetch(ctx, u, v)
		var busy errBusy
		if !errors.As(err, &busy) || attempt == len(Backoff) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(Backoff[attempt]):
		}
	}
}

// Backoff is how long to wait before each retry of a busy response. Tests
// shorten it.
var Backoff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

type errBusy struct{ status string }

func (e errBusy) Error() string { return "busy: " + e.status }

func fetch(ctx context.Context, u string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "wcbn-recorder/1.0")
	req.Header.Set("Accept", "application/json")
	resp, err := httpx.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
	case http.StatusServiceUnavailable, http.StatusTooManyRequests:
		return errBusy{resp.Status}
	default:
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
}

// WikipediaPage returns the URL of artist's English Wikipedia page, or "" if
// MusicBrainz doesn't know the artist or it has no page.
func WikipediaPage(ctx context.Context, artist string) (string, error) {
	var search struct {
		Artists []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Score int    `json:"score"`
		} `json:"artists"`
	}
	q := url.Values{"query": {`artist:"` + strings.ReplaceAll(artist, `"`, ``) + `"`}, "fmt": {"json"}, "limit": {"1"}}
	if err := get(ctx, MusicBrainzURL+"/artist/?"+q.Encode(), &search); err != nil {
		return "", err
	}
	// The artist's own name has to match, not just one of its aliases: "fay"
	// is an alias of Bill Fay.
	if len(search.Artists) == 0 || search.Artists[0].Score < minScore || !sameName(search.Artists[0].Name, artist) {
		return "", nil
	}
	var rels struct {
		Relations []struct {
			Type string `json:"type"`
			URL  struct {
				Resource string `json:"resource"`
			} `json:"url"`
		} `json:"relations"`
	}
	if err := get(ctx, MusicBrainzURL+"/artist/"+url.PathEscape(search.Artists[0].ID)+"?inc=url-rels&fmt=json", &rels); err != nil {
		return "", err
	}
	var qid string
	for _, r := range rels.Relations {
		switch {
		case r.Type == "wikipedia" && strings.HasPrefix(r.URL.Resource, "https://en.wikipedia.org/wiki/"):
			return r.URL.Resource, nil // an older, direct link
		case r.Type == "wikidata":
			qid = r.URL.Resource[strings.LastIndex(r.URL.Resource, "/")+1:]
		}
	}
	if qid == "" {
		return "", nil
	}
	var entity struct {
		Entities map[string]struct {
			Sitelinks map[string]struct {
				Title string `json:"title"`
			} `json:"sitelinks"`
		} `json:"entities"`
	}
	if err := get(ctx, WikidataURL+"/wiki/Special:EntityData/"+url.PathEscape(qid)+".json", &entity); err != nil {
		return "", err
	}
	for _, e := range entity.Entities { // keyed by the ID it redirects to, if it does
		if title := e.Sitelinks["enwiki"].Title; title != "" {
			page := url.URL{Scheme: "https", Host: "en.wikipedia.org", Path: "/wiki/" + strings.ReplaceAll(title, " ", "_")}
			return page.String(), nil
		}
	}
	return "", nil
}
