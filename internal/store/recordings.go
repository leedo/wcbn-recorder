package store

import (
	"context"
	"database/sql"
	"time"

	"wcbn-recorder/internal/spinitron"
)

// Recording is one row of the recordings table: an airing recorded into a
// folder. A resumed airing (the recorder was restarted, or the show was ended
// and added back) carries on in the same row; its own show file and CUE sheet
// are written next to the first ones, with " 2", " 3", … added to the names.
//
// Paths are relative to the configured destination, with forward slashes, so
// the recordings can be moved as a whole. Dir is the folder holding the
// recording's files: the program folder, or, for recordings from when each
// song was also saved as its own file, the song folder. The file paths are
// empty when that file wasn't written.
type Recording struct {
	ID          int64
	ShowID      int64  // 0 if the show isn't (or is no longer) in the shows table
	Airing      string // spinitron.Event.Key
	Title       string
	DJ          string
	Category    string
	Description string
	URL         string
	Start, End  time.Time // the airing's scheduled time slot
	StartedAt   time.Time
	FinishedAt  time.Time // zero while recording, or if the recorder stopped without finishing
	Dir         string
	ShowFile    string // the first run's, when an airing was resumed
	CueFile     string // likewise
	Cover       string // only in a song folder
	Bytes       int64
	DurationMs  int64
	Songs       int // number of songs; filled in by Recordings
	// SongsPending is set when the stream gave no song titles, until the
	// songs are filled in from the DJ's Spinitron playlist (or given up on).
	SongsPending bool
	PlaylistURL  string // the airing's playlist page on Spinitron, once its songs are found there
}

// Song is one row of the songs table: a song in a recording, whether or not
// it was written to its own file.
type Song struct {
	ID          int64
	RecordingID int64
	Number      int // track number, as in the file name; carries on across a resumed airing
	Title       string
	Artist      string
	Album       string    // the release, from the DJ's Spinitron playlist; "" if unknown
	Label       string    // its record label, likewise
	Year        int       // the year it was released; 0 if unknown
	StreamTitle string    // as announced, with " on <show> with <dj>" removed
	PlayedAt    time.Time // when the stream announced it
	StartMs     int64     // position in the recording's show file
	DurationMs  int64
	File        string // the song's own file, relative to the destination; only in a song folder
}

var recordingMigration = `
	CREATE TABLE recordings (
		id          INTEGER PRIMARY KEY,
		show_id     INTEGER REFERENCES shows (id) ON DELETE SET NULL,
		airing      TEXT NOT NULL,
		title       TEXT NOT NULL,
		dj          TEXT NOT NULL DEFAULT '',
		category    TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		url         TEXT NOT NULL DEFAULT '',
		start_at    INTEGER NOT NULL, -- the airing's time slot, unix seconds
		end_at      INTEGER NOT NULL,
		started_at  INTEGER NOT NULL, -- unix seconds
		finished_at INTEGER NOT NULL DEFAULT 0, -- unix seconds; 0 until finished
		dir         TEXT NOT NULL,
		show_file   TEXT NOT NULL DEFAULT '',
		cue_file    TEXT NOT NULL DEFAULT '',
		playlist    TEXT NOT NULL DEFAULT '',
		cover       TEXT NOT NULL DEFAULT '',
		bytes       INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX recordings_show ON recordings (show_id, start_at);
	CREATE INDEX recordings_airing ON recordings (airing);
	CREATE TABLE songs (
		id           INTEGER PRIMARY KEY,
		recording_id INTEGER NOT NULL REFERENCES recordings (id) ON DELETE CASCADE,
		number       INTEGER NOT NULL,
		title        TEXT NOT NULL,
		artist       TEXT NOT NULL DEFAULT '',
		stream_title TEXT NOT NULL DEFAULT '',
		played_at    INTEGER NOT NULL, -- unix seconds
		start_ms     INTEGER NOT NULL,
		duration_ms  INTEGER NOT NULL,
		file         TEXT NOT NULL DEFAULT '',
		UNIQUE (recording_id, number)
	);
	CREATE INDEX songs_artist ON songs (artist);`

