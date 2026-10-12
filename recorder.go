package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	stationName = "WCBN 88.3 FM Ann Arbor"
	idleTimeout = 30 * time.Second
)

// Job is one scheduled recording.
type Job struct {
	Program string // configured program name (used for the folder)
	Event   Event
	Until   time.Time // stop time, including post-roll
}

// trackMark records a stream title change at a position in the audio.
type trackMark struct {
	ms     int64     // position in the recording
	offset int64     // byte offset in the audio; always on a frame boundary
	lead   int64     // offset up to overlapFrames earlier, where a gapless song file starts
	at     time.Time // when the title changed
	title  string    // the stream title with " on <show> with <dj>" removed
}

// recordingIDs numbers recordings in progress, for listen URLs.
var recordingIDs atomic.Int64

type recording struct {
	id      int64
	cfg     *Config
	job     Job
	out     outputs
	part    string
	file    *os.File
	started time.Time

	// mu guards the fields below, which the web UI reads while recording.
	mu         sync.Mutex
	audio      *frameWriter
	tracks     []trackMark
	lastMeta   string
	dj         string
	connected  bool
	lastErr    string
	reconnects int
	finishing  bool
	songCount  int          // songs queued for writing
	songsSaved map[int]bool // song numbers written to files

	// Song files are written by a background goroutine as songs finish.
	songs      chan segment
	songsDone  chan struct{}
	songsOnce  sync.Once
	songErrors int // written only by the song writer

	infoOnce sync.Once
	info     ShowInfo // from the show's Spinitron page, fetched once
}

// Record streams the live program to disk until job.Until (or ctx is
// cancelled), then writes the tagged show file and/or one file per song.
func Record(ctx context.Context, cfg *Config, job Job) error {
	rec, err := startRecording(cfg, job)
	if err != nil {
		return err
	}
	return rec.Run(ctx)
}

// startRecording picks the output names and creates the file the audio is
// streamed into. Run does the recording.
func startRecording(cfg *Config, job Job) (*recording, error) {
	dir := filepath.Join(cfg.Destination, sanitize(job.Program))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	date := job.Event.Start.In(cfg.location)
	base := uniqueBase(dir, sanitize(fmt.Sprintf("%s - %s", job.Event.Title, date.Format("2006-01-02"))), date.Format("1504"))
	out := outputs{
		base:     base,
		showFile: filepath.Join(dir, base+".mp3"),
		trackDir: filepath.Join(dir, base),
	}
	part := filepath.Join(dir, "."+base+".mp3.part")

	f, err := os.Create(part)
	if err != nil {
		return nil, err
	}
	return &recording{id: recordingIDs.Add(1), cfg: cfg, job: job, out: out, part: part, file: f, started: time.Now(), audio: &frameWriter{w: f}}, nil
}

// Run records until job.Until (or ctx is cancelled), reconnecting as needed,
// then writes the outputs.
func (r *recording) Run(ctx context.Context) error {
	job, out := r.job, r.out
	log.Printf("recording %q until %s -> %s", job.Event.Title, job.Until.In(r.cfg.location).Format(time.Kitchen), out.trackDir)
	recCtx, cancel := context.WithDeadline(ctx, job.Until)
	defer cancel()
	go stopAtWallTime(recCtx, job.Until, cancel)
	r.startSongWriter(context.WithoutCancel(ctx))
	backoff := time.Second
	for recCtx.Err() == nil {
		before := r.bytesWritten()
		err := r.streamOnce(recCtx)
		r.mu.Lock()
		r.connected = false
		if recCtx.Err() == nil {
			r.reconnects++
			r.lastErr = fmt.Sprint(err)
		}
		r.mu.Unlock()
		if recCtx.Err() != nil {
			break
		}
		if r.bytesWritten() > before {
			backoff = time.Second
		}
		log.Printf("stream for %q interrupted (%v); reconnecting in %s", job.Event.Title, err, backoff)
		select {
		case <-recCtx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	r.mu.Lock()
	r.finishing = true
	r.mu.Unlock()
	if err := r.file.Close(); err != nil {
		return err
	}
	if r.audio.written == 0 {
		r.stopSongWriter()
		os.Remove(r.part)
		return fmt.Errorf("no audio received for %q", job.Event.Title)
	}
	// Finalize even if we were interrupted by shutdown so partial shows keep their tags.
	if err := r.finalize(context.WithoutCancel(ctx), r.part, out); err != nil {
		log.Printf("writing output failed (%v); keeping untagged audio as %s", err, out.showFile)
		if err := os.Rename(r.part, out.showFile); err != nil {
			return err
		}
	}
	log.Printf("finished %q: %s (%.1f MB, %d tracks)", job.Event.Title, out.trackDir, float64(r.audio.written)/1e6, len(r.tracks))
	return nil
}

// stopAtWallTime cancels once the wall clock passes until. Go timers (and so
// context deadlines) can stop counting while the machine sleeps, which would
// let a recording run past its stop time by however long it slept.
func stopAtWallTime(ctx context.Context, until time.Time, cancel func()) {
	until = until.Round(0) // compare wall clock, not monotonic time
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if !now.Round(0).Before(until) {
				cancel()
				return
			}
		}
	}
}

