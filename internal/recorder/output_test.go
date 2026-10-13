package recorder

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wcbn-recorder/internal/config"
	"wcbn-recorder/internal/mpeg"
	"wcbn-recorder/internal/spinitron"
)

// fakeRecording writes frames to a part file the way streamOnce does, with
// stream titles arriving between chunks of audio.
type fakeRecording struct {
	*recording
	part  string
	audio []byte // every frame written, in order
}

func newFakeRecording(t *testing.T, cfg *config.Config) *fakeRecording {
	t.Helper()
	dir := t.TempDir()
	part := filepath.Join(dir, ".show.mp3.part")
	f, err := os.Create(part)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	start := time.Date(2026, 10, 13, 23, 0, 0, 0, time.UTC)
	job := Job{Program: "Train To Skaville", Event: spinitron.Event{
		Title: "Train To Skaville", DJ: "brian tomsic", Category: "Specialty",
		Start: start, End: start.Add(time.Hour),
	}}
	cfg.Destination = dir
	rec := &recording{cfg: cfg, job: job, part: part, file: f, audio: mpeg.NewFrameWriter(f),
		out: outputPaths(cfg, dir, "Train To Skaville - 2026-10-13")}
	rec.out.part = part // the fake writes its part file before choosing paths
	rec.startSongWriter(t.Context())
	t.Cleanup(rec.stopSongWriter)
	return &fakeRecording{recording: rec, part: part}
}

func (f *fakeRecording) frames(fill byte, n int) {
	for range n {
		fr := mp3Frame(fill)
		f.audio = append(f.audio, fr...)
		f.recording.Write(fr) // the locked path the stream uses
	}
}

func (f *fakeRecording) title(s string) {
	f.onMetadata("StreamTitle='" + s + "';StreamUrl='';")
}

func testConfig() *config.Config {
	loc, _ := time.LoadLocation("America/Detroit")
	return &config.Config{Location: loc, ShowFile: true, TrackFiles: true, StreamURL: "https://example.org/stream.mp3"}
}

func readTag(t *testing.T, path string) (map[string]string, []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:3]) != "ID3" {
		t.Fatalf("%s: no ID3 tag", path)
	}
	size := unsynchsafe(b[6:10])
	tags := map[string]string{}
	for _, fr := range parseFrames(t, b[10:10+size]) {
		switch {
		case fr.id[0] == 'T' && fr.id != "TXXX":
			tags[fr.id] = string(fr.body[1:])
		case fr.id == "TXXX":
			desc, val, _ := strings.Cut(string(fr.body[1:]), "\x00")
			tags["TXXX:"+desc] = val
		}
	}
	return tags, b[10+size:]
}

