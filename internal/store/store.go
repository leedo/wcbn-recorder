// Package store keeps show details, recording selections, and the catalog of
// recorded shows and songs in SQLite.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"wcbn-recorder/internal/spinitron"

	_ "modernc.org/sqlite"
)

// Store keeps show metadata and recording selections in SQLite.
type Store struct{ db *sql.DB }

// Show is one row of the shows table.
type Show struct {
	ID          int64
	Key         string // normalized title, used to match calendar entries
	Title       string
	DJ          string
	Category    string
	URL         string
	Schedule    string
	Description string
	Image       string // artwork URL from the show page's og:image
	Record      bool
	LastSeen    time.Time // last time the show appeared on the calendar; zero if never
	DetailsAt   time.Time // last time the show page was fetched; zero if never
}

// migrations are applied in order; PRAGMA user_version tracks progress.
// Each is SQL, or a func for one SQL can't easily express.
var migrations = []any{
	`CREATE TABLE shows (
		id          INTEGER PRIMARY KEY,
		key         TEXT NOT NULL UNIQUE,
		title       TEXT NOT NULL,
		dj          TEXT NOT NULL DEFAULT '',
		category    TEXT NOT NULL DEFAULT '',
		url         TEXT NOT NULL DEFAULT '',
		schedule    TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		record      INTEGER NOT NULL DEFAULT 0,
		last_seen   INTEGER NOT NULL DEFAULT 0, -- unix seconds
		details_at  INTEGER NOT NULL DEFAULT 0  -- unix seconds
	);
	CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
	// Artwork URLs. Clearing details_at refetches every show page on the next
	// refresh, so existing databases get artwork right away.
	`ALTER TABLE shows ADD COLUMN image TEXT NOT NULL DEFAULT '';
	UPDATE shows SET details_at = 0;`,
	recordingMigration,     // see recordings.go
	mergeResumedRecordings, // see recordings.go
	// Where playback of each recording got to, to resume from.
	`ALTER TABLE recordings ADD COLUMN position_ms INTEGER NOT NULL DEFAULT 0;`,
	// Recordings whose songs are to come from the DJ's Spinitron playlist.
	`ALTER TABLE recordings ADD COLUMN songs_pending INTEGER NOT NULL DEFAULT 0;`,
	// The airing's playlist page on Spinitron, found with its songs. Recent
	// recordings look for their songs again, to find it.
	`ALTER TABLE recordings ADD COLUMN playlist_url TEXT NOT NULL DEFAULT '';
	UPDATE recordings SET songs_pending = 1
		WHERE finished_at > 0 AND end_at > CAST(strftime('%s', 'now') AS INTEGER) - 7 * 24 * 60 * 60;`,
	// Song folders' playlist files are no longer listed.
	`ALTER TABLE recordings DROP COLUMN playlist;`,
	// Playback positions are kept in the browser instead.
	`ALTER TABLE recordings DROP COLUMN position_ms;`,
	artistLinksMigration, // see artists.go
	// Songs' releases, from the DJs' Spinitron playlists.
	`ALTER TABLE songs ADD COLUMN album TEXT NOT NULL DEFAULT '';
	ALTER TABLE songs ADD COLUMN label TEXT NOT NULL DEFAULT '';
	ALTER TABLE songs ADD COLUMN year INTEGER NOT NULL DEFAULT 0;`,
}

func Open(path string) (*Store, error) {
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("database path %q must not contain '?'", path)
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite allows one writer; serialize to avoid SQLITE_BUSY
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		switch m := migrations[i].(type) {
		case string:
			_, err = tx.Exec(m)
		case func(*sql.Tx) error:
			err = m(tx)
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ImportPrograms marks the config file's legacy "programs" list as selected,
// once, so later changes made in the UI aren't overridden on restart.
func (s *Store) ImportPrograms(ctx context.Context, programs []string) error {
	if len(programs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'programs_imported'`).Scan(&done)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	for _, p := range programs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO shows (key, title, record) VALUES (?, ?, 1)
			ON CONFLICT (key) DO UPDATE SET record = 1`, spinitron.Normalize(p), strings.TrimSpace(p)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('programs_imported', ?)`, time.Now().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertEvents records every show that appears on the calendar.
