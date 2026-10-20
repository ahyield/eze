// Package store persists site metadata, API tokens and site documents in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sites (
	name        TEXT PRIMARY KEY,
	owner       TEXT NOT NULL,
	deployed_by TEXT NOT NULL,
	files       INTEGER NOT NULL,
	bytes       INTEGER NOT NULL,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS deploys (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	site        TEXT NOT NULL,
	deployed_by TEXT NOT NULL,
	files       INTEGER NOT NULL,
	bytes       INTEGER NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS deploys_site ON deploys(site, created_at);

CREATE TABLE IF NOT EXISTS tokens (
	hash         TEXT PRIMARY KEY,
	email        TEXT NOT NULL,
	name         TEXT NOT NULL,
	label        TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER
);
CREATE INDEX IF NOT EXISTS tokens_email ON tokens(email);

CREATE TABLE IF NOT EXISTS docs (
	site       TEXT NOT NULL,
	collection TEXT NOT NULL,
	id         TEXT NOT NULL,
	data       TEXT NOT NULL,
	created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (site, collection, id)
);
CREATE INDEX IF NOT EXISTS docs_order ON docs(site, collection, created_at, id);

CREATE TABLE IF NOT EXISTS files (
	site         TEXT NOT NULL,
	id           TEXT NOT NULL,
	name         TEXT NOT NULL,
	content_type TEXT NOT NULL,
	size         INTEGER NOT NULL,
	created_by   TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (site, id)
);
CREATE INDEX IF NOT EXISTS files_order ON files(site, created_at, id);
`

func Open(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "eze.db")
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serialises writes and avoids SQLITE_BUSY; plenty for a single-VM deployment.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().UnixMilli() }

// Sites

type Site struct {
	Name       string    `json:"name"`
	Owner      string    `json:"owner"`
	DeployedBy string    `json:"deployed_by"`
	Files      int       `json:"files"`
	Bytes      int64     `json:"bytes"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (s *Store) GetSite(ctx context.Context, name string) (*Site, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, owner, deployed_by, files, bytes, created_at, updated_at FROM sites WHERE name = ?`, name)
	site, err := scanSite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return site, err
}

func (s *Store) ListSites(ctx context.Context) ([]Site, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, owner, deployed_by, files, bytes, created_at, updated_at FROM sites ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sites := []Site{}
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		sites = append(sites, *site)
	}
	return sites, rows.Err()
}

// RecordDeploy upserts the site row and appends to the deploy log. The first deployer becomes the owner.
func (s *Store) RecordDeploy(ctx context.Context, name, email string, files int, bytes int64) error {
	t := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sites (name, owner, deployed_by, files, bytes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			deployed_by = excluded.deployed_by, files = excluded.files,
			bytes = excluded.bytes, updated_at = excluded.updated_at`,
		name, email, email, files, bytes, t, t); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO deploys (site, deployed_by, files, bytes, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, email, files, bytes, t); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteSite removes the site row, its documents and its file records. The deploy log is kept as history.
func (s *Store) DeleteSite(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sites WHERE name = ?`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM docs WHERE site = ?`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE site = ?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

type scanner interface{ Scan(...any) error }

func scanSite(r scanner) (*Site, error) {
	var site Site
	var created, updated int64
	if err := r.Scan(&site.Name, &site.Owner, &site.DeployedBy, &site.Files, &site.Bytes, &created, &updated); err != nil {
		return nil, err
	}
	site.CreatedAt = time.UnixMilli(created).UTC()
	site.UpdatedAt = time.UnixMilli(updated).UTC()
	return &site, nil
}

// Tokens

type TokenOwner struct {
	Email string
	Name  string
}

func (s *Store) CreateToken(ctx context.Context, hash, email, name, label string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (hash, email, name, label, created_at) VALUES (?, ?, ?, ?, ?)`,
		hash, email, name, label, now())
	return err
}

func (s *Store) LookupToken(ctx context.Context, hash string) (*TokenOwner, error) {
	var o TokenOwner
	err := s.db.QueryRowContext(ctx, `SELECT email, name FROM tokens WHERE hash = ?`, hash).Scan(&o.Email, &o.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE hash = ?`, now(), hash)
	return &o, nil
}
