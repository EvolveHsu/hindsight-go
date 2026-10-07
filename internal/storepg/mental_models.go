package storepg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// ensureMentalModelsTable creates the lite mental_models table (idempotent).
// In upstream mode the table is owned by the Python migrations: running DDL
// here would either be a no-op or add columns the upstream schema lacks.
func (s *Store) ensureMentalModelsTable(ctx context.Context) error {
	if s.upstream {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS mental_models (
    id               TEXT PRIMARY KEY,
    bank_id          TEXT NOT NULL,
    name             TEXT NOT NULL,
    source_query     TEXT NOT NULL DEFAULT '',
    content          TEXT NOT NULL DEFAULT '',
    tags             TEXT[] NOT NULL DEFAULT '{}',
    max_tokens       INTEGER,
    last_refreshed   TIMESTAMPTZ,
    last_seen        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS mental_models_bank_idx ON mental_models (bank_id);`)
	return err
}

// CreateMentalModel stores a new model definition.
func (s *Store) CreateMentalModel(ctx context.Context, bankID string, m *model.MentalModel) (*model.MentalModel, error) {
	if err := s.ensureMentalModelsTable(ctx); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO mental_models (id, bank_id, name, source_query, content, tags, max_tokens, created_at)
VALUES ($1,$2,$3,$4,$5,$6::text[],$7,$8)
ON CONFLICT %s DO NOTHING`, s.mmConflictTarget()),
		m.ID, bankID, m.Name, m.SourceQuery, m.Content, normalizeTags(m.Tags), s.mmMaxTokens(m.MaxTokens), now)
	if err != nil {
		return nil, err
	}
	m.BankID = bankID
	m.CreatedAt = now.Format(time.RFC3339)
	return m, nil
}

// GetMentalModel returns nil when absent.
func (s *Store) GetMentalModel(ctx context.Context, bankID, id string) (*model.MentalModel, error) {
	if err := s.ensureMentalModelsTable(ctx); err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT id, bank_id, name, source_query, content, COALESCE(tags::text[], '{}'), max_tokens,
       COALESCE(to_char(%s, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), ''),
       COALESCE(to_char(%s, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), ''),
       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
FROM mental_models WHERE id = $1 AND bank_id = $2`, s.mmRefreshColumn(), s.mmSeenColumn()), id, bankID)
	m := &model.MentalModel{}
	var maxTokens *int
	var lastRef, lastSeen, created *string
	if err := row.Scan(&m.ID, &m.BankID, &m.Name, &m.SourceQuery, &m.Content, &m.Tags,
		&maxTokens, &lastRef, &lastSeen, &created); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	m.Tags = normalizeTags(m.Tags)
	if maxTokens != nil {
		m.MaxTokens = *maxTokens
	}
	if lastRef != nil {
		m.LastRefreshed = *lastRef
	}
	if lastSeen != nil {
		m.LastSeen = *lastSeen
	}
	if created != nil {
		m.CreatedAt = *created
	}
	return m, nil
}

// ListMentalModels returns all models for the bank, oldest first.
func (s *Store) ListMentalModels(ctx context.Context, bankID string) ([]*model.MentalModel, error) {
	if err := s.ensureMentalModelsTable(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT id, bank_id, name, source_query, content, COALESCE(tags::text[], '{}'), max_tokens,
       COALESCE(to_char(%s, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), ''),
       COALESCE(to_char(%s, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), ''),
       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
FROM mental_models WHERE bank_id = $1`, s.mmRefreshColumn(), s.mmSeenColumn()), bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.MentalModel
	for rows.Next() {
		m := &model.MentalModel{}
		var maxTokens *int
		if err := rows.Scan(&m.ID, &m.BankID, &m.Name, &m.SourceQuery, &m.Content, &m.Tags,
			&maxTokens, &m.LastRefreshed, &m.LastSeen, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Tags = normalizeTags(m.Tags)
		if maxTokens != nil {
			m.MaxTokens = *maxTokens
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, rows.Err()
}

// UpdateMentalModel applies fn and writes the row back; nil when absent.
func (s *Store) UpdateMentalModel(ctx context.Context, bankID, id string, fn func(*model.MentalModel)) (*model.MentalModel, error) {
	m, err := s.GetMentalModel(ctx, bankID, id)
	if err != nil || m == nil {
		return nil, err
	}
	fn(m)
	_, err = s.pool.Exec(ctx, fmt.Sprintf(`
UPDATE mental_models SET name = $3, source_query = $4, content = $5, tags = $6::text[],
       max_tokens = $7, %s = $8, %s = $9%s
WHERE id = $1 AND bank_id = $2`, s.mmRefreshColumn(), s.mmSeenColumn(), s.mmTouchSet()),
		id, bankID, m.Name, m.SourceQuery, m.Content, normalizeTags(m.Tags),
		s.mmMaxTokens(m.MaxTokens), nilTime(m.LastRefreshed), nilTime(m.LastSeen))
	if err != nil {
		return nil, err
	}
	return m, nil
}

// DeleteMentalModel removes one; false when absent.
func (s *Store) DeleteMentalModel(ctx context.Context, bankID, id string) (bool, error) {
	if err := s.ensureMentalModelsTable(ctx); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM mental_models WHERE id = $1 AND bank_id = $2`, id, bankID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// nilInt maps 0 to SQL NULL.
func nilInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

// nilTime maps an empty or unparseable timestamp to SQL NULL. Postgres'
// OF format renders a two-digit offset (+08) instead of RFC3339's +08:00, so
// both layouts are accepted.
func nilTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-07"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}
