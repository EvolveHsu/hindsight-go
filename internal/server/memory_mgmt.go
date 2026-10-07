package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// MemoryReadSource is the additional storage surface needed for Cut B
// (memory management endpoints). Both backends implement it.
type MemoryReadSource interface {
	MentalModelSource
	ListMemories(ctx context.Context, bankID string, limit, offset int) ([]model.Unit, int, error)
	GetMemory(ctx context.Context, bankID, memoryID string) (*model.Unit, error)
	UpdateMemory(ctx context.Context, bankID, memoryID string, fn func(*model.Unit)) (*model.Unit, error)
	InvalidateMemory(ctx context.Context, bankID, memoryID, reason string) error
	ClearBankMemories(ctx context.Context, bankID string) (int, error)
	ClearMemoryObservations(ctx context.Context, bankID, memoryID string) (int, error)
	ListDocuments(ctx context.Context, bankID string, limit, offset int) ([]DocumentInfo, int, error)
	GetDocument(ctx context.Context, bankID, docID string) (*DocumentInfo, error)
	DeleteDocument(ctx context.Context, bankID, docID string) (bool, error)
}

type typedMemoryClearer interface {
	ClearBankMemoriesByType(ctx context.Context, bankID, factType string) (int, error)
}

type memoryCurationSource interface {
	ArchiveMemory(ctx context.Context, bankID, memoryID, reason string) error
	RestoreMemory(ctx context.Context, bankID, memoryID string) error
}

// DocumentInfo is the storage-level document projection for the wire. It is an
// alias of the shared model type so both backends satisfy MemoryReadSource.
type DocumentInfo = model.DocumentInfo

