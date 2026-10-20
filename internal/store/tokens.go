package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DefaultTokenTTL applies to tokens created before tokens had an expiry.
const DefaultTokenTTL = 90 * 24 * time.Hour

// tokenIDLen is how much of a token's hash identifies it in listings: enough to be unique,
// and safe to show because it's a hash, not the token.
const tokenIDLen = 12

// migrateTokenExpiry adds expires_at to databases created before tokens expired.
func migrateTokenExpiry(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tokens') WHERE name = 'expires_at'`).Scan(&n); err != nil || n > 0 {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE tokens ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE tokens SET expires_at = created_at + ?`, DefaultTokenTTL.Milliseconds())
	return err
}

type TokenOwner struct {
	Email string
	Name  string
}

type Token struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
}

func (s *Store) CreateToken(ctx context.Context, hash, email, name, label string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (hash, email, name, label, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		hash, email, name, label, now(), expires.UnixMilli())
	return err
}

// LookupToken returns the owner of an unexpired token.
func (s *Store) LookupToken(ctx context.Context, hash string) (*TokenOwner, error) {
	var o TokenOwner
	err := s.db.QueryRowContext(ctx, `SELECT email, name FROM tokens WHERE hash = ? AND expires_at > ?`, hash, now()).Scan(&o.Email, &o.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE hash = ?`, now(), hash)
	return &o, nil
}

// ListTokens returns email's unexpired tokens, newest first.
func (s *Store) ListTokens(ctx context.Context, email string) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT substr(hash, 1, ?), label, created_at, last_used_at, expires_at FROM tokens
		WHERE email = ? AND expires_at > ? ORDER BY created_at DESC`, tokenIDLen, email, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []Token{}
	for rows.Next() {
		var t Token
		var created, expires int64
		var used sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Label, &created, &used, &expires); err != nil {
			return nil, err
		}
		t.CreatedAt = time.UnixMilli(created).UTC()
		t.ExpiresAt = time.UnixMilli(expires).UTC()
		if used.Valid {
			u := time.UnixMilli(used.Int64).UTC()
			t.LastUsedAt = &u
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// TokenID is the identifier ListTokens shows for the token with this hash.
func TokenID(hash string) string { return hash[:tokenIDLen] }

// RevokeToken deletes one of email's tokens by its ID.
func (s *Store) RevokeToken(ctx context.Context, email, id string) error {
	if len(id) != tokenIDLen {
		return ErrNotFound
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE email = ? AND substr(hash, 1, ?) = ?`, email, tokenIDLen, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeTokensFor deletes every token belonging to email, e.g. when someone leaves.
func (s *Store) RevokeTokensFor(ctx context.Context, email string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE email = ?`, email)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
