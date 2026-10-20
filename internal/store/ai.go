package store

import (
	"context"
	"time"
)

// AI usage is kept per user, per UTC day, per site: budgets are enforced on the user's
// daily total, and the site breakdown shows where the spend went.

func usageDay(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// AITokensUsed is the user's total tokens (input + output) on the UTC day containing at.
func (s *Store) AITokensUsed(ctx context.Context, email string, at time.Time) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM ai_usage WHERE email = ? AND day = ?`,
		email, usageDay(at)).Scan(&total)
	return total, err
}

func (s *Store) RecordAIUsage(ctx context.Context, email, site string, at time.Time, input, output int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ai_usage (email, day, site, requests, input_tokens, output_tokens) VALUES (?, ?, ?, 1, ?, ?)
		ON CONFLICT(email, day, site) DO UPDATE SET
			requests = requests + 1,
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens`,
		email, usageDay(at), site, input, output)
	return err
}
