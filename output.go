package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// outputs are the paths one recording is written to.
type outputs struct {
	base     string // "<show> - YYYY-MM-DD", possibly with a time suffix
	showFile string // <dir>/<base>.mp3: the whole show
	trackDir string // <dir>/<base>/: one file per song plus a playlist
}

// showMeta is the show-level information shared by every output file.
type showMeta struct {
	Title       string
	Start, End  time.Time
	DJ          string
	Artist      string // DJ, or the station if unknown
	Genre       string
	Description string
	URL         string
	Comment     string
}

func (r *recording) showMetadata(ctx context.Context) showMeta {
	ev := r.job.Event
	loc := r.cfg.location
	m := showMeta{Title: ev.Title, Start: ev.Start.In(loc), End: ev.End.In(loc), URL: ev.URL, Genre: "Freeform"}

	// The show description lives on the show page, not in the calendar feed.
	if ev.URL != "" {
		info, err := fetchShowInfo(ctx, ev.URL)
		if err != nil {
			log.Printf("couldn't load show details for %q: %v", ev.Title, err)
		}
		m.Description = info.Description
		if info.URL != "" {
			m.URL = info.URL
		}
	}
	m.DJ = ev.DJ
	if m.DJ == "" {
		m.DJ = r.dj
	}
	m.Artist = stationName
	if m.DJ != "" {
		m.Artist = m.DJ
	}
	if ev.Category != "" {
		m.Genre = ev.Category
	}
	var c strings.Builder
	fmt.Fprintf(&c, "%s, %s–%s", m.Start.Format("Monday, January 2, 2006"), m.Start.Format("3:04 PM"), m.End.Format("3:04 PM MST"))
	if m.Description != "" {
		c.WriteString("\n\n" + m.Description)
	}
	if m.URL != "" {
		c.WriteString("\n\n" + m.URL)
	}
	m.Comment = c.String()
	return m
}

func (m showMeta) date() string { return m.Start.Format("2006-01-02") }

// albumTitle is used as the show file's title and each song's album.
func (m showMeta) albumTitle() string { return fmt.Sprintf("%s (%s)", m.Title, m.date()) }

// finalize writes the configured outputs from the raw audio in part, then
// removes it. It returns an error only if no output could be written, in
// which case the caller keeps the raw audio.
func (r *recording) finalize(ctx context.Context, part string, out outputs) error {
	meta := r.showMetadata(ctx)
	wrote := false
	if r.cfg.ShowFile {
		if err := r.writeShowFile(meta, part, out.showFile); err != nil {
			return err
		}
		wrote = true
	}
	if r.cfg.TrackFiles {
		if err := r.writeTracks(meta, part, out); err != nil {
			if !wrote {
				return err
			}
			log.Printf("splitting %q into tracks failed: %v", meta.Title, err)
		}
	}
	return os.Remove(part)
}

// writeShowFile writes the whole show with an ID3 tag and a chapter per song.
func (r *recording) writeShowFile(meta showMeta, part, path string) error {
	var tag id3Tag
	tag.text("TIT2", meta.albumTitle())
	tag.text("TPE1", meta.Artist)
	tag.text("TALB", meta.Title)
	tag.text("TPE2", stationName)
	tag.text("TDRC", meta.date())
	tag.text("TCON", meta.Genre)
	tag.text("TPUB", stationName)
	tag.text("TLEN", fmt.Sprint(r.audio.durationMs()))
	tag.comment(meta.Comment)
	tag.userText("show", meta.Title)
	tag.userText("dj", meta.DJ)
	tag.userText("description", meta.Description)
	tag.userText("show_start", meta.Start.Format(time.RFC3339))
	tag.userText("show_end", meta.End.Format(time.RFC3339))
	tag.userText("show_url", meta.URL)
	tag.userText("source", r.cfg.StreamURL)
	tag.userURL("show", meta.URL)

	var chs []chapter
	for _, s := range r.segments() {
		chs = append(chs, chapter{startMs: s.startMs, endMs: s.endMs, title: s.mark.title})
	}
	if len(chs) > maxChapters {
		log.Printf("%q has %d tracks; only the first %d get chapters", meta.Title, len(chs), maxChapters)
	}
	tag.chapters(chs)

	src, err := os.Open(part)
	if err != nil {
		return err
	}
	defer src.Close()
	return writeTagged(path, &tag, src)
}

