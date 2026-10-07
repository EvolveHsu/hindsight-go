package storepg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

const knowledgeTimeLayout = `YYYY-MM-DD"T"HH24:MI:SSTZH:TZM`

func (s *Store) ListKnowledgeRows(ctx context.Context, bankID string) ([]model.KnowledgeRow, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT kp.id, COALESCE(kp.parent_id,''), kp.kind, kp.name, COALESCE(kp.mental_model_id,''),
       kp.managed, kp.sort_order, COALESCE(mm.source_query,''), COALESCE(mm.tags::text[], '{}'),
       COALESCE(mm.content,''), COALESCE(to_char(mm.last_refreshed_at, '%[1]s'), ''),
       COALESCE(to_char(mm.last_refresh_failed_at, '%[1]s'), ''),
       to_char(kp.created_at, '%[1]s'), to_char(kp.updated_at, '%[1]s')
FROM knowledge_pages kp
LEFT JOIN mental_models mm ON mm.bank_id = kp.bank_id AND mm.id = kp.mental_model_id
WHERE kp.bank_id = $1
ORDER BY kp.sort_order, kp.name`, knowledgeTimeLayout), bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.KnowledgeRow{}
	for rows.Next() {
		var r model.KnowledgeRow
		if err := rows.Scan(&r.ID, &r.ParentID, &r.Kind, &r.Name, &r.MentalModelID,
			&r.Managed, &r.SortOrder, &r.Description, &r.Tags, &r.Body,
			&r.LastRefreshedAt, &r.LastRefreshFailedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetKnowledgeRow(ctx context.Context, bankID, id string) (*model.KnowledgeRow, error) {
	var r model.KnowledgeRow
	err := s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT kp.id, COALESCE(kp.parent_id,''), kp.kind, kp.name, COALESCE(kp.mental_model_id,''),
       kp.managed, kp.sort_order, COALESCE(mm.source_query,''), COALESCE(mm.tags::text[], '{}'),
       COALESCE(mm.content,''), COALESCE(to_char(mm.last_refreshed_at, '%[1]s'), ''),
       COALESCE(to_char(mm.last_refresh_failed_at, '%[1]s'), ''),
       to_char(kp.created_at, '%[1]s'), to_char(kp.updated_at, '%[1]s')
FROM knowledge_pages kp
LEFT JOIN mental_models mm ON mm.bank_id = kp.bank_id AND mm.id = kp.mental_model_id
WHERE kp.bank_id = $1 AND kp.id = $2`, knowledgeTimeLayout), bankID, id).Scan(
		&r.ID, &r.ParentID, &r.Kind, &r.Name, &r.MentalModelID, &r.Managed, &r.SortOrder,
		&r.Description, &r.Tags, &r.Body, &r.LastRefreshedAt, &r.LastRefreshFailedAt,
		&r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) CreateKnowledgeRow(ctx context.Context, bankID string, r model.KnowledgeRow) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO knowledge_pages (id, bank_id, parent_id, kind, name, mental_model_id, sort_order, managed)
VALUES ($1,$2,NULLIF($3,''),$4,$5,NULLIF($6,''),$7,$8)`,
		r.ID, bankID, r.ParentID, r.Kind, r.Name, r.MentalModelID, r.SortOrder, r.Managed)
	return err
}

func (s *Store) UpdateKnowledgeRow(ctx context.Context, bankID, id string, patch model.KnowledgePatch) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if patch.Name != nil || patch.ParentID != nil {
		_, err = tx.Exec(ctx, `
UPDATE knowledge_pages
SET name = COALESCE($3, name),
    parent_id = CASE WHEN $4::boolean THEN $5 ELSE parent_id END,
    updated_at = now()
WHERE bank_id = $1 AND id = $2`,
			bankID, id, patch.Name, patch.ParentID != nil, derefString(patch.ParentID),
		)
		if err != nil {
			return err
		}
	}
	if patch.SourceQuery != nil || patch.Tags != nil || patch.MaxTokens != nil || len(patch.Trigger) > 0 {
		tag, err := tx.Exec(ctx, `
