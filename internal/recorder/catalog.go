package recorder

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"wcbn-recorder/internal/store"
)

// Each recording, and each song in it, is listed in the database, with paths
// relative to the destination, so past recordings can be browsed. The files
// on disk are the recording; a database error is logged and recording carries
// on. Recordings made without a database (the -test flag) aren't listed.

// relPath is path relative to the destination, with forward slashes.
func (r *recording) relPath(path string) string {
	rel, err := filepath.Rel(r.cfg.Destination, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// recordingDir is the folder holding a recording's files: the song folder,
// or the program folder when song files are off.
func (r *recording) recordingDir() string {
	if r.cfg.TrackFiles {
		return r.out.trackDir
	}
	return filepath.Dir(r.out.showFile)
}

// catalogStart adds the recording to the database as it starts.
func (r *recording) catalogStart(ctx context.Context) {
	if r.db == nil {
		return
	}
	ev := r.job.Event
	rec := store.Recording{
		Airing: ev.Key(), Title: ev.Title, DJ: ev.DJ, Category: ev.Category, URL: ev.URL,
		Start: ev.Start, End: ev.End, StartedAt: r.started, Dir: r.relPath(r.recordingDir()),
	}
	id, err := r.db.AddRecording(ctx, rec)
	if err != nil {
		log.Printf("adding %q to the database: %v", ev.Title, err)
		return
	}
	r.catalogID = id
}

// catalogSong adds a song to the database. file is the song file's path, or
// empty if it has none.
func (r *recording) catalogSong(ctx context.Context, s segment, file string) {
	if r.db == nil || r.catalogID == 0 {
		return
	}
	title, artist := splitSong(s.mark.title)
	song := store.Song{
		RecordingID: r.catalogID, Number: s.n, Title: title, Artist: artist, StreamTitle: s.mark.title,
		PlayedAt: s.mark.at, StartMs: s.startMs, DurationMs: s.endMs - s.startMs,
	}
	if file != "" {
		song.File = r.relPath(file)
	}
	if err := r.db.AddSong(ctx, song); err != nil {
		log.Printf("adding song %d of %q to the database: %v", s.n, r.job.Event.Title, err)
	}
}

// catalogFinish saves the recording's files, size and length once it has
// been written. showFile and cueFile are the paths written, or empty.
func (r *recording) catalogFinish(ctx context.Context, meta showMeta, showFile, cueFile string) {
	if r.db == nil || r.catalogID == 0 {
		return
	}
	if !r.cfg.TrackFiles {
		// No song writer ran, so list the songs (the show file's chapters) now.
		for i, s := range r.segments() {
			s.n = r.songBase + i + 1
			r.catalogSong(ctx, s, "")
		}
	}
	rec := store.Recording{
		ID: r.catalogID, DJ: meta.DJ, Description: meta.Description, URL: meta.URL, FinishedAt: time.Now(),
		Bytes: r.audio.Written(), DurationMs: r.audio.DurationMs(),
	}
	if showFile != "" {
		rec.ShowFile = r.relPath(showFile)
	}
	if cueFile != "" {
		rec.CueFile = r.relPath(cueFile)
	}
	if r.cfg.TrackFiles {
		if playlist := filepath.Join(r.out.trackDir, r.out.base+".m3u8"); exists(playlist) {
			rec.Playlist = r.relPath(playlist)
		}
		if meta.Art != nil {
			if cover := filepath.Join(r.out.trackDir, meta.Art.coverName()); exists(cover) {
				rec.Cover = r.relPath(cover)
			}
		}
	}
	if err := r.db.UpdateRecording(ctx, rec); err != nil {
		log.Printf("updating %q in the database: %v", r.job.Event.Title, err)
	}
}

// catalogDiscard removes a recording that produced no audio.
func (r *recording) catalogDiscard(ctx context.Context) {
	if r.db == nil || r.catalogID == 0 {
		return
	}
	if err := r.db.DeleteRecording(ctx, r.catalogID); err != nil {
		log.Printf("removing %q from the database: %v", r.job.Event.Title, err)
	}
}