// segment is the stretch of audio between one stream title change and the next.
type segment struct {
	mark             trackMark
	startOff, endOff int64
	startMs, endMs   int64
}

func (r *recording) segments() []segment {
	marks := r.tracks
	if len(marks) == 0 {
		// No stream titles at all: treat the whole show as one track.
		marks = []trackMark{{title: r.job.Event.Title, at: r.job.Event.Start}}
	}
	var segs []segment
	for i, m := range marks {
		s := segment{mark: m, startOff: m.offset, startMs: m.ms, endOff: r.audio.written, endMs: r.audio.durationMs()}
		if i+1 < len(marks) {
			s.endOff, s.endMs = marks[i+1].offset, marks[i+1].ms
		}
		if s.endOff > s.startOff {
			segs = append(segs, s)
		}
	}
	return segs
}

// writeTracks writes each song as its own MP3, cut on frame boundaries from
// the recorded audio, plus an M3U playlist in air order. Everything is built
// in a hidden folder and renamed into place once complete.
func (r *recording) writeTracks(meta showMeta, part string, out outputs) error {
	segs := r.segments()
	src, err := os.Open(part)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp := filepath.Join(filepath.Dir(out.trackDir), "."+out.base+".tracks")
	os.RemoveAll(tmp) // left over from a crash
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	width := max(2, len(fmt.Sprint(len(segs))))
	playlist := []string{"#EXTM3U", "#PLAYLIST:" + meta.albumTitle()}
	for i, s := range segs {
		n := i + 1
		title, artist := splitSong(s.mark.title)
		name := title
		if artist != "" {
			name = artist + " - " + title
		}
		file := fmt.Sprintf("%0*d - %s.mp3", width, n, truncateName(sanitize(name), 150))

		var tag id3Tag
		tag.text("TIT2", title)
		tag.text("TPE1", artist)
		tag.text("TALB", meta.albumTitle())
		tag.text("TPE2", meta.Artist)
		tag.text("TRCK", fmt.Sprintf("%d/%d", n, len(segs)))
		tag.text("TDRC", meta.date())
		tag.text("TCON", meta.Genre)
		tag.text("TPUB", stationName)
		tag.text("TLEN", fmt.Sprint(s.endMs-s.startMs))
		played := s.mark.at.In(r.cfg.location)
		tag.comment(fmt.Sprintf("Played on %s with %s, %s at %s", meta.Title, meta.Artist, played.Format("Monday, January 2, 2006"), played.Format("3:04 PM")))
		tag.userText("show", meta.Title)
		tag.userText("dj", meta.DJ)
		tag.userText("aired_at", played.Format(time.RFC3339))
		tag.userText("stream_title", s.mark.title)
		tag.userText("show_url", meta.URL)
		tag.userURL("show", meta.URL)

		audio := io.NewSectionReader(src, s.startOff, s.endOff-s.startOff)
		if err := writeTagged(filepath.Join(tmp, file), &tag, audio); err != nil {
			return err
		}
		playlist = append(playlist,
			fmt.Sprintf("#EXTINF:%d,%s", (s.endMs-s.startMs+500)/1000, strings.ReplaceAll(name, "\n", " ")),
			file)
	}
	m3u := filepath.Join(tmp, out.base+".m3u8")
	if err := os.WriteFile(m3u, []byte(strings.Join(playlist, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, out.trackDir)
}

// writeTagged writes an ID3 tag followed by audio to path, via a temp file.
func writeTagged(path string, tag *id3Tag, audio io.Reader) error {
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := tag.WriteTo(out); err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(out, audio); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// splitSong splits WCBN's "<title> by <artist>" track titles. The last " by "
// is used, since song titles ("Stand by Me") contain it more often than
// artist names do.
func splitSong(s string) (title, artist string) {
	if i := strings.LastIndex(s, " by "); i > 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+4:])
	}
	return strings.TrimSpace(s), ""
}

// truncateName shortens s to at most n bytes without splitting a character,
// keeping file names well under filesystem limits.
func truncateName(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.TrimSpace(s)
}
