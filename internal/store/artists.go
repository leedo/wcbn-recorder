package store

import (
	"context"
	"time"

	"wcbn-recorder/internal/spinitron"
)

// The artist_links table caches each artist's Wikipedia page, found through
// MusicBrainz, by the artist's name as Normalize gives it, so each artist is
// looked up once (spinitron.Normalize: lower case, spaces collapsed). An artist with no page is kept too, with an empty URL, and
// looked up again after a while, in case one's been added.

var artistLinksMigration = `
	CREATE TABLE artist_links (
		name       TEXT PRIMARY KEY, -- as normalized
		url        TEXT NOT NULL,    -- the English Wikipedia page; '' for none
		checked_at INTEGER NOT NULL  -- unix seconds
	);`

// ArtistLinks returns every artist's Wikipedia page that's been found, by
// normalized name.
func (s *Store) ArtistLinks(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, url FROM artist_links WHERE url <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := map[string]string{}
	for rows.Next() {
		var name, url string
		if err := rows.Scan(&name, &url); err != nil {
			return nil, err
		}
		links[name] = url
	}
	return links, rows.Err()
}

// ArtistsChecked returns the artists (by normalized name) that needn't be
// looked up again: those with a page, and those without one looked up since
// since.
func (s *Store) ArtistsChecked(ctx context.Context, since time.Time) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM artist_links WHERE url <> '' OR checked_at >= ?`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	checked := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		checked[name] = true
	}
	return checked, rows.Err()
}

// SetArtistLink saves an artist's Wikipedia page, or that it has none ("").
func (s *Store) SetArtistLink(ctx context.Context, name, url string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artist_links (name, url, checked_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET url = excluded.url, checked_at = excluded.checked_at`,
		spinitron.Normalize(name), url, at.Unix())
	return err
}

// Artists returns every artist named in the songs table.
func (s *Store) Artists(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT artist FROM songs WHERE artist <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var artists []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		artists = append(artists, a)
	}
	return artists, rows.Err()
}