func (r *recording) bytesWritten() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.audio.written
}

// Write passes stream audio to the frame writer under the lock, so the web UI
// can read progress while recording.
func (r *recording) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connected = true
	return r.audio.Write(p)
}

// streamOnce makes one connection to the stream and copies audio until the
// connection drops, stalls, or ctx ends.
func (r *recording) streamOnce(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.StreamURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Icy-MetaData", "1")
	req.Header.Set("User-Agent", "wcbn-recorder/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream returned %s", resp.Status)
	}
	metaint, _ := strconv.Atoi(resp.Header.Get("icy-metaint"))

	// Cancel the connection if the server stops sending data.
	watchdog := time.AfterFunc(idleTimeout, cancel)
	defer watchdog.Stop()
	body := bufio.NewReaderSize(&kickReader{resp.Body, watchdog}, 64*1024)

	// A new connection can start mid-frame, and a dropped one can end mid-frame.
	r.mu.Lock()
	r.audio.Reset()
	r.mu.Unlock()
	if metaint <= 0 {
		_, err := io.Copy(r, body)
		return err
	}
	for {
		if _, err := io.CopyN(r, body, int64(metaint)); err != nil {
			return err
		}
		lenByte, err := body.ReadByte()
		if err != nil {
			return err
		}
		if lenByte == 0 {
			continue
		}
		meta := make([]byte, int(lenByte)*16)
		if _, err := io.ReadFull(body, meta); err != nil {
			return err
		}
		r.onMetadata(string(meta))
	}
}

type kickReader struct {
	r io.Reader
	t *time.Timer
}

func (k *kickReader) Read(p []byte) (int, error) {
	n, err := k.r.Read(p)
	if n > 0 {
		k.t.Reset(idleTimeout)
	}
	return n, err
}

var streamTitleRe = regexp.MustCompile(`StreamTitle='(.*?)';`)

func (r *recording) onMetadata(meta string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := streamTitleRe.FindStringSubmatch(strings.TrimRight(meta, "\x00"))
	if m == nil || m[1] == "" || m[1] == r.lastMeta {
		return
	}
	r.lastMeta = m[1]
	track, dj, ours := splitStreamTitle(m[1], r.job.Event.Title)
	// Only trust the DJ name when the stream title names this show, so pre-
	// and post-roll from adjacent shows don't override it.
	if ours && dj != "" {
		r.dj = dj
	}
	mark := trackMark{ms: r.audio.durationMs(), offset: r.audio.written, lead: r.audio.overlapStart(), at: time.Now(), title: track}
	if len(r.tracks) == 0 {
		// The first title covers the audio from the start of the file.
		mark.ms, mark.offset, mark.lead = 0, 0, 0
	}
	if n := len(r.tracks); n > 0 {
		r.queueSong(r.tracks[n-1], mark) // the previous song just finished
	}
	r.tracks = append(r.tracks, mark)
	log.Printf("[%s] now playing: %s", r.job.Event.Title, track)
}

var genericTitleRe = regexp.MustCompile(`^(.*) on (.+?) with (.+)$`)

// splitStreamTitle splits WCBN's "<song> by <artist> on <show> with <dj>"
// stream titles into the track part and DJ name. ours reports whether the
// title named the given show.
func splitStreamTitle(title, show string) (track, dj string, ours bool) {
	marker := " on " + strings.ToLower(show)
	if i := strings.LastIndex(strings.ToLower(title), marker); i >= 0 {
		rest := title[i+len(marker):]
		if _, after, ok := strings.Cut(rest, " with "); ok {
			dj = strings.TrimSpace(after)
		}
		return strings.TrimSpace(title[:i]), dj, true
	}
	if m := genericTitleRe.FindStringSubmatch(title); m != nil {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[3]), false
	}
	return title, "", false
}

var unsafeChars = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]+`)

func sanitize(s string) string {
	s = strings.TrimSpace(unsafeChars.ReplaceAllString(s, "-"))
	s = strings.Trim(s, ". ")
	if s == "" {
		return "untitled"
	}
	return s
}

// uniqueBase returns a name for both the show file (name.mp3) and the track
// folder (name/) that doesn't collide with an earlier recording, appending
// suffix (then a number) if needed.
func uniqueBase(dir, name, suffix string) string {
	free := func(n string) bool {
		for _, p := range []string{n + ".mp3", n} {
			if _, err := os.Stat(filepath.Join(dir, p)); !errors.Is(err, os.ErrNotExist) {
				return false
			}
		}
		return true
	}
	if free(name) {
		return name
	}
	for i := 1; ; i++ {
		n := name + " " + suffix
		if i > 1 {
			n = fmt.Sprintf("%s %s-%d", name, suffix, i)
		}
		if free(n) {
			return n
		}
	}
}
