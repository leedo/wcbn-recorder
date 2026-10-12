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
	at     time.Time // when the title changed
	title  string    // the stream title with " on <show> with <dj>" removed
}

type recording struct {
	cfg      *Config
	job      Job
	audio    *frameWriter
	tracks   []trackMark
	lastMeta string
	dj       string
}

// Record streams the live program to disk until job.Until (or ctx is
// cancelled), then writes the tagged show file and/or one file per song.
func Record(ctx context.Context, cfg *Config, job Job) error {
	dir := filepath.Join(cfg.Destination, sanitize(job.Program))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
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
		return err
	}
	rec := &recording{cfg: cfg, job: job, audio: &frameWriter{w: f}}

	log.Printf("recording %q until %s -> %s", job.Event.Title, job.Until.In(cfg.location).Format(time.Kitchen), filepath.Join(dir, base))
	recCtx, cancel := context.WithDeadline(ctx, job.Until)
	defer cancel()
	backoff := time.Second
	for recCtx.Err() == nil {
		before := rec.audio.written
		err := rec.streamOnce(recCtx)
		if recCtx.Err() != nil {
			break
		}
		if rec.audio.written > before {
			backoff = time.Second
		}
		log.Printf("stream for %q interrupted (%v); reconnecting in %s", job.Event.Title, err, backoff)
		select {
		case <-recCtx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if rec.audio.written == 0 {
		os.Remove(part)
		return fmt.Errorf("no audio received for %q", job.Event.Title)
	}
	// Finalize even if we were interrupted by shutdown so partial shows keep their tags.
	if err := rec.finalize(context.WithoutCancel(ctx), part, out); err != nil {
		log.Printf("writing output failed (%v); keeping untagged audio as %s", err, out.showFile)
		if err := os.Rename(part, out.showFile); err != nil {
			return err
		}
	}
	log.Printf("finished %q: %s (%.1f MB, %d tracks)", job.Event.Title, filepath.Join(dir, base), float64(rec.audio.written)/1e6, len(rec.tracks))
	return nil
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
	r.audio.Reset()
	if metaint <= 0 {
		_, err := io.Copy(r.audio, body)
		return err
	}
	for {
		if _, err := io.CopyN(r.audio, body, int64(metaint)); err != nil {
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
	mark := trackMark{ms: r.audio.durationMs(), offset: r.audio.written, at: time.Now(), title: track}
	if len(r.tracks) == 0 {
		// The first title covers the audio from the start of the file.
		mark.ms, mark.offset = 0, 0
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
