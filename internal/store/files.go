package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// File is the metadata of an uploaded file. The bytes live on disk, see package uploads.
type File struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	CreatedBy   string    `json:"_created_by"`
	CreatedAt   time.Time `json:"_created_at"`
}

func (s *Store) CreateFile(ctx context.Context, site string, f *File) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO files (site, id, name, content_type, size, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		site, f.ID, f.Name, f.ContentType, f.Size, f.CreatedBy, f.CreatedAt.UnixMilli())
	return err
}

func (s *Store) GetFile(ctx context.Context, site, id string) (*File, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, content_type, size, created_by, created_at FROM files WHERE site = ? AND id = ?`, site, id)
	f, err := scanFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// ListFiles returns a site's files, newest first.
func (s *Store) ListFiles(ctx context.Context, site string, limit, offset int) ([]File, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, content_type, size, created_by, created_at FROM files
		WHERE site = ? ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, site, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := []File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}
	return files, rows.Err()
}

func (s *Store) DeleteFile(ctx context.Context, site, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE site = ? AND id = ?`, site, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UploadedBytes is the total size of a site's files, for quota checks.
func (s *Store) UploadedBytes(ctx context.Context, site string) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM files WHERE site = ?`, site).Scan(&total)
	return total, err
}

func scanFile(r scanner) (*File, error) {
	var f File
	var created int64
	if err := r.Scan(&f.ID, &f.Name, &f.ContentType, &f.Size, &f.CreatedBy, &created); err != nil {
		return nil, err
	}
	f.CreatedAt = time.UnixMilli(created).UTC()
	return &f, nil
}
