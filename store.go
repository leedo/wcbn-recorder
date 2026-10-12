package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

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
	Record      bool
	LastSeen    time.Time // last time the show appeared on the calendar; zero if never
	DetailsAt   time.Time // last time the show page was fetched; zero if never
}

// migrations are applied in order; PRAGMA user_version tracks progress.
var migrations = []string{
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
}

func openStore(path string) (*Store, error) {
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
		if _, err := tx.Exec(migrations[i]); err != nil {
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
			ON CONFLICT (key) DO UPDATE SET record = 1`, normalize(p), strings.TrimSpace(p)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('programs_imported', ?)`, time.Now().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertEvents records every show that appears on the calendar.
func (s *Store) UpsertEvents(ctx context.Context, events []Event, seen time.Time) error {
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
		if _, err := stmt.ExecContext(ctx, normalize(ev.Title), ev.Title, ev.DJ, ev.Category, ev.URL, seen.Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// StaleDetails returns shows whose page hasn't been fetched since before.
func (s *Store) StaleDetails(ctx context.Context, before time.Time) ([]Show, error) {
	return s.query(ctx, `WHERE url <> '' AND details_at < ?`, before.Unix())
}

func (s *Store) UpdateDetails(ctx context.Context, id int64, info ShowInfo, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE shows SET
			description = ?,
			schedule    = ?,
			dj          = CASE WHEN ? <> '' THEN ? ELSE dj END,
			url         = CASE WHEN ? <> '' THEN ? ELSE url END,
			details_at  = ?
		WHERE id = ?`,
		info.Description, strings.Join(info.Schedule, "; "),
		info.DJ, info.DJ, info.URL, info.URL, at.Unix(), id)
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
		SELECT id, key, title, dj, category, url, schedule, description, record, last_seen, details_at
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
			&sh.Schedule, &sh.Description, &sh.Record, &lastSeen, &detailsAt); err != nil {
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
