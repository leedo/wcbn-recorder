package recorder

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wcbn-recorder/internal/id3"
	"wcbn-recorder/internal/store"
)

// A resumed run's songs are timed from the start of its own show file; played
// back, they follow the earlier run's.
func TestPlaybackSongs(t *testing.T) {
	songs := []store.Song{
		{Title: "One", StartMs: 0, DurationMs: 1000},
		{Title: "Two", Artist: "B", StartMs: 1000, DurationMs: 500},
		{Title: "Three", StartMs: 0, DurationMs: 2000}, // the resumed run
		{Title: "Four", StartMs: 2000, DurationMs: 100},
	}
	got := fmt.Sprint(playbackSongs(songs))
	if want := "[{0 1000 One  []} {1000 1500 Two B []} {1500 3500 Three  []} {3500 3600 Four  []}]"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if playbackSongs(nil) != nil {
		t.Error("no songs")
	}
}

// The progress bar marks each song change after the first, with its tooltip
// kept on the bar near the ends.
func TestSongMarks(t *testing.T) {
	songs := []playbackSong{{Start: 0, Title: "One"}, {Start: 100, Title: "Two", Artist: "B"}, {Start: 500, Title: "Three"}, {Start: 950, Title: "Four"}, {Start: 1000, Title: "Past the end"}}
	got := fmt.Sprint(songMarks(songs, 1000))
	if want := "[{100 10 Two — B left} {500 50 Three } {950 95 Four right}]"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if songMarks(songs, 0) != nil {
		t.Error("marks without a length")
	}
}

func TestKbps(t *testing.T) {
	for _, c := range []struct{ n, ms, want int64 }{{144_000_000, 3_600_000, 320}, {1000, 63, 127}, {5, 0, 0}} {
		if got := kbps(c.n, c.ms); got != c.want {
			t.Errorf("kbps(%d, %d) = %d, want %d", c.n, c.ms, got, c.want)
		}
	}
}

