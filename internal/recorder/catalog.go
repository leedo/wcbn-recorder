package recorder

import (
	"context"
	"database/sql"
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

// recordingDir is the folder holding a recording's files: the program folder.
func (r *recording) recordingDir() string { return filepath.Dir(r.out.showFile) }

// catalogStart adds the recording to the database as it starts. A resumed
// airing carries on in its earlier recording's row, so the airing is listed,
// and deleted, as one recording.
func (r *recording) catalogStart(ctx context.Context) {
	if r.db == nil {
		return
	}
	ev := r.job.Event
	if r.resumed {
		earlier, err := r.db.ResumedRecording(ctx, ev.Key(), r.relPath(r.recordingDir()))
		if err == nil {
			r.catalogID, r.earlier = earlier.ID, earlier
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("finding %q in the database: %v", ev.Title, err)
		}
	}
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

// catalogFinish saves the recording's files, size and length once it has
// been written. showFile and cueFile are the paths written, or empty.
func (r *recording) catalogFinish(ctx context.Context, meta showMeta, showFile, cueFile string) {
	if r.db == nil || r.catalogID == 0 {
		return
	}
	// The songs come from the DJ's Spinitron playlist, once it's up (see
	// playlists.go), not the stream's titles, which are often missing or stuck.
	if err := r.db.SetSongsPending(ctx, r.catalogID, true); err != nil {
		log.Printf("updating %q in the database: %v", r.job.Event.Title, err)
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
	if e := r.earlier; e.ID != 0 {
		// A resumed airing: add this run to the earlier one's, and keep the
		// earlier one's files, which name the airing's (see deleteRecording).
		// Added to its show file, the size and length are the file's, with the
		// silence for the gap; otherwise the runs' added up.
		if r.merged {
			rec.Bytes += r.prefixBytes
			rec.DurationMs += r.prefixMs
		} else {
			rec.Bytes += e.Bytes
			rec.DurationMs += e.DurationMs
		}
		if e.ShowFile != "" {
			rec.ShowFile = e.ShowFile
		}
		if e.CueFile != "" {
			rec.CueFile = e.CueFile
		}
	}
	if err := r.db.UpdateRecording(ctx, rec); err != nil {
		log.Printf("updating %q in the database: %v", r.job.Event.Title, err)
	}
}

// catalogDiscard removes a recording that produced no audio, unless it's a
// resumed airing's, which has the earlier run's.
func (r *recording) catalogDiscard(ctx context.Context) {
	if r.db == nil || r.catalogID == 0 || r.earlier.ID != 0 {
		return
	}
	if err := r.db.DeleteRecording(ctx, r.catalogID); err != nil {
		log.Printf("removing %q from the database: %v", r.job.Event.Title, err)
	}
}

// errInProgress is returned for deleting a recording that's still going.
var errInProgress = errors.New("that recording is still in progress")

// ownsFolder reports whether a recording's folder (store.Recording.Dir) is
// its own song folder, "<program>/<show - date>", from when each song was
// also saved as its own file, rather than the program folder it shares with
// other recordings. Names are sanitized, so they never contain a slash
// themselves.
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
// database, returning how many recordings that removed. A recording's files
// sit in the program folder, which other recordings share, so only its own
// files go: its show file and CUE sheet, and those of any runs that resumed
// it, named " 2", " 3", … after them. An older recording with its own song
// folder holds everything it wrote there, so the folder goes (and the
// program folder, if that leaves it empty).
func (a *App) deleteRecording(ctx context.Context, id int64) (int64, error) {
	rec, err := a.store.Recording(ctx, id)
	if err != nil {
		return 0, err
	}
	own := ownsFolder(rec.Dir)
	a.mu.Lock()
	for _, r := range a.active {
		if r.catalogID == id {
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
		ext := filepath.Ext(path)
		for n := 2; ; n++ {
			p := fmt.Sprintf("%s %d%s", strings.TrimSuffix(path, ext), n, ext)
			if err := os.Remove(p); errors.Is(err, os.ErrNotExist) {
				break
			} else if err != nil {
				return 0, err
			}
			log.Printf("deleted %s", p)
		}
	}
	if err := a.store.DeleteRecording(ctx, id); err != nil {
		return 0, err
	}
	return 1, nil
}
