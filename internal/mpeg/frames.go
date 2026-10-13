// Package mpeg handles MPEG audio (MP3) at the frame level: parsing frame
// headers, passing through only whole frames from a stream, and building the
// LAME/Xing "Info" frame that makes song files cut from a stream gapless.
package mpeg

import "io"

// FrameWriter passes only whole MPEG audio frames through to w. On each new
// connection (after Reset) it skips bytes until it finds a real frame header,
// confirmed by a second header where the first frame ends, and it drops any
// partial frame left when a connection breaks. It also counts samples, giving
// an exact duration for Chapter marks.
type FrameWriter struct {
	w          io.Writer
	buf        []byte
	synced     bool
	samples    int64
	sampleRate int
	written    int64   // bytes passed through
	recent     []int64 // start offsets of the last few frames on this connection
	bitrate    int     // kbps, of the latest frame
}

// overlapFrames is how many frames before a song's cut point are repeated at
// the start of its file, so the decoder has the bit reservoir data and filter
// history it needs to decode the first real frame correctly.
const overlapFrames = 2

// MaxFrameLen bounds the largest MPEG audio frame (Layer II/III at 448 kbps,
// 32 kHz, padded is under 2 KB), so we never buffer much while syncing.
const MaxFrameLen = 4096

func (f *FrameWriter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		if !f.synced {
			i, ok := FindSync(f.buf)
			if !ok {
				// Keep enough tail to finish checking a candidate next time.
				if keep := MaxFrameLen + 4; len(f.buf) > keep {
					f.buf = append(f.buf[:0], f.buf[len(f.buf)-keep:]...)
				}
				return len(p), nil
			}
			f.buf = f.buf[i:]
			f.synced = true
		}
		if len(f.buf) < 4 {
			break // wait for the rest of the next header
		}
		h, ok := ParseFrameHeader(f.buf)
		if !ok {
			f.synced = false // lost sync mid-stream; search again
			f.buf = f.buf[1:]
			continue
		}
		if len(f.buf) < h.Length {
			break
		}
		if _, err := f.w.Write(f.buf[:h.Length]); err != nil {
			return 0, err
		}
		if len(f.recent) == overlapFrames {
			f.recent = append(f.recent[:0], f.recent[1:]...)
		}
		f.recent = append(f.recent, f.written)
		f.written += int64(h.Length)
		f.samples += int64(h.Samples)
		f.sampleRate = h.SampleRate
		f.bitrate = h.Bitrate
		f.buf = f.buf[h.Length:]
	}
	// Compact so the buffer doesn't grow without bound.
	f.buf = append(make([]byte, 0, MaxFrameLen*2), f.buf...)
	return len(p), nil
}

// Reset discards any partial frame and resynchronizes on the next write.
func (f *FrameWriter) Reset() {
	f.buf = f.buf[:0]
	f.synced = false
	f.recent = f.recent[:0] // frames from an earlier connection aren't continuous with the next
}

// NewFrameWriter returns a FrameWriter that writes whole frames to w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Bitrate is the latest frame's bitrate in kbps, or 0 before any audio.
func (f *FrameWriter) Bitrate() int { return f.bitrate }

// Written is the number of bytes passed through so far.
func (f *FrameWriter) Written() int64 { return f.written }

// OverlapStart is where a song starting now should begin its file to include
// up to overlapFrames earlier frames from the same connection.
func (f *FrameWriter) OverlapStart() int64 {
	if len(f.recent) == 0 {
		return f.written
	}
	return f.recent[0]
}

// durationMs is the length of audio written so far.
func (f *FrameWriter) DurationMs() int64 {
	if f.sampleRate == 0 {
		return 0
	}
	return f.samples * 1000 / int64(f.sampleRate)
}

// FindSync returns the offset of the first frame header that is followed by
// another matching header, or false if more data is needed.
func FindSync(b []byte) (int, bool) {
	for i := 0; i+4 <= len(b); i++ {
		h, ok := ParseFrameHeader(b[i:])
		if !ok {
			continue
		}
		next := i + h.Length
		if next+4 > len(b) {
			return 0, false // need more data to confirm
		}
		if h2, ok := ParseFrameHeader(b[next:]); ok && h2.SampleRate == h.SampleRate && h2.Layer == h.Layer {
			return i, true
		}
	}
	return 0, false
}

type FrameHeader struct {
	Layer      int
	SampleRate int
	Bitrate    int // kbps
	Samples    int // per frame
	Length     int // bytes, including header
}

var (
	// kbps, indexed by [version is MPEG-1][layer-1][bitrate index]
	bitrates = [2][3][16]int{
		{ // MPEG-2 / 2.5
			{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		},
		{ // MPEG-1
			{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0},
			{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},
			{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
		},
	}
	// indexed by version bits (0 = MPEG-2.5, 2 = MPEG-2, 3 = MPEG-1)
	sampleRates = [4][3]int{{11025, 12000, 8000}, {}, {22050, 24000, 16000}, {44100, 48000, 32000}}
)

func ParseFrameHeader(b []byte) (FrameHeader, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return FrameHeader{}, false
	}
	version := int(b[1]>>3) & 3
	layer := 4 - int(b[1]>>1)&3 // 1, 2 or 3; 4 means reserved
	brIndex := int(b[2] >> 4)
	srIndex := int(b[2]>>2) & 3
	padding := int(b[2]>>1) & 1
	if version == 1 || layer == 4 || brIndex == 0 || brIndex == 15 || srIndex == 3 {
		return FrameHeader{}, false
	}
	mpeg1 := 0
	if version == 3 {
		mpeg1 = 1
	}
	bitrate := bitrates[mpeg1][layer-1][brIndex] * 1000
	sr := sampleRates[version][srIndex]

	h := FrameHeader{Layer: layer, SampleRate: sr, Bitrate: bitrate / 1000}
	switch {
	case layer == 1:
		h.Samples = 384
		h.Length = (12*bitrate/sr + padding) * 4
	case layer == 3 && mpeg1 == 0:
		h.Samples = 576
		h.Length = 72*bitrate/sr + padding
	default:
		h.Samples = 1152
		h.Length = 144*bitrate/sr + padding
	}
	return h, h.Length > 4
}
