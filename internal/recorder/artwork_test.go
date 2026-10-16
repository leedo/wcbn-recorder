package recorder

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"wcbn-recorder/internal/spinitron"
)

// Smallest valid-looking image headers, enough for content sniffing.
var (
	testJPEG = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, bytes.Repeat([]byte{7}, 64)...)
	testPNG  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 64)...)
)

func TestFetchArtwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.jpg":
			w.Write(testJPEG)
		case "/a.png":
			w.Write(testPNG)
		case "/lie.jpg": // an error page served with an image name
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("<!doctype html><title>oops</title>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if a, err := fetchArtwork(t.Context(), srv.URL+"/a.jpg"); err != nil || a.mime != "image/jpeg" || !bytes.Equal(a.data, testJPEG) {
		t.Errorf("jpeg: %v %+v", err, a)
	}
	if a, err := fetchArtwork(t.Context(), srv.URL+"/a.png"); err != nil || a.mime != "image/png" {
		t.Errorf("png: %v %+v", err, a)
	}
	if _, err := fetchArtwork(t.Context(), srv.URL+"/lie.jpg"); err == nil {
		t.Error("accepted HTML as artwork")
	}
	if _, err := fetchArtwork(t.Context(), srv.URL+"/missing.jpg"); err == nil {
		t.Error("accepted a 404")
	}
}

func TestRecordingArtwork(t *testing.T) {
	f := newFakeRecording(t, testConfig())
	f.art = &artwork{mime: "image/jpeg", data: testJPEG}
	f.title("Pressure Drop by Toots & the Maytals")
	f.frames(1, 3)
	f.title("Monkey Man by Toots & the Maytals")
	f.frames(2, 3)
	if err := f.finalize(t.Context(), f.part, f.out); err != nil {
		t.Fatal(err)
	}
	if apic := readAPIC(t, f.out.showFile); !bytes.HasSuffix(apic, testJPEG) {
		t.Error("artwork not embedded")
	}
}

func TestRecordingWithoutArtwork(t *testing.T) {
	f := newFakeRecording(t, testConfig())
	f.title("Pressure Drop by Toots & the Maytals")
	f.frames(1, 3)
	if err := f.finalize(t.Context(), f.part, f.out); err != nil {
		t.Fatal(err)
	}
	if apic := readAPIC(t, f.out.showFile); apic != nil {
		t.Error("embedded artwork without any")
	}
}

// readAPIC returns the body of a file's APIC frame, or nil.
func readAPIC(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, fr := range parseFrames(t, b[10:10+unsynchsafe(b[6:10])]) {
		if fr.id == "APIC" {
			return fr.body
		}
	}
	return nil
}

func TestShowArtworkFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/logo.jpg" {
			w.Write(testJPEG)
			return
		}
		http.NotFound(w, r) // the show's own image is gone
	}))
	defer srv.Close()
	info := spinitron.ShowInfo{Image: srv.URL + "/gone.jpg", StationImage: srv.URL + "/logo.jpg"}
	if a := showArtwork(t.Context(), "Club H", info); a == nil || !bytes.Equal(a.data, testJPEG) {
		t.Errorf("expected the station logo, got %+v", a)
	}
	if a := showArtwork(t.Context(), "Club H", spinitron.ShowInfo{Image: srv.URL + "/gone.jpg"}); a != nil {
		t.Error("expected no artwork")
	}
}
