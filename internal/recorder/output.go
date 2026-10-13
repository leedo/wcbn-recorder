package recorder

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

	"wcbn-recorder/internal/config"
	"wcbn-recorder/internal/id3"
	"wcbn-recorder/internal/spinitron"
)

// outputs are the paths one recording is written to.
type outputs struct {
	base     string // "<show> - YYYY-MM-DD", possibly with a time suffix
	showFile string // the whole show; see outputPaths
	trackDir string // <dir>/<base>/: one file per song plus a playlist
	part     string // the hidden file audio is recorded into, next to showFile
}

// outputPaths lays out a recording's files in the program folder dir. With
// song files on, the whole show is only a backup, so it goes inside the song
// folder as a hidden file (<dir>/<base>/.<base>.mp3), where music libraries
// such as Plex skip it. Otherwise it's the main output: <dir>/<base>.mp3.
// The in-progress recording sits beside it, hidden, as <same name>.part.
func outputPaths(cfg *config.Config, dir, base string) outputs {
	out := outputs{base: base, trackDir: filepath.Join(dir, base)}
	if cfg.TrackFiles {
		out.showFile = filepath.Join(out.trackDir, "."+base+".mp3")
		out.part = out.showFile + ".part"
	} else {
		out.showFile = filepath.Join(dir, base+".mp3")
		out.part = filepath.Join(dir, "."+base+".mp3.part")
	}
	return out
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
	Art         *artwork // nil if the show has no usable image
}

func (r *recording) showMetadata(ctx context.Context) showMeta {
	ev := r.job.Event
	loc := r.cfg.Location
	m := showMeta{Title: ev.Title, Start: ev.Start.In(loc), End: ev.End.In(loc), URL: ev.URL, Genre: "Freeform"}

	// The show description lives on the show page, not in the calendar feed.
	r.infoOnce.Do(func() {
		if ev.URL == "" {
			return
		}
		var err error
		if r.info, err = spinitron.FetchShowInfo(ctx, ev.URL); err != nil {
			log.Printf("couldn't load show details for %q: %v", ev.Title, err)
		}
		r.art = showArtwork(ctx, ev.Title, r.info)
	})
	m.Art = r.art
	m.Description = r.info.Description
	if r.info.URL != "" {
		m.URL = r.info.URL
	}
	m.DJ = ev.DJ
	if m.DJ == "" {
		r.mu.Lock()
		m.DJ = r.dj
		r.mu.Unlock()
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
	// Most songs are already saved; this writes the last one.
	failed := r.finishSongs()
	meta := r.showMetadata(ctx)
	if r.cfg.ShowFile {
		if err := r.writeShowFile(meta, part, out.showFile); err != nil {
			return err
		}
	} else if failed > 0 {
		// The song files are the only output, so keep the raw audio too.
		return fmt.Errorf("%d song files couldn't be written", failed)
	}
	return os.Remove(part)
}

// writeShowFile writes the whole show with an ID3 tag and a Chapter per song.
func (r *recording) writeShowFile(meta showMeta, part, path string) error {
	var tag id3.Tag
	tag.Text("TIT2", meta.albumTitle())
	tag.Text("TPE1", meta.Artist)
	tag.Text("TALB", meta.Title)
	tag.Text("TPE2", stationName)
	tag.Text("TDRC", meta.date())
	tag.Text("TCON", meta.Genre)
	tag.Text("TPUB", stationName)
	tag.Text("TLEN", fmt.Sprint(r.audio.DurationMs()))
	tag.Comment(meta.Comment)
	tag.UserText("show", meta.Title)
	tag.UserText("dj", meta.DJ)
	tag.UserText("description", meta.Description)
	tag.UserText("show_start", meta.Start.Format(time.RFC3339))
	tag.UserText("show_end", meta.End.Format(time.RFC3339))
	tag.UserText("show_url", meta.URL)
	tag.UserText("source", r.cfg.StreamURL)
	tag.UserURL("show", meta.URL)
	if meta.Art != nil {
		tag.Picture(meta.Art.mime, meta.Art.data)
	}

	segs := r.segments()
	var chs []id3.Chapter
	for _, s := range segs {
		chs = append(chs, id3.Chapter{StartMs: s.startMs, EndMs: s.endMs, Title: s.mark.title})
	}
	if len(chs) > id3.MaxChapters {
		log.Printf("%q has %d tracks; only the first %d get chapters", meta.Title, len(chs), id3.MaxChapters)
	}
	tag.Chapters(chs)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	src, err := os.Open(part)
	if err != nil {
		return err
	}
	defer src.Close()
	path = uniquePath(path, ".mp3") // a resumed recording adds a second one
	if err := writeTagged(path, &tag, src); err != nil {
		return err
	}
	r.showOut = path
	if err := r.writeCue(meta, path, segs); err != nil {
		log.Printf("writing CUE sheet for %q: %v", meta.Title, err) // the show file itself is fine
	} else if exists(cuePath(path)) {
		r.cueOut = cuePath(path)
	}
	return nil
}

// segment is the stretch of audio between one stream title change and the next.
type segment struct {
	n                int // song number, for song files
	mark             trackMark
	leadOff          int64 // where a gapless file starts, OverlapFrames before startOff
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
		s := segment{mark: m, leadOff: m.lead, startOff: m.offset, startMs: m.ms, endOff: r.audio.Written(), endMs: r.audio.DurationMs()}
		if i+1 < len(marks) {
			s.endOff, s.endMs = marks[i+1].offset, marks[i+1].ms
		}
		if s.endOff > s.startOff {
			segs = append(segs, s)
		}
	}
	return segs
}

// writeTagged writes an ID3 tag followed by audio to path, via a temp file.
func writeTagged(path string, tag *id3.Tag, audio io.Reader) error {
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
