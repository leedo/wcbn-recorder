package recorder

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"wcbn-recorder/internal/mpeg"
)

// TestGaplessDecode encodes real MP3 audio (which uses the bit reservoir),
// records it with song changes, and checks that decoding the song files one
// after another gives exactly the same samples as decoding the whole show.
func TestGaplessDecode(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp3")
	enc := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "anoisesrc=d=12:c=pink:r=44100:a=0.3",
		"-ac", "2", "-c:a", "libmp3lame", "-b:a", "320k", "-write_xing", "0", "-id3v2_version", "0", "-f", "mp3", src)
	if out, err := enc.CombinedOutput(); err != nil {
		t.Skipf("can't encode MP3 (%v): %s", err, out)
	}
	stream, _ := os.ReadFile(src)

	decode := func(path string) []byte {
		t.Helper()
		out, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "s16le", "-ac", "2", "-").Output()
		if err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		return out
	}

	run := func(gapless bool) (whole, joined []byte) {
		cfg := testConfig()
		cfg.Gapless = gapless
		f := newFakeRecording(t, cfg)
		// Feed the stream in 16000-byte chunks, like ICY metadata intervals,
		// changing the title every few chunks.
		for i, n := 0, 0; i < len(stream); i, n = i+16000, n+1 {
			if n%7 == 0 {
				f.title(string(rune('A'+n/7)) + " by Band")
			}
			f.Write(stream[i:min(i+16000, len(stream))])
		}
		out := f.out
		if err := f.finalize(t.Context(), f.part, out); err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join(out.trackDir, "[0-9]*.mp3")) // not the hidden show file
		sort.Strings(files)
		if len(files) < 4 {
			t.Fatalf("only %d song files", len(files))
		}
		for _, file := range files {
			joined = append(joined, decode(file)...)
		}
		return decode(out.showFile), joined
	}

	whole, joined := run(true)
	// The first song has no earlier audio to overlap, so the player's own
	// 529-sample decoder delay is skipped there; everything after is exact.
	whole = whole[mpeg.DecoderDelay*4:]
	if !bytes.Equal(joined, whole) {
		i := 0
		for i < min(len(joined), len(whole)) && joined[i] == whole[i] {
			i++
		}
		t.Fatalf("gapless songs differ from the whole show: %d vs %d bytes, first difference at sample %d", len(joined), len(whole), i/4)
	}

	// Without the overlap, each cut damages the start of the next song.
	whole, joined = run(false)
	if len(joined) != len(whole) {
		t.Fatalf("plain cuts: %d vs %d bytes", len(joined), len(whole))
	}
	if bytes.Equal(joined, whole) {
		t.Error("expected plain cuts to differ from the whole show; the gapless test may not be exercising the bit reservoir")
	}
}
