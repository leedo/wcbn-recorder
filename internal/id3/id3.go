// Package id3 writes ID3v2.4 tags: text frames, comments, URLs and cover
// pictures.
package id3

import (
	"bytes"
	"fmt"
	"io"
)

// Tag builds an ID3v2.4 tag (https://id3.org/id3v2.4.0-structure) with
// the frames wcbn-recorder uses. All text is UTF-8.
type Tag struct{ frames bytes.Buffer }

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
func (t *Tag) Text(id, v string) {
	if v != "" {
		t.frames.Write(id3Frame(id, textBody(v)))
	}
}

// userText adds a TXXX frame; most tools show desc as the tag name.
func (t *Tag) UserText(desc, v string) {
	if v != "" {
		t.frames.Write(id3Frame("TXXX", fmt.Appendf(nil, "%c%s\x00%s", id3UTF8, desc, v)))
	}
}

func (t *Tag) Comment(v string) { t.CommentDesc("", v) }

// commentDesc adds a COMM frame with a content description, as used by
// iTunes for values like iTunSMPB.
func (t *Tag) CommentDesc(desc, v string) {
	if v != "" {
		t.frames.Write(id3Frame("COMM", fmt.Appendf(nil, "%ceng%s\x00%s", id3UTF8, desc, v)))
	}
}

// userURL adds a WXXX frame. The URL itself is always ISO-8859-1.
func (t *Tag) UserURL(desc, u string) {
	if u != "" {
		t.frames.Write(id3Frame("WXXX", fmt.Appendf(nil, "%c%s\x00%s", id3UTF8, desc, u)))
	}
}

func (t *Tag) WriteTo(w io.Writer) (int64, error) {
	hdr := append([]byte("ID3\x04\x00\x00"), synchsafe(t.frames.Len())...)
	n, err := w.Write(append(hdr, t.frames.Bytes()...))
	return int64(n), err
}

// Picture adds an APIC frame with an image (JPEG or PNG) as the front cover.
func (t *Tag) Picture(mime string, data []byte) {
	body := []byte{0x00} // ISO-8859-1 description: the most widely supported
	body = append(body, mime...)
	body = append(body, 0x00, 0x03, 0x00) // MIME terminator, "front cover", empty description
	body = append(body, data...)
	t.frames.Write(id3Frame("APIC", body))
}
