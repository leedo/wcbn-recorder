package mpeg

import (
	"bytes"
	"testing"
)

// mp3Frame returns a 320 kbps, 44.1 kHz MPEG-1 Layer III frame (1044 bytes)
// whose payload is filled with fill.
func mp3Frame(fill byte) []byte {
	f := bytes.Repeat([]byte{fill}, 1044)
	copy(f, []byte{0xFF, 0xFB, 0xE0, 0x44})
	return f
}

func TestParseFrameHeader(t *testing.T) {
	h, ok := ParseFrameHeader(mp3Frame(0))
	if !ok || h.Length != 1044 || h.Samples != 1152 || h.SampleRate != 44100 || h.Layer != 3 {
		t.Fatalf("got %+v, %v", h, ok)
	}
	// Padding bit adds a byte.
	if h, _ := ParseFrameHeader([]byte{0xFF, 0xFB, 0xE2, 0x44}); h.Length != 1045 {
		t.Errorf("padded length = %d", h.Length)
	}
	// MPEG-2 Layer III, 64 kbps, 22.05 kHz.
	if h, ok := ParseFrameHeader([]byte{0xFF, 0xF3, 0x80, 0x44}); !ok || h.Samples != 576 || h.Length != 72*64000/22050 {
		t.Errorf("MPEG-2 header = %+v", h)
	}
	for _, bad := range [][]byte{
		{0xFF, 0xFB, 0xF0, 0x44}, // bitrate index 15
		{0xFF, 0xFB, 0x0C, 0x44}, // free bitrate, reserved sample rate
		{0xFF, 0xEB, 0xE0, 0x44}, // reserved version
		{0xFF, 0xF9, 0xE0, 0x44}, // reserved layer
		{0xFE, 0xFB, 0xE0, 0x44}, // no sync
	} {
		if _, ok := ParseFrameHeader(bad); ok {
			t.Errorf("accepted bad header % x", bad)
		}
	}
}

func TestFrameWriter(t *testing.T) {
	var out bytes.Buffer
	fw := &FrameWriter{w: &out}

	// Connection 1: starts mid-frame with junk, including a valid-looking
	// header that isn't followed by another header one frame later, then two
	// whole frames, then is cut off partway through a third.
	conn1 := []byte{0x12, 0xFF, 0xFB, 0xE0, 0x44, 0x99, 0x98}
	conn1 = append(conn1, mp3Frame(1)...)
	conn1 = append(conn1, mp3Frame(2)...)
	conn1 = append(conn1, mp3Frame(3)[:500]...)
	// Write in awkward chunk sizes, like a network read would.
	for len(conn1) > 0 {
		n := min(333, len(conn1))
		fw.Write(conn1[:n])
		conn1 = conn1[n:]
	}

	// Connection 2: the stream resumes mid-frame.
	fw.Reset()
	conn2 := append(mp3Frame(9)[700:], mp3Frame(4)...)
	conn2 = append(conn2, mp3Frame(5)...)
	conn2 = append(conn2, mp3Frame(6)[:4]...) // header of the next frame confirms frame 5
	fw.Write(conn2)

	want := append(append(append(mp3Frame(1), mp3Frame(2)...), mp3Frame(4)...), mp3Frame(5)...)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("wrote %d bytes, want %d (frames 1,2,4,5)", out.Len(), len(want))
	}
	if fw.written != int64(len(want)) {
		t.Errorf("written = %d", fw.written)
	}
	// 4 frames x 1152 samples at 44.1 kHz = 104 ms.
	if ms := fw.DurationMs(); ms != 4*1152*1000/44100 {
		t.Errorf("duration = %d ms", ms)
	}
}

// Splitting the input at every possible point must not change the output.
func TestFrameWriterChunking(t *testing.T) {
	var in []byte
	for i := range 5 {
		in = append(in, mp3Frame(byte(i))...)
	}
	want := in // once synced, each frame is written as soon as it's complete
	for chunk := 1; chunk <= 1100; chunk++ {
		var out bytes.Buffer
		fw := &FrameWriter{w: &out}
		for b := in; len(b) > 0; {
			n := min(chunk, len(b))
			fw.Write(b[:n])
			b = b[n:]
		}
		if !bytes.Equal(out.Bytes(), want) {
			t.Fatalf("chunk size %d: wrote %d bytes, want %d", chunk, out.Len(), len(want))
		}
	}
}