func TestWebPlayback(t *testing.T) {
	app, srv := testApp(t)
	app.cfg.Destination = t.TempDir()
	ctx := t.Context()

	// A resumed recording: two show files, each with its own tag.
	at := time.Date(2026, 10, 6, 19, 0, 0, 0, time.UTC)
	id, err := app.store.AddRecording(ctx, store.Recording{Airing: "a", Title: "Train To Skaville", DJ: "brian tomsic", Start: at, End: at.Add(time.Hour), StartedAt: at, Dir: "Train To Skaville"})
	if err != nil {
		t.Fatal(err)
	}
	app.store.UpdateRecording(ctx, store.Recording{ID: id, DJ: "brian tomsic", FinishedAt: at.Add(time.Hour), ShowFile: "Train To Skaville/Train To Skaville - 2026-10-06.mp3", DurationMs: 3000, Bytes: 120_000})
	app.store.AddSong(ctx, store.Song{RecordingID: id, Number: 1, Title: "Pressure Drop", Artist: "Toots & the Maytals", PlayedAt: at, DurationMs: 2000})
	app.store.AddSong(ctx, store.Song{RecordingID: id, Number: 2, Title: "Rudy", Artist: "The Specials", PlayedAt: at, DurationMs: 1000})
	dir := filepath.Join(app.cfg.Destination, "Train To Skaville")
	os.MkdirAll(dir, 0o755)
	for name, audio := range map[string]string{"Train To Skaville - 2026-10-06.mp3": "first", "Train To Skaville - 2026-10-06 2.mp3": "second"} {
		var tag id3.Tag
		tag.Text("TIT2", "x")
		if err := writeTagged(filepath.Join(dir, name), &tag, strings.NewReader(audio)); err != nil {
			t.Fatal(err)
		}
	}

	// Its details page, and its row on the Recordings page, have Play.
	for _, path := range []string{fmt.Sprintf("/recordings/%d", id), "/recordings"} {
		if page := get(t, srv.URL+path); !strings.Contains(page, fmt.Sprintf(`class="listen play-rec" data-id="%d"`, id)) {
			t.Errorf("no Play button on %s", path)
		}
	}

	var info struct {
		Audio, Card, Player string
		DurationMs          int64
		Songs               []playbackSong
	}
	if err := json.Unmarshal([]byte(get(t, fmt.Sprintf("%s/recordings/%d/play", srv.URL, id))), &info); err != nil {
		t.Fatal(err)
	}
	if info.Audio != fmt.Sprintf("/recordings/%d/audio", id) || info.DurationMs != 3000 || len(info.Songs) != 2 || info.Songs[1].Start != 2000 || info.Songs[1].Artist != "The Specials" {
		t.Errorf("play: %+v", info)
	}
	for _, want := range []string{`class="card live-card playback"`, "Train To Skaville", "with brian tomsic", `class="progress seek"`, "back-live", `class="listen"`, "/ 0:03 · 320 kbps ·", `<span class="slot">Tue Oct 6, 3 – 4 PM</span>`,
		`<span class="mark" style="left: 66.667%" data-start="2000" data-label="Rudy — The Specials" aria-hidden="true"></span>`} {
		if !strings.Contains(info.Card, want) {
			t.Errorf("card missing %q:\n%s", want, info.Card)
		}
	}
	if !strings.Contains(info.Player, "<strong>Train To Skaville</strong>") || strings.Contains(info.Player, "REC") {
		t.Errorf("player bar:\n%s", info.Player)
	}

	// The audio is both files' audio, without their tags, and can be read in ranges.
	if audio := get(t, srv.URL+info.Audio); audio != "firstsecond" {
		t.Errorf("audio = %q", audio)
	}
	req, _ := http.NewRequest("GET", srv.URL+info.Audio, nil)
	req.Header.Set("Range", "bytes=3-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(b) != "stsec" || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Errorf("range: %s %q %s", resp.Status, b, resp.Header.Get("Content-Type"))
	}

	// A recording whose files are gone, or that's still being recorded, can't be played.
	gone, _ := app.store.AddRecording(ctx, store.Recording{Airing: "b", Title: "Gone", Start: at, End: at, StartedAt: at, Dir: "Gone"})
	app.store.UpdateRecording(ctx, store.Recording{ID: gone, ShowFile: "Gone/Gone.mp3"})
	f := newFakeRecording(t, testConfig())
	f.catalogID = id
	app.mu.Lock()
	app.active["x"] = f.recording
	app.mu.Unlock()
	for path, status := range map[string]int{
		fmt.Sprintf("/recordings/%d/play", gone): http.StatusNotFound, fmt.Sprintf("/recordings/%d/audio", gone): http.StatusNotFound,
		"/recordings/999/play": http.StatusNotFound, "/recordings/x/audio": http.StatusNotFound,
		fmt.Sprintf("/recordings/%d/play", id): http.StatusConflict, info.Audio: http.StatusConflict,
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != status {
			t.Errorf("%s: %s, want %d", path, resp.Status, status)
		}
	}
}

