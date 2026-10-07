package storepg

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/EvolveHsu/hindsight-go/internal/consolidate"
	"github.com/EvolveHsu/hindsight-go/internal/embeddings"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// ensureConsolidationColumns adds the lite consolidation columns
// idempotently. Upstream already ships consolidated_at and source_memory_ids,
// so nothing is altered there.
func (s *Store) ensureConsolidationColumns(ctx context.Context) error {
	if s.upstream {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
ALTER TABLE units ADD COLUMN IF NOT EXISTS consolidated_at TIMESTAMPTZ;
ALTER TABLE units ADD COLUMN IF NOT EXISTS source_fact_ids TEXT[] NOT NULL DEFAULT '{}';`)
	return err
}

// factsSelect is the shared projection for unconsolidated facts and existing
// observations; the table name differs per mode.
func (s *Store) factsSelect(where string) string {
	return fmt.Sprintf(`SELECT id::text, bank_id, COALESCE(document_id,''), fact_type, text,
       COALESCE(context,''), COALESCE(chunk_id,''), COALESCE(tags::text[],'{}'),
       COALESCE(to_char(mentioned_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), ''),
       COALESCE(to_char(created_at,   'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), '')
FROM %s
WHERE bank_id = $1 AND %s
ORDER BY created_at
LIMIT $2`, s.unitsTable(), where)
}

func scanUnits(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]model.Unit, error) {
	var out []model.Unit
	for rows.Next() {
		u := &model.Unit{}
		var tags []string
		if err := rows.Scan(&u.ID, &u.BankID, &u.DocumentID, &u.FactType, &u.Text,
			&u.Context, &u.ChunkID, &tags, &u.MentionedAt, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Tags = tags
		out = append(out, *u)
	}
	return out, rows.Err()
}

// UnconsolidatedFacts implements consolidate.Source.
func (s *Store) UnconsolidatedFacts(ctx context.Context, bankID string, limit int) ([]model.Unit, error) {
	if err := s.ensureConsolidationColumns(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, s.factsSelect("consolidated_at IS NULL AND fact_type <> 'observation'"), bankID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUnits(rows)
}

// Observations implements consolidate.Source.
func (s *Store) Observations(ctx context.Context, bankID string, limit int) ([]model.Unit, error) {
	if err := s.ensureConsolidationColumns(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, s.factsSelect("fact_type = 'observation'"), bankID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUnits(rows)
}

// ApplyBatch implements consolidate.Source. One transaction, mirroring the
// upstream rule that a consolidation batch commits atomically.
func (s *Store) ApplyBatch(ctx context.Context, bankID string, b consolidate.Batch) error {
	if err := s.ensureConsolidationColumns(ctx); err != nil {
		return err
	}
	ut := s.unitsTable()
	srcCol := s.sourceIDsColumn()
	srcCast := s.sourceIDsCast()
	markSQL := fmt.Sprintf(`UPDATE %s SET consolidated_at = $1 WHERE id = $2 AND bank_id = $3`, ut)
	if s.upstream {
		markSQL = fmt.Sprintf(`UPDATE %s SET consolidated_at = $1, updated_at = now() WHERE id = $2 AND bank_id = $3`, ut)
	}

	now := time.Now().UTC()
	createVectors, err := s.embedDocumentBatch(ctx, batchCreateTexts(b.Creates))
	if err != nil {
		return err
	}
	updateVectors, err := s.embedDocumentBatch(ctx, batchUpdateTexts(b.Updates))
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	markFacts := func(tx pgxTx, ids []string) error {
		for _, id := range ids {
			tag, err := tx.Exec(ctx, markSQL, now, id, bankID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("%w: %s", ErrUnknownFact, id)
			}
		}
		return nil
	}

	for i, c := range b.Creates {
		id := model.UnitID(bankID, "observation", c.Text)
		// memory_units.document_id carries an FK upstream, so observations are
		// stored with a NULL document there; the lite table has no FK.
		columns := "id, bank_id, document_id, fact_type, text, context, chunk_id, tags, " + srcCol + ", consolidated_at"
		values := fmt.Sprintf("$1,$2,%s,'observation',$3,'',%s,$4,$5%s,$6", s.observationDocumentID(), s.observationChunkID(), srcCast)
		args := []any{id, bankID, c.Text, []string{}, normalizeTags(c.SourceFactIDs), now}
		conflict := "ON CONFLICT (id) DO UPDATE SET text = EXCLUDED.text, consolidated_at = EXCLUDED.consolidated_at"
		if i < len(createVectors) && len(createVectors[i]) > 0 {
			columns += ", embedding"
			literal := embeddingLiteral(createVectors[i])
			if s.upstream && s.pgvector {
				values += fmt.Sprintf(",'%s'::vector", literal)
			} else {
				values += fmt.Sprintf(",'%s'", literal)
			}
			conflict += ", embedding = EXCLUDED.embedding"
		}
		insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) %s", ut, columns, values, conflict)
		if _, err := tx.Exec(ctx, insert, args...); err != nil {
			return err
		}
		if err := markFacts(tx, c.SourceFactIDs); err != nil {
			return err
		}
	}
	for i, u := range b.Updates {
		setClause := fmt.Sprintf("text = $3, mentioned_at = $4, %s = COALESCE(%s,'{}') || $5%s, consolidated_at = $6", srcCol, srcCol, srcCast)
		args := []any{u.ObservationID, bankID, u.Text, now, normalizeTags(u.SourceFactIDs), now}
		if i < len(updateVectors) && len(updateVectors[i]) > 0 {
			setClause += fmt.Sprintf(", embedding = '%s'", embeddingLiteral(updateVectors[i]))
			if s.upstream && s.pgvector {
				setClause += "::vector"
			}
		}
		if s.upstream {
			setClause += ", updated_at = now()"
		}
		update := fmt.Sprintf("UPDATE %s SET %s WHERE id = $1 AND bank_id = $2 AND fact_type = 'observation'", ut, setClause)
		tag, err := tx.Exec(ctx, update, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: %s", ErrUnknownObservation, u.ObservationID)
		}
		if err := markFacts(tx, u.SourceFactIDs); err != nil {
			return err
		}
	}
	for _, d := range b.Deletes {
		tag, err := tx.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE id = $1 AND bank_id = $2 AND fact_type = 'observation'`, ut),
			d.ObservationID, bankID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: %s", ErrUnknownObservation, d.ObservationID)
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) embedDocumentBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if s.embedder == nil || len(texts) == 0 {
		return nil, nil
	}
	vectors, err := s.embedder.Embed(ctx, texts, embeddings.InputDocument)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("consolidation embeddings: got %d vectors for %d texts", len(vectors), len(texts))
	}
	return vectors, nil
}

func batchCreateTexts(creates []consolidate.CreateAction) []string {
	out := make([]string, len(creates))
	for i := range creates {
		out[i] = creates[i].Text
	}
	return out
}

func batchUpdateTexts(updates []consolidate.UpdateAction) []string {
	out := make([]string, len(updates))
	for i := range updates {
		out[i] = updates[i].Text
	}
	return out
}

// pgxTx is the transaction interface markFacts needs.
type pgxTx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// sortUnits is a helper kept for stable ordering in tests.
func sortUnits(us []model.Unit) {
	sort.Slice(us, func(i, j int) bool { return us[i].CreatedAt < us[j].CreatedAt })
}