// ListMemories implements list_memories (GET /memories/list).
func (e *Engine) ListMemories(ctx context.Context, params api.ListMemoriesParams) (api.ListMemoriesRes, error) {
	src, err := e.memoryReadSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	limit, offset := params.Limit.Or(50), params.Offset.Or(0)
	units, total, err := src.ListMemories(ctx, params.BankID, limit, offset)
	if err != nil {
		return nil, internal(err)
	}
	items := make([]api.MemoryUnitListItem, 0, len(units))
	for i := range units {
		items = append(items, memoryUnitListItem(&units[i]))
	}
	return &api.ListMemoryUnitsResponse{Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

// GetMemory implements get_memory (GET /memories/{memory_id}).
func (e *Engine) GetMemory(ctx context.Context, params api.GetMemoryParams) (api.GetMemoryRes, error) {
	src, err := e.memoryReadSource()
	if err != nil {
		return nil, err
	}
	u, err := src.GetMemory(ctx, params.BankID, params.MemoryID)
	if err != nil {
		return nil, internal(err)
	}
	if u == nil {
		return nil, notFound("memory %q not found", params.MemoryID)
	}
	raw, err := json.Marshal(memoryUnitListItem(u))
	if err != nil {
		return nil, internal(err)
	}
	return (*api.GetMemoryOKApplicationJSON)(&raw), nil
}

// UpdateMemory implements update_memory (PATCH /memories/{memory_id}).
func (e *Engine) UpdateMemory(ctx context.Context, req *api.UpdateMemoryRequest, params api.UpdateMemoryParams) (api.UpdateMemoryRes, error) {
	src, err := e.memoryReadSource()
	if err != nil {
		return nil, err
	}
	if req.State.Set {
		curation, ok := src.(memoryCurationSource)
		if !ok {
			return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support memory curation"}}
		}
		switch req.State.Value {
		case "invalidated":
			reason := ""
			if req.Reason.Set {
				reason = req.Reason.Value
			}
			if err := curation.ArchiveMemory(ctx, params.BankID, params.MemoryID, reason); err != nil {
				return nil, internal(err)
			}
		case "valid":
			if err := curation.RestoreMemory(ctx, params.BankID, params.MemoryID); err != nil {
				return nil, internal(err)
			}
		default:
			return nil, badRequest("state must be 'invalidated' or 'valid'")
		}
		u, err := src.GetMemory(ctx, params.BankID, params.MemoryID)
		if err != nil {
			return nil, internal(err)
		}
		if u == nil {
			return nil, notFound("memory %q not found", params.MemoryID)
		}
		if !req.Text.Set && !req.Context.Set && !req.FactType.Set {
			raw, err := json.Marshal(memoryUnitListItem(u))
			if err != nil {
				return nil, internal(err)
			}
			return (*api.UpdateMemoryOKApplicationJSON)(&raw), nil
		}
	}
	var updated *model.Unit
	updated, err = src.UpdateMemory(ctx, params.BankID, params.MemoryID, func(u *model.Unit) {
		if req.Text.Set {
			u.Text = req.Text.Value
		}
		if req.Context.Set {
			u.Context = req.Context.Value
		}
		if req.FactType.Set && (req.FactType.Value == "world" || req.FactType.Value == "experience") {
			u.FactType = model.FactType(req.FactType.Value)
		}
	})
	if err != nil {
		return nil, internal(err)
	}
	if updated == nil {
		return nil, notFound("memory %q not found", params.MemoryID)
	}
	raw, err := json.Marshal(memoryUnitListItem(updated))
	if err != nil {
		return nil, internal(err)
	}
	return (*api.UpdateMemoryOKApplicationJSON)(&raw), nil
}

// ClearBankMemories implements clear_memories (DELETE /memories).
func (e *Engine) ClearBankMemories(ctx context.Context, params api.ClearBankMemoriesParams) (api.ClearBankMemoriesRes, error) {
	src, err := e.memoryReadSource()
	if err != nil {
		return nil, err
	}
	var n int
	if params.Type.Set && params.Type.Value != "" {
		switch params.Type.Value {
		case "world", "experience", "observation":
		default:
			return nil, badRequest("unknown fact type %q", params.Type.Value)
		}
		tc, ok := src.(typedMemoryClearer)
		if !ok {
			return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support typed memory deletion"}}
		}
		n, err = tc.ClearBankMemoriesByType(ctx, params.BankID, params.Type.Value)
	} else {
		n, err = src.ClearBankMemories(ctx, params.BankID)
	}
	if err != nil {
		return nil, internal(err)
	}
	return &api.DeleteResponse{
		Success:      true,
		DeletedCount: api.OptInt{Set: true, Value: n},
	}, nil
}

// ClearMemoryObservations implements clear_memory_observations.
func (e *Engine) ClearMemoryObservations(ctx context.Context, params api.ClearMemoryObservationsParams) (api.ClearMemoryObservationsRes, error) {
	src, err := e.memoryReadSource()
	if err != nil {
		return nil, err
	}
	n, err := src.ClearMemoryObservations(ctx, params.BankID, params.MemoryID)
	if err != nil {
		return nil, internal(err)
	}
	return &api.ClearMemoryObservationsResponse{DeletedCount: n}, nil
}

// ListDocuments implements list_documents (GET /documents).
func (e *Engine) ListDocuments(ctx context.Context, params api.ListDocumentsParams) (api.ListDocumentsRes, error) {
	src, err := e.memoryReadSource()
	if err != nil {
		return nil, err
	}
	limit, offset := params.Limit.Or(50), params.Offset.Or(0)
	docs, total, err := src.ListDocuments(ctx, params.BankID, limit, offset)
	if err != nil {
		return nil, internal(err)
	}
	items := make([]api.DocumentListItem, 0, len(docs))
	for _, d := range docs {
		items = append(items, api.DocumentListItem{
			ID:              d.ID,
			BankID:          optString(d.BankID),
			ContentHash:     optString(d.ContentHash),
			CreatedAt:       optString(d.CreatedAt),
			UpdatedAt:       optString(d.UpdatedAt),
			Tags:            d.Tags,
			MemoryUnitCount: api.OptInt{Set: true, Value: d.UnitCount},
		})
	}
	return &api.ListDocumentsResponse{Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

// GetDocument implements get_document (GET /documents/{document_id}).
func (e *Engine) GetDocument(ctx context.Context, params api.GetDocumentParams) (api.GetDocumentRes, error) {
	src, err := e.memoryReadSource()
	if err != nil {
		return nil, err
	}
	d, err := src.GetDocument(ctx, params.BankID, params.DocumentID)
	if err != nil {
		return nil, internal(err)
	}
	if d == nil {
		return nil, notFound("document %q not found", params.DocumentID)
	}
	return &api.DocumentResponse{
		ID:              d.ID,
		BankID:          d.BankID,
		OriginalText:    d.OriginalText,
		ContentHash:     d.ContentHash,
		CreatedAt:       d.CreatedAt,
		UpdatedAt:       d.UpdatedAt,
		MemoryUnitCount: d.UnitCount,
		Tags:            d.Tags,
	}, nil
}

// DeleteDocument implements delete_document (DELETE /documents/{document_id}).
func (e *Engine) DeleteDocument(ctx context.Context, params api.DeleteDocumentParams) (api.DeleteDocumentRes, error) {
	src, err := e.memoryReadSource()
	if err != nil {
		return nil, err
	}
	_, err = src.DeleteDocument(ctx, params.BankID, params.DocumentID)
	if err != nil {
		return nil, internal(err)
	}
	return &api.DeleteDocumentResponse{
		Success:            true,
		Message:            fmt.Sprintf("deleted document %s", params.DocumentID),
		DocumentID:         params.DocumentID,
		MemoryUnitsDeleted: 0, // bool return from store; exact count not tracked in lite
	}, nil
}

// GetOperationStatus implements get_operation_status. Lite runs everything sync,
// so operations are always "completed".
func (e *Engine) GetOperationStatus(ctx context.Context, params api.GetOperationStatusParams) (api.GetOperationStatusRes, error) {
	// Operations this process ran inline are remembered with their result so a
	// caller polling an export id can read its download_url.
	if rec, ok := lookupOperation(params.OperationID); ok {
		return &api.OperationStatusResponse{
			OperationID:    params.OperationID,
			ID:             api.OptString{Set: true, Value: params.OperationID},
			Status:         api.OperationStatusResponseStatusCompleted,
			OperationType:  api.OptString{Set: true, Value: rec.Operation},
			TaskType:       api.OptString{Set: true, Value: rec.Operation},
			CreatedAt:      api.OptString{Set: true, Value: rec.CreatedAt.Format(time.RFC3339)},
			UpdatedAt:      api.OptString{Set: true, Value: rec.CreatedAt.Format(time.RFC3339)},
			CompletedAt:    api.OptString{Set: true, Value: rec.CreatedAt.Format(time.RFC3339)},
			ResultMetadata: marshalMetadata(rec.Metadata),
		}, nil
	}
	return &api.OperationStatusResponse{
		OperationID: params.OperationID,
		Status:      api.OperationStatusResponseStatusCompleted,
	}, nil
}

// ListOperations implements list_operations.
func (e *Engine) ListOperations(ctx context.Context, params api.ListOperationsParams) (api.ListOperationsRes, error) {
	// The lite build records the operations it ran inline, so the list shows
	// the exports, imports and file conversions this process performed.
	items := recordedOperations(params.BankID)
	limit, offset := params.Limit.Or(50), params.Offset.Or(0)
	total := len(items)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}
	return &api.OperationsListResponse{
		BankID:     params.BankID,
		Total:      total,
		Limit:      limit,
		Offset:     offset,
		Operations: items[offset:end],
	}, nil
}

// memoryReadSource casts the seam.
func (e *Engine) memoryReadSource() (MemoryReadSource, error) {
	if s, ok := e.store.(MemoryReadSource); ok {
		return s, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support memory management"}}
}

// memoryUnitListItem converts a model.Unit to the wire shape.
func memoryUnitListItem(u *model.Unit) api.MemoryUnitListItem {
	return api.MemoryUnitListItem{
		ID:          u.ID,
		Text:        optString(u.Text),
		Context:     optString(u.Context),
		FactType:    optString(string(u.FactType)),
		DocumentID:  optString(u.DocumentID),
		ChunkID:     optString(u.ChunkID),
		Tags:        u.Tags,
		MentionedAt: optString(u.MentionedAt),
	}
}

var _ = strings.TrimSpace
