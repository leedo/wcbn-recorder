package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// writeShowList writes every show on the next four weeks of the schedule,
// with details from its Spinitron show page, to path.
func writeShowList(ctx context.Context, cfg *Config, path string) error {
	now := time.Now()
	events, err := fetchSchedule(ctx, cfg, now, now.Add(28*24*time.Hour))
	if err != nil {
		return err
	}

	// A title can have several show pages (e.g. one per time slot).
	type show struct {
		title, category string
		djs, urls       []string
	}
	byTitle := map[string]*show{}
	for _, ev := range events {
		k := normalize(ev.Title)
		s := byTitle[k]
		if s == nil {
			s = &show{title: ev.Title, category: ev.Category}
			byTitle[k] = s
		}
		if ev.DJ != "" && !slices.Contains(s.djs, ev.DJ) {
			s.djs = append(s.djs, ev.DJ)
		}
		if !slices.Contains(s.urls, ev.URL) {
			s.urls = append(s.urls, ev.URL)
		}
	}

	var urls []string
	for _, s := range byTitle {
		urls = append(urls, s.urls...)
	}
	log.Printf("%d shows on the schedule; fetching %d show pages", len(byTitle), len(urls))
	infos := fetchShowInfos(ctx, urls)

	shows := make([]*show, 0, len(byTitle))
	for _, s := range byTitle {
		shows = append(shows, s)
	}
	sort.Slice(shows, func(i, j int) bool { return normalize(shows[i].title) < normalize(shows[j].title) })

	var b strings.Builder
	fmt.Fprintf(&b, "WCBN-FM Ann Arbor shows\nSource: %s (generated %s)\n%d shows\n", cfg.ScheduleURL, now.In(cfg.location).Format("January 2, 2006"), len(shows))
	for _, s := range shows {
		var schedule []string
		var description, link string
		for _, u := range s.urls {
			info := infos[u]
			for _, line := range info.Schedule {
				if !slices.Contains(schedule, line) {
					schedule = append(schedule, line)
				}
			}
			if description == "" {
				description = info.Description
			}
			if link == "" {
				link = info.URL
			}
		}
		if link == "" {
			link = s.urls[0]
		}

		fmt.Fprintf(&b, "\n%s\n%s\n", s.title, strings.Repeat("=", len([]rune(s.title))))
		if len(s.djs) > 0 {
			fmt.Fprintf(&b, "DJ: %s\n", strings.Join(s.djs, ", "))
		}
		if s.category != "" {
			fmt.Fprintf(&b, "Category: %s\n", s.category)
		}
		if len(schedule) > 0 {
			fmt.Fprintf(&b, "Schedule: %s\n", strings.Join(schedule, "; "))
		}
		fmt.Fprintf(&b, "Link: %s\n", link)
		if description == "" {
			description = "(no description)"
		}
		fmt.Fprintf(&b, "\n%s\n", description)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	log.Printf("wrote %d shows to %s", len(shows), path)
	return nil
}

// fetchShowInfos fetches show pages with a small amount of concurrency.
func fetchShowInfos(ctx context.Context, urls []string) map[string]ShowInfo {
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
			info, err := fetchShowInfo(ctx, u)
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
