// Package spinitron reads WCBN's program schedule and show pages from
// Spinitron: the calendar's event feed, and each show's description,
// schedule, DJ and artwork.
package spinitron

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"wcbn-recorder/internal/httpx"

	"golang.org/x/net/html"
)

// Event is a single scheduled occurrence of a show on the Spinitron calendar.
type Event struct {
	ID       int
	Title    string
	DJ       string
	Category string
	URL      string // absolute URL of the show (or playlist) page
	Start    time.Time
	End      time.Time
}

func (e Event) Key() string { return fmt.Sprintf("%s|%d", Normalize(e.Title), e.Start.Unix()) }

// ShowInfo is the detail shown on a Spinitron show page.
type ShowInfo struct {
	Title        string
	URL          string
	DJ           string
	Category     string
	Schedule     []string // e.g. "Every week on Mon at 7:00 PM"
	Description  string
	Image        string // the show's artwork URL
	StationImage string // the station logo, a fallback when Image is gone
}

// The calendar page is a fullcalendar widget configured in an inline script,
// e.g. jQuery('#calendar-w0').calendar({"events":"/WCBN/calendar-feed?timeslot=30",...});
var calendarConfigRe = regexp.MustCompile(`\.calendar\((\{.*?\})\);`)

// FetchSchedule loads the HTML calendar page, finds the event source the
// calendar uses, and requests the events between from and to.
func FetchSchedule(ctx context.Context, scheduleURL string, loc *time.Location, from, to time.Time) ([]Event, error) {
	page, err := httpx.Get(ctx, scheduleURL)
	if err != nil {
		return nil, err
	}
	feed, err := calendarFeedURL(scheduleURL, page)
	if err != nil {
		return nil, err
	}
	q := feed.Query()
	q.Set("start", from.In(loc).Format("2006-01-02"))
	q.Set("end", to.In(loc).Format("2006-01-02"))
	q.Set("timeZone", loc.String())
	feed.RawQuery = q.Encode()

	data, err := httpx.Get(ctx, feed.String())
	if err != nil {
		return nil, err
	}
	return parseCalendarFeed(data, feed)
}

func calendarFeedURL(pageURL string, page []byte) (*url.URL, error) {
	m := calendarConfigRe.FindSubmatch(page)
	if m == nil {
		return nil, fmt.Errorf("no calendar found on %s", pageURL)
	}
	var conf struct {
		Events string `json:"events"`
	}
	if err := json.Unmarshal(m[1], &conf); err != nil || conf.Events == "" {
		return nil, fmt.Errorf("unrecognized calendar config on %s", pageURL)
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}
	return base.Parse(conf.Events)
}

