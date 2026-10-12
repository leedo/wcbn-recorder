package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// id3Tag builds an ID3v2.4 tag (https://id3.org/id3v2.4.0-structure) with
// the frames wcbn-recorder uses. All text is UTF-8.
type id3Tag struct{ frames bytes.Buffer }

const id3UTF8 = 0x03

// synchsafe encodes n in 4 bytes of 7 bits each, as ID3v2.4 sizes require.
func synchsafe(n int) []byte {
	return []byte{byte(n>>21) & 0x7f, byte(n>>14) & 0x7f, byte(n>>7) & 0x7f, byte(n) & 0x7f}
}

func id3Frame(id string, body []byte) []byte {
	b := make([]byte, 0, 10+len(body))
	b = append(b, id...)
	b = append(b, synchsafe(len(body))...)
	b = append(b, 0, 0) // flags
	return append(b, body...)
}

func textBody(v string) []byte { return append([]byte{id3UTF8}, v...) }

// text adds a text information frame such as TIT2 (title) or TPE1 (artist).
func (t *id3Tag) text(id, v string) {
	if v != "" {
		t.frames.Write(id3Frame(id, textBody(v)))
	}
}

// userText adds a TXXX frame; most tools show desc as the tag name.
func (t *id3Tag) userText(desc, v string) {
	if v != "" {
		t.frames.Write(id3Frame("TXXX", fmt.Appendf(nil, "%c%s\x00%s", id3UTF8, desc, v)))
	}
}

func (t *id3Tag) comment(v string) {
	if v != "" {
		t.frames.Write(id3Frame("COMM", fmt.Appendf(nil, "%ceng\x00%s", id3UTF8, v)))
	}
}

// userURL adds a WXXX frame. The URL itself is always ISO-8859-1.
func (t *id3Tag) userURL(desc, u string) {
	if u != "" {
		t.frames.Write(id3Frame("WXXX", fmt.Appendf(nil, "%c%s\x00%s", id3UTF8, desc, u)))
	}
}

type chapter struct {
	startMs, endMs int64
	title          string
}

// maxChapters is the most a CTOC frame can list (its entry count is one byte).
const maxChapters = 255

// chapters adds CHAP frames (http://id3.org/id3v2-chapters-1.0) and the
// top-level CTOC frame that lets players list them in order.
func (t *id3Tag) chapters(chs []chapter) {
	if len(chs) == 0 {
		return
	}
	chs = chs[:min(len(chs), maxChapters)]
	toc := []byte("toc\x00")
	toc = append(toc, 0x03, byte(len(chs))) // flags: top-level, ordered
	for i, c := range chs {
		id := fmt.Sprintf("ch%d", i)
		body := append([]byte(id), 0)
		body = binary.BigEndian.AppendUint32(body, uint32(c.startMs))
		body = binary.BigEndian.AppendUint32(body, uint32(c.endMs))
		body = append(body, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff) // byte offsets unused
		body = append(body, id3Frame("TIT2", textBody(c.title))...)
		t.frames.Write(id3Frame("CHAP", body))
		toc = append(append(toc, id...), 0)
	}
	t.frames.Write(id3Frame("CTOC", toc))
}

func (t *id3Tag) WriteTo(w io.Writer) (int64, error) {
	hdr := append([]byte("ID3\x04\x00\x00"), synchsafe(t.frames.Len())...)
	n, err := w.Write(append(hdr, t.frames.Bytes()...))
	return int64(n), err
}
