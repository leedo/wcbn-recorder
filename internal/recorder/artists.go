package recorder

import (
	"context"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"wcbn-recorder/internal/musicbrainz"
	"wcbn-recorder/internal/spinitron"
	"wcbn-recorder/internal/store"
)

// Artists link to their Wikipedia pages, where MusicBrainz knows one (see
// package musicbrainz): the songs' artists on a recording's details page and
// in playback, and the song playing on the live card. Lookups are cached in
// the database (store.ArtistLinks), and in memory (artistCache) for the live
// card, which is drawn every second. They're made in the background, a
// request a second: the songs' artists each time the schedule is refreshed,
// so a new recording's are linked a little after its songs are listed, and
// the live card's as soon as it shows one not looked up yet.

// artistCache is what's known of artists' pages, by normalized name, loaded
// from the database on first use.
type artistCache struct {
	mu      sync.Mutex
	loaded  bool
	links   map[string]string    // the page of each artist found to have one
	checked map[string]bool      // artists needing no lookup (see store.ArtistsChecked)
	failed  map[string]time.Time // when a lookup last failed, to wait before trying again
}

// artistRetry is how long after a failed lookup the live card's artist is
// looked up again.
const artistRetry = 10 * time.Minute

// load fills the cache from the database, the first time.
func (c *artistCache) load(ctx context.Context, db *store.Store, now time.Time) error {
	if c.loaded {
		return nil
	}
	links, err := db.ArtistLinks(ctx)
	if err != nil {
		return err
	}
	checked, err := db.ArtistsChecked(ctx, now.Add(-artistRecheck))
	if err != nil {
		return err
	}
	c.links, c.checked, c.failed, c.loaded = links, checked, map[string]time.Time{}, true
	return nil
}

// credit splits an artist credit into names linked to their pages, as known
// so far.
func (a *App) credit(credit string) []artistPart {
	a.artists.mu.Lock()
	defer a.artists.mu.Unlock()
	return creditParts(credit, a.artists.links)
}

// wantArtists looks up the artists of credits shown on the live card that
// haven't been looked up, in the background.
func (a *App) wantArtists(ctx context.Context, credits ...string) {
	a.mu.Lock()
	runCtx := a.runCtx
	a.mu.Unlock()
	if runCtx == nil {
		return // not running: nothing to look them up with
	}
	now := time.Now()
	a.artists.mu.Lock()
	err := a.artists.load(ctx, a.store, now)
	want := false
	for _, credit := range credits {
		if credit == "" || err != nil {
			continue
		}
		for _, name := range append([]string{credit}, creditNames(credit)...) {
			key := spinitron.Normalize(name)
			if !a.artists.checked[key] && now.Sub(a.artists.failed[key]) > artistRetry {
				want = true
			}
		}
	}
	a.artists.mu.Unlock()
	if err != nil {
		log.Printf("loading artists: %v", err)
	}
	if want {
		a.checkArtists(runCtx, credits...)
	}
}

// creditNames is the names of the artists a credit names, split as
// creditParts does.
func creditNames(credit string) []string {
	var names []string
	for _, p := range creditParts(credit, nil) {
		if name := strings.TrimSpace(p.Text); p.name && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// artistRecheck is how long until an artist with no page is looked up again.
const artistRecheck = 30 * 24 * time.Hour

// creditSep splits a credit naming several artists: "roy shirley/glen adams",
// "x feat. y". Not "&" or "and", which are often in one act's name ("Justin
// Hinds & the Dominoes"), and whose halves can name some other act.
var creditSep = regexp.MustCompile(`(?i)\s*(?:/|\bfeat\.|\bft\.|\bfeaturing\b|\bwith\b|\bvs\.)\s*`)

// artistPart is part of a song's artist credit: an artist's name, linked to
// their page if it has one, or the text between names.
type artistPart struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
	name bool
}

// creditParts splits an artist credit into the artists' names and what's
// between them. A credit that's one name, or that links as a whole, is one
// part.
func creditParts(credit string, links map[string]string) []artistPart {
	if u := links[spinitron.Normalize(credit)]; u != "" {
		return []artistPart{{Text: credit, URL: u, name: true}}
	}
	var parts []artistPart
	last := 0
	for _, sep := range creditSep.FindAllStringIndex(credit, -1) {
		if name := credit[last:sep[0]]; name != "" {
			parts = append(parts, artistPart{Text: name, URL: links[spinitron.Normalize(name)], name: true})
		}
		parts = append(parts, artistPart{Text: credit[sep[0]:sep[1]]})
		last = sep[1]
	}
	if name := credit[last:]; name != "" {
		parts = append(parts, artistPart{Text: name, URL: links[spinitron.Normalize(name)], name: true})
	}
	return parts
}

// checkArtists looks up the artists not yet looked up, in the background,
// unless that's already running: the songs', and those of the credits given,
// first.
func (a *App) checkArtists(ctx context.Context, credits ...string) {
	if !a.artistsRunning.CompareAndSwap(false, true) {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer a.artistsRunning.Store(false)
		a.lookUpArtists(ctx, time.Now(), credits...)
	}()
}

// lookUpArtists finds the Wikipedia pages of the artists of first, then the
// songs', that haven't been looked up (or had none artistRecheck ago): each
// credit as a whole, then, if that has no page, each artist it names.
func (a *App) lookUpArtists(ctx context.Context, now time.Time, first ...string) {
	songs, err := a.store.Artists(ctx)
	if err != nil {
		log.Printf("loading artists: %v", err)
		return
	}
	var credits []string
	for _, c := range first {
		if c != "" {
			credits = append(credits, c)
		}
	}
	credits = append(credits, songs...)
	checked, err := a.store.ArtistsChecked(ctx, now.Add(-artistRecheck))
	if err != nil {
		log.Printf("loading artists: %v", err)
		return
	}
	links, err := a.store.ArtistLinks(ctx)
	if err != nil {
		log.Printf("loading artists: %v", err)
		return
	}
	found := 0
	lookUp := func(name string) bool {
		key := spinitron.Normalize(name)
		if checked[key] {
			return true
		}
		page, err := musicbrainz.WikipediaPage(ctx, name)
		if err == nil {
			err = a.store.SetArtistLink(ctx, name, page, now)
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("looking up %q on MusicBrainz: %v", name, err)
			}
			a.artists.mu.Lock()
			if a.artists.loaded {
				a.artists.failed[key] = time.Now()
			}
			a.artists.mu.Unlock()
			return false // try again next time
		}
		checked[key], links[key] = true, page
		a.artists.mu.Lock()
		if a.artists.loaded {
			a.artists.checked[key] = true
			if page != "" {
				a.artists.links[key] = page
			}
		}
		a.artists.mu.Unlock()
		if page != "" {
			found++
		}
		return true
	}
credits:
	for _, credit := range credits {
		// Stopping at the first failure: MusicBrainz is unreachable, or busy.
		// The rest are tried next time.
		if ctx.Err() != nil || !lookUp(credit) {
			break
		}
		if links[spinitron.Normalize(credit)] != "" {
			continue
		}
		for _, name := range creditNames(credit) {
			if !lookUp(name) {
				break credits
			}
		}
	}
	if found > 0 {
		log.Printf("found Wikipedia pages for %d artists", found)
		a.songsUpdated()
	}
}
