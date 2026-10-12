package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// listenLead is how far behind the live edge a listener starts, so the
// browser can begin playing without waiting for its buffer to fill.
const listenLead = 5 * time.Second

// serveListen streams the audio being recorded to a browser: it starts
// listenLead back from the end of the .part file, on a frame boundary, and
// follows the file as it grows until the recording finishes or the listener
// goes away. The bytes are exactly what is being recorded.
func (r *recording) serveListen(w http.ResponseWriter, req *http.Request) {
	f, err := os.Open(r.part)
	if err != nil {
		http.Error(w, "recording not available", http.StatusGone)
		return
	}
	defer f.Close()

	pos := r.listenStart(f)
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		end, finishing := r.listenEnd()
		if end > pos {
			n, err := io.Copy(w, io.NewSectionReader(f, pos, end-pos))
			pos += n
			if err != nil {
				return // listener went away
			}
			if err := rc.Flush(); err != nil {
				return
			}
		} else if finishing {
			return // recording is over and everything has been sent
		}
		select {
		case <-req.Context().Done():
			return
		case <-tick.C:
		}
	}
}

// listenStart picks a frame-aligned offset about listenLead before the end.
func (r *recording) listenStart(f io.ReaderAt) int64 {
	r.mu.Lock()
	written, ms := r.audio.written, r.audio.durationMs()
	r.mu.Unlock()
	if ms <= 0 {
		return 0
	}
	start := max(written-written*listenLead.Milliseconds()/ms, 0)
	buf := make([]byte, 2*maxFrameLen+4)
	n, _ := f.ReadAt(buf, start)
	if i, ok := findSync(buf[:n]); ok {
		return start + int64(i)
	}
	// Fall back to the live edge, which is always on a frame boundary.
	log.Printf("listen: no frame boundary near offset %d; starting at the live edge", start)
	return written
}

// listenEnd reports how much audio is on disk and whether recording has stopped.
func (r *recording) listenEnd() (written int64, finishing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.audio.written, r.finishing
}
