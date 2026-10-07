package storepg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

func (s *Store) ListWebhookRows(ctx context.Context, bankID string) ([]model.WebhookRow, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT id::text, bank_id, url, COALESCE(secret,''), event_types::text[], enabled,
       COALESCE(http_config::text,'{}'), to_char(created_at, '%[1]s'), to_char(updated_at, '%[1]s')
FROM webhooks WHERE bank_id = $1 ORDER BY created_at, id`, knowledgeTimeLayout), bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.WebhookRow{}
	for rows.Next() {
		var r model.WebhookRow
		if err := rows.Scan(&r.ID, &r.BankID, &r.URL, &r.Secret, &r.EventTypes, &r.Enabled,
			&r.HTTPConfig, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetWebhookRow(ctx context.Context, bankID, id string) (*model.WebhookRow, error) {
	var r model.WebhookRow
	err := s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT id::text, bank_id, url, COALESCE(secret,''), event_types::text[], enabled,
       COALESCE(http_config::text,'{}'), to_char(created_at, '%[1]s'), to_char(updated_at, '%[1]s')
FROM webhooks WHERE bank_id = $1 AND id = $2`, knowledgeTimeLayout), bankID, id).Scan(
		&r.ID, &r.BankID, &r.URL, &r.Secret, &r.EventTypes, &r.Enabled,
		&r.HTTPConfig, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) CreateWebhookRow(ctx context.Context, bankID string, r model.WebhookRow) (*model.WebhookRow, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
INSERT INTO webhooks (bank_id, url, secret, event_types, enabled, http_config)
VALUES ($1,$2,NULLIF($3,''),$4::text[],$5,COALESCE(NULLIF($6,'')::jsonb,'{}'::jsonb))
RETURNING id::text`,
		bankID, r.URL, r.Secret, r.EventTypes, r.Enabled, string(r.HTTPConfig)).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetWebhookRow(ctx, bankID, id)
}

func (s *Store) UpdateWebhookRow(ctx context.Context, bankID, id string, patch model.WebhookPatch) (*model.WebhookRow, error) {
	var url, secret any
	if patch.URL != nil {
		url = *patch.URL
	}
	if patch.SecretPresent {
		if patch.Secret != nil {
			secret = *patch.Secret
		} else {
			secret = ""
		}
	}
	var events any
	if patch.EventTypes != nil {
		events = *patch.EventTypes
	}
	var enabled any
	if patch.Enabled != nil {
		enabled = *patch.Enabled
	}
	var httpConfig any
	if len(patch.HTTPConfig) > 0 {
		httpConfig = string(patch.HTTPConfig)
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE webhooks SET
  url = COALESCE($3, url),
  secret = CASE WHEN $4::boolean THEN $5 ELSE secret END,
  event_types = COALESCE($6::text[], event_types),
  enabled = COALESCE($7, enabled),
  http_config = COALESCE($8::jsonb, http_config),
  updated_at = now()
WHERE bank_id = $1 AND id = $2`,
		bankID, id, url, patch.SecretPresent, secret, events, enabled, httpConfig)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	return s.GetWebhookRow(ctx, bankID, id)
}

func (s *Store) DeleteWebhookRow(ctx context.Context, bankID, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM webhooks WHERE bank_id = $1 AND id = $2`, bankID, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) ListWebhookDeliveryRows(ctx context.Context, bankID, webhookID string, limit int) ([]model.WebhookDeliveryRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT operation_id::text,
       COALESCE(task_payload->>'webhook_id',''),
       COALESCE(task_payload->>'url',''),
       COALESCE(task_payload->>'event_type',''),
       status, retry_count,
       COALESCE(to_char(next_retry_at, '%[1]s'), ''),
       COALESCE(error_message, ''),
       COALESCE((result_metadata->>'last_response_status')::int, 0),
       COALESCE(result_metadata->>'last_response_body',''),
       COALESCE(to_char(completed_at, '%[1]s'), ''),
       to_char(created_at, '%[1]s'),
       to_char(updated_at, '%[1]s')
FROM async_operations
WHERE bank_id = $1 AND operation_type = 'webhook_delivery'
  AND task_payload->>'webhook_id' = $2
ORDER BY created_at DESC LIMIT $3`, knowledgeTimeLayout), bankID, webhookID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.WebhookDeliveryRow{}
	for rows.Next() {
		var r model.WebhookDeliveryRow
		if err := rows.Scan(&r.ID, &r.WebhookID, &r.URL, &r.EventType, &r.Status, &r.Attempts,
			&r.NextRetryAt, &r.LastError, &r.LastResponseStatus, &r.LastResponseBody,
			&r.LastAttemptAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
