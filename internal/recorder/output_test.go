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
		out: outputPaths(dir, "Train To Skaville - 2026-10-13")}
	rec.out.part = part // the fake writes its part file before choosing paths
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
	return &config.Config{Location: loc, StreamURL: "https://example.org/stream.mp3"}
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

func TestWriteShowFile(t *testing.T) {
	f := newFakeRecording(t, testConfig())
	f.title("Pressure Drop by Toots & the Maytals on Train To Skaville with brian tomsic")
	f.frames(1, 3)
	f.title("Stand by Me by Ben E. King on Train To Skaville with brian tomsic")
	f.title("A Message to You, Rudy by The Specials on Train To Skaville with brian tomsic") // no audio before next title
	f.frames(2, 2)
	f.title("WCBN station ID")
	f.frames(3, 4)

	out := f.out
	if err := f.finalize(t.Context(), f.part, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.part); !os.IsNotExist(err) {
		t.Error("part file was not removed")
	}

	// The whole show and its CUE sheet, visible in the program folder.
	entries, _ := os.ReadDir(f.cfg.Destination)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	wantNames := []string{"Train To Skaville - 2026-10-13.cue", "Train To Skaville - 2026-10-13.mp3"}
	if strings.Join(names, "|") != strings.Join(wantNames, "|") {
		t.Fatalf("files:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(wantNames, "\n"))
	}
	tags, audio := readTag(t, out.showFile)
	if !bytes.Equal(audio, f.audio) {
		t.Errorf("show file audio: %d bytes, want %d", len(audio), len(f.audio))
	}
	if tags["TIT2"] != "Train To Skaville (2026-10-13)" || tags["TPE1"] != "brian tomsic" || tags["TDRC"] != "2026-10-13" || tags["TCON"] != "Specialty" {
		t.Errorf("show file tags: %v", tags)
	}

	cue, _ := os.ReadFile(filepath.Join(f.cfg.Destination, wantNames[0]))
	wantCue := `REM GENRE "Specialty"
REM DATE 2026-10-13
PERFORMER "brian tomsic"
TITLE "Train To Skaville (2026-10-13)"
FILE "Train To Skaville - 2026-10-13.mp3" MP3
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
}

// With no stream titles, the whole show is one track.
func TestWriteShowFileNoTitles(t *testing.T) {
	f := newFakeRecording(t, testConfig())
	f.frames(7, 5)
	if err := f.finalize(t.Context(), f.part, f.out); err != nil {
		t.Fatal(err)
	}
	if _, audio := readTag(t, f.out.showFile); !bytes.Equal(audio, f.audio) {
		t.Errorf("show file audio: %d bytes, want %d", len(audio), len(f.audio))
	}
	if segs := f.segments(); len(segs) != 1 || segs[0].mark.title != "Train To Skaville" {
		t.Errorf("segments: %+v", segs)
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

func TestChooseBase(t *testing.T) {
	dir := t.TempDir()
	name := "Show - 2026-10-13"
	start := func(airing string) (string, bool) {
		base, resume := chooseBase(dir, name, "1900", airing)
		out := outputPaths(dir, base)
		os.WriteFile(out.showFile, nil, 0o644)
		os.WriteFile(markerPath(out), []byte(airing+"\n"), 0o644)
		return base, resume
	}
	if base, resume := start("show|100"); base != name || resume {
		t.Errorf("first airing: %q, resume %v", base, resume)
	}
	// The same airing again (a restart) resumes beside the same files.
	if base, resume := start("show|100"); base != name || !resume {
		t.Errorf("same airing: %q, resume %v", base, resume)
	}
	// Another airing that day gets its own name, and so does a third.
	if base, resume := start("show|200"); base != name+" 1900" || resume {
		t.Errorf("second airing: %q, resume %v", base, resume)
	}
	if base, _ := start("show|300"); base != name+" 1900-2" {
		t.Errorf("third airing: %q", base)
	}
	// ...and each resumes its own.
	if base, resume := chooseBase(dir, name, "1900", "show|200"); base != name+" 1900" || !resume {
		t.Errorf("second airing again: %q, resume %v", base, resume)
	}
	// Files from before markers existed are never taken over, nor is a song
	// folder from when songs were saved as their own files.
	for _, existing := range []string{name + ".mp3", name} {
		other := t.TempDir()
		if existing == name {
			os.Mkdir(filepath.Join(other, name), 0o755)
		} else {
			os.WriteFile(filepath.Join(other, existing), nil, 0o644)
		}
		if base, resume := chooseBase(other, name, "1900", "show|100"); base != name+" 1900" || resume {
			t.Errorf("unmarked %q: %q, resume %v", existing, base, resume)
		}
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

func TestOutputPaths(t *testing.T) {
	if out := outputPaths("/r/Club H", "Club H - 2026-10-13"); out.showFile != "/r/Club H/Club H - 2026-10-13.mp3" ||
		out.part != "/r/Club H/.Club H - 2026-10-13.mp3.part" {
		t.Errorf("%+v", out)
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
