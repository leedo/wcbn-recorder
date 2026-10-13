package recorder

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wcbn-recorder/internal/store"
)

// Each recording, and each song in it, is listed in the database, with paths
// relative to the destination, so past recordings can be browsed, and
// deleted from the Recordings page. The files on disk are the recording; a
// database error is logged and recording carries on. Recordings made without
// a database (the -test flag) aren't listed.

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

// errInProgress is returned for deleting a recording that's still going.
var errInProgress = errors.New("that recording is still in progress")

// ownsFolder reports whether a recording's folder (store.Recording.Dir) is
// its own song folder, "<program>/<show - date>", rather than the program
// folder it shares with other recordings, as when song files are off. Names
// are sanitized, so they never contain a slash themselves.
func ownsFolder(dir string) bool { return strings.Count(dir, "/") == 1 }

// destPath turns a path relative to the destination, from the database,
// into a file path, refusing any that isn't inside the destination.
func (a *App) destPath(rel string) (string, error) {
	p := filepath.Join(a.cfg.Destination, filepath.FromSlash(rel))
	r, err := filepath.Rel(a.cfg.Destination, p)
	if rel == "" || err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q isn't a path inside the destination", rel)
	}
	return p, nil
}

// deleteRecording deletes a recording's files and removes it from the
// database, returning how many recordings that removed. A recording's song
// folder holds everything it wrote, along with any recording of the same
// airing it resumed, so the folder goes, with every recording in it (and
// the program folder, if that leaves it empty). With song files off, the
// recording's folder is the program folder, which other recordings share,
// so only its own files go.
func (a *App) deleteRecording(ctx context.Context, id int64) (int64, error) {
	rec, err := a.store.Recording(ctx, id)
	if err != nil {
		return 0, err
	}
	own := ownsFolder(rec.Dir)
	a.mu.Lock()
	for _, r := range a.active {
		if r.catalogID == id || own && r.relPath(r.recordingDir()) == rec.Dir {
			a.mu.Unlock()
			return 0, errInProgress
		}
	}
	a.mu.Unlock()

	if own {
		dir, err := a.destPath(rec.Dir)
		if err != nil {
			return 0, err
		}
		if err := os.RemoveAll(dir); err != nil {
			return 0, err
		}
		os.Remove(filepath.Dir(dir)) // the program folder, only if that left it empty
		log.Printf("deleted %s", dir)
		return a.store.DeleteRecordingsIn(ctx, rec.Dir)
	}
	for _, rel := range []string{rec.ShowFile, rec.CueFile} {
		if rel == "" {
			continue
		}
		path, err := a.destPath(rel)
		if err != nil {
			return 0, err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		log.Printf("deleted %s", path)
	}
	if err := a.store.DeleteRecording(ctx, id); err != nil {
		return 0, err
	}
	return 1, nil
}
