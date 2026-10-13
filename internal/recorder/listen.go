package recorder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"wcbn-recorder/internal/config"
	"wcbn-recorder/internal/httpx"
	"wcbn-recorder/internal/mpeg"
)

// Listening goes through one endpoint, /listen, fed by a stationTap: a single
// connection to the station shared by every listener. It carries on regardless
// of which shows are being recorded, so playback continues across show
// changes, and it keeps the last few seconds of audio so a new listener can
// start a little behind live, which lets the browser start playing at once.
//
// The station sends every client the same bytes, so audio from different
// connections can be lined up frame for frame. That's used to splice a
// reconnect onto what was already sent, and to start the tap from the audio a
// recording has just written, with no repeated or missing audio.

const (
	listenLead  = 5 * time.Second  // how far behind live a new listener starts
	tapBuffer   = 10 * time.Second // audio the tap keeps for new listeners and splicing
	tapLinger   = time.Minute      // the tap stays connected this long after the last listener
	listenQueue = 1024             // frames buffered per listener (~27 s) before it's dropped as stuck
)

type tapFrame struct {
	data []byte
	ms   int64
}

type stationTap struct {
	cfg    *config.Config    // for the stream URL, read at each connection
	seed   func() []tapFrame // recent audio from a recording in progress, if any
	linger time.Duration

	mu      sync.Mutex
	ring    []tapFrame // the most recent audio, oldest first
	ringMs  int64
	pending []tapFrame // frames from a new connection, not yet spliced in
	splice  bool
	subs    map[chan []byte]bool
	cancel  context.CancelFunc // stops the station connection; nil when not running
	idle    *time.Timer
	closed  bool
}

func newStationTap(cfg *config.Config, seed func() []tapFrame) *stationTap {
	return &stationTap{cfg: cfg, seed: seed, linger: tapLinger, subs: map[chan []byte]bool{}}
}

// subscribe adds a listener. Its channel starts with the last listenLead of
// audio and then receives each frame as it arrives; it's closed when the tap
// shuts down or the listener falls too far behind.
func (t *stationTap) subscribe() (chan []byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, fmt.Errorf("shutting down")
	}
	if t.idle != nil {
		t.idle.Stop()
		t.idle = nil
	}
	if t.cancel == nil {
		t.start()
	}
	ch := make(chan []byte, listenQueue)
	var ms int64
	start := len(t.ring)
	for start > 0 && ms < listenLead.Milliseconds() {
		start--
		ms += t.ring[start].ms
	}
	for _, f := range t.ring[start:] {
		ch <- f.data
	}
	t.subs[ch] = true
	return ch, nil
}

func (t *stationTap) unsubscribe(ch chan []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.subs, ch)
	if len(t.subs) == 0 && t.cancel != nil && !t.closed {
		t.idle = time.AfterFunc(t.linger, t.stopIfIdle)
	}
}

func (t *stationTap) stopIfIdle() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.subs) == 0 && t.cancel != nil {
		t.cancel()
		t.cancel = nil
		t.ring, t.ringMs = nil, 0
	}
}

// close disconnects from the station and ends every listener's stream.
func (t *stationTap) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	for ch := range t.subs {
		close(ch)
		delete(t.subs, ch)
	}
}

// start connects to the station. Called with t.mu held.
func (t *stationTap) start() {
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	if len(t.ring) == 0 && t.seed != nil {
		for _, f := range t.seed() {
			t.appendLocked(f)
		}
	}
	go t.run(ctx)
}

func (t *stationTap) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := t.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		log.Printf("listen: station connection lost (%v); reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (t *stationTap) connect(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.cfg.StreamURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "wcbn-recorder/1.0") // no Icy-MetaData: plain MP3
	resp, err := httpx.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream returned %s", resp.Status)
	}
	t.mu.Lock()
	t.splice, t.pending = true, nil // line this connection up with what's buffered
	t.mu.Unlock()
	_, err = io.Copy(mpeg.NewFrameWriter(frameSink{t}), resp.Body)
	return err
}

// frameSink receives whole frames from the FrameWriter.
type frameSink struct{ t *stationTap }