UPDATE mental_models mm
SET source_query = COALESCE($3, mm.source_query),
    tags = COALESCE($4::text[], mm.tags),
    max_tokens = COALESCE($5, mm.max_tokens),
    trigger = COALESCE($6::jsonb, mm.trigger),
    last_updated = now()
FROM knowledge_pages kp
WHERE kp.bank_id = $1 AND kp.id = $2
  AND mm.bank_id = kp.bank_id
  AND mm.id = kp.mental_model_id`,
			bankID, id, patch.SourceQuery, patch.Tags, patch.MaxTokens, nullableJSON(patch.Trigger),
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// A folder with no backing model still accepts name/parent updates.
			var kind string
			if err := tx.QueryRow(ctx,
				`SELECT kind FROM knowledge_pages WHERE bank_id = $1 AND id = $2`, bankID, id).Scan(&kind); err != nil {
				return err
			}
			if kind == "page" {
				return fmt.Errorf("knowledge page %q has no mental model", id)
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteKnowledgeSubtree(ctx context.Context, bankID, id string) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
WITH RECURSIVE sub AS (
  SELECT id, mental_model_id FROM knowledge_pages WHERE bank_id = $1 AND id = $2
  UNION ALL
  SELECT kp.id, kp.mental_model_id
  FROM knowledge_pages kp
  JOIN sub ON kp.parent_id = sub.id
  WHERE kp.bank_id = $1
)
SELECT id, COALESCE(mental_model_id,'') FROM sub`, bankID, id)
	if err != nil {
		return nil, err
	}
	var ids []string
	var modelIDs []string
	for rows.Next() {
		var nodeID, modelID string
		if err := rows.Scan(&nodeID, &modelID); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, nodeID)
		if modelID != "" {
			modelIDs = append(modelIDs, modelID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM knowledge_pages WHERE bank_id = $1 AND id = ANY($2::text[])`, bankID, ids); err != nil {
		return nil, err
	}
	if len(modelIDs) > 0 {
		_, err = tx.Exec(ctx, `DELETE FROM mental_models WHERE bank_id = $1 AND id = ANY($2::text[])`, bankID, modelIDs)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *Store) SearchKnowledgeRows(ctx context.Context, bankID, query string, limit int) ([]model.KnowledgeRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT kp.id, COALESCE(kp.parent_id,''), kp.kind, kp.name, COALESCE(kp.mental_model_id,''),
       kp.managed, kp.sort_order, COALESCE(mm.source_query,''), COALESCE(mm.tags::text[], '{}'),
       COALESCE(mm.content,''), COALESCE(to_char(mm.last_refreshed_at, '%[1]s'), ''),
       COALESCE(to_char(mm.last_refresh_failed_at, '%[1]s'), ''),
       to_char(kp.created_at, '%[1]s'), to_char(kp.updated_at, '%[1]s')
FROM knowledge_pages kp
LEFT JOIN mental_models mm ON mm.bank_id = kp.bank_id AND mm.id = kp.mental_model_id
WHERE kp.bank_id = $1 AND kp.kind = 'page'
  AND (kp.name ILIKE '%%' || $2 || '%%'
       OR COALESCE(mm.source_query,'') ILIKE '%%' || $2 || '%%'
       OR COALESCE(mm.content,'') ILIKE '%%' || $2 || '%%')
ORDER BY GREATEST(similarity(kp.name, $2), similarity(COALESCE(mm.content,''), $2)) DESC, kp.name
LIMIT $3`, knowledgeTimeLayout), bankID, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.KnowledgeRow{}
	for rows.Next() {
		var r model.KnowledgeRow
		if err := rows.Scan(&r.ID, &r.ParentID, &r.Kind, &r.Name, &r.MentalModelID,
			&r.Managed, &r.SortOrder, &r.Description, &r.Tags, &r.Body,
			&r.LastRefreshedAt, &r.LastRefreshFailedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func derefString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableJSON(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return string(v)
}
