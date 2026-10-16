package recorder

import (
	"context"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"time"

	"wcbn-recorder/internal/mpeg"
	"wcbn-recorder/internal/spinitron"
	"wcbn-recorder/internal/store"
)

// A recording's songs come from the DJ's playlist on Spinitron, not the
// stream's titles, which are often missing or stuck (those still name the
// song playing on the live card, and the first CUE sheet). While it's
// recording, the songs logged so far are listed as they turn up (see
// watchPlaylist). A finished recording is marked as waiting for its songs
// (store.Recording.SongsPending) until they're filled in from the whole
// playlist, and its CUE sheet rewritten with them. The playlist may not be up when the show
// ends, so it's looked for each time the schedule is refreshed, until it
// turns up or playlistGiveUp has passed.

const (
	// playlistGrace is how long after an airing ends its playlist is left for
	// the DJ to finish, before it's read.
	playlistGrace = 10 * time.Minute
	// playlistGiveUp is how long after an airing its playlist is looked for.
	playlistGiveUp = 7 * 24 * time.Hour
	// playlistLiveInterval is how often a recording in progress reads its
	// playlist, for DJs who log songs as they play them.
	playlistLiveInterval = time.Minute
)

// watchPlaylist reads rec's playlist every playlistLiveInterval while it's
// recording (until ctx ends), listing the songs logged so far. The recording
// still waits for its songs: the full playlist is read once the airing's
// over, as songs are often logged late.
func (a *App) watchPlaylist(ctx context.Context, rec *recording) {
	if rec.db == nil || rec.catalogID == 0 {
		return
	}
	t := time.NewTicker(playlistLiveInterval)
	defer t.Stop()
	w := liveSongs{rec: rec}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.updateLiveSongs(ctx, &w)
	}
}

// liveSongs is what watchPlaylist knows of a recording's playlist so far.
type liveSongs struct {
	rec    *recording
	listed int  // songs listed so far
	failed bool // the last read failed (logged once, until one works)
}

// updateLiveSongs reads the playlist of w's recording, and lists its songs
// so far, timed in the audio recorded so far.
func (a *App) updateLiveSongs(ctx context.Context, w *liveSongs) {
	rec := w.rec
	ev := rec.job.Event
	pls, err := spinitron.FetchPlaylists(ctx, a.cfg.ScheduleURL, a.cfg.Location, ev.Title, ev.Start, ev.End)
	if err != nil {
		if !w.failed && ctx.Err() == nil {
			log.Printf("reading the Spinitron playlist of %q: %v", ev.Title, err)
		}
		w.failed = true
		return
	}
	w.failed = false
	rec.mu.Lock()
	row := store.Recording{ID: rec.catalogID, StartedAt: rec.started.Truncate(time.Second), DurationMs: rec.audio.DurationMs()}
	rec.mu.Unlock()
	if e := rec.earlier; e.ID != 0 {
		// A resumed airing: the songs are timed from when it was first
		// recorded, as its audio will be once this run's is added to the
		// earlier run's file, after silence for the gap (see mergeShowFile).
		row.StartedAt = e.StartedAt
		row.DurationMs += rec.started.Sub(e.StartedAt).Milliseconds()
	}
	songs := playlistSongs(pls, row, a.cfg.Location)
	if len(songs) == 0 {
		return
	}
	if err := a.store.SetSongsSoFar(ctx, rec.catalogID, songs, pls[0].URL); err != nil {
		log.Printf("saving the songs of %q: %v", ev.Title, err)
		return
	}
	if len(songs) != w.listed {
		log.Printf("[%s] %d songs so far from its Spinitron playlist", ev.Title, len(songs))
		w.listed = len(songs)
		a.songsUpdated()
	}
}

// checkPlaylists fills in pending songs in the background, unless that's
// already running.
func (a *App) checkPlaylists(ctx context.Context) {
	if !a.playlistsRunning.CompareAndSwap(false, true) {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer a.playlistsRunning.Store(false)
		if a.fillPendingSongs(ctx, time.Now()) {
			a.checkArtists(ctx) // the new songs' artists
		}
	}()
}