func (s frameSink) Write(p []byte) (int, error) {
	h, ok := mpeg.ParseFrameHeader(p)
	ms := int64(0)
	if ok && h.SampleRate > 0 {
		ms = int64(h.Samples) * 1000 / int64(h.SampleRate)
	}
	s.t.add(tapFrame{data: bytes.Clone(p), ms: ms})
	return len(p), nil
}

// spliceFrames is how many frames from a new connection are compared with the
// buffer to find where it continues, or to decide it doesn't (a gap).
const spliceFrames = 3

func (t *stationTap) add(f tapFrame) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.splice {
		t.appendLocked(f)
		return
	}
	t.pending = append(t.pending, f)
	if len(t.pending) < spliceFrames && len(t.ring) > 0 {
		return
	}
	// Find where the new connection's frames line up with the buffer, latest
	// match first, and skip the frames we already have.
	for i := len(t.ring) - 1; i >= 0; i-- {
		n := 0
		for n < len(t.pending) && i+n < len(t.ring) && bytes.Equal(t.ring[i+n].data, t.pending[n].data) {
			n++
		}
		switch {
		case n == 0:
			continue
		case i+n == len(t.ring): // overlaps the end of the buffer: continue from there
			t.commitLocked(t.pending[n:])
			return
		case n == len(t.pending): // still inside the buffer: wait for more
			return
		}
	}
	t.commitLocked(t.pending) // no overlap: a gap in the audio
}

func (t *stationTap) commitLocked(frames []tapFrame) {
	for _, f := range frames {
		t.appendLocked(f)
	}
	t.pending, t.splice = nil, false
}

// appendLocked adds a frame to the buffer and sends it to every listener.
func (t *stationTap) appendLocked(f tapFrame) {
	t.ring = append(t.ring, f)
	t.ringMs += f.ms
	for len(t.ring) > 1 && t.ringMs-t.ring[0].ms >= tapBuffer.Milliseconds() {
		t.ringMs -= t.ring[0].ms
		t.ring = t.ring[1:]
	}
	for ch := range t.subs {
		select {
		case ch <- f.data:
		default: // stuck listener: drop it rather than hold everyone up
			close(ch)
			delete(t.subs, ch)
		}
	}
}

// serveListen streams the station to a browser through the tap.
func (a *App) serveListen(w http.ResponseWriter, r *http.Request) {
	ch, err := a.tap.subscribe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer a.tap.unsubscribe(ch)
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if len(ch) == 0 {
				if err := rc.Flush(); err != nil {
					return
				}
			}
		}
	}
}

// recentAudio returns the last tapBuffer of audio from a recording in
// progress, as whole frames, so the tap can start with a backlog.
func (a *App) recentAudio() []tapFrame {
	a.mu.Lock()
	var rec *recording
	for _, r := range a.active {
		if rec == nil || r.bytesWritten() > rec.bytesWritten() {
			rec = r
		}
	}
	a.mu.Unlock()
	if rec == nil {
		return nil
	}
	return rec.recentFrames(tapBuffer)
}

// recentFrames reads about d of the most recent audio from the .part file.
func (r *recording) recentFrames(d time.Duration) []tapFrame {
	r.mu.Lock()
	written, ms := r.audio.Written(), r.audio.DurationMs()
	r.mu.Unlock()
	if written == 0 || ms <= 0 {
		return nil
	}
	f, err := os.Open(r.part)
	if err != nil {
		return nil
	}
	defer f.Close()
	start := max(written-written*d.Milliseconds()/ms, 0)
	data := make([]byte, written-start)
	if _, err := f.ReadAt(data, start); err != nil {
		return nil
	}
	i, ok := mpeg.FindSync(data)
	if !ok {
		return nil
	}
	var frames []tapFrame
	for data = data[i:]; len(data) >= 4; {
		h, ok := mpeg.ParseFrameHeader(data)
		if !ok || h.Length > len(data) {
			break
		}
		frames = append(frames, tapFrame{data: bytes.Clone(data[:h.Length]), ms: int64(h.Samples) * 1000 / int64(h.SampleRate)})
		data = data[h.Length:]
	}
	return frames
}
