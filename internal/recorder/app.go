package recorder

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wcbn-recorder/internal/config"
	"wcbn-recorder/internal/spinitron"
	"wcbn-recorder/internal/store"
)

const (
	lookahead = 7 * 24 * time.Hour
	maxSleep  = time.Minute
	// Show pages (descriptions, schedules) change rarely; refetch daily.
	detailsMaxAge = 24 * time.Hour
)

// App runs the scheduler and holds the state the web UI displays.
type App struct {
	cfg   *config.Config
	store *store.Store

	mu          sync.Mutex
	events      []spinitron.Event
	lastRefresh time.Time
	refreshErr  error
	started     map[string]bool
	active      map[string]*recording
	runCtx      context.Context // the scheduler's context, once run has started
	tap         *stationTap     // the shared station connection for listeners

	// The station's status (current song, bitrate), for the "On air" panel; cached.
	statusMu sync.Mutex
	status   stationStatus
	statusAt time.Time

	// The page's event streams wait on changed, which is closed (and replaced)
	// whenever what the page shows changes, and end once done is closed.
	changedMu sync.Mutex
	changed   chan struct{}
	done      chan struct{}

	wake           chan struct{} // re-evaluate selections now
	refreshNow     chan struct{} // re-fetch the schedule now
	detailsRunning atomic.Bool
	wg             sync.WaitGroup
}

func NewApp(cfg *config.Config, store *store.Store) *App {
	a := &App{
		cfg:        cfg,
		store:      store,
		started:    map[string]bool{},
		active:     map[string]*recording{},
		wake:       make(chan struct{}, 1),
		refreshNow: make(chan struct{}, 1),
		changed:    make(chan struct{}),
		done:       make(chan struct{}),
	}
	a.tap = newStationTap(cfg, a.recentAudio)
	return a
}

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// notify wakes the page's event streams to send what's changed.
func (a *App) notify() {
	a.changedMu.Lock()
	defer a.changedMu.Unlock()
	close(a.changed)
	a.changed = make(chan struct{})
}

// changes returns a channel that's closed at the next notify.
func (a *App) changes() <-chan struct{} {
	a.changedMu.Lock()
	defer a.changedMu.Unlock()
	return a.changed
}

// run polls the schedule and starts a recording whenever a selected show's
// window (with pre/post-roll) is open. It returns once ctx is cancelled and
// in-progress recordings have been finalized.
func (a *App) Run(ctx context.Context) {
	a.mu.Lock()
	a.runCtx = ctx
	a.mu.Unlock()
	nextRefresh := time.Now()
	for {
		now := time.Now()
		if !now.Before(nextRefresh) {
			if a.Refresh(ctx) {
				nextRefresh = now.Add(a.cfg.PollInterval.Duration)
			} else {
				nextRefresh = now.Add(time.Minute)
			}
		}

		// Wake for whichever comes first: a refresh, the next show, or maxSleep.
		wake := min(time.Until(nextRefresh), a.startDue(ctx, now))

		select {
		case <-ctx.Done():
			log.Printf("shutting down; finishing in-progress recordings")
			a.tap.close() // end listeners' streams, so the web server can stop
			close(a.done) // and the page's event streams
			a.wg.Wait()
			return
		case <-a.refreshNow:
			nextRefresh = time.Now()
		case <-a.wake:
		case <-time.After(wake):
		}
	}
}

