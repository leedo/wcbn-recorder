package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

func TestCRC16(t *testing.T) {
	if got := crc16([]byte("123456789")); got != 0xBB3D { // CRC-16/ARC check value
		t.Errorf("crc16 = %#04x, want 0xbb3d", got)
	}
}

func TestInfoFrame(t *testing.T) {
	g := gaplessInfo{header: [4]byte{0xFF, 0xFA, 0xE2, 0x44}, frames: 100, bytes: 104400, lead: 2}
	f, err := infoFrame(g)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := parseFrameHeader(f)
	if !ok || h.length != len(f) || len(f) != 1044 {
		t.Fatalf("not a valid frame: %+v, %d bytes", h, len(f))
	}
	if f[1]&1 != 1 || f[2]&2 != 0 {
		t.Error("Info frame should have no CRC and no padding")
	}
	x := f[36:] // after the header and 32 bytes of MPEG-1 stereo side info
	if string(x[:4]) != "Info" || binary.BigEndian.Uint32(x[4:]) != 0x0F ||
		binary.BigEndian.Uint32(x[8:]) != 100 || binary.BigEndian.Uint32(x[12:]) != 1044+104400 {
		t.Errorf("bad Xing header % x", x[:16])
	}
	l := x[120:]
	delay := int(l[21])<<4 | int(l[22])>>4
	padding := int(l[22]&0x0F)<<8 | int(l[23])
	if string(l[:9]) != "LAME3.100" || delay != 2*1152-529 || padding != 529 {
		t.Errorf("LAME tag %q delay %d padding %d", l[:9], delay, padding)
	}
	if got := binary.BigEndian.Uint16(l[34:]); got != crc16(f[:36+120+34]) {
		t.Errorf("tag CRC %#04x doesn't match", got)
	}
	if s := iTunSMPB(g, 1152); s[:28] != " 00000000 00000900 00000000 " { // priming 1775+529 = 2304
		t.Errorf("iTunSMPB = %q", s)
	}
	// The first song of a recording has no overlap; nothing extra is skipped.
	if g := (gaplessInfo{lead: 0}); g.encoderDelay(1152) != 0 {
		t.Error("lead 0 should give delay 0")
	}
}

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
		out := outputs{base: "show"}
		out.showFile = filepath.Join(f.cfg.Destination, "show.mp3")
		out.trackDir = filepath.Join(f.cfg.Destination, "show")
		if err := f.finalize(t.Context(), f.part, out); err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join(out.trackDir, "*.mp3"))
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
	whole = whole[decoderDelay*4:]
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
