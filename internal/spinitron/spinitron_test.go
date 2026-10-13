package spinitron

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

const calendarPage = `<script>jQuery(function ($) {
jQuery('#calendar-w0').calendar({"events":"\/WCBN\/calendar-feed?timeslot=30","header":{"left":"prev,next today"},"defaultView":"agendaWeek"});
});</script>`

const feedJSON = `[
{"id":248524,"scheduleId":315224,"title":"Yazoo City Calling","text":"rotating hosts","start":"2026-10-19T19:00:00-0400","end":"2026-10-19T20:00:00-0400","oneOff":false,"className":"spin-cal-show category-specialty","url":"/WCBN/show/248524/Yazoo-City-Calling","editable":false,"data":{"category":"Specialty"}},
{"id":23157664,"title":"VARIETY IS DEAD","text":"roverie","start":"2026-10-11T00:00:00-0400","end":"2026-10-11T01:00:00-0400","className":"spin-cal-pl category-freeform","url":"/WCBN/pl/23157664/VARIETY-IS-DEAD","editable":false,"data":{"category":"Freeform"}},
{"id":299137,"title":"VARIETY IS DEAD","text":"roverie","start":"2026-10-11T00:00:00-0400","end":"2026-10-11T01:00:00-0400","className":"spin-cal-show category-freeform","url":"/WCBN/show/299137/VARIETY-IS-DEAD","editable":false,"data":{"category":"Freeform"}},
{"id":307726,"title":"Overnight Service","text":"","start":"2026-10-13T01:00:00-0400","end":"2026-10-13T02:00:00-0400","className":"spin-cal-show category-robot","url":"/WCBN/show/307726/Overnight-Service","data":{"category":"Robot"}},
{"id":312268,"title":"Overnight Service","text":"","start":"2026-10-13T02:00:00-0400","end":"2026-10-13T03:00:00-0400","className":"spin-cal-show category-robot","url":"/WCBN/show/312268/Overnight-Service","data":{"category":"Robot"}},
{"id":312269,"title":"Overnight Service","text":"","start":"2026-10-14T02:00:00-0400","end":"2026-10-14T03:00:00-0400","className":"spin-cal-show category-robot","url":"/WCBN/show/312269/Overnight-Service","data":{"category":"Robot"}},
{"id":1,"title":"Broken","text":"","start":"2026-10-11T00:00:00-0400","end":"2026-10-11T00:00:00-0400","className":"spin-cal-show","url":"/x"}
]`

const showPage = `<html><head><meta property="og:image" content="https://farm4.staticflickr.com/3896/14733200816_f92c8554a7_m.jpg"></head><body>
<div class="head station"><div class="image"><img src="/images/Station/22/2263-img_logo.225x225.jpg"></div><h1 class="station-title">WCBN-FM Ann Arbor</h1><div class="station-description"><p>Station blurb.</p></div></div>
<div class="head playlist"><div class="data">
<h3 class="show-title"><a href="/WCBN/show/299137/VARIETY-IS-DEAD">VARIETY IS DEAD</a></h3>
<ul class="timeslot show-schedule"><li>Every  week on Sun at 12:00 AM</li></ul>
<p class="show-categoty">Freeform</p>
<p class="dj-name">With <a href="/WCBN/dj/1/roverie">roverie</a></p>
<div class="description"><p>We escape our   personalized sonic bubbles.</p><p>If variety is dead,<br>we mourn it.</p></div>
</div></div>
<div class="episode-description"><p>Not this one.</p></div></body></html>`

func TestCalendarFeedURL(t *testing.T) {
	u, err := calendarFeedURL("https://spinitron.com/WCBN/calendar", []byte(calendarPage))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.String(); got != "https://spinitron.com/WCBN/calendar-feed?timeslot=30" {
		t.Errorf("feed url = %s", got)
	}
}

func TestParseCalendarFeed(t *testing.T) {
	base, _ := url.Parse("https://spinitron.com/WCBN/calendar-feed")
	evs, err := parseCalendarFeed([]byte(feedJSON), base)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by start: VARIETY IS DEAD, Overnight Service x2 (first merged), Yazoo City Calling.
	if len(evs) != 4 {
		t.Fatalf("got %d events, want 4 (playlist deduped, slots merged, zero-length dropped): %+v", len(evs), evs)
	}
	if o := evs[1]; o.Title != "Overnight Service" || o.End.Sub(o.Start) != 2*time.Hour || evs[2].End.Sub(evs[2].Start) != time.Hour {
		t.Errorf("bad merge: %+v / %+v", evs[1], evs[2])
	}
	e := evs[3]
	if e.Title != "Yazoo City Calling" || e.DJ != "rotating hosts" || e.Category != "Specialty" ||
		e.URL != "https://spinitron.com/WCBN/show/248524/Yazoo-City-Calling" ||
		!e.Start.Equal(time.Date(2026, 10, 19, 23, 0, 0, 0, time.UTC)) || e.End.Sub(e.Start) != time.Hour {
		t.Errorf("bad event: %+v", e)
	}
	if evs[0].URL != "https://spinitron.com/WCBN/show/299137/VARIETY-IS-DEAD" {
		t.Errorf("expected show entry to win over playlist entry, got %s", evs[0].URL)
	}
}

func TestParseShowPage(t *testing.T) {
	info, err := parseShowPage([]byte(showPage), "https://spinitron.com/WCBN/pl/23157664/VARIETY-IS-DEAD")
	if err != nil {
		t.Fatal(err)
	}
	want := ShowInfo{
		Title:        "VARIETY IS DEAD",
		URL:          "https://spinitron.com/WCBN/show/299137/VARIETY-IS-DEAD",
		DJ:           "roverie",
		Category:     "Freeform",
		Schedule:     []string{"Every week on Sun at 12:00 AM"},
		Description:  "We escape our personalized sonic bubbles.\n\nIf variety is dead,\n\nwe mourn it.",
		Image:        "https://farm4.staticflickr.com/3896/14733200816_f92c8554a7_m.jpg",
		StationImage: "https://spinitron.com/images/Station/22/2263-img_logo.225x225.jpg",
	}
	if info.Title != want.Title || info.URL != want.URL || info.DJ != want.DJ || info.Category != want.Category ||
		strings.Join(info.Schedule, "|") != strings.Join(want.Schedule, "|") || info.Description != want.Description || info.Image != want.Image || info.StationImage != want.StationImage {
		t.Errorf("got  %+v\nwant %+v", info, want)
	}
}