// startDue starts recording every selected show whose window is open and
// that isn't already recorded this airing, and returns how long until the
// next one is due (at most maxSleep).
func (a *App) startDue(ctx context.Context, now time.Time) time.Duration {
	wake := maxSleep
	jobs, err := a.Upcoming(ctx, now)
	if err != nil {
		log.Printf("loading selections: %v", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	started := false
	defer func() {
		if started {
			a.notify()
		}
	}()
	for _, job := range jobs {
		key := job.Event.Key()
		if a.started[key] {
			continue
		}
		if begin := job.Event.Start.Add(-a.cfg.PreRoll.Duration); now.Before(begin) {
			wake = min(wake, begin.Sub(now))
			continue
		}
		a.started[key] = true
		rec, err := startRecording(a.cfg, job)
		if err != nil {
			log.Printf("recording %q failed: %v", job.Event.Title, err)
			continue
		}
		a.active[key] = rec
		started = true
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			if err := rec.Run(ctx); err != nil {
				log.Printf("recording %q failed: %v", job.Event.Title, err)
			}
			a.mu.Lock()
			delete(a.active, key)
			a.mu.Unlock()
			a.notify()
		}()
	}
	return wake
}

// startNow starts any recording that's due immediately, rather than waiting
// for the scheduler to wake, so the page shows it right away.
func (a *App) startNow() {
	a.mu.Lock()
	ctx := a.runCtx
	a.mu.Unlock()
	if ctx == nil {
		poke(a.wake)
		return
	}
	a.startDue(ctx, time.Now())
}

// allowRestart forgets which airings of a show have been recorded, except
// ones still recording, so selecting it again records the current airing even
// if an earlier recording of it was ended or failed.
func (a *App) allowRestart(showKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.started {
		if strings.HasPrefix(key, showKey+"|") && a.active[key] == nil {
			delete(a.started, key)
		}
	}
}

// refresh fetches the calendar, records its shows in the database, and
// kicks off a background update of show page details.
func (a *App) Refresh(ctx context.Context) bool {
	evs, err := fetchUpcoming(ctx, a.cfg)
	if err == nil {
		err = a.store.UpsertEvents(ctx, evs, time.Now())
	}
	a.mu.Lock()
	a.refreshErr = err
	if err == nil {
		a.events = evs
		a.lastRefresh = time.Now()
	}
	a.mu.Unlock()
	a.notify()
	if err != nil {
		log.Printf("schedule refresh failed: %v", err)
		return false
	}
	jobs, _ := a.Upcoming(ctx, time.Now())
	log.Printf("schedule refreshed: %d events, %d upcoming recordings", len(evs), len(jobs))
	if len(jobs) > 0 {
		log.Printf("next: %q at %s", jobs[0].Event.Title, jobs[0].Event.Start.In(a.cfg.Location).Format("Mon Jan 2 3:04 PM"))
	}
	go a.refreshDetails(ctx, time.Now().Add(-detailsMaxAge))
	return true
}

// refreshDetails fetches show pages last updated before the given time.
func (a *App) refreshDetails(ctx context.Context, before time.Time) {
	if !a.detailsRunning.CompareAndSwap(false, true) {
		return
	}
	defer a.detailsRunning.Store(false)
	shows, err := a.store.StaleDetails(ctx, before)
	if err != nil || len(shows) == 0 {
		return
	}
	urls := make([]string, len(shows))
	for i, sh := range shows {
		urls[i] = sh.URL
	}
	infos := spinitron.FetchShowInfos(ctx, urls)
	now := time.Now()
	stationLogo := ""
	for _, sh := range shows {
		if info, ok := infos[sh.URL]; ok {
			if err := a.store.UpdateDetails(ctx, sh.ID, info, now); err != nil {
				log.Printf("saving details for %q: %v", sh.Title, err)
			}
			if info.StationImage != "" {
				stationLogo = info.StationImage
			}
		}
	}
	if stationLogo != "" {
		// The UI shows the station logo for shows without working artwork.
		if err := a.store.SetSetting(ctx, "station_logo", stationLogo); err != nil {
			log.Printf("saving station logo: %v", err)
		}
	}
	log.Printf("updated details for %d of %d shows", len(infos), len(shows))
}

// upcoming returns selected shows that haven't finished yet, soonest first.
func (a *App) Upcoming(ctx context.Context, now time.Time) ([]Job, error) {
	selected, err := a.store.Selected(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	events := a.events
	a.mu.Unlock()
	return upcomingJobs(a.cfg, events, selected, now), nil
}

func upcomingJobs(cfg *config.Config, events []spinitron.Event, selected map[string]store.Show, now time.Time) []Job {
	seen := map[string]bool{}
	var jobs []Job
	for _, ev := range events {
		show, ok := selected[spinitron.Normalize(ev.Title)]
		if !ok {
			continue
		}
		until := ev.End.Add(cfg.PostRoll.Duration)
		if !now.Before(until) || seen[ev.Key()] {
			continue
		}
		seen[ev.Key()] = true
		jobs = append(jobs, Job{Program: show.Title, Event: ev, Until: until})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Event.Start.Before(jobs[j].Event.Start) })
	return jobs
}

func fetchUpcoming(ctx context.Context, cfg *config.Config) ([]spinitron.Event, error) {
	now := time.Now()
	return spinitron.FetchSchedule(ctx, cfg.ScheduleURL, cfg.Location, now.Add(-24*time.Hour), now.Add(lookahead))
}
