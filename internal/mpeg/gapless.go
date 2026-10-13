package mpeg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Gapless playback of song files cut from one continuous stream.
//
// Cutting on a frame boundary keeps timing exact, but the first frame of a
// song can't be decoded on its own: Layer III frames borrow bits from earlier
// frames (the bit reservoir), and the decoder needs one frame of history. So
// each song file starts overlapFrames early, and an Info frame tells the
// player to skip those samples.
//
// The skip counts follow the LAME convention used by ffmpeg, mpg123,
// foobar2000 and others: players drop delay+529 samples at the start (529 is
// the decoder's own delay) and padding-529 at the end. With padding = 529
// nothing is dropped at the end, and the next file continues exactly there.

const DecoderDelay = 529

// GaplessInfo describes one song file's audio for its Info frame.
type GaplessInfo struct {
	Header [4]byte // header of the first audio frame; copied for format
	Frames int     // audio frames in the file, including overlap frames
	Bytes  int64   // audio bytes, not counting the Info frame
	Lead   int     // overlap frames at the start
}

// EncoderDelay is the number of samples, beyond the decoder's own delay, that
// players should skip at the start.
func (g GaplessInfo) EncoderDelay(samplesPerFrame int) int {
	return max(g.Lead*samplesPerFrame-DecoderDelay, 0)
}

// InfoFrame builds a silent MPEG frame carrying a Xing "Info" (CBR) header
// and a LAME extension with the encoder delay and padding.
func InfoFrame(g GaplessInfo) ([]byte, error) {
	h := g.Header
	h[1] |= 0x01  // no CRC
	h[2] &^= 0x02 // no padding
	fh, ok := ParseFrameHeader(h[:])
	if !ok || fh.Layer != 3 {
		return nil, errors.New("gapless: not an MPEG Layer III stream")
	}
	mpeg1 := h[1]>>3&3 == 3
	mono := h[3]>>6 == 3
	sideInfo := map[[2]bool]int{{true, false}: 32, {true, true}: 17, {false, false}: 17, {false, true}: 9}[[2]bool{mpeg1, mono}]
	if fh.Length < 4+sideInfo+120+36 {
		return nil, fmt.Errorf("gapless: %d-byte frames are too small for an Info header", fh.Length)
	}

	f := make([]byte, fh.Length)
	copy(f, h[:])
	b := f[4+sideInfo:]

	// Xing header, "Info" variant for CBR streams.
	copy(b, "Info")
	binary.BigEndian.PutUint32(b[4:], 0x0F) // frames, bytes, TOC and quality present
	binary.BigEndian.PutUint32(b[8:], uint32(g.Frames))
	total := int64(fh.Length) + g.Bytes
	binary.BigEndian.PutUint32(b[12:], uint32(total))
	for i := range 100 { // CBR seek table: evenly spaced
		b[16+i] = byte(i * 256 / 100)
	}
	// b[116:120] quality: 0

	// LAME extension. Players look for the "LAME" encoder string before
	// trusting the delay and padding fields, so the tag is labelled as such.
	l := b[120:]
	copy(l, "LAME3.100")
	l[9] = 0x01 // tag revision 0, CBR
	// l[10] lowpass, l[11:19] ReplayGain, l[19] flags: 0
	l[20] = byte(min(fh.Length*8*fh.SampleRate/fh.Samples/1000, 255)) // bitrate, kbps
	delay, padding := g.EncoderDelay(fh.Samples), DecoderDelay
	l[21], l[22], l[23] = byte(delay>>4), byte(delay<<4|padding>>8), byte(padding)
	// l[24] misc, l[25] gain, l[26:28] preset: 0
	binary.BigEndian.PutUint32(l[28:], uint32(total))
	// l[32:34] music CRC: unused by players
	crcAt := len(f) - len(l) + 34
	binary.BigEndian.PutUint16(l[34:], crc16(f[:crcAt]))
	return f, nil
}

// iTunSMPB is Apple's equivalent of the LAME delay and padding, stored as an
// ID3 comment. Its priming count includes the decoder delay.
func ITunSMPB(g GaplessInfo, samplesPerFrame int) string {
	priming := g.EncoderDelay(samplesPerFrame) + DecoderDelay
	samples := int64(g.Frames*samplesPerFrame - priming)
	return fmt.Sprintf(" 00000000 %08X %08X %016X 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000",
		priming, 0, samples)
}

// ScanFrames counts the frames in src[from:to] and returns the header of the
// frame at first, where the song's own audio begins.
func ScanFrames(src io.ReaderAt, from, to, first int64) (frames int, header [4]byte, err error) {
	for off := from; off < to; {
		var h [4]byte
		if _, err := src.ReadAt(h[:], off); err != nil {
			return 0, h, err
		}
		fh, ok := ParseFrameHeader(h[:])
		if !ok {
			return 0, h, fmt.Errorf("gapless: no frame header at offset %d", off)
		}
		if off == first {
			header = h
		}
		frames++
		off += int64(fh.Length)
	}
	return frames, header, nil
}

// crc16 is CRC-16/ARC (polynomial 0x8005, reflected), as used for the LAME
// tag's own checksum.
func crc16(b []byte) uint16 {
	var crc uint16
	for _, c := range b {
		crc ^= uint16(c)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}
