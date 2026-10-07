package storepg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// UpdateBank applies a partial bank update, merging config overrides with the
// same shallow jsonb merge upstream uses. Nil patch fields leave the column
// untouched.
func (s *Store) UpdateBank(ctx context.Context, bankID string, patch model.BankPatch) (*model.Bank, error) {
	var name, mission *string
	if patch.Name != nil {
		name = patch.Name
	}
	if patch.Mission != nil {
		mission = patch.Mission
	}
	config, err := encodeConfig(patch.Config)
	if err != nil {
		return nil, err
	}
	var nameArg, missionArg any
	if name != nil {
		nameArg = *name
	}
	if mission != nil {
		missionArg = *mission
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE banks SET name = COALESCE($2, name), mission = COALESCE($3, mission), "+
			"config = COALESCE(config, '{}'::jsonb) || $4::jsonb, updated_at = now() WHERE bank_id = $1",
		bankID, nameArg, missionArg, config)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	return s.GetBank(ctx, bankID)
}

// DeleteBank removes the bank row and every bank-scoped row. Upstream keeps the
// bank id as a plain column rather than a foreign key on most tables, so the
// cascade is explicit: the three counted tables first, then a catalog sweep for
// any other table that carries bank_id (mental models, aliases, webhooks,
// operations, caches, ...).
func (s *Store) DeleteBank(ctx context.Context, bankID string) (model.BankDeleteCounts, error) {
	if b, err := s.GetBank(ctx, bankID); err != nil {
		return model.BankDeleteCounts{}, err
	} else if b == nil {
		return model.BankDeleteCounts{}, nil
	}
	counts := model.BankDeleteCounts{}
	ut, dt := s.unitsTable(), s.docsTable()
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+ut+" WHERE bank_id = $1", bankID).Scan(&counts.MemoryUnits); err != nil {
		return counts, err
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+dt+" WHERE bank_id = $1", bankID).Scan(&counts.Documents); err != nil {
		return counts, err
	}
	if s.upstream {
		var hasEntities bool
		if err := s.pool.QueryRow(ctx, "SELECT to_regclass('entities') IS NOT NULL").Scan(&hasEntities); err != nil {
			return counts, err
		}
		if hasEntities {
			if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM entities WHERE bank_id = $1", bankID).Scan(&counts.Entities); err != nil {
				return counts, err
			}
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return counts, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		"DELETE FROM " + ut + " WHERE bank_id = $1",
		"DELETE FROM " + dt + " WHERE bank_id = $1",
	} {
		if _, err := tx.Exec(ctx, stmt, bankID); err != nil {
			return counts, err
		}
	}
	if err := s.deleteBankScopedTables(ctx, tx, bankID, ut, dt); err != nil {
		return counts, err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM banks WHERE bank_id = $1", bankID); err != nil {
		return counts, err
	}
	if err := tx.Commit(ctx); err != nil {
		return counts, err
	}
	return counts, nil
}

// deleteBankScopedTables removes rows for the bank from every other ordinary
// table that has a bank_id column. Tables the caller already emptied are
// skipped so the statement list stays deterministic.
func (s *Store) deleteBankScopedTables(ctx context.Context, tx pgx.Tx, bankID string, skip ...string) error {
	rows, err := tx.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid
		WHERE n.nspname = current_schema()
		  AND c.relkind IN ('r', 'p')
		  AND a.attname = 'bank_id'
		  AND a.attnum > 0
		  AND NOT a.attisdropped
		ORDER BY c.relname`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	skipped := map[string]bool{"banks": true}
	for _, name := range skip {
		skipped[name] = true
	}
	for _, table := range tables {
		if skipped[table] {
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE bank_id = $1", pgx.Identifier{table}.Sanitize()), bankID); err != nil {
			return fmt.Errorf("delete bank rows from %s: %w", table, err)
		}
	}
	return nil
}

// BankConfig reads the bank's override map.
func (s *Store) BankConfig(ctx context.Context, bankID string) (map[string]any, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, "SELECT COALESCE(config, '{}'::jsonb) FROM banks WHERE bank_id = $1", bankID).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBankNotFound
		}
		return nil, err
	}
	return decodeConfig(raw)
}

// UpdateBankConfig merges overrides into the bank's config column.
func (s *Store) UpdateBankConfig(ctx context.Context, bankID string, updates map[string]any) error {
	config, err := encodeConfig(updates)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE banks SET config = COALESCE(config, '{}'::jsonb) || $2::jsonb, updated_at = now() WHERE bank_id = $1",
		bankID, config)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrBankNotFound
	}
	return nil
}

// ResetBankConfig clears every override for the bank.
func (s *Store) ResetBankConfig(ctx context.Context, bankID string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE banks SET config = '{}'::jsonb, updated_at = now() WHERE bank_id = $1", bankID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrBankNotFound
	}
	return nil
}

// ListBankAliases returns the bank's aliases, primary first then oldest first.
func (s *Store) ListBankAliases(ctx context.Context, bankID string) ([]model.BankAlias, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT alias, is_primary FROM bank_aliases WHERE bank_id = $1 ORDER BY is_primary DESC, created_at ASC", bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.BankAlias
	for rows.Next() {
		var a model.BankAlias
		if err := rows.Scan(&a.Alias, &a.Primary); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateBankAlias adds an alias unless it already names a bank or an alias.
func (s *Store) CreateBankAlias(ctx context.Context, bankID, alias string, primary bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM banks WHERE bank_id = $1)", alias).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return model.ErrAliasConflict
	}
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM bank_aliases WHERE alias = $1)", alias).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return model.ErrAliasConflict
	}
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM banks WHERE bank_id = $1)", bankID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrBankNotFound
	}
	if primary {
		if _, err := tx.Exec(ctx, "UPDATE bank_aliases SET is_primary = false WHERE bank_id = $1", bankID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO bank_aliases (alias, bank_id, is_primary) VALUES ($1, $2, $3)",
		alias, bankID, primary); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteBankAlias detaches one alias; false when the bank has no such alias.
func (s *Store) DeleteBankAlias(ctx context.Context, bankID, alias string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM bank_aliases WHERE bank_id = $1 AND alias = $2", bankID, alias)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetBankAliasPrimary promotes or demotes one alias; false when absent.
func (s *Store) SetBankAliasPrimary(ctx context.Context, bankID, alias string, primary bool) (bool, error) {
	if primary {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "UPDATE bank_aliases SET is_primary = false WHERE bank_id = $1", bankID); err != nil {
			return false, err
		}
		tag, err := tx.Exec(ctx,
			"UPDATE bank_aliases SET is_primary = true WHERE bank_id = $1 AND alias = $2", bankID, alias)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() == 0 {
			return false, nil
		}
		return true, tx.Commit(ctx)
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE bank_aliases SET is_primary = false WHERE bank_id = $1 AND alias = $2", bankID, alias)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// encodeConfig marshals an override map for a jsonb parameter; nil stays an
// empty object so the merge never nulls the column.
func encodeConfig(cfg map[string]any) ([]byte, error) {
	if len(cfg) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(cfg)
}

// decodeConfig lands a jsonb column value in a plain map.
func decodeConfig(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}
