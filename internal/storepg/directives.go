package storepg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// Directive is the shared model row (lite columns).
type Directive = model.Directive

// ensureDirectivesTable creates the lite directives table (idempotent).
// Upstream owns its directives table (uuid id, priority, tags, updated_at), so
// no DDL runs in that mode.
func (s *Store) ensureDirectivesTable(ctx context.Context) error {
	if s.upstream {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS directives (
    id         TEXT PRIMARY KEY,
    bank_id    TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    content    TEXT NOT NULL,
    is_active  BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS directives_bank_idx ON directives (bank_id);`)
	return err
}

// CreateDirective inserts a directive. The lite table dedupes through a
// content-derived id; upstream generates a uuid, so the same idempotency is
// kept with an explicit lookup before the insert.
func (s *Store) CreateDirective(ctx context.Context, bankID, name, content string) (*Directive, error) {
	if err := s.ensureDirectivesTable(ctx); err != nil {
		return nil, err
	}
	if s.upstream {
		var id string
		err := s.pool.QueryRow(ctx,
			`SELECT id::text FROM directives WHERE bank_id = $1 AND name = $2 AND content = $3 LIMIT 1`,
			bankID, name, content).Scan(&id)
		switch {
		case err == nil:
			return &Directive{ID: id, Name: name, Content: content}, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, err
		}
		if err := s.pool.QueryRow(ctx,
			`INSERT INTO directives (bank_id, name, content) VALUES ($1,$2,$3) RETURNING id::text`,
			bankID, name, content).Scan(&id); err != nil {
			return nil, err
		}
		return &Directive{ID: id, Name: name, Content: content}, nil
	}

	id := "dir-" + model.UnitID(bankID, "directive", name+"\x00"+content)[:12]
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO directives (id, bank_id, name, content) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (id) DO NOTHING`,
		id, bankID, name, content); err != nil {
		return nil, err
	}
	return &Directive{ID: id, Name: name, Content: content}, nil
}

// ListDirectives returns active directives in stable order. Upstream orders by
// priority first; the lite table has no priority column. The id column is a
// uuid upstream, so it is always projected as text.
func (s *Store) ListDirectives(ctx context.Context, bankID string) ([]Directive, error) {
	if err := s.ensureDirectivesTable(ctx); err != nil {
		return nil, err
	}
	order := "created_at, id"
	if s.upstream {
		order = "priority DESC, created_at, id"
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT id::text, name, content FROM directives
WHERE bank_id = $1 AND is_active
ORDER BY %s`, order), bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Directive
	for rows.Next() {
		var d Directive
		if err := rows.Scan(&d.ID, &d.Name, &d.Content); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// BankMission returns the bank's mission string. mission is nullable upstream
// and the bank key is bank_id (not id) in both modes.
func (s *Store) BankMission(ctx context.Context, bankID string) (string, error) {
	row := s.pool.QueryRow(ctx, `SELECT COALESCE(mission,'') FROM banks WHERE bank_id = $1`, bankID)
	var mission string
	if err := row.Scan(&mission); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrBankNotFound
		}
		return "", err
	}
	return mission, nil
}
