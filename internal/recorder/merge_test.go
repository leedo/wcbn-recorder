package recorder

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wcbn-recorder/internal/mpeg"
	"wcbn-recorder/internal/store"
)

// resumeAfter records a first run, then a second resuming it gap after the
// first one's audio ended, by the earlier show file's time (or, with
// earlierStart set, the database's start time and the audio's length), and
// returns both.
func resumeAfter(t *testing.T, gap time.Duration, earlierStart bool) (first, second *fakeRecording) {
	t.Helper()
	first = newFakeRecording(t, testConfig())
	first.title("Pressure Drop by Toots & the Maytals")
	first.frames(1, 10)
	if err := first.finalize(t.Context(), first.part, first.out); err != nil {
		t.Fatal(err)
	}
	ended := time.Date(2026, 10, 13, 19, 20, 0, 0, time.UTC)
	os.Chtimes(first.out.showFile, ended, ended)

	second = newFakeRecording(t, testConfig())
	second.cfg.Destination = first.cfg.Destination
	second.out, second.resumed = first.out, true
	second.started = ended.Add(gap)
	if earlierStart {
		// Started earlier than the file's time suggests, by an hour.
		earlierMs := time.Duration(10*1152*1000/44100) * time.Millisecond
		second.earlier = store.Recording{ID: 1, StartedAt: ended.Add(-time.Hour - earlierMs)}
	}
	second.title("A Message to You, Rudy by The Specials")
	second.frames(2, 5)
	if err := second.finalize(t.Context(), second.part, second.out); err != nil {
		t.Fatal(err)
	}
	return first, second
}

// A resumed run's audio is added to the earlier run's show file, after
// silence as long as the gap, and the CUE sheet has both runs' songs.
func TestMergeShowFile(t *testing.T) {
	first, second := resumeAfter(t, 5*time.Second, false)
	files, _ := filepath.Glob(filepath.Join(first.cfg.Destination, "*.mp3"))
	if len(files) != 1 || !second.merged || second.showOut != first.out.showFile {
		t.Fatalf("files %v, merged %v into %s", files, second.merged, second.showOut)
	}
	silent, _, _ := mpeg.SilentFrame(mp3Frame(0))
	frames := int(5 * 44100 / 1152) // 191, 4.99 s
	want := append(append(append([]byte{}, first.audio...), bytes.Repeat(silent, frames)...), second.audio...)
	tags, audio := readTag(t, first.out.showFile)
	if !bytes.Equal(audio, want) {
		t.Errorf("audio is %d bytes, want %d", len(audio), len(want))
	}
	earlierMs, silenceMs := int64(10*1152*1000/44100), int64(frames*1152*1000/44100)
	if second.prefixMs != earlierMs+silenceMs || second.prefixBytes != int64(len(first.audio)+frames*len(silent)) {
		t.Errorf("prefix %d ms, %d bytes", second.prefixMs, second.prefixBytes)
	}
	if want := fmt.Sprint(earlierMs + silenceMs + 5*1152*1000/44100); tags["TLEN"] != want { // the whole file's length
		t.Errorf("TLEN = %s", tags["TLEN"])
	}
	cue, _ := os.ReadFile(cuePath(first.out.showFile))
	for _, want := range []string{"TRACK 01 AUDIO\n    TITLE \"Pressure Drop\"\n    PERFORMER \"Toots & the Maytals\"\n    INDEX 01 00:00:00",
		"TRACK 02 AUDIO\n    TITLE \"A Message to You, Rudy\"\n    PERFORMER \"The Specials\"\n    INDEX 01 " + cueTime(earlierMs+silenceMs)} {
		if !strings.Contains(string(cue), want) {
			t.Errorf("CUE sheet missing %q:\n%s", want, cue)
		}
	}
	if strings.Count(string(cue), "TRACK") != 2 {
		t.Errorf("CUE sheet:\n%s", cue)
	}
}

// With the earlier run in the database, the gap is from its start and length.
func TestMergeShowFileGapFromDatabase(t *testing.T) {
	_, second := resumeAfter(t, 5*time.Second, true)
	silenceMs := second.prefixMs - int64(10*1152*1000/44100)
	if gap := time.Duration(silenceMs) * time.Millisecond; gap < time.Hour || gap > time.Hour+6*time.Second {
		t.Errorf("silence %s, want an hour and 5 s", gap)
	}
}

// Audio that can't be added to the earlier file gets a file of its own.
func TestMergeShowFileFallback(t *testing.T) {
	first := newFakeRecording(t, testConfig())
	first.frames(1, 10)
	if err := first.finalize(t.Context(), first.part, first.out); err != nil {
		t.Fatal(err)
	}
	second := newFakeRecording(t, testConfig())
	second.cfg.Destination = first.cfg.Destination
	second.out, second.resumed = first.out, true
	second.file.Write([]byte("not an mp3 frame")) // not through the FrameWriter
	if err := second.finalize(t.Context(), second.part, second.out); err != nil {
		t.Fatal(err)
	}
	if second.merged || second.showOut != strings.TrimSuffix(first.out.showFile, ".mp3")+" 2.mp3" {
		t.Errorf("merged %v, wrote %s", second.merged, second.showOut)
	}
}

func TestCueTracks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.cue")
	writeCueSheet(showMeta{Title: "Club H", Artist: "dj h"}, filepath.Join(filepath.Dir(path), "x.mp3"),
		[]cueTrack{{"One", "A", 0}, {"Two \"2\"", "", 61_500}})
	got := cueTracks(path)
	if len(got) != 2 || got[0] != (cueTrack{"One", "A", 0}) || got[1].title != "Two '2'" || got[1].startMs != 61_493 {
		t.Errorf("tracks = %+v", got)
	}
	if cueTracks(filepath.Join(t.TempDir(), "missing.cue")) != nil {
		t.Error("tracks from no CUE sheet")
	}
}