func TestWriteTracks(t *testing.T) {
	f := newFakeRecording(t, testConfig())
	f.title("Pressure Drop by Toots & the Maytals on Train To Skaville with brian tomsic")
	f.frames(1, 3)
	f.title("Stand by Me by Ben E. King on Train To Skaville with brian tomsic")
	f.title("A Message to You, Rudy by The Specials on Train To Skaville with brian tomsic") // no audio before next title
	f.frames(2, 2)
	f.title("WCBN station ID")
	f.frames(3, 4)

	// The finished songs are saved while the show is still recording.
	out := f.out
	waitForFiles(t, out.trackDir, "01 - Toots & the Maytals - Pressure Drop.mp3", "02 - The Specials - A Message to You, Rudy.mp3")
	if _, err := os.Stat(filepath.Join(out.trackDir, "03 - WCBN station ID.mp3")); !os.IsNotExist(err) {
		t.Error("the song still playing was written early")
	}
	// The playlist is updated just after each song file is saved.
	m3uPath := filepath.Join(out.trackDir, out.base+".m3u8")
	var m3u []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if m3u, _ = os.ReadFile(m3uPath); strings.Count(string(m3u), ".mp3") == 2 {
			break
		}
	}
	if strings.Count(string(m3u), ".mp3") != 2 {
		t.Errorf("playlist mid-show:\n%s", m3u)
	}

	if err := f.finalize(t.Context(), f.part, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.part); !os.IsNotExist(err) {
		t.Error("part file was not removed")
	}

	// The whole show keeps every frame, hidden in the song folder; nothing is
	// left next to the folder.
	if out.showFile != filepath.Join(out.trackDir, ".Train To Skaville - 2026-10-13.mp3") {
		t.Errorf("show file at %s", out.showFile)
	}
	if left, _ := filepath.Glob(filepath.Join(f.cfg.Destination, "*.mp3")); len(left) != 0 {
		t.Errorf("files left next to the folder: %v", left)
	}
	if _, audio := readTag(t, out.showFile); !bytes.Equal(audio, f.audio) {
		t.Errorf("show file audio: %d bytes, want %d", len(audio), len(f.audio))
	}

	entries, _ := os.ReadDir(out.trackDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	wantNames := []string{
		".Train To Skaville - 2026-10-13.cue", // CUE sheet for the whole show
		".Train To Skaville - 2026-10-13.mp3", // the whole show, hidden alongside the songs
		"01 - Toots & the Maytals - Pressure Drop.mp3",
		"02 - The Specials - A Message to You, Rudy.mp3", // "Stand by Me" had no audio
		"03 - WCBN station ID.mp3",
		"Train To Skaville - 2026-10-13.m3u8",
	}
	if strings.Join(names, "|") != strings.Join(wantNames, "|") {
		t.Fatalf("files:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(wantNames, "\n"))
	}

	frameLen := len(mp3Frame(0))
	wantAudio := [][]byte{f.audio[:3*frameLen], f.audio[3*frameLen : 5*frameLen], f.audio[5*frameLen:]}
	for i, name := range wantNames[2:5] {
		tags, audio := readTag(t, filepath.Join(out.trackDir, name))
		if !bytes.Equal(audio, wantAudio[i]) {
			t.Errorf("%s: %d audio bytes, want %d", name, len(audio), len(wantAudio[i]))
		}
		if tags["TRCK"] != []string{"1", "2", "3"}[i] || tags["TALB"] != "Train To Skaville (2026-10-13)" ||
			tags["TPE2"] != "brian tomsic" || tags["TDRC"] != "2026-10-13" || tags["TCON"] != "Specialty" {
			t.Errorf("%s tags: %v", name, tags)
		}
		if i == 0 && (tags["TIT2"] != "Pressure Drop" || tags["TPE1"] != "Toots & the Maytals" || tags["TLEN"] != "78") {
			t.Errorf("%s tags: %v", name, tags)
		}
		if i == 2 && (tags["TIT2"] != "WCBN station ID" || tags["TPE1"] != "") {
			t.Errorf("%s tags: %v", name, tags)
		}
	}

	cue, _ := os.ReadFile(filepath.Join(out.trackDir, wantNames[0]))
	wantCue := `REM GENRE "Specialty"
REM DATE 2026-10-13
PERFORMER "brian tomsic"
TITLE "Train To Skaville (2026-10-13)"
FILE ".Train To Skaville - 2026-10-13.mp3" MP3
  TRACK 01 AUDIO
    TITLE "Pressure Drop"
    PERFORMER "Toots & the Maytals"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "A Message to You, Rudy"
    PERFORMER "The Specials"
    INDEX 01 00:00:05
  TRACK 03 AUDIO
    TITLE "WCBN station ID"
    INDEX 01 00:00:09
`
	if string(cue) != wantCue {
		t.Errorf("CUE sheet:\n%s\nwant:\n%s", cue, wantCue)
	}

	m3u, _ = os.ReadFile(filepath.Join(out.trackDir, wantNames[5]))
	wantM3U := "#EXTM3U\n#PLAYLIST:Train To Skaville (2026-10-13)\n" +
		"#EXTINF:0,Toots & the Maytals - Pressure Drop\n01 - Toots & the Maytals - Pressure Drop.mp3\n" +
		"#EXTINF:0,The Specials - A Message to You, Rudy\n02 - The Specials - A Message to You, Rudy.mp3\n" +
		"#EXTINF:0,WCBN station ID\n03 - WCBN station ID.mp3\n"
	if string(m3u) != wantM3U {
		t.Errorf("playlist:\n%s\nwant:\n%s", m3u, wantM3U)
	}
}

func TestWriteTracksNoTitles(t *testing.T) {
	cfg := testConfig()
	cfg.ShowFile = false
	f := newFakeRecording(t, cfg)
	f.frames(7, 5)
	out := f.out
	if err := f.finalize(t.Context(), f.part, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out.showFile); !os.IsNotExist(err) {
		t.Error("show file written although show_file is off")
	}
	tags, audio := readTag(t, filepath.Join(out.trackDir, "01 - Train To Skaville.mp3"))
	if !bytes.Equal(audio, f.audio) || tags["TRCK"] != "1" {
		t.Errorf("single track: %d bytes, tags %v", len(audio), tags)
	}
}

func TestSplitSong(t *testing.T) {
	for _, tc := range []struct{ in, title, artist string }{
		{"Pressure Drop by Toots & the Maytals", "Pressure Drop", "Toots & the Maytals"},
		{"Stand by Me by Ben E. King", "Stand by Me", "Ben E. King"},
		{"WCBN station ID", "WCBN station ID", ""},
		{"by the way", "by the way", ""},
	} {
		if title, artist := splitSong(tc.in); title != tc.title || artist != tc.artist {
			t.Errorf("splitSong(%q) = %q, %q", tc.in, title, artist)
		}
	}
}

func TestTruncateName(t *testing.T) {
	if got := truncateName(strings.Repeat("é", 100), 151); len(got) != 150 || !strings.HasPrefix(got, "é") {
		t.Errorf("truncated to %d bytes", len(got))
	}
	if got := truncateName("short", 150); got != "short" {
		t.Errorf("got %q", got)
	}
}

func TestChooseBase(t *testing.T) {
	cfg := &config.Config{TrackFiles: true}
	dir := t.TempDir()
	name := "Show - 2026-10-13"
	start := func(airing string) (string, bool) {
		base, resume := chooseBase(cfg, dir, name, "1900", airing)
		out := outputPaths(cfg, dir, base)
		os.MkdirAll(out.trackDir, 0o755)
		os.WriteFile(markerPath(out), []byte(airing+"\n"), 0o644)
		return base, resume
	}
	if base, resume := start("show|100"); base != name || resume {
		t.Errorf("first airing: %q, resume %v", base, resume)
	}
	// The same airing again (a restart) resumes in the same folder.
	if base, resume := start("show|100"); base != name || !resume {
		t.Errorf("same airing: %q, resume %v", base, resume)
	}
	// Another airing that day gets its own folder, and so does a third.
	if base, resume := start("show|200"); base != name+" 1900" || resume {
		t.Errorf("second airing: %q, resume %v", base, resume)
	}
	if base, _ := start("show|300"); base != name+" 1900-2" {
		t.Errorf("third airing: %q", base)
	}
	// ...and each resumes in its own folder.
	if base, resume := chooseBase(cfg, dir, name, "1900", "show|200"); base != name+" 1900" || !resume {
		t.Errorf("second airing again: %q, resume %v", base, resume)
	}
	// A folder from before markers existed is never taken over.
	other := t.TempDir()
	os.Mkdir(filepath.Join(other, name), 0o755)
	if base, resume := chooseBase(cfg, other, name, "1900", "show|100"); base != name+" 1900" || resume {
		t.Errorf("unmarked folder: %q, resume %v", base, resume)
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".Show - 2026-10-13.mp3")
	if got := uniquePath(p, ".mp3"); got != p {
		t.Errorf("free name changed: %s", got)
	}
	os.WriteFile(p, nil, 0o644)
	if got := uniquePath(p, ".mp3"); got != filepath.Join(dir, ".Show - 2026-10-13 2.mp3") {
		t.Errorf("got %s", got)
	}
	os.WriteFile(filepath.Join(dir, ".Show - 2026-10-13 2.mp3"), nil, 0o644)
	if got := uniquePath(p, ".mp3"); got != filepath.Join(dir, ".Show - 2026-10-13 3.mp3") {
		t.Errorf("got %s", got)
	}
	part := filepath.Join(dir, ".Show.mp3.part")
	os.WriteFile(part, nil, 0o644)
	if got := uniquePath(part, ".mp3.part"); got != filepath.Join(dir, ".Show 2.mp3.part") {
		t.Errorf("part: %s", got)
	}
}

func TestResumeHelpers(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"01 - A - One.mp3", "02 - B - Two.mp3", "10 - C - Ten.mp3", ".hidden 99.mp3", "cover.jpg", "Show.m3u8"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	if n := lastSongNumber(dir); n != 10 {
		t.Errorf("last song = %d", n)
	}
	m3u := filepath.Join(dir, "Show.m3u8")
	os.WriteFile(m3u, []byte("#EXTM3U\n#PLAYLIST:Show (2026-10-13)\n#EXTINF:180,A - One\n01 - A - One.mp3\n"), 0o644)
	if got := strings.Join(playlistEntries(m3u), "|"); got != "#EXTINF:180,A - One|01 - A - One.mp3" {
		t.Errorf("entries = %q", got)
	}
	if playlistEntries(filepath.Join(dir, "missing.m3u8")) != nil {
		t.Error("missing playlist should have no entries")
	}
}

// waitForFiles waits for the background song writer to produce files.
func waitForFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, name := range names {
		for {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				entries, _ := os.ReadDir(dir)
				t.Fatalf("%s not written; folder has %v", name, entries)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestOutputPaths(t *testing.T) {
	cfg := &config.Config{TrackFiles: true}
	if out := outputPaths(cfg, "/r/Club H", "Club H - 2026-10-13"); out.showFile != "/r/Club H/Club H - 2026-10-13/.Club H - 2026-10-13.mp3" ||
		out.trackDir != "/r/Club H/Club H - 2026-10-13" || out.part != "/r/Club H/Club H - 2026-10-13/.Club H - 2026-10-13.mp3.part" {
		t.Errorf("with song files: %+v", out)
	}
	cfg.TrackFiles = false
	if out := outputPaths(cfg, "/r/Club H", "Club H - 2026-10-13"); out.showFile != "/r/Club H/Club H - 2026-10-13.mp3" ||
		out.part != "/r/Club H/.Club H - 2026-10-13.mp3.part" {
		t.Errorf("without song files: %+v", out)
	}
}

func TestCueFormatting(t *testing.T) {
	for ms, want := range map[int64]string{0: "00:00:00", 1000: "00:01:00", 61_500: "01:01:37", 3*3600_000 + 5*60_000: "185:00:00"} {
		if got := cueTime(ms); got != want {
			t.Errorf("cueTime(%d) = %s, want %s", ms, got, want)
		}
	}
	if got := cueText("Say \"Hello\"\n  Again"); got != `"Say 'Hello' Again"` {
		t.Errorf("cueText = %s", got)
	}
	if got := cuePath("/r/Show/.Show - 2026-10-13 2.mp3"); got != "/r/Show/.Show - 2026-10-13 2.cue" {
		t.Errorf("cuePath = %s", got)
	}
}

// With song files off, the whole show is the visible main file, and so is its CUE sheet.
func TestCueWithoutSongFiles(t *testing.T) {
	cfg := testConfig()
	cfg.TrackFiles = false
	f := newFakeRecording(t, cfg)
	f.title("Pressure Drop by Toots & the Maytals")
	f.frames(1, 3)
	if err := f.finalize(t.Context(), f.part, f.out); err != nil {
		t.Fatal(err)
	}
	cue, err := os.ReadFile(filepath.Join(f.cfg.Destination, "Train To Skaville - 2026-10-13.cue"))
	if err != nil || !strings.Contains(string(cue), `FILE "Train To Skaville - 2026-10-13.mp3" MP3`) {
		t.Errorf("CUE sheet: %v\n%s", err, cue)
	}
}
