package spinitron

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"wcbn-recorder/internal/httpx"

	"golang.org/x/net/html"
)

// Playlist is the songs a DJ logged in Spinitron for one slot of an airing
// (a long show has a playlist per hourly slot).
type Playlist struct {
	URL        string
	Start, End time.Time // the slot
	Spins      []Spin    // in air order, however the page lists them
}

// Spin is a song in a playlist.
type Spin struct {
	Artist string
	Song   string
	Album  string // the release, if the DJ logged one
	Label  string // its record label, likewise
	Year   int    // the year it was released; 0 if not logged
	Minute int    // when it was logged, in minutes past midnight; -1 if the page didn't say
}

// FetchPlaylists finds the playlists of the airing of title between start
// and end on the calendar, and reads their songs. An airing whose playlist
// isn't on the calendar yet has none.
func FetchPlaylists(ctx context.Context, scheduleURL string, loc *time.Location, title string, start, end time.Time) ([]Playlist, error) {
	data, feed, err := fetchFeed(ctx, scheduleURL, loc, start.Add(-24*time.Hour), end.Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	pls, err := playlistsInFeed(data, feed, title, start, end)
	if err != nil {
		return nil, err
	}
	for i := range pls {
		page, err := httpx.Get(ctx, pls[i].URL)
		if err != nil {
			return nil, err
		}
		if pls[i].Spins, err = parsePlaylistPage(page); err != nil {
			return nil, fmt.Errorf("%s: %w", pls[i].URL, err)
		}
	}
	return pls, nil
}

// playlistsInFeed picks the playlists ("spin-cal-pl") of title that overlap
// start to end out of the calendar feed, in order.
func playlistsInFeed(data []byte, base *url.URL, title string, start, end time.Time) ([]Playlist, error) {
	var raw []feedEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing calendar feed: %w", err)
	}
	var pls []Playlist
	for _, r := range raw {
		if !strings.Contains(" "+r.ClassName+" ", " spin-cal-pl ") || Normalize(html.UnescapeString(r.Title)) != Normalize(title) {
			continue
		}
		s, err1 := time.Parse("2006-01-02T15:04:05-0700", r.Start)
		e, err2 := time.Parse("2006-01-02T15:04:05-0700", r.End)
		if err1 != nil || err2 != nil || !s.Before(end) || !e.After(start) {
			continue
		}
		u, err := base.Parse(r.URL)
		if err != nil || r.URL == "" {
			continue
		}
		pls = append(pls, Playlist{URL: u.String(), Start: s, End: e})
	}
	sort.Slice(pls, func(i, j int) bool { return pls[i].Start.Before(pls[j].Start) })
	return pls, nil
}

// parsePlaylistPage reads the spins from a playlist page.
func parsePlaylistPage(data []byte) ([]Spin, error) {
	doc, err := html.Parse(strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	var spins []Spin
	for tr := range doc.Descendants() {
		if tr.Data != "tr" || !hasClass(tr, "spin-item") {
			continue
		}
		sp := Spin{Minute: -1}
		if n := findFirst(tr, func(n *html.Node) bool { return hasClass(n, "artist") }); n != nil {
			sp.Artist = textOf(n)
		}
		if n := findFirst(tr, func(n *html.Node) bool { return hasClass(n, "song") }); n != nil {
			sp.Song = textOf(n)
		}
		// The release, its label and year, in the row's "info" line.
		if n := findFirst(tr, func(n *html.Node) bool { return hasClass(n, "release") }); n != nil {
			sp.Album = textOf(n)
		}
		if n := findFirst(tr, func(n *html.Node) bool { return hasClass(n, "label") }); n != nil {
			sp.Label = textOf(n)
		}
		if n := findFirst(tr, func(n *html.Node) bool { return hasClass(n, "released") }); n != nil {
			sp.Year, _ = strconv.Atoi(textOf(n))
		}
		if sp.Artist == "" && sp.Song == "" {
			continue
		}
		if n := findFirst(tr, func(n *html.Node) bool { return hasClass(n, "spin-time") }); n != nil {
			if t, err := time.Parse("3:04 PM", textOf(n)); err == nil {
				sp.Minute = t.Hour()*60 + t.Minute()
			}
		}
		spins = append(spins, sp)
	}
	return airOrder(spins), nil
}

// airOrder puts spins in the order they aired. A finished playlist's page
// lists them oldest first, but one still on the air lists the newest first.
// The order is whichever has them spanning less time, going forward from
// one spin to the next (and past midnight, if need be).
func airOrder(spins []Spin) []Spin {
	span := func(spins []Spin) int {
		total, last := 0, -1
		for _, sp := range spins {
			if sp.Minute < 0 {
				continue
			}
			if last >= 0 {
				total += (sp.Minute - last + 24*60) % (24 * 60)
			}
			last = sp.Minute
		}
		return total
	}
	reversed := slices.Clone(spins)
	slices.Reverse(reversed)
	if span(reversed) < span(spins) {
		return reversed
	}
	return spins
}

// earlyLogging is how far before its slot a playlist's first spin can be and
// still be taken at face value: DJs sometimes log the first song early.
const earlyLogging = 15 * time.Minute

// Times places the spins in time. Spinitron only gives the minute, and the
// times can be off by hours (a DJ's playlist logged with the wrong clock),
// but the gaps between them are right. So when the first spin falls in the
// slot, the times are used as they are; otherwise they're shifted to put the
// first spin at the start of the slot.
func (p Playlist) Times(loc *time.Location) []time.Time {
	if len(p.Spins) == 0 {
		return nil
	}
	// Minutes after the first spin, carrying on past midnight.
	offsets := make([]int, len(p.Spins))
	last := p.Spins[0].Minute
	for i := 1; i < len(p.Spins); i++ {
		offsets[i] = offsets[i-1]
		if m := p.Spins[i].Minute; m >= 0 && last >= 0 {
			offsets[i] += (m - last + 24*60) % (24 * 60)
		}
		if m := p.Spins[i].Minute; m >= 0 {
			last = m
		}
	}
	anchor := p.Start
	if m := p.Spins[0].Minute; m >= 0 {
		// The first spin's time on whichever day puts it nearest the slot.
		s := p.Start.In(loc)
		var best time.Time
		for d := -1; d <= 1; d++ {
			t := time.Date(s.Year(), s.Month(), s.Day()+d, m/60, m%60, 0, 0, loc)
			if best.IsZero() || t.Sub(p.Start).Abs() < best.Sub(p.Start).Abs() {
				best = t
			}
		}
		if !best.Before(p.Start.Add(-earlyLogging)) && best.Before(p.End) {
			anchor = best
		}
	}
	times := make([]time.Time, len(p.Spins))
	for i, off := range offsets {
		times[i] = anchor.Add(time.Duration(off) * time.Minute)
	}
	return times
}
