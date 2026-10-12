package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

type jobView struct {
	ShowID     int64
	Title      string
	DJ         string
	Start, End time.Time
}

type showView struct {
	Show
	NextAiring   time.Time
	OnCalendar   bool
	RecordingNow bool
	ScheduleKey  int // sort position of the show's first weekly slot; -1 if unknown
}

type pageData struct {
	LastRefresh time.Time
	RefreshErr  string
	Recording   []liveStatus
	Upcoming    []jobView
	Shows       []showView // every known show, for the "Add shows" list
	Mine        []showView // shows selected for recording, soonest first
	Categories  []string
	Selected    int
	StationLogo string // fallback artwork
	Browse      bool   // show the full list (search + table) on load
}

func (a *App) handler() http.Handler {
	tmpl := template.Must(template.New("").Funcs(template.FuncMap{
		"when":  func(t time.Time) string { return t.In(a.cfg.location).Format("Mon Jan 2, 3:04 PM") },
		"clock": func(t time.Time) string { return t.In(a.cfg.location).Format("3:04 PM") },
		"ago":   ago,
		"lower": strings.ToLower,
		"art":   artImage,
		"dur":   formatDuration,
		"mb":    func(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/1e6) },
	}).ParseFS(templateFS, "templates/*.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		data, err := a.pageData(r.Context())
		if err != nil {
			httpError(w, err)
			return
		}
		// The full list opens via the "Add shows" button (?add=1), and on its own
		// when nothing is selected yet, since there's nothing else to show.
		data.Browse = r.URL.Query().Get("add") == "1" || data.Selected == 0
		render(w, tmpl, "index.html", data)
	})
	// The page polls this to keep the live recording panel current.
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
		data, err := a.pageData(r.Context())
		if err != nil {
			httpError(w, err)
			return
		}
		var live, upcoming bytes.Buffer
		if err := tmpl.ExecuteTemplate(&live, "live", data); err != nil {
			httpError(w, err)
			return
		}
		if err := tmpl.ExecuteTemplate(&upcoming, "upcoming", data); err != nil {
			httpError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"live": live.String(), "upcoming": upcoming.String(), "recording": len(data.Recording)})
	})
	// Listen to a recording in progress, from just behind the live edge.
	mux.HandleFunc("GET /listen/{id}", func(w http.ResponseWriter, r *http.Request) {
		rec := a.activeRecording(r.PathValue("id"))
		if rec == nil {
			http.Error(w, "nothing is being recorded with that id", http.StatusNotFound)
			return
		}
		rec.serveListen(w, r)
	})
	// End a recording early; what's been recorded is saved, and the show stays
	// selected for future airings.
	mux.HandleFunc("POST /recordings/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		rec := a.activeRecording(r.PathValue("id"))
		if rec == nil {
			http.Error(w, "nothing is being recorded with that id", http.StatusNotFound)
			return
		}
		rec.Stop()
		if wantsJSON(r) {
			writeJSON(w, map[string]any{"stopped": true})
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /shows/{id}/record", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		record := r.FormValue("record") == "1"
		if err := a.store.SetRecord(r.Context(), id, record); err != nil {
			httpError(w, err)
			return
		}
		if record {
			// Selecting a show records its current airing, even if an earlier
			// recording of this airing was ended.
			if sh, err := a.store.Show(r.Context(), id); err == nil {
				a.allowRestart(sh.Key)
			}
		}
		poke(a.wake) // start right away if the show is on now
		if !wantsJSON(r) {
			// Without JS, go back to where the button was pressed.
			target := "/#your-shows"
			if r.FormValue("from") == "add" {
				target = fmt.Sprintf("/?add=1#show-%d", id)
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		// Send back the re-rendered lists so the page can update in place.
		data, err := a.pageData(r.Context())
		if err != nil {
			httpError(w, err)
			return
		}
		var upcoming, mine bytes.Buffer
		if err := tmpl.ExecuteTemplate(&upcoming, "upcoming", data); err != nil {
			httpError(w, err)
			return
		}
		if err := tmpl.ExecuteTemplate(&mine, "mine", data); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]any{
			"id": id, "record": record, "selected": data.Selected,
			"upcoming": upcoming.String(), "mine": mine.String(),
		})
	})
	mux.HandleFunc("POST /shows/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if err := a.store.Delete(r.Context(), id); err != nil {
			httpError(w, err)
			return
		}
		http.Redirect(w, r, "/?add=1", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /refresh", func(w http.ResponseWriter, r *http.Request) {
		poke(a.refreshNow)
		// Also refetch every show page, not just stale ones.
		go a.refreshDetails(context.WithoutCancel(r.Context()), time.Now())
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	// Reject cross-site form posts, so other web pages can't change selections.
	return http.NewCrossOriginProtection().Handler(mux)
}

// activeRecording finds a recording in progress by its id.
func (a *App) activeRecording(id string) *recording {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, rec := range a.active {
		if rec.id == n {
			return rec
		}
	}
	return nil
}

