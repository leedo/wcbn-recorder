package recorder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"wcbn-recorder/internal/store"
)

// Test helpers shared by the recorder tests; the mpeg, id3 and store
// packages have their own copies for their tests.

// mp3Frame returns a 320 kbps, 44.1 kHz MPEG-1 Layer III frame (1044 bytes)
// whose payload is filled with fill.
func mp3Frame(fill byte) []byte {
	f := bytes.Repeat([]byte{fill}, 1044)
	copy(f, []byte{0xFF, 0xFB, 0xE0, 0x44})
	return f
}

type parsedFrame struct {
	id   string
	body []byte
}

func unsynchsafe(b []byte) int {
	return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3])
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

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// firstEvent returns the data of the first message on the page's event
// stream, decoded into v, and the response's headers.
func firstEvent(t *testing.T, url string, v any) http.Header {
	t.Helper()
	resp, err := http.Get(url + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			if err := json.Unmarshal([]byte(data), v); err != nil {
				t.Fatal(err)
			}
			return resp.Header
		}
	}
	t.Fatalf("no event: %v", sc.Err())
	return nil
}
