package spinitron

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

// A playlist page's spins, trimmed from a real one: two songs logged in the
// same minute, one with its release, label and year, and the art and icons
// cut out.
const playlistPage = `<html><body><div class="spins public-spins"><table class="table"><tbody>
<tr id="sp-1" class="spin-item" data-key="1"><td class="spin-time"><a href="/WCBN/pl/23086806/Train-To-Skaville?sp=1">11:58 AM</a></td><td class="spin-art"></td>
<td class="spin-text"><div class="spin"><span class="artist">justin hinds &amp; the dominoes</span> <span class="song">over the river</span></div></td></tr>
<tr id="sp-2" class="spin-item" data-key="2"><td class="spin-time"><a href="?sp=2">12:26 PM</a></td><td class="spin-text"><div class="spin"><span class="artist">derrick morgan</span> <span class="song">father killam</span></div></td></tr>
<tr id="sp-3" class="spin-item" data-key="3"><td class="spin-time"><a href="?sp=3">12:26 PM</a></td><td class="spin-text"><div class="spin"><span class="artist">derrick harriott</span> <span class="song">the loser</span></div><div class="info"><span class="release">The Loser</span> <span class="parenthesis"><span class="label">Crystal</span> <span class="released">1967</span></span></div></td></tr>
<tr class="spin-item"><td class="spin-time">12:30 PM</td><td class="spin-text"></td></tr>
</tbody></table></div></body></html>`

func TestParsePlaylistPage(t *testing.T) {
	spins, err := parsePlaylistPage([]byte(playlistPage))
	if err != nil {
		t.Fatal(err)
	}
	want := "[{justin hinds & the dominoes over the river   0 718} {derrick morgan father killam   0 746} {derrick harriott the loser The Loser Crystal 1967 746}]"
	if got := fmt.Sprint(spins); got != want {
		t.Errorf("spins = %s, want %s", got, want)
	}
}

func TestPlaylistsInFeed(t *testing.T) {
	feed := `[
		{"id":2,"title":"Train To Skaville","start":"2026-10-13T20:00:00-0400","end":"2026-10-13T21:00:00-0400","className":"spin-cal-pl category-specialty","url":"/WCBN/pl/2/Train-To-Skaville"},
		{"id":1,"title":"Train To Skaville","start":"2026-10-13T19:00:00-0400","end":"2026-10-13T20:00:00-0400","className":"spin-cal-pl category-specialty","url":"/WCBN/pl/1/Train-To-Skaville"},
		{"id":3,"title":"Train To Skaville","start":"2026-10-13T19:00:00-0400","end":"2026-10-13T20:00:00-0400","className":"spin-cal-show","url":"/WCBN/show/3/Train-To-Skaville"},
		{"id":4,"title":"Train To Skaville","start":"2026-10-06T19:00:00-0400","end":"2026-10-06T20:00:00-0400","className":"spin-cal-pl","url":"/WCBN/pl/4/Train-To-Skaville"},
		{"id":5,"title":"Yazoo City Calling","start":"2026-10-13T19:00:00-0400","end":"2026-10-13T20:00:00-0400","className":"spin-cal-pl","url":"/WCBN/pl/5/Yazoo"}
	]`
	base, _ := url.Parse("https://spinitron.com/WCBN/calendar-feed")
	start := time.Date(2026, 10, 13, 23, 0, 0, 0, time.UTC)
	pls, err := playlistsInFeed([]byte(feed), base, "train to skaville", start, start.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(pls) != 2 || pls[0].URL != "https://spinitron.com/WCBN/pl/1/Train-To-Skaville" || pls[1].URL != "https://spinitron.com/WCBN/pl/2/Train-To-Skaville" || !pls[0].Start.Equal(start) {
		t.Errorf("playlists = %+v", pls)
	}
}

func TestPlaylistTimes(t *testing.T) {
	loc, _ := time.LoadLocation("America/Detroit")
	slot := time.Date(2026, 10, 13, 19, 0, 0, 0, loc)
	at := func(h, m int) string { return time.Date(2026, 10, 13, h, m, 0, 0, loc).Format("Jan 2 15:04") }
	times := func(minutes ...int) string {
		pl := Playlist{Start: slot, End: slot.Add(time.Hour)}
		for _, m := range minutes {
			pl.Spins = append(pl.Spins, Spin{Minute: m})
		}
		var out []string
		for _, t := range pl.Times(loc) {
			out = append(out, t.Format("Jan 2 15:04"))
		}
		return fmt.Sprint(out)
	}
	// Logged with the wrong clock (as on the page above): the first song goes
	// at the start of the slot, and the rest keep their gaps.
	if got, want := times(11*60+58, 12*60+26, 12*60+26), fmt.Sprint([]string{at(19, 0), at(19, 28), at(19, 28)}); got != want {
		t.Errorf("wrong clock: %s, want %s", got, want)
	}
	// Logged at the right times, even a little before the slot: taken as is.
	if got, want := times(19*60+3, 19*60+10), fmt.Sprint([]string{at(19, 3), at(19, 10)}); got != want {
		t.Errorf("right clock: %s, want %s", got, want)
	}
	if got, want := times(18*60+50, 19*60+2), fmt.Sprint([]string{at(18, 50), at(19, 2)}); got != want {
		t.Errorf("logged early: %s, want %s", got, want)
	}
	// Past midnight, and a spin with no time, which goes with the one before.
	if got, want := times(23*60+58, -1, 3), fmt.Sprint([]string{at(19, 0), at(19, 0), at(19, 5)}); got != want {
		t.Errorf("past midnight: %s, want %s", got, want)
	}
	if (Playlist{}).Times(loc) != nil {
		t.Error("times without spins")
	}
}

// A playlist on the air lists the newest song first; it's put in air order.
func TestAirOrder(t *testing.T) {
	spins := func(minutes ...int) []Spin {
		var out []Spin
		for _, m := range minutes {
			out = append(out, Spin{Minute: m})
		}
		return out
	}
	minutes := func(spins []Spin) []int {
		var out []int
		for _, sp := range spins {
			out = append(out, sp.Minute)
		}
		return out
	}
	for _, c := range []struct{ in, want []int }{
		{[]int{13*60 + 20, 13*60 + 16, 13*60 + 13, 13*60 + 1}, []int{13*60 + 1, 13*60 + 13, 13*60 + 16, 13*60 + 20}}, // newest first
		{[]int{13*60 + 1, 13*60 + 13, 13*60 + 13, 13*60 + 20}, []int{13*60 + 1, 13*60 + 13, 13*60 + 13, 13*60 + 20}}, // oldest first
		{[]int{2, 23*60 + 58, -1, 23*60 + 50}, []int{23*60 + 50, -1, 23*60 + 58, 2}},                                 // newest first, past midnight
		{[]int{23*60 + 50, 23*60 + 58, 2}, []int{23*60 + 50, 23*60 + 58, 2}},                                         // oldest first, past midnight
	} {
		if got := minutes(airOrder(spins(c.in...))); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("airOrder(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
