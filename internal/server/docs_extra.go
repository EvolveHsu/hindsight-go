package server

import (
	"context"
	"net/http"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// DocumentExtraSource is the storage surface behind the document endpoints that
// sit past plain list/get: chunks, tag updates and reprocessing. Both backends
// implement it.
type DocumentExtraSource interface {
	DocumentChunks(ctx context.Context, bankID, docID string, limit, offset int) (*model.ChunkPage, error)
	Chunk(ctx context.Context, chunkID string) (*model.ChunkInfo, error)
	UpdateDocumentTags(ctx context.Context, bankID, docID string, tags []string) (bool, error)
	PatchDocumentTags(ctx context.Context, bankID, docID string, replace, add, remove []string) (bool, error)
	ReprocessDocument(ctx context.Context, bankID, docID string) (*model.ReprocessResult, error)
}

func (e *Engine) documentExtraSource() (DocumentExtraSource, error) {
	if s, ok := e.store.(DocumentExtraSource); ok {
		return s, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support document chunks"}}
}

// ListDocumentChunks implements list_document_chunks
// (GET /banks/{bank_id}/documents/{document_id}/chunks).
func (e *Engine) ListDocumentChunks(ctx context.Context, params api.ListDocumentChunksParams) (api.ListDocumentChunksRes, error) {
	src, err := e.documentExtraSource()
	if err != nil {
		return nil, err
	}
	limit, offset := params.Limit.Or(100), params.Offset.Or(0)
	page, err := src.DocumentChunks(ctx, params.BankID, params.DocumentID, limit, offset)
	if err != nil {
		return nil, internal(err)
	}
	if page == nil {
		return nil, notFound("Document not found")
	}
	items := make([]api.ChunkResponse, 0, len(page.Items))
	for _, c := range page.Items {
		items = append(items, api.ChunkResponse{
			ChunkID:    c.ChunkID,
			DocumentID: c.DocumentID,
			BankID:     c.BankID,
			ChunkIndex: c.ChunkIndex,
			ChunkText:  c.ChunkText,
			CreatedAt:  c.CreatedAt,
		})
	}
	return &api.ListChunksResponse{Items: items, Total: page.Total, Limit: limit, Offset: offset}, nil
}

// GetChunk implements get_chunk (GET /default/chunks/{chunk_id}).
func (e *Engine) GetChunk(ctx context.Context, params api.GetChunkParams) (api.GetChunkRes, error) {
	src, err := e.documentExtraSource()
	if err != nil {
		return nil, err
	}
	c, err := src.Chunk(ctx, params.ChunkID)
	if err != nil {
		return nil, internal(err)
	}
	if c == nil {
		return nil, notFound("Chunk not found")
	}
	return &api.ChunkResponse{
		ChunkID:    c.ChunkID,
		DocumentID: c.DocumentID,
		BankID:     c.BankID,
		ChunkIndex: c.ChunkIndex,
		ChunkText:  c.ChunkText,
		CreatedAt:  c.CreatedAt,
	}, nil
}

// UpdateDocument implements update_document (PATCH /documents/{document_id}).
// The tags array replaces the document's tags; add_tags/remove_tags apply an
// incremental update. Both modes push the final tags down to memory units and
// invalidate derived observations so they are rebuilt under the new scope.
func (e *Engine) UpdateDocument(ctx context.Context, req *api.UpdateDocumentRequest, params api.UpdateDocumentParams) (api.UpdateDocumentRes, error) {
	src, err := e.documentExtraSource()
	if err != nil {
		return nil, err
	}
	if req == nil || (req.Tags == nil && req.AddTags == nil && req.RemoveTags == nil) {
		return nil, &statusError{status: http.StatusUnprocessableEntity, body: map[string]any{"detail": "At least one field (tags, add_tags or remove_tags) must be provided"}}
	}
	if req.Tags != nil && (req.AddTags != nil || req.RemoveTags != nil) {
		return nil, &statusError{status: http.StatusUnprocessableEntity, body: map[string]any{"detail": "tags cannot be combined with add_tags or remove_tags"}}
	}
	var ok bool
	if req.Tags != nil {
		ok, err = src.UpdateDocumentTags(ctx, params.BankID, params.DocumentID, req.Tags)
	} else {
		ok, err = src.PatchDocumentTags(ctx, params.BankID, params.DocumentID, nil, req.AddTags, req.RemoveTags)
	}
	if err != nil {
		return nil, internal(err)
	}
	if !ok {
		return nil, notFound("Document not found")
	}
	return &api.UpdateDocumentResponse{Success: api.OptBool{Set: true, Value: true}}, nil
}

// ReprocessDocument implements reprocess_document
// (POST /documents/{document_id}/reprocess): the stored document is re-run
// through retain, replacing its memory units with freshly extracted ones.
func (e *Engine) ReprocessDocument(ctx context.Context, params api.ReprocessDocumentParams) (api.ReprocessDocumentRes, error) {
	src, err := e.documentExtraSource()
	if err != nil {
		return nil, err
	}
	res, err := src.ReprocessDocument(ctx, params.BankID, params.DocumentID)
	if err != nil {
		return nil, internal(err)
	}
	if res == nil {
		return nil, notFound("Document not found")
	}
	return &api.ReprocessDocumentResponse{
		Success:     true,
		OperationID: res.OperationID,
		ItemsCount:  res.ItemsCount,
	}, nil
}
