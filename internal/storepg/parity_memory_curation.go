package storepg

import (
	"context"
	"fmt"
)

func (s *Store) ClearBankMemoriesByType(ctx context.Context, bankID, factType string) (int, error) {
	tag, err := s.pool.Exec(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE bank_id=$1 AND fact_type=$2", s.unitsTable()),
		bankID, factType)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) ArchiveMemory(ctx context.Context, bankID, memoryID, reason string) error {
	if !s.upstream {
		return fmt.Errorf("memory invalidation requires the upstream schema")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
INSERT INTO invalidated_memory_units
  (id, bank_id, document_id, text, context, event_date, occurred_start, occurred_end,
   mentioned_at, fact_type, metadata, created_at, updated_at, chunk_id, tags, proof_count,
   source_memory_ids, consolidated_at, observation_scopes, text_signals, consolidation_failed_at,
   edited_at, invalidation_reason, invalidated_at, entity_ids, causal_links, attachment_ids)
SELECT mu.id, mu.bank_id, mu.document_id, mu.text, mu.context, mu.event_date, mu.occurred_start,
       mu.occurred_end, mu.mentioned_at, mu.fact_type, mu.metadata, mu.created_at, mu.updated_at,
       mu.chunk_id, mu.tags, mu.proof_count, mu.source_memory_ids, mu.consolidated_at,
       mu.observation_scopes, mu.text_signals, mu.consolidation_failed_at, mu.edited_at,
       NULLIF($3,''), now(),
       (SELECT array_agg(ue.entity_id) FROM unit_entities ue WHERE ue.unit_id = mu.id),
       '[]'::jsonb, mu.attachment_ids
FROM memory_units mu
WHERE mu.bank_id=$1 AND mu.id=$2 AND mu.fact_type IN ('world','experience')
ON CONFLICT (id) DO NOTHING`, bankID, memoryID, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("memory %s is missing or is not a curatable fact", memoryID)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM memory_links WHERE bank_id=$1 AND (from_unit_id=$2 OR to_unit_id=$2)`, bankID, memoryID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM unit_entities WHERE unit_id=$1`, memoryID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE bank_id=$1 AND id=$2`, s.unitsTable()), bankID, memoryID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RestoreMemory(ctx context.Context, bankID, memoryID string) error {
	if !s.upstream {
		return fmt.Errorf("memory restoration requires the upstream schema")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var entityIDs []string
	tag, err := tx.Exec(ctx, `
INSERT INTO memory_units
  (id, bank_id, document_id, text, embedding, context, event_date, occurred_start, occurred_end,
   mentioned_at, fact_type, metadata, created_at, updated_at, chunk_id, tags, proof_count,
   source_memory_ids, consolidated_at, observation_scopes, text_signals, consolidation_failed_at,
   edited_at, attachment_ids)
SELECT id, bank_id, document_id, text, NULL, context, event_date, occurred_start, occurred_end,
       mentioned_at, fact_type, metadata, created_at, updated_at, chunk_id, tags, proof_count,
       source_memory_ids, consolidated_at, observation_scopes, text_signals, consolidation_failed_at,
       edited_at, attachment_ids
FROM invalidated_memory_units
WHERE bank_id=$1 AND id=$2
ON CONFLICT (id) DO NOTHING`, bankID, memoryID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("invalidated memory %s not found", memoryID)
	}
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(entity_ids::text[],'{}') FROM invalidated_memory_units WHERE bank_id=$1 AND id=$2`,
		bankID, memoryID).Scan(&entityIDs); err != nil {
		return err
	}
	for _, entityID := range entityIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO unit_entities (unit_id, entity_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			memoryID, entityID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM invalidated_memory_units WHERE bank_id=$1 AND id=$2`, bankID, memoryID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
