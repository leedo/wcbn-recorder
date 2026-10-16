package recorder

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"wcbn-recorder/internal/store"
)

// The details page (/recordings/{id}) shows everything known about one
// recording: its airing, when it was recorded, its files on disk and their
// sizes, and its songs.

type recordingDetails struct {
	recordingView
	Files   []fileView
	Art     recordingArt
	Songs   []store.Song
	Artists [][]artistPart // each song's artist credit, linked to Wikipedia where it can be
	Starts  []int64        // where each song starts in the played-back audio, to jump to
	State   string         // see recordingsState
}

// fileView is one of a recording's files.
type fileView struct {
	Name    string // without its folder, which is the show's
	Kind    string
	Size    int64
	Missing bool
}

// recordingDetails gathers a recording's details. active is the recordings
// in progress.
func (a *App) recordingDetails(ctx context.Context, id int64, active []liveStatus) (recordingDetails, error) {
	rec, err := a.store.Recording(ctx, id)
	if err != nil {
		return recordingDetails{}, err
	}
	songs, err := a.store.Songs(ctx, id)
	if err != nil {
		return recordingDetails{}, err
	}
	d := recordingDetails{
		recordingView: recordingView{Recording: rec, OwnFolder: ownsFolder(rec.Dir)},
		Songs:         songs,
		State:         recordingsState(active, a.songsChanged.Load()),
	}
	if d.Art, err = a.recordingArt(ctx, rec); err != nil {
		return recordingDetails{}, err
	}
	for _, s := range playbackSongs(songs) {
		d.Starts = append(d.Starts, s.Start)
	}
	a.artists.mu.Lock()
	err = a.artists.load(ctx, a.store, time.Now())
	a.artists.mu.Unlock()
	if err != nil {
		return recordingDetails{}, err
	}
	for _, s := range songs {
		d.Artists = append(d.Artists, a.credit(s.Artist))
	}
	for _, st := range active {
		if st.ID == id {
			d.InProgress, d.Ending = true, st.Finishing || st.Stopped
		}
	}
	add := func(rel, kind string) {
		if rel == "" {
			return
		}
		f := fileView{Name: path.Base(rel), Kind: kind}
		if path, err := a.destPath(rel); err != nil {
			f.Missing = true
		} else if fi, err := os.Stat(path); err != nil {
			f.Missing = true
		} else {
			f.Size = fi.Size()
		}
		d.Files = append(d.Files, f)
	}
	rel := func(path string) string {
		r, err := filepath.Rel(a.cfg.Destination, path)
		if err != nil {
			return path
		}
		return filepath.ToSlash(r)
	}
	if files, err := a.showFiles(rec); err == nil {
		for i, path := range files {
			kind, cueKind := "Show file", "CUE sheet"
			if len(files) > 1 {
				kind += " (run " + strconv.Itoa(i+1) + ")"
				cueKind += " (run " + strconv.Itoa(i+1) + ")"
			}
			add(rel(path), kind)
			if exists(cuePath(path)) {
				add(rel(cuePath(path)), cueKind)
			}
		}
	} else {
		add(rec.ShowFile, "Show file")
		add(rec.CueFile, "CUE sheet")
	}
	// Older recordings saved each song as its own file, with a cover.
	add(rec.Cover, "Cover")
	for _, s := range songs {
		add(s.File, "Song "+strconv.Itoa(s.Number))
	}
	return d, nil
}