// fillPendingSongs looks for the playlists of recordings waiting for their
// songs, and lists the songs of any it finds, reporting whether it did.
func (a *App) fillPendingSongs(ctx context.Context, now time.Time) (filled bool) {
	recs, err := a.store.SongsPending(ctx)
	if err != nil {
		log.Printf("loading recordings waiting for songs: %v", err)
		return false
	}
	for _, rec := range recs {
		if ctx.Err() != nil {
			return
		}
		if a.activeRecording(strconv.FormatInt(rec.ID, 10)) != nil || now.Before(rec.End.Add(playlistGrace)) {
			continue
		}
		if now.After(rec.End.Add(playlistGiveUp)) {
			log.Printf("no Spinitron playlist for %q from %s; giving up on its songs", rec.Title, rec.Start.In(a.cfg.Location).Format("Jan 2"))
			if err := a.store.SetSongsPending(ctx, rec.ID, false); err != nil {
				log.Printf("updating %q in the database: %v", rec.Title, err)
			}
			continue
		}
		pls, err := spinitron.FetchPlaylists(ctx, a.cfg.ScheduleURL, a.cfg.Location, rec.Title, rec.Start, rec.End)
		if err != nil {
			log.Printf("looking for the Spinitron playlist of %q: %v", rec.Title, err)
			continue
		}
		songs := playlistSongs(pls, rec, a.cfg.Location)
		if len(songs) == 0 {
			continue // not up yet; try again next time
		}
		if err := a.store.SetSongs(ctx, rec.ID, songs, pls[0].URL); err != nil {
			log.Printf("saving the songs of %q: %v", rec.Title, err)
			continue
		}
		log.Printf("listed %d songs for %q from its Spinitron playlist", len(songs), rec.Title)
		if err := a.rewriteCues(rec, songs); err != nil {
			log.Printf("rewriting the CUE sheet of %q: %v", rec.Title, err)
		}
		filled = true
	}
	if filled {
		a.songsUpdated()
	}
	return filled
}

// playlistSongs places a recording's playlists' songs in its audio, from
// when recording started. The song playing when it started starts at 0, and
// songs logged after it ended are left out. A resumed recording's runs are
// joined with silence for the gap between them (see mergeShowFile), so its
// audio keeps time; older ones, resumed into files of their own, were joined
// with none, so their later songs may be early by the time they missed.
func playlistSongs(pls []spinitron.Playlist, rec store.Recording, loc *time.Location) []store.Song {
	type placed struct {
		at   time.Time
		spin spinitron.Spin
	}
	var all []placed
	for _, pl := range pls {
		for i, at := range pl.Times(loc) {
			all = append(all, placed{at, pl.Spins[i]})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })

	var songs []store.Song
	for _, p := range all {
		ms := p.at.Sub(rec.StartedAt).Milliseconds()
		if ms >= rec.DurationMs {
			break
		}
		if ms < 0 {
			songs = songs[:0] // logged before it started: only the last of those was playing at the start
			ms = 0
		}
		songs = append(songs, store.Song{RecordingID: rec.ID, Title: p.spin.Song, Artist: p.spin.Artist,
			Album: p.spin.Album, Label: p.spin.Label, Year: p.spin.Year, PlayedAt: p.at, StartMs: ms})
	}
	for i := range songs {
		songs[i].Number = i + 1
		end := rec.DurationMs
		if i+1 < len(songs) {
			end = songs[i+1].StartMs
		}
		songs[i].DurationMs = end - songs[i].StartMs
	}
	return songs
}

// rewriteCues rewrites a recording's CUE sheets with songs, timed in its
// audio as a whole. A resumed recording has a show file per run, and so a
// CUE sheet per run: each lists the songs in its file, starting with the one
// already playing when the file starts.
func (a *App) rewriteCues(rec store.Recording, songs []store.Song) error {
	files, err := a.showFiles(rec)
	if err != nil {
		return err
	}
	meta := showMeta{Title: rec.Title, Start: rec.Start.In(a.cfg.Location), DJ: rec.DJ, Artist: stationName, Genre: "Freeform", URL: rec.URL}
	if rec.DJ != "" {
		meta.Artist = rec.DJ
	}
	if rec.Category != "" {
		meta.Genre = rec.Category
	}
	var offset int64 // where this file starts in the audio as a whole
	for i, path := range files {
		end := rec.DurationMs
		if i < len(files)-1 {
			ms, err := audioDurationMs(path)
			if err != nil {
				return err
			}
			end = offset + ms
		}
		var tracks []cueTrack
		for j, s := range songs {
			playingAtStart := s.StartMs <= offset && (j+1 == len(songs) || songs[j+1].StartMs > offset)
			if (s.StartMs >= offset && s.StartMs < end) || playingAtStart {
				tracks = append(tracks, cueTrack{s.Title, s.Artist, max(s.StartMs-offset, 0)})
			}
		}
		if len(tracks) > 0 {
			if err := writeCueSheet(meta, path, tracks); err != nil {
				return err
			}
		}
		offset = end
	}
	return nil
}

// audioDurationMs is the length of the audio in an MP3 file, counted from
// its frames.
func audioDurationMs(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	tag, err := id3Size(f)
	if err != nil {
		return 0, err
	}
	fw := mpeg.NewFrameWriter(io.Discard)
	if _, err := io.Copy(fw, io.NewSectionReader(f, tag, fi.Size()-min(tag, fi.Size()))); err != nil {
		return 0, err
	}
	return fw.DurationMs(), nil
}
