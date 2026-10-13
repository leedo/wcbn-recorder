package mpeg

import (
	"encoding/binary"
	"testing"
)

func TestCRC16(t *testing.T) {
	if got := crc16([]byte("123456789")); got != 0xBB3D { // CRC-16/ARC check value
		t.Errorf("crc16 = %#04x, want 0xbb3d", got)
	}
}

func TestInfoFrame(t *testing.T) {
	g := GaplessInfo{Header: [4]byte{0xFF, 0xFA, 0xE2, 0x44}, Frames: 100, Bytes: 104400, Lead: 2}
	f, err := InfoFrame(g)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := ParseFrameHeader(f)
	if !ok || h.Length != len(f) || len(f) != 1044 {
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
	if s := ITunSMPB(g, 1152); s[:28] != " 00000000 00000900 00000000 " { // priming 1775+529 = 2304
		t.Errorf("iTunSMPB = %q", s)
	}
	// The first song of a recording has no overlap; nothing extra is skipped.
	if g := (GaplessInfo{Lead: 0}); g.EncoderDelay(1152) != 0 {
		t.Error("lead 0 should give delay 0")
	}
}
