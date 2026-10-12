// Command wcbn-recorder watches the WCBN schedule and records configured
// shows from the live stream, without transcoding.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	lookahead = 7 * 24 * time.Hour
	maxSleep  = time.Minute
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	list := flag.Bool("list", false, "print upcoming matching shows and exit")
	test := flag.Duration("test", 0, "record a short test clip of this length now and exit (e.g. 30s)")
	shows := flag.String("shows", "", "write all shows on the schedule, with descriptions, to this text file and exit")
	flag.Parse()
	if os.Getenv("JOURNAL_STREAM") != "" {
		log.SetFlags(0) // journald adds its own timestamps
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case *test > 0:
		now := time.Now()
		job := Job{
			Program: "Test",
			Event:   Event{Title: "Test Recording", Start: now, End: now.Add(*test)},
			Until:   now.Add(*test),
		}
		if err := Record(ctx, cfg, job); err != nil {
			log.Fatal(err)
		}
	case *shows != "":
		if err := writeShowList(ctx, cfg, *shows); err != nil {
			log.Fatal(err)
		}
	case *list:
		events, err := fetchUpcoming(ctx, cfg)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%d events in schedule\n", len(events))
		for _, j := range matchingJobs(cfg, events, time.Now()) {
			fmt.Printf("%s  %-30s  %s – %s\n", j.Program, j.Event.Title,
				j.Event.Start.In(cfg.location).Format("Mon Jan 2 3:04 PM"),
				j.Event.End.In(cfg.location).Format("3:04 PM"))
		}
	default:
		run(ctx, cfg)
	}
}

// run polls the schedule and starts a recording whenever a matching show's
// window (with pre/post-roll) is open.
func run(ctx context.Context, cfg *Config) {
	log.Printf("watching for: %s", strings.Join(cfg.Programs, ", "))
	var (
		mu      sync.Mutex
		events  []Event
		started = map[string]bool{}
		wg      sync.WaitGroup
	)

	refresh := func() bool {
		evs, err := fetchUpcoming(ctx, cfg)
		if err != nil {
			log.Printf("schedule refresh failed: %v", err)
			return false
		}
		mu.Lock()
		events = evs
		mu.Unlock()
		jobs := matchingJobs(cfg, evs, time.Now())
		log.Printf("schedule refreshed: %d events, %d upcoming matches", len(evs), len(jobs))
		if len(jobs) > 0 {
			log.Printf("next: %q at %s", jobs[0].Event.Title, jobs[0].Event.Start.In(cfg.location).Format("Mon Jan 2 3:04 PM"))
		}
		return true
	}

	nextRefresh := time.Now()
	for {
		now := time.Now()
		if !now.Before(nextRefresh) {
			if refresh() {
				nextRefresh = now.Add(cfg.PollInterval.Duration)
			} else {
				nextRefresh = now.Add(time.Minute)
			}
		}

		// Wake for whichever comes first: a refresh, the next show, or maxSleep.
		wake := min(time.Until(nextRefresh), maxSleep)
		mu.Lock()
		for _, job := range matchingJobs(cfg, events, now) {
			key := job.Event.key()
			if started[key] {
				continue
			}
			if begin := job.Event.Start.Add(-cfg.PreRoll.Duration); now.Before(begin) {
				wake = min(wake, begin.Sub(now))
				continue
			}
			started[key] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := Record(ctx, cfg, job); err != nil {
					log.Printf("recording %q failed: %v", job.Event.Title, err)
				}
			}()
		}
		mu.Unlock()

		select {
		case <-ctx.Done():
			log.Printf("shutting down; finishing in-progress recordings")
			wg.Wait()
			return
		case <-time.After(wake):
		}
	}
}

// matchingJobs returns configured shows that haven't finished yet, soonest first.
func matchingJobs(cfg *Config, events []Event, now time.Time) []Job {
	seen := map[string]bool{}
	var jobs []Job
	for _, ev := range events {
		program, ok := cfg.matches(ev.Title)
		if !ok {
			continue
		}
		until := ev.End.Add(cfg.PostRoll.Duration)
		if !now.Before(until) || seen[ev.key()] {
			continue
		}
		seen[ev.key()] = true
		jobs = append(jobs, Job{Program: program, Event: ev, Until: until})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Event.Start.Before(jobs[j].Event.Start) })
	return jobs
}

func fetchUpcoming(ctx context.Context, cfg *Config) ([]Event, error) {
	now := time.Now()
	return fetchSchedule(ctx, cfg, now.Add(-24*time.Hour), now.Add(lookahead))
}
