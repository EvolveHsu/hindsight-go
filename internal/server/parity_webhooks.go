package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// WebhookSource is the persistence slice for bank webhooks.
type WebhookSource interface {
	ListWebhookRows(ctx context.Context, bankID string) ([]model.WebhookRow, error)
	GetWebhookRow(ctx context.Context, bankID, id string) (*model.WebhookRow, error)
	CreateWebhookRow(ctx context.Context, bankID string, row model.WebhookRow) (*model.WebhookRow, error)
	UpdateWebhookRow(ctx context.Context, bankID, id string, patch model.WebhookPatch) (*model.WebhookRow, error)
	DeleteWebhookRow(ctx context.Context, bankID, id string) (bool, error)
	ListWebhookDeliveryRows(ctx context.Context, bankID, webhookID string, limit int) ([]model.WebhookDeliveryRow, error)
}

func (e *Engine) webhookSource() (WebhookSource, error) {
	if ws, ok := e.store.(WebhookSource); ok {
		return ws, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support webhooks"}}
}

func webhookResponse(row model.WebhookRow) api.WebhookResponse {
	out := api.WebhookResponse{
		ID:         row.ID,
		BankID:     row.BankID,
		URL:        row.URL,
		EventTypes: row.EventTypes,
		Enabled:    row.Enabled,
		CreatedAt:  optString(row.CreatedAt),
		UpdatedAt:  optString(row.UpdatedAt),
	}
	if len(row.HTTPConfig) > 0 {
		var cfg api.WebhookHttpConfig
		if err := json.Unmarshal(row.HTTPConfig, &cfg); err == nil {
			out.HTTPConfig = api.OptWebhookHttpConfig{Set: true, Value: cfg}
		}
	}
	return out
}

func (e *Engine) ListWebhooks(ctx context.Context, params api.ListWebhooksParams) (api.ListWebhooksRes, error) {
	ws, err := e.webhookSource()
	if err != nil {
		return nil, err
	}
	rows, err := ws.ListWebhookRows(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	limit, offset := params.Limit.Or(50), params.Offset.Or(0)
	total := len(rows)
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}
	items := make([]api.WebhookResponse, 0, end-offset)
	for _, row := range rows[offset:end] {
		items = append(items, webhookResponse(row))
	}
	return &api.WebhookListResponse{Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

func (e *Engine) CreateWebhook(ctx context.Context, req *api.CreateWebhookRequest, params api.CreateWebhookParams) (api.CreateWebhookRes, error) {
	ws, err := e.webhookSource()
	if err != nil {
		return nil, err
	}
	if req == nil || strings.TrimSpace(req.URL) == "" {
		return nil, badRequest("url is required")
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	row := model.WebhookRow{
		URL:        req.URL,
		EventTypes: normalizeTags(req.EventTypes),
		Enabled:    true,
	}
	if req.Secret.Set {
		row.Secret = req.Secret.Value
	}
	if req.Enabled.Set {
		row.Enabled = req.Enabled.Value
	}
	if req.HTTPConfig.Set {
		raw, err := json.Marshal(req.HTTPConfig.Value)
		if err != nil {
			return nil, badRequest("invalid http_config: %v", err)
		}
		row.HTTPConfig = raw
	}
	created, err := ws.CreateWebhookRow(ctx, params.BankID, row)
	if err != nil {
		return nil, internal(err)
	}
	return &api.WebhookResponse{
		ID: created.ID, BankID: created.BankID, URL: created.URL,
		EventTypes: created.EventTypes, Enabled: created.Enabled,
		CreatedAt: optString(created.CreatedAt), UpdatedAt: optString(created.UpdatedAt),
		HTTPConfig: req.HTTPConfig,
	}, nil
}

func (e *Engine) UpdateWebhook(ctx context.Context, req *api.UpdateWebhookRequest, params api.UpdateWebhookParams) (api.UpdateWebhookRes, error) {
	ws, err := e.webhookSource()
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, badRequest("request is required")
	}
	patch := model.WebhookPatch{}
	if req.URL.Set {
		patch.URL = &req.URL.Value
	}
	if req.Secret.Set {
		value := req.Secret.Value
		patch.Secret = &value
		patch.SecretPresent = true
	}
	if req.EventTypes != nil {
		patch.EventTypes = &req.EventTypes
	}
	if req.Enabled.Set {
		patch.Enabled = &req.Enabled.Value
	}
	if req.HTTPConfig.Set {
		raw, err := json.Marshal(req.HTTPConfig.Value)
		if err != nil {
			return nil, badRequest("invalid http_config: %v", err)
		}
		patch.HTTPConfig = raw
	}
	updated, err := ws.UpdateWebhookRow(ctx, params.BankID, params.WebhookID, patch)
	if err != nil {
		return nil, internal(err)
	}
	if updated == nil {
		return nil, notFound("webhook %q not found", params.WebhookID)
	}
	resp := webhookResponse(*updated)
	return &resp, nil
}

func (e *Engine) DeleteWebhook(ctx context.Context, params api.DeleteWebhookParams) (api.DeleteWebhookRes, error) {
	ws, err := e.webhookSource()
	if err != nil {
		return nil, err
	}
	ok, err := ws.DeleteWebhookRow(ctx, params.BankID, params.WebhookID)
	if err != nil {
		return nil, internal(err)
	}
	if !ok {
		return nil, notFound("webhook %q not found", params.WebhookID)
	}
	return &api.DeleteResponse{Success: true}, nil
}

func (e *Engine) ListWebhookDeliveries(ctx context.Context, params api.ListWebhookDeliveriesParams) (api.ListWebhookDeliveriesRes, error) {
	ws, err := e.webhookSource()
	if err != nil {
		return nil, err
	}
	limit := params.Limit.Or(50)
	rows, err := ws.ListWebhookDeliveryRows(ctx, params.BankID, params.WebhookID, limit)
	if err != nil {
		return nil, internal(err)
	}
	items := make([]api.WebhookDeliveryResponse, 0, len(rows))
	for _, row := range rows {
		item := api.WebhookDeliveryResponse{
			ID: row.ID, WebhookID: row.WebhookID, URL: row.URL, EventType: row.EventType,
			Status: row.Status, Attempts: row.Attempts,
			NextRetryAt: optString(row.NextRetryAt), LastError: optString(row.LastError),
			LastResponseBody: optString(row.LastResponseBody), LastAttemptAt: optString(row.LastAttemptAt),
			CreatedAt: optString(row.CreatedAt), UpdatedAt: optString(row.UpdatedAt),
		}
		if row.LastResponseStatus != 0 {
			item.LastResponseStatus = api.OptInt{Set: true, Value: row.LastResponseStatus}
		}
		items = append(items, item)
	}
	return &api.WebhookDeliveryListResponse{Items: items}, nil
}
