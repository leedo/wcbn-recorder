package recorder

import (
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"wcbn-recorder/internal/config"
	"wcbn-recorder/internal/mpeg"
)

// numberedFrame is a valid 1044-byte frame carrying n, so a test can check a
// stream for repeated or missing audio.
func numberedFrame(n int) []byte {
	f := mp3Frame(0)
	binary.BigEndian.PutUint32(f[4:], uint32(n))
	return f
}

func frameNumber(f []byte) int { return int(binary.BigEndian.Uint32(f[4:])) }

// fakeLiveStation streams numbered frames. Like Icecast, a new connection
// first gets a burst of the frames just before the live point, then follows
// it. Frames are numbered by a counter shared across connections, as a real
// station's are by time. After dropAfter frames, a connection is cut (once).
type fakeLiveStation struct {
	mu        sync.Mutex
	next      int // the live point: the next frame to be sent
	dropAfter int
	conns     int
}

func (s *fakeLiveStation) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.conns++
	conn, n := s.conns, max(s.next-60, 0)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "audio/mpeg")
	rc := http.NewResponseController(w)
	for sent := 0; r.Context().Err() == nil; sent++ {
		if conn == 1 && s.dropAfter > 0 && sent == s.dropAfter {
			return // the connection drops
		}
		if _, err := w.Write(numberedFrame(n)); err != nil {
			return
		}
		rc.Flush()
		s.mu.Lock()
		if n >= s.next {
			s.next = n + 1
		}
		s.mu.Unlock()
		n++
		time.Sleep(2 * time.Millisecond)
	}
}

// readFrames reads n whole frames from a listener's stream.
func readFrames(t *testing.T, r io.Reader, n int) []int {
	t.Helper()
	got := make([]int, 0, n)
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1044)
		for len(got) < n {
			if _, err := io.ReadFull(r, buf); err != nil {
				done <- err
				return
			}
			got = append(got, frameNumber(buf))
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("after %d frames: %v", len(got), err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out after %d of %d frames", len(got), n)
	}
	return got
}

func checkConsecutive(t *testing.T, frames []int) {
	t.Helper()
	for i := 1; i < len(frames); i++ {
		if frames[i] != frames[i-1]+1 {
			t.Fatalf("frame %d is #%d after #%d: repeated or missing audio", i, frames[i], frames[i-1])
		}
	}
}

func TestTapSplice(t *testing.T) {
	tap := newStationTap(&config.Config{}, nil)
	add := func(from, to int) {
		for n := from; n <= to; n++ {
			tap.add(tapFrame{data: numberedFrame(n), ms: 26})
		}
	}
	ring := func() []int {
		var got []int
		for _, f := range tap.ring {
			got = append(got, frameNumber(f.data))
		}
		return got
	}
	add(0, 99)
	// A new connection that starts inside the buffer: the overlap is skipped.
	tap.splice = true
	add(90, 110)
	checkConsecutive(t, ring())
	if got := ring(); got[len(got)-1] != 110 {
		t.Errorf("buffer ends at #%d", got[len(got)-1])
	}
	// One that starts after it: a real gap, kept as one.
	tap.splice = true
	add(500, 503)
	if got := ring(); got[len(got)-5] != 110 || got[len(got)-4] != 500 || got[len(got)-1] != 503 {
		t.Errorf("gap not kept: ...%v", got[len(got)-6:])
	}
	if len(tap.ring) > 10000/26+1 {
		t.Errorf("buffer holds %d frames, more than %s", len(tap.ring), tapBuffer)
	}
}

// One /listen stream: it starts from a recording's audio, joins the station
// connection seamlessly, keeps going after the recording ends, and survives a
// dropped station connection, all without repeated or missing audio.
func TestListenStream(t *testing.T) {
	station := &fakeLiveStation{next: 300, dropAfter: 400}
	stationSrv := httptest.NewServer(station)
	t.Cleanup(func() { stationSrv.CloseClientConnections(); stationSrv.Close() })

	app, srv := testApp(t)
	app.cfg.StreamURL = stationSrv.URL

	// A recording in progress that has the station's frames up to now.
	f := newFakeRecording(t, testConfig())
	for n := 0; n < 300; n++ {
		f.Write(numberedFrame(n))
	}
	app.mu.Lock()
	app.active[f.job.Event.Key()] = f.recording
	app.mu.Unlock()

	resp, err := http.Get(srv.URL + "/listen")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("Content-Type %q", resp.Header.Get("Content-Type"))
	}

	// The first ~5 s comes straight from the recording's file.
	lead := int(listenLead.Milliseconds() / 26)
	got := readFrames(t, resp.Body, 100)
	if want := 300 - lead - 1; got[0] < want-1 || got[0] > want+1 {
		t.Errorf("started at #%d, want about #%d (5 s back)", got[0], want)
	}

	// The recording ends: the stream carries on regardless.
	app.mu.Lock()
	delete(app.active, f.job.Event.Key())
	app.mu.Unlock()
	got = append(got, readFrames(t, resp.Body, 600)...) // past the dropped connection at #700
	checkConsecutive(t, got)
	station.mu.Lock()
	conns := station.conns
	station.mu.Unlock()
	if conns < 2 {
		t.Errorf("expected a reconnect, got %d connections", conns)
	}

	// Shutting down ends the stream.
	app.tap.close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Errorf("stream didn't end cleanly: %v", err)
	}
	if _, err := app.tap.subscribe(); err == nil {
		t.Error("subscribing after close should fail")
	}
}

func TestRecentFrames(t *testing.T) {
	f := newFakeRecording(t, testConfig())
	for n := 0; n < 1000; n++ {
		f.Write(numberedFrame(n))
	}
	frames := f.recentFrames(2 * time.Second)
	if len(frames) == 0 || frameNumber(frames[len(frames)-1].data) != 999 {
		t.Fatalf("got %d frames", len(frames))
	}
	var nums []int
	var ms int64
	for _, fr := range frames {
		if _, ok := mpeg.ParseFrameHeader(fr.data); !ok {
			t.Fatal("not a whole frame")
		}
		nums = append(nums, frameNumber(fr.data))
		ms += fr.ms
	}
	checkConsecutive(t, nums)
	if ms < 1900 || ms > 2100 {
		t.Errorf("%d ms of audio, want about 2000", ms)
	}
}