func parseCalendarFeed(data []byte, base *url.URL) ([]Event, error) {
	var raw []struct {
		ID        int    `json:"id"`
		Title     string `json:"title"`
		Text      string `json:"text"`
		Start     string `json:"start"`
		End       string `json:"end"`
		URL       string `json:"url"`
		ClassName string `json:"className"`
		Data      struct {
			Category string `json:"category"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing calendar feed: %w", err)
	}
	// The feed lists past/current episodes as playlists ("spin-cal-pl") and
	// scheduled ones as shows ("spin-cal-show"); a slot can appear as both.
	byKey := map[string]int{}
	var events []Event
	for _, r := range raw {
		start, err1 := time.Parse("2006-01-02T15:04:05-0700", r.Start)
		end, err2 := time.Parse("2006-01-02T15:04:05-0700", r.End)
		if err1 != nil || err2 != nil || !end.After(start) || r.Title == "" {
			continue
		}
		u := r.URL
		if ref, err := base.Parse(r.URL); err == nil {
			u = ref.String()
		}
		ev := Event{
			ID:       r.ID,
			Title:    html.UnescapeString(strings.TrimSpace(r.Title)),
			DJ:       html.UnescapeString(strings.TrimSpace(r.Text)),
			Category: r.Data.Category,
			URL:      u,
			Start:    start,
			End:      end,
		}
		if i, ok := byKey[ev.Key()]; ok {
			if strings.Contains(r.ClassName, "spin-cal-show") {
				events[i] = ev
			}
			continue
		}
		byKey[ev.Key()] = len(events)
		events = append(events, ev)
	}
	return mergeAdjacent(events), nil
}

// mergeAdjacent joins back-to-back slots of the same show (Spinitron lists
// long shows as consecutive one-hour slots) into a single event.
func mergeAdjacent(events []Event) []Event {
	sort.Slice(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
	var out []Event
	last := map[string]int{} // normalized title -> index in out
	for _, ev := range events {
		k := Normalize(ev.Title)
		if i, ok := last[k]; ok && !ev.Start.After(out[i].End) {
			if ev.End.After(out[i].End) {
				out[i].End = ev.End
			}
			continue
		}
		last[k] = len(out)
		out = append(out, ev)
	}
	return out
}

// FetchShowInfo reads the show header from a Spinitron show or playlist page.
func FetchShowInfo(ctx context.Context, pageURL string) (ShowInfo, error) {
	data, err := httpx.Get(ctx, pageURL)
	if err != nil {
		return ShowInfo{}, err
	}
	return parseShowPage(data, pageURL)
}

func parseShowPage(data []byte, pageURL string) (ShowInfo, error) {
	doc, err := html.Parse(strings.NewReader(string(data)))
	if err != nil {
		return ShowInfo{}, err
	}
	info := ShowInfo{URL: pageURL}
	// Pages also have a "head station" block, so pick the one with the show title.
	head := findFirst(doc, func(n *html.Node) bool {
		return hasClass(n, "head") && findFirst(n, func(c *html.Node) bool { return hasClass(c, "show-title") }) != nil
	})
	if head == nil {
		return info, fmt.Errorf("no show header on %s", pageURL)
	}
	if n := findFirst(head, func(n *html.Node) bool { return hasClass(n, "show-title") }); n != nil {
		info.Title = textOf(n)
		// Playlist pages link the title to the show page.
		if a := findFirst(n, func(n *html.Node) bool { return n.Data == "a" }); a != nil {
			if base, err := url.Parse(pageURL); err == nil {
				if ref, err := base.Parse(attr(a, "href")); err == nil {
					info.URL = ref.String()
				}
			}
		}
	}
	if n := findFirst(head, func(n *html.Node) bool { return hasClass(n, "dj-name") }); n != nil {
		info.DJ = strings.TrimSpace(strings.TrimPrefix(textOf(n), "With "))
	}
	if n := findFirst(head, func(n *html.Node) bool { return hasClass(n, "show-categoty") || hasClass(n, "show-category") }); n != nil {
		info.Category = textOf(n)
	}
	if n := findFirst(head, func(n *html.Node) bool { return hasClass(n, "show-schedule") }); n != nil {
		for li := range n.Descendants() {
			if li.Type == html.ElementNode && li.Data == "li" {
				if s := textOf(li); s != "" {
					info.Schedule = append(info.Schedule, s)
				}
			}
		}
	}
	if n := findFirst(head, func(n *html.Node) bool { return hasClass(n, "description") }); n != nil {
		info.Description = blockText(n)
	}
	// The show's image is in og:image; the station header has the logo.
	resolve := func(ref string) string {
		base, err := url.Parse(pageURL)
		if ref == "" || err != nil {
			return ""
		}
		u, err := base.Parse(ref)
		if err != nil {
			return ""
		}
		return u.String()
	}
	if n := findFirst(doc, func(n *html.Node) bool { return n.Data == "meta" && attr(n, "property") == "og:image" }); n != nil {
		info.Image = resolve(attr(n, "content"))
	}
	if station := findFirst(doc, func(n *html.Node) bool { return hasClass(n, "head") && hasClass(n, "station") }); station != nil {
		if img := findFirst(station, func(n *html.Node) bool { return n.Data == "img" }); img != nil {
			info.StationImage = resolve(attr(img, "src"))
		}
	}
	return info, nil
}

func findFirst(n *html.Node, match func(*html.Node) bool) *html.Node {
	for d := range n.Descendants() {
		if d.Type == html.ElementNode && match(d) {
			return d
		}
	}
	return nil
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, class string) bool {
	return n.Type == html.ElementNode && strings.Contains(" "+attr(n, "class")+" ", " "+class+" ")
}

// textOf returns the node's text with whitespace collapsed.
func textOf(n *html.Node) string {
	var b strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			b.WriteString(d.Data)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// blockText is like textOf but keeps paragraph and line breaks.
func blockText(n *html.Node) string {
	var paras []string
	var cur strings.Builder
	flush := func() {
		if s := strings.Join(strings.Fields(cur.String()), " "); s != "" {
			paras = append(paras, s)
		}
		cur.Reset()
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			cur.WriteString(n.Data)
		case n.Type == html.ElementNode && n.Data == "br":
			flush()
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && (n.Data == "p" || n.Data == "div" || n.Data == "li") {
			flush()
		}
	}
	walk(n)
	flush()
	return strings.Join(paras, "\n\n")
}

// FetchShowInfos fetches show pages with a small amount of concurrency.
func FetchShowInfos(ctx context.Context, urls []string) map[string]ShowInfo {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		out   = map[string]ShowInfo{}
		limit = make(chan struct{}, 4)
	)
	for _, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			info, err := FetchShowInfo(ctx, u)
			if err != nil {
				log.Printf("skipping %s: %v", u, err)
				return
			}
			mu.Lock()
			out[u] = info
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// Normalize folds a show title for matching: lowercase, single spaces.
func Normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
