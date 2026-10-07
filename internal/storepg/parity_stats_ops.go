package storepg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

func (s *Store) BankStats(ctx context.Context, bankID string) (*model.BankStats, error) {
	out := &model.BankStats{
		NodesByFactType:    map[string]int{},
		LinksByLinkType:    map[string]int{},
		OperationsByStatus: map[string]int{},
	}
	if err := s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT
  (SELECT count(*) FROM %[1]s WHERE bank_id=$1),
  (SELECT count(*) FROM documents WHERE bank_id=$1),
  (SELECT count(*) FROM memory_links WHERE bank_id=$1)`, s.unitsTable()), bankID).
		Scan(&out.TotalNodes, &out.TotalDocuments, &out.TotalLinks); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT fact_type, count(*) FROM %s WHERE bank_id=$1 GROUP BY fact_type`, s.unitsTable()), bankID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ft string
		var n int
		if err := rows.Scan(&ft, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out.NodesByFactType[ft] = n
		if ft == "observation" {
			out.TotalObservations = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT link_type, count(*) FROM memory_links WHERE bank_id=$1 GROUP BY link_type`, bankID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var lt string
		var n int
		if err := rows.Scan(&lt, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out.LinksByLinkType[lt] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT status, count(*) FROM async_operations WHERE bank_id=$1 GROUP BY status`, bankID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out.OperationsByStatus[status] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var lastConsolidated, lastWrite *time.Time
	var createdAt, updatedAt, lastDocument *time.Time
	var pending, failed int
	if err := s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT
  (SELECT created_at FROM banks WHERE bank_id=$1),
  (SELECT updated_at FROM banks WHERE bank_id=$1),
  (SELECT last_consolidated_at FROM banks WHERE bank_id=$1),
  (SELECT max(COALESCE(updated_at, created_at)) FROM %[1]s WHERE bank_id=$1),
  (SELECT max(updated_at) FROM documents WHERE bank_id=$1),
  (SELECT count(*) FROM %[1]s WHERE bank_id=$1 AND fact_type IN ('world','experience') AND consolidated_at IS NULL AND consolidation_failed_at IS NULL),
  (SELECT count(*) FROM %[1]s WHERE bank_id=$1 AND consolidation_failed_at IS NOT NULL)`, s.unitsTable()), bankID).
		Scan(&createdAt, &updatedAt, &lastConsolidated, &lastWrite, &lastDocument, &pending, &failed); err != nil {
		if createdAt != nil {
			out.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		}
		if updatedAt != nil {
			out.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		}
		if lastDocument != nil {
			out.LastDocumentAt = lastDocument.UTC().Format(time.RFC3339)
		}
		return nil, err
	}
	if lastConsolidated != nil {
		out.LastConsolidatedAt = lastConsolidated.UTC().Format(time.RFC3339)
	}
	if lastWrite != nil {
		out.LastMemoryWriteAt = lastWrite.UTC().Format(time.RFC3339)
	}
	out.PendingConsolidation = pending
	out.FailedConsolidation = failed
	return out, nil
}

func (s *Store) MemoryBuckets(ctx context.Context, bankID, period, timeField string) ([]model.MemoryBucket, error) {
	trunc := "day"
	interval := "7 days"
	switch period {
	case "1h":
		trunc, interval = "minute", "1 hour"
	case "12h":
		trunc, interval = "hour", "12 hours"
	case "1d":
		trunc, interval = "hour", "1 day"
	case "7d":
		trunc, interval = "day", "7 days"
	case "30d":
		trunc, interval = "day", "30 days"
	case "90d":
		trunc, interval = "day", "90 days"
	}
	col := "created_at"
	switch timeField {
	case "mentioned_at", "occurred_start", "occurred_end":
		col = timeField
	}
	query := fmt.Sprintf(`
SELECT to_char(date_trunc('%s', COALESCE(%s, created_at)), 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
       fact_type, count(*)
FROM %s
WHERE bank_id=$1 AND COALESCE(%s, created_at) >= now() - interval '%s'
GROUP BY 1,2 ORDER BY 1`, trunc, col, s.unitsTable(), col, interval)
	rows, err := s.pool.Query(ctx, query, bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byTime := map[string]*model.MemoryBucket{}
	for rows.Next() {
		var ts, ft string
		var n int
		if err := rows.Scan(&ts, &ft, &n); err != nil {
			return nil, err
		}
		b := byTime[ts]
		if b == nil {
			b = &model.MemoryBucket{Time: ts}
			byTime[ts] = b
		}
		switch ft {
		case "world":
			b.World = n
		case "experience":
			b.Experience = n
		case "observation":
			b.Observation = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(byTime))
	for k := range byTime {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]model.MemoryBucket, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byTime[k])
	}
	return out, nil
}

func (s *Store) ObservationScopeCounts(ctx context.Context, bankID string) ([]model.ScopeCount, error) {
	rows, err := s.pool.Query(ctx, `
SELECT observation_scopes::text, count(*)
FROM memory_units
WHERE bank_id=$1 AND fact_type='observation' AND observation_scopes IS NOT NULL
GROUP BY observation_scopes::text
ORDER BY count(*) DESC`, bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ScopeCount{}
	for rows.Next() {
		var raw string
		var n int
		if err := rows.Scan(&raw, &n); err != nil {
			return nil, err
		}
		tags := parseScopeTags(raw)
		out = append(out, model.ScopeCount{Tags: tags, Count: n})
	}
	return out, rows.Err()
}

func parseScopeTags(raw string) []string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return []string{}
	}
	var tags []string
	switch value := v.(type) {
	case []any:
		for _, item := range value {
			if s, ok := item.(string); ok {
				tags = append(tags, s)
			}
			if nested, ok := item.([]any); ok {
				for _, n := range nested {
					if s, ok := n.(string); ok {
						tags = append(tags, s)
					}
				}
			}
		}
	case map[string]any:
		if rawTags, ok := value["tags"].([]any); ok {
			for _, item := range rawTags {
				if s, ok := item.(string); ok {
					tags = append(tags, s)
				}
			}
		}
	}
	sort.Strings(tags)
	return tags
}

func (s *Store) GetDirectiveRow(ctx context.Context, bankID, id string) (*model.Directive, error) {
	var d model.Directive
	err := s.pool.QueryRow(ctx, `
SELECT id::text, bank_id, name, content, priority, is_active, COALESCE(tags::text[],'{}')
FROM directives WHERE bank_id=$1 AND id=$2`, bankID, id).
		Scan(&d.ID, &d.BankID, &d.Name, &d.Content, &d.Priority, &d.IsActive, &d.Tags)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) UpdateDirectiveRow(ctx context.Context, bankID, id string, patch model.DirectivePatch) (*model.Directive, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE directives SET
  name = COALESCE($3, name),
  content = COALESCE($4, content),
  priority = COALESCE($5, priority),
  is_active = COALESCE($6, is_active),
  tags = COALESCE($7::text[], tags),
  updated_at = now()
WHERE bank_id=$1 AND id=$2`,
		bankID, id, patch.Name, patch.Content, patch.Priority, patch.IsActive, patch.Tags)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	return s.GetDirectiveRow(ctx, bankID, id)
}

func (s *Store) ClearObservationRows(ctx context.Context, bankID string) (int, error) {
	tag, err := s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE bank_id=$1 AND fact_type='observation'`, s.unitsTable()), bankID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) DeleteOperationRow(ctx context.Context, bankID, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
DELETE FROM async_operations
WHERE bank_id=$1 AND operation_id::text=$2 AND status IN ('completed','failed','cancelled')`,
		bankID, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) RetryOperationRow(ctx context.Context, bankID, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE async_operations
SET status='pending', error_message=NULL, next_retry_at=NULL, claimed_at=NULL, worker_id=NULL, updated_at=now()
WHERE bank_id=$1 AND operation_id::text=$2 AND status IN ('failed','cancelled')`,
		bankID, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
