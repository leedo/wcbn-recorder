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
	ms    int64
	title string
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
// cancelled), then prepends an ID3 tag with the show metadata.
func Record(ctx context.Context, cfg *Config, job Job) error {
	dir := filepath.Join(cfg.Destination, sanitize(job.Program))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	date := job.Event.Start.In(cfg.location)
	base := sanitize(fmt.Sprintf("%s - %s", job.Event.Title, date.Format("2006-01-02")))
	final := uniquePath(filepath.Join(dir, base), date.Format("1504"), ".mp3")
	part := filepath.Join(dir, "."+filepath.Base(final)+".part")

	f, err := os.Create(part)
	if err != nil {
		return err
	}
	rec := &recording{cfg: cfg, job: job, audio: &frameWriter{w: f}}

	log.Printf("recording %q until %s -> %s", job.Event.Title, job.Until.In(cfg.location).Format(time.Kitchen), final)
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
	if err := rec.finalize(context.WithoutCancel(ctx), part, final); err != nil {
		os.Remove(final + ".tmp")
		log.Printf("writing metadata failed (%v); keeping untagged audio", err)
		if err := os.Rename(part, final); err != nil {
			return err
		}
	}
	log.Printf("finished %q: %s (%.1f MB)", job.Event.Title, final, float64(rec.audio.written)/1e6)
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
	ms := r.audio.durationMs()
	if len(r.tracks) == 0 {
		ms = 0 // the first title covers the audio from the start of the file
	}
	r.tracks = append(r.tracks, trackMark{ms: ms, title: track})
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

func (r *recording) finalize(ctx context.Context, part, final string) error {
	ev := r.job.Event
	loc := r.cfg.location
	start, end := ev.Start.In(loc), ev.End.In(loc)

	// The show description lives on the show page, not in the calendar feed.
	var info ShowInfo
	if ev.URL != "" {
		var err error
		if info, err = fetchShowInfo(ctx, ev.URL); err != nil {
			log.Printf("couldn't load show details for %q: %v", ev.Title, err)
		}
	}
	showURL := ev.URL
	if info.URL != "" {
		showURL = info.URL
	}

	dj := ev.DJ
	if dj == "" {
		dj = r.dj
	}
	artist := stationName
	if dj != "" {
		artist = dj
	}
	genre := "Freeform"
	if ev.Category != "" {
		genre = ev.Category
	}
	var comment strings.Builder
	fmt.Fprintf(&comment, "%s, %s–%s", start.Format("Monday, January 2, 2006"), start.Format("3:04 PM"), end.Format("3:04 PM MST"))
	if info.Description != "" {
		comment.WriteString("\n\n" + info.Description)
	}
	if showURL != "" {
		comment.WriteString("\n\n" + showURL)
	}

	var tag id3Tag
	tag.text("TIT2", fmt.Sprintf("%s (%s)", ev.Title, start.Format("2006-01-02")))
	tag.text("TPE1", artist)
	tag.text("TALB", ev.Title)
	tag.text("TPE2", stationName)
	tag.text("TDRC", start.Format("2006-01-02"))
	tag.text("TCON", genre)
	tag.text("TPUB", stationName)
	tag.text("TLEN", fmt.Sprint(r.audio.durationMs()))
	tag.comment(comment.String())
	tag.userText("show", ev.Title)
	tag.userText("dj", dj)
	tag.userText("description", info.Description)
	tag.userText("show_start", start.Format(time.RFC3339))
	tag.userText("show_end", end.Format(time.RFC3339))
	tag.userText("show_url", showURL)
	tag.userText("source", r.cfg.StreamURL)
	tag.userURL("show", showURL)

	var chs []chapter
	total := r.audio.durationMs()
	for i, t := range r.tracks {
		endMs := total
		if i+1 < len(r.tracks) {
			endMs = r.tracks[i+1].ms
		}
		if endMs > t.ms {
			chs = append(chs, chapter{startMs: t.ms, endMs: endMs, title: t.title})
		}
	}
	if len(chs) > maxChapters {
		log.Printf("%q has %d tracks; only the first %d get chapters", ev.Title, len(chs), maxChapters)
	}
	tag.chapters(chs)

	// Write the tag followed by the audio, then swap it into place.
	tmp := final + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer out.Close()
	src, err := os.Open(part)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := tag.WriteTo(out); err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return os.Remove(part)
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

// uniquePath returns base+ext, or base+" "+suffix+ext (then numbered) if taken.
func uniquePath(base, suffix, ext string) string {
	p := base + ext
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	for i := 1; ; i++ {
		p = fmt.Sprintf("%s %s%s", base, suffix, ext)
		if i > 1 {
			p = fmt.Sprintf("%s %s-%d%s", base, suffix, i, ext)
		}
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
	}
}
