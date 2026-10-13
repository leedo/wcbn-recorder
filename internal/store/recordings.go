package store

import (
	"context"
	"database/sql"
	"time"

	"wcbn-recorder/internal/spinitron"
)

// Recording is one row of the recordings table: one run of the recorder over
// an airing. A resumed airing gets a second row with the same Airing, since it
// writes its own show file and CUE sheet into the same folder.
//
// Paths are relative to the configured destination, with forward slashes, so
// the recordings can be moved as a whole. Dir is the folder holding the
// recording's files: the song folder, or the program folder when song files
// are off. The file paths are empty when that file wasn't written.
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
	ShowFile    string
	CueFile     string
	Playlist    string
	Cover       string
	Bytes       int64
	DurationMs  int64
	Songs       int // number of songs; filled in by Recordings
}

// Song is one row of the songs table: a song in a recording, whether or not
// it was written to its own file.
type Song struct {
	ID          int64
	RecordingID int64
	Number      int // track number, as in the file name; carries on across a resumed airing
	Title       string
	Artist      string
	StreamTitle string    // as announced, with " on <show> with <dj>" removed
	PlayedAt    time.Time // when the stream announced it
	StartMs     int64     // position in the recording's show file
	DurationMs  int64
	File        string // relative to the destination; empty if song files are off
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

// AddRecording records that a recording has started and returns its ID. The
// show is linked by title, if it's in the shows table.
func (s *Store) AddRecording(ctx context.Context, rec Recording) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO recordings (show_id, airing, title, dj, category, description, url, start_at, end_at, started_at, dir, playlist, cover)
		VALUES ((SELECT id FROM shows WHERE key = ?), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spinitron.Normalize(rec.Title), rec.Airing, rec.Title, rec.DJ, rec.Category, rec.Description, rec.URL,
		rec.Start.Unix(), rec.End.Unix(), rec.StartedAt.Unix(), rec.Dir, rec.Playlist, rec.Cover)
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
			show_file = ?, cue_file = ?, playlist = ?, cover = ?, bytes = ?, duration_ms = ?
		WHERE id = ?`,
		rec.DJ, rec.Description, rec.URL, unixOrZero(rec.FinishedAt),
		rec.ShowFile, rec.CueFile, rec.Playlist, rec.Cover, rec.Bytes, rec.DurationMs, rec.ID)
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
		INSERT INTO songs (recording_id, number, title, artist, stream_title, played_at, start_ms, duration_ms, file)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (recording_id, number) DO UPDATE SET
			title = excluded.title, artist = excluded.artist, stream_title = excluded.stream_title,
			played_at = excluded.played_at, start_ms = excluded.start_ms,
			duration_ms = excluded.duration_ms, file = excluded.file`,
		song.RecordingID, song.Number, song.Title, song.Artist, song.StreamTitle,
		song.PlayedAt.Unix(), song.StartMs, song.DurationMs, song.File)
	return err
}

// Recordings returns every recording, most recent airing first.
func (s *Store) Recordings(ctx context.Context) ([]Recording, error) {
	return s.queryRecordings(ctx, ``)
}

// Recording returns one recording, or sql.ErrNoRows.
func (s *Store) Recording(ctx context.Context, id int64) (Recording, error) {
	recs, err := s.queryRecordings(ctx, `WHERE r.id = ?`, id)
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

func (s *Store) queryRecordings(ctx context.Context, where string, args ...any) ([]Recording, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, COALESCE(r.show_id, 0), r.airing, r.title, r.dj, r.category, r.description, r.url,
			r.start_at, r.end_at, r.started_at, r.finished_at, r.dir, r.show_file, r.cue_file, r.playlist, r.cover,
			r.bytes, r.duration_ms, (SELECT COUNT(*) FROM songs WHERE recording_id = r.id)
		FROM recordings r `+where+` ORDER BY r.start_at DESC, r.started_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recording
	for rows.Next() {
		var r Recording
		var start, end, started, finished int64
		if err := rows.Scan(&r.ID, &r.ShowID, &r.Airing, &r.Title, &r.DJ, &r.Category, &r.Description, &r.URL,
			&start, &end, &started, &finished, &r.Dir, &r.ShowFile, &r.CueFile, &r.Playlist, &r.Cover,
			&r.Bytes, &r.DurationMs, &r.Songs); err != nil {
			return nil, err
		}
		r.Start, r.End, r.StartedAt, r.FinishedAt = time.Unix(start, 0), time.Unix(end, 0), time.Unix(started, 0), timeOrZero(finished)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Songs returns a recording's songs in air order.
func (s *Store) Songs(ctx context.Context, recordingID int64) ([]Song, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, recording_id, number, title, artist, stream_title, played_at, start_ms, duration_ms, file
		FROM songs WHERE recording_id = ? ORDER BY number`, recordingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Song
	for rows.Next() {
		var sg Song
		var played int64
		if err := rows.Scan(&sg.ID, &sg.RecordingID, &sg.Number, &sg.Title, &sg.Artist, &sg.StreamTitle,
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