func (s *Store) UpsertEvents(ctx context.Context, events []spinitron.Event, seen time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO shows (key, title, dj, category, url, last_seen) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			title     = excluded.title,
			dj        = CASE WHEN excluded.dj <> '' THEN excluded.dj ELSE shows.dj END,
			category  = CASE WHEN excluded.category <> '' THEN excluded.category ELSE shows.category END,
			-- prefer show pages over individual playlist pages
			url       = CASE WHEN shows.url = '' OR (excluded.url LIKE '%/show/%' AND shows.url NOT LIKE '%/show/%')
			                 THEN excluded.url ELSE shows.url END,
			last_seen = MAX(shows.last_seen, excluded.last_seen)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, ev := range events {
		if _, err := stmt.ExecContext(ctx, spinitron.Normalize(ev.Title), ev.Title, ev.DJ, ev.Category, ev.URL, seen.Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// StaleDetails returns shows whose page hasn't been fetched since before.
func (s *Store) StaleDetails(ctx context.Context, before time.Time) ([]Show, error) {
	return s.query(ctx, `WHERE url <> '' AND details_at < ?`, before.Unix())
}

func (s *Store) UpdateDetails(ctx context.Context, id int64, info spinitron.ShowInfo, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE shows SET
			description = ?,
			schedule    = ?,
			dj          = CASE WHEN ? <> '' THEN ? ELSE dj END,
			url         = CASE WHEN ? <> '' THEN ? ELSE url END,
			image       = ?,
			details_at  = ?
		WHERE id = ?`,
		info.Description, strings.Join(info.Schedule, "; "),
		info.DJ, info.DJ, info.URL, info.URL, info.Image, at.Unix(), id)
	return err
}

// Setting and SetSetting read and write a value in the settings table.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) Shows(ctx context.Context) ([]Show, error) {
	return s.query(ctx, ``)
}

func (s *Store) Show(ctx context.Context, id int64) (Show, error) {
	shows, err := s.query(ctx, `WHERE id = ?`, id)
	if err != nil {
		return Show{}, err
	}
	if len(shows) == 0 {
		return Show{}, sql.ErrNoRows
	}
	return shows[0], nil
}

// Selected returns the shows marked for recording, keyed by normalized title.
func (s *Store) Selected(ctx context.Context) (map[string]Show, error) {
	shows, err := s.query(ctx, `WHERE record = 1`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Show, len(shows))
	for _, sh := range shows {
		out[sh.Key] = sh
	}
	return out, nil
}

func (s *Store) SetRecord(ctx context.Context, id int64, record bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE shows SET record = ? WHERE id = ?`, record, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete removes a show; the UI only offers this for shows not on the calendar.
func (s *Store) Delete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM shows WHERE id = ?`, id)
	return err
}

func (s *Store) query(ctx context.Context, where string, args ...any) ([]Show, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, key, title, dj, category, url, schedule, description, image, record, last_seen, details_at
		FROM shows `+where+` ORDER BY key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Show
	for rows.Next() {
		var sh Show
		var lastSeen, detailsAt int64
		if err := rows.Scan(&sh.ID, &sh.Key, &sh.Title, &sh.DJ, &sh.Category, &sh.URL,
			&sh.Schedule, &sh.Description, &sh.Image, &sh.Record, &lastSeen, &detailsAt); err != nil {
			return nil, err
		}
		if lastSeen > 0 {
			sh.LastSeen = time.Unix(lastSeen, 0)
		}
		if detailsAt > 0 {
			sh.DetailsAt = time.Unix(detailsAt, 0)
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}
