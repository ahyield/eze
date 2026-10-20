package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"
)

var (
	collectionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	docIDRe      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	fieldPathRe  = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}(\.[A-Za-z0-9_]{1,64}){0,7}$`)
)

func ValidCollection(name string) bool { return collectionRe.MatchString(name) }
func ValidDocID(id string) bool        { return docIDRe.MatchString(id) }

// Doc is a schemaless JSON document. Reserved keys (id, _created_at, _updated_at, _created_by)
// are added on read and stripped on write.
type Doc map[string]any

type DocQuery struct {
	Where  map[string]any // top-level or dotted-path equality filters
	Limit  int
	Offset int
	Desc   bool
}

type BadQueryError struct{ msg string }

func (e *BadQueryError) Error() string { return e.msg }

func (s *Store) ListDocs(ctx context.Context, site, collection string, q DocQuery) ([]Doc, error) {
	var sb strings.Builder
	args := []any{site, collection}
	sb.WriteString(`SELECT id, data, created_by, created_at, updated_at FROM docs WHERE site = ? AND collection = ?`)
	for field, value := range q.Where {
		if !fieldPathRe.MatchString(field) {
			return nil, &BadQueryError{fmt.Sprintf("invalid field %q", field)}
		}
		if field == "id" {
			sb.WriteString(` AND id = ?`)
			args = append(args, fmt.Sprint(value))
			continue
		}
		switch v := value.(type) {
		case nil:
			sb.WriteString(` AND json_extract(data, ?) IS NULL`)
			args = append(args, "$."+field)
		case string, float64:
			sb.WriteString(` AND json_extract(data, ?) = ?`)
			args = append(args, "$."+field, v)
		case bool:
			// json_extract returns 1/0 for JSON booleans.
			sb.WriteString(` AND json_type(data, ?) = ?`)
			args = append(args, "$."+field, map[bool]string{true: "true", false: "false"}[v])
		default:
			return nil, &BadQueryError{fmt.Sprintf("field %q: only string, number, boolean and null filters are supported", field)}
		}
	}
	if q.Desc {
		sb.WriteString(` ORDER BY created_at DESC, id DESC`)
	} else {
		sb.WriteString(` ORDER BY created_at ASC, id ASC`)
	}
	sb.WriteString(` LIMIT ? OFFSET ?`)
	args = append(args, q.Limit, q.Offset)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs := []Doc{}
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func (s *Store) GetDoc(ctx context.Context, site, collection, id string) (Doc, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, data, created_by, created_at, updated_at FROM docs WHERE site = ? AND collection = ? AND id = ?`,
		site, collection, id)
	d, err := scanDoc(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

// CreateDoc inserts a new document with a generated id.
func (s *Store) CreateDoc(ctx context.Context, site, collection, by string, data Doc) (Doc, error) {
	body, err := encode(data)
	if err != nil {
		return nil, err
	}
	id := NewID()
	t := now()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO docs (site, collection, id, data, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		site, collection, id, body, by, t, t); err != nil {
		return nil, err
	}
	return s.GetDoc(ctx, site, collection, id)
}

// SetDoc creates or replaces a document under a caller-chosen id. created reports whether it was new.
func (s *Store) SetDoc(ctx context.Context, site, collection, id, by string, data Doc) (doc Doc, created bool, err error) {
	body, err := encode(data)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM docs WHERE site = ? AND collection = ? AND id = ?`, site, collection, id).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	created = errors.Is(err, sql.ErrNoRows)
	t := now()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO docs (site, collection, id, data, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(site, collection, id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		site, collection, id, body, by, t, t); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	doc, err = s.GetDoc(ctx, site, collection, id)
	return doc, created, err
}

// UpdateDoc shallow-merges patch into an existing document. A null value removes the key.
func (s *Store) UpdateDoc(ctx context.Context, site, collection, id string, patch Doc) (Doc, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var body string
	err = tx.QueryRowContext(ctx, `SELECT data FROM docs WHERE site = ? AND collection = ? AND id = ?`, site, collection, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	current := Doc{}
	if err := json.Unmarshal([]byte(body), &current); err != nil {
		return nil, err
	}
	for k, v := range strip(patch) {
		if v == nil {
			delete(current, k)
		} else {
			current[k] = v
		}
	}
	merged, err := encode(current)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE docs SET data = ?, updated_at = ? WHERE site = ? AND collection = ? AND id = ?`,
		merged, now(), site, collection, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetDoc(ctx, site, collection, id)
}

func (s *Store) DeleteDoc(ctx context.Context, site, collection, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM docs WHERE site = ? AND collection = ? AND id = ?`, site, collection, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanDoc(r scanner) (Doc, error) {
	var id, body, by string
	var created, updated int64
	if err := r.Scan(&id, &body, &by, &created, &updated); err != nil {
		return nil, err
	}
	d := Doc{}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		return nil, err
	}
	d["id"] = id
	d["_created_by"] = by
	d["_created_at"] = time.UnixMilli(created).UTC().Format(time.RFC3339Nano)
	d["_updated_at"] = time.UnixMilli(updated).UTC().Format(time.RFC3339Nano)
	return d, nil
}

// strip removes reserved keys a client may echo back from a previous read.
func strip(d Doc) Doc {
	out := maps.Clone(d)
	if out == nil {
		out = Doc{}
	}
	delete(out, "id")
	for k := range out {
		if strings.HasPrefix(k, "_") {
			delete(out, k)
		}
	}
	return out
}

func encode(d Doc) (string, error) {
	b, err := json.Marshal(strip(d))
	return string(b), err
}

const idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// NewID returns a 20-character random id.
func NewID() string {
	b := make([]byte, 20)
	rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}