func (a *App) pageData(ctx context.Context) (pageData, error) {
	shows, err := a.store.Shows(ctx)
	if err != nil {
		return pageData{}, err
	}
	stationLogo, err := a.store.Setting(ctx, "station_logo")
	if err != nil {
		return pageData{}, err
	}
	now := time.Now()
	jobs, err := a.upcoming(ctx, now)
	if err != nil {
		return pageData{}, err
	}

	a.mu.Lock()
	events := a.events
	active := make([]liveStatus, 0, len(a.active))
	for _, rec := range a.active {
		active = append(active, rec.status(now))
	}
	data := pageData{LastRefresh: a.lastRefresh, StationLogo: stationLogo}
	if a.refreshErr != nil {
		data.RefreshErr = a.refreshErr.Error()
	}
	a.mu.Unlock()

	// Next airing (or current one) for each show on the calendar.
	next := map[string]time.Time{}
	for _, ev := range events {
		k := normalize(ev.Title)
		if ev.End.After(now) && (next[k].IsZero() || ev.Start.Before(next[k])) {
			next[k] = ev.Start
		}
	}
	recordingNow := map[string]bool{}
	for _, st := range active {
		recordingNow[normalize(st.Title)] = true
	}
	ids := map[string]Show{}
	categories := map[string]bool{}
	for _, sh := range shows {
		ids[sh.Key] = sh
		if sh.Category != "" {
			categories[sh.Category] = true
		}
		if sh.Record {
			data.Selected++
		}
		_, onCal := next[sh.Key]
		v := showView{
			Show: sh, NextAiring: next[sh.Key], OnCalendar: onCal,
			RecordingNow: recordingNow[sh.Key], ScheduleKey: scheduleSortKey(sh.Schedule),
		}
		data.Shows = append(data.Shows, v)
		if sh.Record {
			data.Mine = append(data.Mine, v)
		}
	}
	// Your shows: on air or airing soonest first, then shows off the calendar.
	slices.SortStableFunc(data.Mine, func(x, y showView) int {
		switch {
		case x.OnCalendar != y.OnCalendar:
			if x.OnCalendar {
				return -1
			}
			return 1
		case x.OnCalendar:
			return x.NextAiring.Compare(y.NextAiring)
		}
		return 0 // already sorted by title
	})
	for c := range categories {
		data.Categories = append(data.Categories, c)
	}
	slices.Sort(data.Categories)

	view := func(j Job) jobView {
		sh := ids[normalize(j.Event.Title)]
		dj := j.Event.DJ
		if dj == "" {
			dj = sh.DJ // the calendar often omits the DJ; the show page has it
		}
		return jobView{ShowID: sh.ID, Title: j.Event.Title, DJ: dj, Start: j.Event.Start, End: j.Event.End}
	}
	slices.SortFunc(active, func(x, y liveStatus) int { return x.Start.Compare(y.Start) })
	for _, st := range active {
		sh := ids[normalize(st.Title)]
		st.ShowID, st.Image = sh.ID, sh.Image
		if st.DJ == "" {
			st.DJ = sh.DJ
		}
		data.Recording = append(data.Recording, st)
	}
	for _, j := range jobs {
		if !recordingNow[normalize(j.Event.Title)] && len(data.Upcoming) < 15 {
			data.Upcoming = append(data.Upcoming, view(j))
		}
	}
	return data, nil
}

// scheduleSlotRe matches Spinitron schedule lines such as
// "Every week on Mon, Tue, Wed, Thu at 6:00 PM".
var scheduleSlotRe = regexp.MustCompile(`on ((?:(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)(?:, | and | & )?)+) at (\d{1,2}):(\d{2}) ?([AP]M)`)

var dayRe = regexp.MustCompile(`Mon|Tue|Wed|Thu|Fri|Sat|Sun`)

var weekOrder = map[string]int{"Mon": 0, "Tue": 1, "Wed": 2, "Thu": 3, "Fri": 4, "Sat": 5, "Sun": 6}

// scheduleSortKey orders shows by their earliest weekly slot, Monday first,
// as minutes into the week. It returns -1 if the schedule can't be parsed.
func scheduleSortKey(schedule string) int {
	best := -1
	for _, m := range scheduleSlotRe.FindAllStringSubmatch(schedule, -1) {
		h, _ := strconv.Atoi(m[2])
		mins, _ := strconv.Atoi(m[3])
		h %= 12
		if m[4] == "PM" {
			h += 12
		}
		for _, d := range dayRe.FindAllString(m[1], -1) {
			if k := weekOrder[d]*24*60 + h*60 + mins; best < 0 || k < best {
				best = k
			}
		}
	}
	return best
}

// artView is what the "art" template needs to draw a show's artwork.
type artView struct {
	Src, Fallback, Size string
	Lazy                bool
}

// artImage picks a show's artwork, falling back to the station logo; the
// page swaps in the logo if the show's image fails to load.
func artImage(image, stationLogo, size string, lazy bool) artView {
	v := artView{Src: image, Size: size, Lazy: lazy}
	if v.Src == "" {
		v.Src = stationLogo
	} else if stationLogo != image {
		v.Fallback = stationLogo
	}
	return v
}

// formatDuration renders milliseconds as m:ss or h:mm:ss.
func formatDuration(ms int64) string {
	sec := max(ms, 0) / 1000
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func render(w http.ResponseWriter, tmpl *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func httpError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	log.Printf("web: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