// mergeResumedRecordings merges the rows of each resumed airing, which used
// to get a row per run, into its first: the songs (numbered on from the
// earlier runs' if a run started over at 1), size and length are added up,
// and the first run's files kept.
func mergeResumedRecordings(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT airing, dir FROM recordings GROUP BY airing, dir HAVING COUNT(*) > 1`)
	if err != nil {
		return err
	}
	type group struct{ airing, dir string }
	var groups []group
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.airing, &g.dir); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range groups {
		rows, err := tx.Query(`SELECT id, finished_at, bytes, duration_ms FROM recordings WHERE airing = ? AND dir = ? ORDER BY started_at, id`, g.airing, g.dir)
		if err != nil {
			return err
		}
		type run struct{ id, finished, bytes, ms int64 }
		var runs []run
		for rows.Next() {
			var r run
			if err := rows.Scan(&r.id, &r.finished, &r.bytes, &r.ms); err != nil {
				rows.Close()
				return err
			}
			runs = append(runs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		first := runs[0]
		bytes, ms := first.bytes, first.ms
		for _, r := range runs[1:] {
			var last, low int64
			if err := tx.QueryRow(`SELECT COALESCE(MAX(number), 0) FROM songs WHERE recording_id = ?`, first.id).Scan(&last); err != nil {
				return err
			}
			if err := tx.QueryRow(`SELECT COALESCE(MIN(number), 0) FROM songs WHERE recording_id = ?`, r.id).Scan(&low); err != nil {
				return err
			}
			offset := int64(0)
			if low <= last {
				offset = last
			}
			if _, err := tx.Exec(`UPDATE songs SET recording_id = ?, number = number + ? WHERE recording_id = ?`, first.id, offset, r.id); err != nil {
				return err
			}
			// Details the first run didn't get, such as when it never finished.
			if _, err := tx.Exec(`
				UPDATE recordings SET
					dj = IIF(dj = '', (SELECT dj FROM recordings WHERE id = ?2), dj),
					description = IIF(description = '', (SELECT description FROM recordings WHERE id = ?2), description),
					url = IIF(url = '', (SELECT url FROM recordings WHERE id = ?2), url),
					show_file = IIF(show_file = '', (SELECT show_file FROM recordings WHERE id = ?2), show_file),
					cue_file = IIF(cue_file = '', (SELECT cue_file FROM recordings WHERE id = ?2), cue_file),
					cover = IIF(cover = '', (SELECT cover FROM recordings WHERE id = ?2), cover)
				WHERE id = ?1`, first.id, r.id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM recordings WHERE id = ?`, r.id); err != nil {
				return err
			}
			bytes += r.bytes
			ms += r.ms
		}
		finished := runs[len(runs)-1].finished
		if _, err := tx.Exec(`UPDATE recordings SET bytes = ?, duration_ms = ?, finished_at = ? WHERE id = ?`, bytes, ms, finished, first.id); err != nil {
			return err
		}
	}
	return nil
}

// AddRecording records that a recording has started and returns its ID. The
// show is linked by title, if it's in the shows table.
func (s *Store) AddRecording(ctx context.Context, rec Recording) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO recordings (show_id, airing, title, dj, category, description, url, start_at, end_at, started_at, dir, cover)
		VALUES ((SELECT id FROM shows WHERE key = ?), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spinitron.Normalize(rec.Title), rec.Airing, rec.Title, rec.DJ, rec.Category, rec.Description, rec.URL,
		rec.Start.Unix(), rec.End.Unix(), rec.StartedAt.Unix(), rec.Dir, rec.Cover)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateRecording saves what's known once a recording has been written: the
// show details (from the show page), the files, and the size and length.
func (s *Store) UpdateRecording(ctx context.Context, rec Recording) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE recordings SET
			dj = ?, description = ?, url = ?, finished_at = ?,
			show_file = ?, cue_file = ?, cover = ?, bytes = ?, duration_ms = ?
		WHERE id = ?`,
		rec.DJ, rec.Description, rec.URL, unixOrZero(rec.FinishedAt),
		rec.ShowFile, rec.CueFile, rec.Cover, rec.Bytes, rec.DurationMs, rec.ID)
	return err
}

// DeleteRecording removes a recording and its songs, for one that produced
// no audio.
func (s *Store) DeleteRecording(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM recordings WHERE id = ?`, id)
	return err
}

