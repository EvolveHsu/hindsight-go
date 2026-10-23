package storepg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AuditLogJSON returns one full audit log entry, or nil when it does not exist.
func (s *Store) AuditLogJSON(ctx context.Context, bankID, logID string) ([]byte, error) {
	var r auditJSONRow
	var duration *int
	var request, response, metadata string
	err := s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT id::text, action, transport, COALESCE(bank_id,''), to_char(started_at, '%[1]s'),
       COALESCE(to_char(ended_at, '%[1]s'),''),
       CASE WHEN ended_at IS NULL THEN NULL ELSE (EXTRACT(EPOCH FROM (ended_at-started_at))*1000)::int END,
       COALESCE(request::text,'null'), COALESCE(response::text,'null'), COALESCE(metadata::text,'{}')
FROM audit_log WHERE bank_id=$1 AND id::text=$2`, auditTimeLayout), bankID, logID).
		Scan(&r.ID, &r.Action, &r.Transport, &r.BankID, &r.StartedAt, &r.EndedAt,
			&duration, &request, &response, &metadata)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.DurationMs = duration
	r.Request = json.RawMessage(request)
	r.Response = json.RawMessage(response)
	r.Metadata = json.RawMessage(metadata)
	return json.Marshal(r)
}

// DocumentHistoryJSON rebuilds a document's history from the document_events
// recorded on async_operations.result_metadata (the upstream schema has no
// dedicated history table).
func (s *Store) DocumentHistoryJSON(ctx context.Context, bankID, documentID string, limit, offset int) ([]byte, error) {
	if limit <= 0 {
		limit = 50
	}
	var total int
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM async_operations
WHERE bank_id=$1
  AND jsonb_typeof(result_metadata->'document_events') = 'array'
  AND EXISTS (
    SELECT 1 FROM jsonb_array_elements(result_metadata->'document_events') e
    WHERE e->>'document_id' = $2
  )`, bankID, documentID).Scan(&total); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT operation_id::text, result_metadata->'document_events'
FROM async_operations
WHERE bank_id=$1
  AND jsonb_typeof(result_metadata->'document_events') = 'array'
  AND EXISTS (
    SELECT 1 FROM jsonb_array_elements(result_metadata->'document_events') e
    WHERE e->>'document_id' = $2
  )
ORDER BY created_at DESC LIMIT $3 OFFSET $4`, bankID, documentID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var opID string
		var raw []byte
		if err := rows.Scan(&opID, &raw); err != nil {
			return nil, err
		}
		var events []map[string]any
		if err := json.Unmarshal(raw, &events); err != nil {
			return nil, err
		}
		for _, event := range events {
			if id, _ := event["document_id"].(string); id == documentID {
				event["operation_id"] = opID
				items = append(items, event)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"document_id": documentID,
		"total":       total,
		"limit":       limit,
		"offset":      offset,
		"items":       items,
	})
}