// A recording's details page lists its airing, files (with their sizes) and
// songs; the Recordings page links to it.
func TestWebRecordingDetails(t *testing.T) {
	app, srv := testApp(t)
	app.cfg.Destination = t.TempDir()
	ctx := t.Context()
	at := time.Date(2026, 10, 6, 19, 0, 0, 0, app.cfg.Location)
	id, err := app.store.AddRecording(ctx, store.Recording{Airing: "a", Title: "Train To Skaville", Category: "Specialty", Start: at, End: at.Add(time.Hour), StartedAt: at.Add(-time.Minute), Dir: "Train To Skaville"})
	if err != nil {
		t.Fatal(err)
	}
	app.store.UpdateRecording(ctx, store.Recording{ID: id, DJ: "brian tomsic", URL: "https://spinitron.com/WCBN/show/264320/Train-To-Skaville", FinishedAt: at.Add(62 * time.Minute),
		ShowFile: "Train To Skaville/Train To Skaville - 2026-10-06.mp3", CueFile: "Train To Skaville/Train To Skaville - 2026-10-06.cue", DurationMs: 3_780_000, Bytes: 151_200_000})
	app.store.SetSongs(ctx, id, []store.Song{
		{Number: 1, Title: "over the river", Artist: "justin hinds & the dominoes", PlayedAt: at, StartMs: 60_000, DurationMs: 61_000},
		{Number: 2, Title: "father killam", Artist: "derrick morgan", Album: "Father Killam", Label: "Island", Year: 1967, PlayedAt: at.Add(time.Minute), StartMs: 121_000, DurationMs: 3_659_000},
	}, "https://spinitron.com/WCBN/pl/23086806/Train-To-Skaville")
	dir := filepath.Join(app.cfg.Destination, "Train To Skaville")
	os.MkdirAll(dir, 0o755)
	for name, size := range map[string]int{"Train To Skaville - 2026-10-06.mp3": 2_500_000, "Train To Skaville - 2026-10-06.cue": 300, "Train To Skaville - 2026-10-06 2.mp3": 1_000_000} {
		os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644)
	}

	app.store.SetSetting(ctx, "station_logo", "https://example.org/logo.jpg")
	if page := get(t, srv.URL+"/recordings"); !strings.Contains(page, fmt.Sprintf(`<a class="rec-link" href="/recordings/%d">`, id)) {
		t.Error("no link to the details page")
	}
	page := get(t, fmt.Sprintf("%s/recordings/%d", srv.URL, id))
	for _, want := range []string{
		`<main id="page" data-page="recording"`, `<a href="/recordings" aria-current="page">Recordings</a>`, "<title>Train To Skaville · WCBN Recorder</title>",
		"with brian tomsic", fmt.Sprintf(`class="listen play-rec" data-id="%d"`, id),
		`<img class="art art-lg" src="https://example.org/logo.jpg"`, // the show has no image of its own
		"<dt>Aired</dt><dd>Tue Oct 6, 7:00 PM – 8:00 PM</dd>", "<dt>Recorded</dt><dd>Tue Oct 6, 6:59 PM – 8:02 PM</dd>",
		"<dt>Duration</dt><dd>1:03:00</dd>", "<dd>151.2 MB · 320 kbps</dd>", "<dd>Specialty</dd>",
		`<a href="https://spinitron.com/WCBN/pl/23086806/Train-To-Skaville" target="_blank" rel="noopener">Playlist</a>`,
		`<thead><tr><th>Name</th><th>Kind</th><th class="num">Size</th></tr></thead>`,
		`<td class="path">Train To Skaville - 2026-10-06.mp3</td><td>Show file (run 1)</td><td class="num">2.5 MB</td>`,
		`<td>CUE sheet (run 1)</td><td class="num">300 bytes</td>`,
		`<td class="path">Train To Skaville - 2026-10-06 2.mp3</td><td>Show file (run 2)</td><td class="num">1.0 MB</td>`,
		fmt.Sprintf(`<tr><td class="num">2</td>
      <td class="num"><button type="button" class="link song-jump" data-id="%d" data-start="121000" title="Play from this song">2:01</button></td>
      <td class="num">1:00:59</td><td class="nowrap">7:01 PM</td><td>father killam</td><td>derrick morgan</td>
      <td><span class="tip" title="Island · 1967">Father Killam</span></td></tr>`, id),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("details page missing %q", want)
		}
	}
	if strings.Contains(page, "<details") {
		t.Error("the details shouldn't be a toggle")
	}
	if songs, del := strings.Index(page, "<h3>Songs</h3>"), strings.Index(page, `class="rec-delete-form"`); del < songs || songs < 0 {
		t.Error("Delete should be at the bottom, after the songs")
	}
	for _, path := range []string{"/recordings/999", "/recordings/x"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %s", path, resp.Status)
		}
	}
}
