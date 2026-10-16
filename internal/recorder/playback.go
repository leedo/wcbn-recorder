package recorder

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wcbn-recorder/internal/store"
)

// A finished recording can be played back in the live card, from the
// Recordings page. The page asks /recordings/{id}/play for the card, the
// player bar and the songs' times, and plays /recordings/{id}/audio: the
// recording's show files (more than one when the airing was resumed) as one
// MP3, without their ID3 tags, so a player sees one continuous stream it can
// seek in. The page keeps where it got to in the browser, and playing it
// again starts from there.

// playbackView is the live card's playback form.
type playbackView struct {
	ID          int64
	ShowID      int64
	Title       string
	DJ          string
	Image       string
	StationLogo string
	Start, End  time.Time
	DurationMs  int64
	Bytes       int64      // of audio, for the bitrate
	Marks       []songMark // where each song after the first starts
}

// songMark is a song change on the playback progress bar.
type songMark struct {
	Start   int64   // ms into the audio, as in playbackSong, for the page to find it by
	Percent float64 // of the way along the bar
	Label   string  // "<title> — <artist>"
	Align   string  // the tooltip's side: "left" near the start, "right" near the end, else ""
}

// songMarks places the song changes along the progress bar.
func songMarks(songs []playbackSong, durationMs int64) []songMark {
	var marks []songMark
	for i, s := range songs {
		if i == 0 || durationMs <= 0 || s.Start <= 0 || s.Start >= durationMs {
			continue
		}
		m := songMark{Start: s.Start, Percent: float64(s.Start) * 100 / float64(durationMs), Label: s.Title}
		if s.Artist != "" {
			m.Label += " — " + s.Artist
		}
		switch {
		case m.Percent < 20:
			m.Align = "left"
		case m.Percent > 80:
			m.Align = "right"
		}
		marks = append(marks, m)
	}
	return marks
}

