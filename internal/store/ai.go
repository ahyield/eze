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

type AIUsage struct {
	Email        string `json:"email"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// AIUsageBySite totals a site's AI usage per person over the UTC days from since to today,
// heaviest users first.
func (s *Store) AIUsageBySite(ctx context.Context, site string, since time.Time) ([]AIUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT email, SUM(requests), SUM(input_tokens), SUM(output_tokens) FROM ai_usage
		WHERE site = ? AND day >= ? GROUP BY email
		ORDER BY SUM(input_tokens + output_tokens) DESC`, site, usageDay(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := []AIUsage{}
	for rows.Next() {
		var u AIUsage
		if err := rows.Scan(&u.Email, &u.Requests, &u.InputTokens, &u.OutputTokens); err != nil {
			return nil, err
		}
		usage = append(usage, u)
	}
	return usage, rows.Err()
}
