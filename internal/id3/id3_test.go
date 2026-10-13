package id3

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func unsynchsafe(b []byte) int {
	return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3])
}

type parsedFrame struct {
	id   string
	body []byte
}

// parseFrames is a minimal ID3v2.4 reader used to check what we write.
func parseFrames(t *testing.T, b []byte) []parsedFrame {
	t.Helper()
	var out []parsedFrame
	for len(b) >= 10 {
		size := unsynchsafe(b[4:8])
		if 10+size > len(b) {
			t.Fatalf("frame %q size %d overruns tag", b[:4], size)
		}
		out = append(out, parsedFrame{string(b[:4]), b[10 : 10+size]})
		b = b[10+size:]
	}
	if len(b) != 0 {
		t.Fatalf("%d trailing bytes", len(b))
	}
	return out
}

func TestSynchsafe(t *testing.T) {
	for _, n := range []int{0, 127, 128, 255, 1 << 20, 1<<28 - 1} {
		b := synchsafe(n)
		for _, c := range b {
			if c&0x80 != 0 {
				t.Errorf("synchsafe(%d) has high bit set: %x", n, b)
			}
		}
		if got := unsynchsafe(b); got != n {
			t.Errorf("round trip %d -> %d", n, got)
		}
	}
}

func TestID3Tag(t *testing.T) {
	var tag Tag
	tag.Text("TIT2", "Train To Skaville (2026-10-13)")
	tag.Text("TPE1", "") // empty values are skipped
	tag.UserText("dj", "brian tomsic")
	tag.Comment("Tuesday\n\nhttps://example.org")
	tag.UserURL("show", "https://spinitron.com/WCBN/show/264320/Train-To-Skaville")
	tag.Chapters([]Chapter{{0, 1500, "Ska Song by Band"}, {1500, 4000, "Café ☕"}})

	var buf bytes.Buffer
	tag.WriteTo(&buf)
	b := buf.Bytes()
	if string(b[:6]) != "ID3\x04\x00\x00" {
		t.Fatalf("bad header % x", b[:6])
	}
	if size := unsynchsafe(b[6:10]); size != len(b)-10 {
		t.Fatalf("header size %d, want %d", size, len(b)-10)
	}

	frames := parseFrames(t, b[10:])
	var ids []string
	for _, f := range frames {
		ids = append(ids, f.id)
	}
	if got := strings.Join(ids, ","); got != "TIT2,TXXX,COMM,WXXX,CHAP,CHAP,CTOC" {
		t.Fatalf("frames = %s", got)
	}
	if got := string(frames[0].body); got != "\x03Train To Skaville (2026-10-13)" {
		t.Errorf("TIT2 = %q", got)
	}
	if got := string(frames[1].body); got != "\x03dj\x00brian tomsic" {
		t.Errorf("TXXX = %q", got)
	}
	if got := string(frames[2].body); got != "\x03eng\x00Tuesday\n\nhttps://example.org" {
		t.Errorf("COMM = %q", got)
	}

	// CHAP: element ID, start, end, 8 bytes of unused offsets, then a TIT2 subframe.
	ch := frames[5].body
	id, rest, _ := bytes.Cut(ch, []byte{0})
	if string(id) != "ch1" || binary.BigEndian.Uint32(rest[0:4]) != 1500 || binary.BigEndian.Uint32(rest[4:8]) != 4000 {
		t.Errorf("bad CHAP % x", ch)
	}
	sub := parseFrames(t, rest[16:])
	if len(sub) != 1 || sub[0].id != "TIT2" || string(sub[0].body) != "\x03Café ☕" {
		t.Errorf("bad CHAP subframes %+v", sub)
	}
	if got := string(frames[6].body); got != "toc\x00\x03\x02ch0\x00ch1\x00" {
		t.Errorf("CTOC = %q", got)
	}
}

func TestChapterLimit(t *testing.T) {
	var tag Tag
	tag.Chapters(make([]Chapter, 300))
	var buf bytes.Buffer
	tag.WriteTo(&buf)
	frames := parseFrames(t, buf.Bytes()[10:])
	if len(frames) != MaxChapters+1 || frames[len(frames)-1].body[5] != MaxChapters {
		t.Errorf("got %d frames", len(frames))
	}
}

func TestPictureFrame(t *testing.T) {
	testJPEG := append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, bytes.Repeat([]byte{7}, 64)...)
	var tag Tag
	tag.Picture("image/jpeg", testJPEG)
	var buf bytes.Buffer
	tag.WriteTo(&buf)
	frames := parseFrames(t, buf.Bytes()[10:])
	if len(frames) != 1 || frames[0].id != "APIC" {
		t.Fatalf("frames = %+v", frames)
	}
	want := append([]byte("\x00image/jpeg\x00\x03\x00"), testJPEG...)
	if !bytes.Equal(frames[0].body, want) {
		t.Errorf("APIC body % x", frames[0].body[:20])
	}
}