// playbackSong is a song's place in the played-back audio.
type playbackSong struct {
	Start  int64  `json:"start"` // ms from the start of the audio
	End    int64  `json:"end"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	// Parts is Artist split into names linked to Wikipedia, for the page.
	Parts []artistPart `json:"parts,omitempty"`
}

// playbackSongs places songs in the audio of every run of a recording, one
// after the other. Each run's songs are timed from the start of its own show
// file, and a run's first song starts at 0.
func playbackSongs(songs []store.Song) []playbackSong {
	var out []playbackSong
	var offset, runEnd int64
	for i, s := range songs {
		if i > 0 && s.StartMs == 0 {
			offset = runEnd // a resumed run, whose audio follows the last run's
		}
		start := offset + s.StartMs
		out = append(out, playbackSong{Start: start, End: start + s.DurationMs, Title: s.Title, Artist: s.Artist})
		runEnd = start + s.DurationMs
	}
	return out
}

// showFiles lists a recording's show files in the order they were recorded:
// the first run's, then those of runs that resumed it, named " 2", " 3", …
// after it.
func (a *App) showFiles(rec store.Recording) ([]string, error) {
	if rec.ShowFile == "" {
		return nil, os.ErrNotExist
	}
	first, err := a.destPath(rec.ShowFile)
	if err != nil {
		return nil, err
	}
	if !exists(first) {
		return nil, os.ErrNotExist
	}
	files := []string{first}
	ext := filepath.Ext(first)
	for n := 2; ; n++ {
		p := fmt.Sprintf("%s %d%s", strings.TrimSuffix(first, ext), n, ext)
		if !exists(p) {
			return files, nil
		}
		files = append(files, p)
	}
}

// id3Size is the length of the ID3v2 tag at the start of r, or 0.
func id3Size(r io.ReaderAt) (int64, error) {
	var h [10]byte
	if _, err := r.ReadAt(h[:], 0); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		return 0, err
	}
	if string(h[:3]) != "ID3" {
		return 0, nil
	}
	size := int64(h[6])<<21 | int64(h[7])<<14 | int64(h[8])<<7 | int64(h[9])
	size += 10
	if h[5]&0x10 != 0 {
		size += 10 // a footer
	}
	return size, nil
}

// joinedAudio is the audio of several files, after their tags, read as one.
type joinedAudio struct {
	parts []*io.SectionReader
	size  int64
	pos   int64
}

func (j *joinedAudio) Read(p []byte) (int, error) {
	off := j.pos
	for _, s := range j.parts {
		if off < s.Size() {
			n, err := s.ReadAt(p[:min(int64(len(p)), s.Size()-off)], off)
			j.pos += int64(n)
			if errors.Is(err, io.EOF) && n > 0 {
				err = nil
			}
			return n, err
		}
		off -= s.Size()
	}
	return 0, io.EOF
}

func (j *joinedAudio) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += j.pos
	case io.SeekEnd:
		offset += j.size
	}
	if offset < 0 {
		return 0, errors.New("seek before start")
	}
	j.pos = offset
	return offset, nil
}

// finishedRecording looks up a recording that can be played back: one in
// the database that isn't being recorded now.
func (a *App) finishedRecording(ctx context.Context, idStr string) (store.Recording, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return store.Recording{}, sql.ErrNoRows
	}
	if a.activeRecording(idStr) != nil {
		return store.Recording{}, errInProgress
	}
	return a.store.Recording(ctx, id)
}

func playbackError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, os.ErrNotExist):
		http.Error(w, "that recording's audio isn't there", http.StatusNotFound)
	case errors.Is(err, errInProgress):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		httpError(w, err)
	}
}

// serveRecordingAudio serves a recording's audio, with range requests, so
// the player can seek.
func (a *App) serveRecordingAudio(w http.ResponseWriter, r *http.Request) {
	rec, err := a.finishedRecording(r.Context(), r.PathValue("id"))
	if err != nil {
		playbackError(w, err)
		return
	}
	files, err := a.showFiles(rec)
	if err != nil {
		playbackError(w, err)
		return
	}
	var audio joinedAudio
	var modified time.Time
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			playbackError(w, err)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			httpError(w, err)
			return
		}
		tag, err := id3Size(f)
		if err != nil {
			httpError(w, err)
			return
		}
		tag = min(tag, fi.Size())
		audio.parts = append(audio.parts, io.NewSectionReader(f, tag, fi.Size()-tag))
		audio.size += fi.Size() - tag
		if fi.ModTime().After(modified) {
			modified = fi.ModTime()
		}
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeContent(w, r, "", modified, &audio)
}

// servePlay describes a recording for playback: the live card and player bar
// to show, the audio's URL, and when each song starts and ends.
func (a *App) servePlay(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rec, err := a.finishedRecording(ctx, r.PathValue("id"))
		if err != nil {
			playbackError(w, err)
			return
		}
		if _, err := a.showFiles(rec); err != nil {
			playbackError(w, err)
			return
		}
		songs, err := a.store.Songs(ctx, rec.ID)
		if err != nil {
			httpError(w, err)
			return
		}
		art, err := a.recordingArt(ctx, rec)
		if err != nil {
			httpError(w, err)
			return
		}
		logo := art.StationLogo
		placed := playbackSongs(songs)
		for i := range placed {
			placed[i].Parts = a.credit(placed[i].Artist)
		}
		v := playbackView{ID: rec.ID, ShowID: rec.ShowID, Title: rec.Title, DJ: rec.DJ, StationLogo: logo,
			Start: rec.Start.In(a.cfg.Location), End: rec.End.In(a.cfg.Location), DurationMs: rec.DurationMs, Bytes: rec.Bytes,
			Marks: songMarks(placed, rec.DurationMs)}
		v.Image = art.Image
		if v.DJ == "" {
			v.DJ = art.DJ
		}
		var card, player bytes.Buffer
		if err := tmpl.ExecuteTemplate(&card, "playback", v); err != nil {
			httpError(w, err)
			return
		}
		pv := playerView{Playback: true, Title: v.Title, DJ: v.DJ, Image: v.Image, StationLogo: logo}
		if err := tmpl.ExecuteTemplate(&player, "player", pv); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]any{
			"audio": fmt.Sprintf("/recordings/%d/audio", rec.ID), "card": card.String(), "player": player.String(),
			"durationMs": rec.DurationMs, "songs": placed,
		})
	}
}

// recordingArt is a recording's show artwork, the station logo to fall back
// on, and the show's DJ, from the shows table, for when the recording has none.
type recordingArt struct {
	Image, StationLogo, DJ string
}

func (a *App) recordingArt(ctx context.Context, rec store.Recording) (recordingArt, error) {
	var art recordingArt
	var err error
	if art.StationLogo, err = a.store.Setting(ctx, "station_logo"); err != nil {
		return art, err
	}
	if rec.ShowID == 0 {
		return art, nil
	}
	shows, err := a.store.Shows(ctx)
	if err != nil {
		return art, err
	}
	for _, sh := range shows {
		if sh.ID == rec.ShowID {
			art.Image, art.DJ = sh.Image, sh.DJ
		}
	}
	return art, nil
}
