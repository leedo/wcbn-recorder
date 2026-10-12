package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Song files are written while the show is still recording: when the stream
// title changes, the previous song is complete (its last frame is already in
// the .part file), so it is queued for a background writer that saves it into
// the show's folder and updates the playlist. Writing never blocks the stream.

// startSongWriter starts the background song writer if song files are on.
func (r *recording) startSongWriter(ctx context.Context) {
	if !r.cfg.TrackFiles {
		return
	}
	r.songs = make(chan segment, 256)
	r.songsDone = make(chan struct{})
	go r.songWriter(ctx)
}

// queueSong queues a finished song. Called with r.mu held, from onMetadata.
func (r *recording) queueSong(prev, next trackMark) {
	if r.songs == nil || next.offset <= prev.offset {
		return // song files off, or a title replaced before any audio arrived
	}
	r.songCount++
	r.songs <- segment{
		n: r.songCount, mark: prev, leadOff: prev.lead,
		startOff: prev.offset, endOff: next.offset, startMs: prev.ms, endMs: next.ms,
	}
}

// finishSongs queues the song still playing when the recording stopped, waits
// for every song to be written, and reports how many failed.
func (r *recording) finishSongs() (failed int) {
	if r.songs == nil {
		return 0
	}
	r.mu.Lock()
	segs := r.segments()
	if len(segs) > 0 {
		last := segs[len(segs)-1]
		if len(r.tracks) == 0 || last.mark == r.tracks[len(r.tracks)-1] {
			r.songCount++
			last.n = r.songCount
			r.songs <- last
		}
	}
	r.mu.Unlock()
	r.stopSongWriter()
	return r.songErrors
}

// stopSongWriter closes the queue and waits for the writer to drain it.
func (r *recording) stopSongWriter() {
	if r.songs == nil {
		return
	}
	r.songsOnce.Do(func() { close(r.songs) })
	<-r.songsDone
}

func (r *recording) songWriter(ctx context.Context) {
	defer close(r.songsDone)
	src, err := os.Open(r.part)
	if err != nil {
		log.Printf("song files for %q: %v", r.job.Event.Title, err)
		for range r.songs {
			r.songErrors++
		}
		return
	}
	defer src.Close()

	var playlist []string
	for seg := range r.songs {
		meta := r.showMetadata(ctx)
		if meta.Art != nil {
			r.coverOnce.Do(func() {
				err := os.MkdirAll(r.out.trackDir, 0o755)
				if err == nil {
					err = meta.Art.writeCover(r.out.trackDir)
				}
				if err != nil {
					log.Printf("writing cover for %q: %v", meta.Title, err)
				}
			})
		}
		entry, err := r.writeSong(meta, src, seg)
		if err != nil {
			r.songErrors++
			log.Printf("writing song %d of %q: %v", seg.n, meta.Title, err)
			continue
		}
		playlist = append(playlist, entry...)
		if err := r.writePlaylist(meta, playlist); err != nil {
			log.Printf("writing playlist for %q: %v", meta.Title, err)
		}
		r.mu.Lock()
		if r.songsSaved == nil {
			r.songsSaved = map[int]bool{}
		}
		r.songsSaved[seg.n] = true
		r.mu.Unlock()
		log.Printf("[%s] saved song %d: %s", meta.Title, seg.n, seg.mark.title)
	}
}

// writeSong writes one song file and returns its playlist entry.
func (r *recording) writeSong(meta showMeta, src io.ReaderAt, s segment) ([]string, error) {
	if err := os.MkdirAll(r.out.trackDir, 0o755); err != nil {
		return nil, err
	}
	title, artist := splitSong(s.mark.title)
	name := title
	if artist != "" {
		name = artist + " - " + title
	}
	file := fmt.Sprintf("%02d - %s.mp3", s.n, truncateName(sanitize(name), 150))

	var tag id3Tag
	tag.text("TIT2", title)
	tag.text("TPE1", artist)
	tag.text("TALB", meta.albumTitle())
	tag.text("TPE2", meta.Artist)
	tag.text("TRCK", fmt.Sprint(s.n)) // the total isn't known until the show ends
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
	tag.picture(meta.Art)

	var audio io.Reader = io.NewSectionReader(src, s.startOff, s.endOff-s.startOff)
	if r.cfg.Gapless {
		// Start a couple of frames early, and say how many samples to skip.
		frames, header, err := scanFrames(src, s.leadOff, s.endOff, s.startOff)
		if err != nil {
			return nil, err
		}
		lead, _, err := scanFrames(src, s.leadOff, s.startOff, s.startOff)
		if err != nil {
			return nil, err
		}
		g := gaplessInfo{header: header, frames: frames, bytes: s.endOff - s.leadOff, lead: lead}
		info, err := infoFrame(g)
		if err != nil {
			return nil, err
		}
		fh, _ := parseFrameHeader(header[:])
		tag.commentDesc("iTunSMPB", iTunSMPB(g, fh.samples))
		audio = io.MultiReader(bytes.NewReader(info), io.NewSectionReader(src, s.leadOff, s.endOff-s.leadOff))
	}
	if err := writeTagged(filepath.Join(r.out.trackDir, file), &tag, audio); err != nil {
		return nil, err
	}
	return []string{
		fmt.Sprintf("#EXTINF:%d,%s", (s.endMs-s.startMs+500)/1000, strings.ReplaceAll(name, "\n", " ")),
		file,
	}, nil
}

// writePlaylist rewrites the show's M3U playlist with the songs saved so far.
func (r *recording) writePlaylist(meta showMeta, entries []string) error {
	lines := append([]string{"#EXTM3U", "#PLAYLIST:" + meta.albumTitle()}, entries...)
	path := filepath.Join(r.out.trackDir, r.out.base+".m3u8")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
