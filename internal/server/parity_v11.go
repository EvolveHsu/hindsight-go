package server

import (
	"context"
	"encoding/json"
	"net/http"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
)

// auditLogSource is the narrow seam for GET /audit-logs/{log_id}.
type auditLogSource interface {
	AuditLogJSON(ctx context.Context, bankID, logID string) ([]byte, error)
}

// documentHistorySource reads document_events off async_operations.
type documentHistorySource interface {
	DocumentHistoryJSON(ctx context.Context, bankID, documentID string, limit, offset int) ([]byte, error)
}

// GetAuditLog implements get_audit_log.
func (e *Engine) GetAuditLog(ctx context.Context, params api.GetAuditLogParams) (api.GetAuditLogRes, error) {
	src, ok := e.store.(auditLogSource)
	if !ok {
		return nil, &statusError{
			status: http.StatusNotImplemented,
			body:   map[string]any{"detail": "backend does not support audit log lookup"},
		}
	}
	raw, err := src.AuditLogJSON(ctx, params.BankID, params.LogID)
	if err != nil {
		return nil, internal(err)
	}
	if raw == nil {
		return &api.GetAuditLogNotFound{}, nil
	}
	var out api.AuditLogEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, internal(err)
	}
	return &out, nil
}

// GetDocumentHistory implements get_document_history.
func (e *Engine) GetDocumentHistory(ctx context.Context, params api.GetDocumentHistoryParams) (api.GetDocumentHistoryRes, error) {
	src, ok := e.store.(documentHistorySource)
	if !ok {
		return nil, &statusError{
			status: http.StatusNotImplemented,
			body:   map[string]any{"detail": "backend does not support document history"},
		}
	}
	raw, err := src.DocumentHistoryJSON(ctx, params.BankID, params.DocumentID,
		params.Limit.Or(50), params.Offset.Or(0))
	if err != nil {
		return nil, internal(err)
	}
	var out api.DocumentHistoryResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, internal(err)
	}
	return &out, nil
}
