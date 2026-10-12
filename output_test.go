package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRecording writes frames to a part file the way streamOnce does, with
// stream titles arriving between chunks of audio.
type fakeRecording struct {
	*recording
	part  string
	audio []byte // every frame written, in order
}

func newFakeRecording(t *testing.T, cfg *Config) *fakeRecording {
	t.Helper()
	dir := t.TempDir()
	part := filepath.Join(dir, ".show.mp3.part")
	f, err := os.Create(part)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	start := time.Date(2026, 10, 13, 23, 0, 0, 0, time.UTC)
	job := Job{Program: "Train To Skaville", Event: Event{
		Title: "Train To Skaville", DJ: "brian tomsic", Category: "Specialty",
		Start: start, End: start.Add(time.Hour),
	}}
	cfg.Destination = dir
	base := "Train To Skaville - 2026-10-13"
	rec := &recording{cfg: cfg, job: job, part: part, file: f, audio: &frameWriter{w: f}, out: outputs{
		base:     base,
		showFile: filepath.Join(dir, base+".mp3"),
		trackDir: filepath.Join(dir, base),
	}}
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

func testConfig() *Config {
	loc, _ := time.LoadLocation("America/Detroit")
	return &Config{location: loc, ShowFile: true, TrackFiles: true, StreamURL: "https://example.org/stream.mp3"}
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
	if m3u, _ := os.ReadFile(filepath.Join(out.trackDir, out.base+".m3u8")); strings.Count(string(m3u), ".mp3") != 2 {
		t.Errorf("playlist mid-show:\n%s", m3u)
	}

	if err := f.finalize(t.Context(), f.part, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.part); !os.IsNotExist(err) {
		t.Error("part file was not removed")
	}

	// The whole show keeps every frame.
	if _, audio := readTag(t, out.showFile); !bytes.Equal(audio, f.audio) {
		t.Errorf("show file audio: %d bytes, want %d", len(audio), len(f.audio))
	}

	entries, _ := os.ReadDir(out.trackDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	wantNames := []string{
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
	for i, name := range wantNames[:3] {
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

	m3u, _ := os.ReadFile(filepath.Join(out.trackDir, wantNames[3]))
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

func TestUniqueBase(t *testing.T) {
	dir := t.TempDir()
	if got := uniqueBase(dir, "Show - 2026-10-13", "1900"); got != "Show - 2026-10-13" {
		t.Errorf("got %q", got)
	}
	os.Mkdir(filepath.Join(dir, "Show - 2026-10-13"), 0o755) // only the track folder exists
	if got := uniqueBase(dir, "Show - 2026-10-13", "1900"); got != "Show - 2026-10-13 1900" {
		t.Errorf("got %q", got)
	}
	os.WriteFile(filepath.Join(dir, "Show - 2026-10-13 1900.mp3"), nil, 0o644)
	if got := uniqueBase(dir, "Show - 2026-10-13", "1900"); got != "Show - 2026-10-13 1900-2" {
		t.Errorf("got %q", got)
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