// AddSong records a song of a recording. Adding the same number again
// replaces it.
func (s *Store) AddSong(ctx context.Context, song Song) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO songs (recording_id, number, title, artist, album, label, year, stream_title, played_at, start_ms, duration_ms, file)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (recording_id, number) DO UPDATE SET
			title = excluded.title, artist = excluded.artist, album = excluded.album, label = excluded.label,
			year = excluded.year, stream_title = excluded.stream_title,
			played_at = excluded.played_at, start_ms = excluded.start_ms,
			duration_ms = excluded.duration_ms, file = excluded.file`,
		song.RecordingID, song.Number, song.Title, song.Artist, song.Album, song.Label, song.Year, song.StreamTitle,
		song.PlayedAt.Unix(), song.StartMs, song.DurationMs, song.File)
	return err
}

// Recordings returns every recording, most recent airing first.
func (s *Store) Recordings(ctx context.Context) ([]Recording, error) {
	return s.queryRecordings(ctx, ``, ``)
}

// RecordingsPage returns up to limit recordings of the show with ID showID
// (or of every show, for 0), most recent airing first, skipping the first
// offset, and how many there are in all.
func (s *Store) RecordingsPage(ctx context.Context, showID int64, limit, offset int) ([]Recording, int, error) {
	where, args := ``, []any{}
	if showID != 0 {
		where, args = `WHERE r.show_id = ?`, []any{showID}
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recordings r `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	recs, err := s.queryRecordings(ctx, where, ` LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	return recs, total, err
}

// Recording returns one recording, or sql.ErrNoRows.
func (s *Store) Recording(ctx context.Context, id int64) (Recording, error) {
	recs, err := s.queryRecordings(ctx, `WHERE r.id = ?`, ``, id)
	if err != nil {
		return Recording{}, err
	}
	if len(recs) == 0 {
		return Recording{}, sql.ErrNoRows
	}
	return recs[0], nil
}

// ResumedRecording returns the recording of an airing in dir, for a resumed
// airing to carry on in, or sql.ErrNoRows.
func (s *Store) ResumedRecording(ctx context.Context, airing, dir string) (Recording, error) {
	recs, err := s.queryRecordings(ctx, `WHERE r.airing = ? AND r.dir = ?`, ``, airing, dir)
	if err != nil {
		return Recording{}, err
	}
	if len(recs) == 0 {
		return Recording{}, sql.ErrNoRows
	}
	return recs[0], nil
}

// DeleteRecordingsIn removes every recording whose files are in dir (more
// than one when an airing was resumed), and their songs.
func (s *Store) DeleteRecordingsIn(ctx context.Context, dir string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM recordings WHERE dir = ?`, dir)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// queryRecordings returns the recordings matching where, most recent airing
// first, and only the part of that list page picks out (a LIMIT clause), if
// any. args fill in the placeholders of both, in order.
func (s *Store) queryRecordings(ctx context.Context, where, page string, args ...any) ([]Recording, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, COALESCE(r.show_id, 0), r.airing, r.title, r.dj, r.category, r.description, r.url,
			r.start_at, r.end_at, r.started_at, r.finished_at, r.dir, r.show_file, r.cue_file, r.cover,
			r.bytes, r.duration_ms, (SELECT COUNT(*) FROM songs WHERE recording_id = r.id), r.songs_pending, r.playlist_url
		FROM recordings r `+where+` ORDER BY r.start_at DESC, r.started_at DESC`+page, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recording
	for rows.Next() {
		var r Recording
		var start, end, started, finished int64
		if err := rows.Scan(&r.ID, &r.ShowID, &r.Airing, &r.Title, &r.DJ, &r.Category, &r.Description, &r.URL,
			&start, &end, &started, &finished, &r.Dir, &r.ShowFile, &r.CueFile, &r.Cover,
			&r.Bytes, &r.DurationMs, &r.Songs, &r.SongsPending, &r.PlaylistURL); err != nil {
			return nil, err
		}
		r.Start, r.End, r.StartedAt, r.FinishedAt = time.Unix(start, 0), time.Unix(end, 0), time.Unix(started, 0), timeOrZero(finished)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetSongsPending marks a recording as waiting for its songs, or not.
func (s *Store) SetSongsPending(ctx context.Context, id int64, pending bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recordings SET songs_pending = ? WHERE id = ?`, pending, id)
	return err
}

// SongsPending returns the recordings waiting for their songs.
func (s *Store) SongsPending(ctx context.Context) ([]Recording, error) {
	return s.queryRecordings(ctx, `WHERE r.songs_pending = 1`, ``)
}

// SetSongs replaces a recording's songs, saves the playlist page they're
// from, and clears SongsPending.
func (s *Store) SetSongs(ctx context.Context, id int64, songs []Song, playlistURL string) error {
	return s.setSongs(ctx, id, songs, playlistURL, true)
}

// SetSongsSoFar is SetSongs for a recording in progress: the songs logged so
// far, with SongsPending left as it is, for the final list to replace.
func (s *Store) SetSongsSoFar(ctx context.Context, id int64, songs []Song, playlistURL string) error {
	return s.setSongs(ctx, id, songs, playlistURL, false)
}

func (s *Store) setSongs(ctx context.Context, id int64, songs []Song, playlistURL string, done bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM songs WHERE recording_id = ?`, id); err != nil {
		return err
	}
	for _, song := range songs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO songs (recording_id, number, title, artist, album, label, year, stream_title, played_at, start_ms, duration_ms, file)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, song.Number, song.Title, song.Artist, song.Album, song.Label, song.Year, song.StreamTitle,
			song.PlayedAt.Unix(), song.StartMs, song.DurationMs, song.File); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recordings SET songs_pending = songs_pending AND NOT ?, playlist_url = ? WHERE id = ?`, done, playlistURL, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Songs returns a recording's songs in air order.
func (s *Store) Songs(ctx context.Context, recordingID int64) ([]Song, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, recording_id, number, title, artist, album, label, year, stream_title, played_at, start_ms, duration_ms, file
		FROM songs WHERE recording_id = ? ORDER BY number`, recordingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Song
	for rows.Next() {
		var sg Song
		var played int64
		if err := rows.Scan(&sg.ID, &sg.RecordingID, &sg.Number, &sg.Title, &sg.Artist, &sg.Album, &sg.Label, &sg.Year, &sg.StreamTitle,
			&played, &sg.StartMs, &sg.DurationMs, &sg.File); err != nil {
			return nil, err
		}
		sg.PlayedAt = time.Unix(played, 0)
		out = append(out, sg)
	}
	return out, rows.Err()
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOrZero(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
