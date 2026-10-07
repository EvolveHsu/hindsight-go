package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
	"github.com/EvolveHsu/hindsight-go/internal/reflect"
)

// MentalModelSource is the storage slice mental models need.
type MentalModelSource interface {
	DirectiveSource
	CreateMentalModel(ctx context.Context, bankID string, m *model.MentalModel) (*model.MentalModel, error)
	GetMentalModel(ctx context.Context, bankID, id string) (*model.MentalModel, error)
	ListMentalModels(ctx context.Context, bankID string) ([]*model.MentalModel, error)
	UpdateMentalModel(ctx context.Context, bankID, id string, fn func(*model.MentalModel)) (*model.MentalModel, error)
	DeleteMentalModel(ctx context.Context, bankID, id string) (bool, error)
}

// mmStore casts the seam; both backends implement MentalModelSource.
func (e *Engine) mmStore() (MentalModelSource, error) {
	if mms, ok := e.store.(MentalModelSource); ok {
		return mms, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support mental models"}}
}

// CreateMentalModel implements create_mental_model. Upstream kicks an async
// refresh; lite runs the first refresh synchronously when a provider exists,
// so content is populated on create.
func (e *Engine) CreateMentalModel(ctx context.Context, req *api.CreateMentalModelRequest, params api.CreateMentalModelParams) (api.CreateMentalModelRes, error) {
	if req == nil || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.SourceQuery) == "" {
		return nil, badRequest("name and source_query are required")
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	mms, err := e.mmStore()
	if err != nil {
		return nil, err
	}

	id := req.ID.Value
	if id == "" {
		id = "mm-" + model.UnitID(params.BankID, "mental-model", req.Name+"\x00"+req.SourceQuery)[:12]
	}
	maxTokens := 0
	if req.MaxTokens.Set {
		maxTokens = req.MaxTokens.Value
	}
	m := &model.MentalModel{
		ID: id, Name: req.Name, SourceQuery: req.SourceQuery,
		Tags: normalizeTags(req.Tags), MaxTokens: maxTokens,
	}
	created, err := mms.CreateMentalModel(ctx, params.BankID, m)
	if err != nil {
		return nil, internal(err)
	}

	// synchronous first refresh when a provider is configured
	if e.provider != nil {
		e.refreshMentalModelNow(ctx, mms, params.BankID, created)
	}

	return &api.CreateMentalModelResponse{
		MentalModelID: optString(created.ID),
		OperationID:   "mm-refresh-" + created.ID,
	}, nil
}

// ListMentalModels implements list_mental_models.
func (e *Engine) ListMentalModels(ctx context.Context, params api.ListMentalModelsParams) (api.ListMentalModelsRes, error) {
	mms, err := e.mmStore()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	models, err := mms.ListMentalModels(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	limit, offset := params.Limit.Or(50), params.Offset.Or(0)
	items := make([]api.MentalModelResponse, 0, len(models))
	for _, m := range models {
		items = append(items, mentalModelResponse(m))
	}
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
	return &api.MentalModelListResponse{Items: items[offset:end], Total: total, Limit: limit, Offset: offset}, nil
}

// GetMentalModel implements get_mental_model.
func (e *Engine) GetMentalModel(ctx context.Context, params api.GetMentalModelParams) (api.GetMentalModelRes, error) {
	mms, err := e.mmStore()
	if err != nil {
		return nil, err
	}
	m, err := mms.GetMentalModel(ctx, params.BankID, params.MentalModelID)
	if err != nil {
		return nil, internal(err)
	}
	if m == nil {
		return nil, notFound("mental model %q not found", params.MentalModelID)
	}
	resp := mentalModelResponse(m)
	return &resp, nil
}

// UpdateMentalModel implements update_mental_model.
func (e *Engine) UpdateMentalModel(ctx context.Context, req *api.UpdateMentalModelRequest, params api.UpdateMentalModelParams) (api.UpdateMentalModelRes, error) {
	mms, err := e.mmStore()
	if err != nil {
		return nil, err
	}
	updated, err := mms.UpdateMentalModel(ctx, params.BankID, params.MentalModelID, func(m *model.MentalModel) {
		if req.Name.Set {
			m.Name = req.Name.Value
		}
		if req.SourceQuery.Set {
			m.SourceQuery = req.SourceQuery.Value
		}
		if req.MaxTokens.Set {
			m.MaxTokens = req.MaxTokens.Value
		}
		if req.Tags != nil {
			m.Tags = normalizeTags(req.Tags)
		}
	})
	if err != nil {
		return nil, internal(err)
	}
	if updated == nil {
		return nil, notFound("mental model %q not found", params.MentalModelID)
	}
	resp := mentalModelResponse(updated)
	return &resp, nil
}

// DeleteMentalModel implements delete_mental_model (204 in the spec; the
// generated Res union is response-type based, so we answer via the 404/200
// shapes the spec allows — actually the spec returns 200 with the model; use
// Update-style semantics via GetMentalModelRes).
func (e *Engine) DeleteMentalModel(ctx context.Context, params api.DeleteMentalModelParams) (api.DeleteMentalModelRes, error) {
	mms, err := e.mmStore()
	if err != nil {
		return nil, err
	}
	ok, err := mms.DeleteMentalModel(ctx, params.BankID, params.MentalModelID)
	if err != nil {
		return nil, internal(err)
	}
	if !ok {
		return nil, notFound("mental model %q not found", params.MentalModelID)
	}
	return &api.DeleteMentalModelOKApplicationJSON{}, nil
}

// RefreshMentalModel implements refresh_mental_model (sync in lite).
func (e *Engine) RefreshMentalModel(ctx context.Context, params api.RefreshMentalModelParams) (api.RefreshMentalModelRes, error) {
	mms, err := e.mmStore()
	if err != nil {
		return nil, err
	}
	if e.provider == nil {
		return nil, &statusError{
			status: http.StatusNotImplemented,
			body:   map[string]any{"detail": "no LLM provider configured; set HINDSIGHT_GO_LLM_BASE_URL / HINDSIGHT_GO_LLM_API_KEY / HINDSIGHT_GO_LLM_MODEL"},
		}
	}
	m, err := mms.GetMentalModel(ctx, params.BankID, params.MentalModelID)
	if err != nil {
		return nil, internal(err)
	}
	if m == nil {
		return nil, notFound("mental model %q not found", params.MentalModelID)
	}
	refreshed := e.refreshMentalModelNow(ctx, mms, params.BankID, m)
	return &api.AsyncOperationSubmitResponse{
		OperationID: "mm-refresh-" + refreshed.ID,
		Status:      "completed",
	}, nil
}

// refreshMentalModelNow runs the reflect agent with the model's source query
// and stamps the result. The reflect loop already enforces the hierarchical
// retrieval and evidence discipline; a mental model is its durable artifact.
func (e *Engine) refreshMentalModelNow(ctx context.Context, mms MentalModelSource, bankID string, m *model.MentalModel) *model.MentalModel {
	agent := &reflect.Agent{Provider: e.provider, Cfg: reflect.DefaultConfig()}
	if m.MaxTokens > 0 {
		agent.Cfg.MaxTokens = m.MaxTokens
	}
	res, err := agent.Run(ctx, &reflectSource{store: mms}, bankID, m.SourceQuery)
	content := ""
	if err == nil && res != nil {
		content = res.Text
	}
	now := time.Now().UTC().Format(time.RFC3339)
	updated, uerr := mms.UpdateMentalModel(ctx, bankID, m.ID, func(stored *model.MentalModel) {
		if content != "" {
			stored.Content = content
		}
		stored.LastRefreshed = now
		if res != nil && len(res.Evidence.Memories) > 0 {
			// stamp the newest memory the refresh saw (upstream last_memory_seen_at)
			stored.LastSeen = now
		}
	})
	if uerr != nil || updated == nil {
		return m
	}
	return updated
}

// mentalModelResponse maps the stored row onto the wire shape.
func mentalModelResponse(m *model.MentalModel) api.MentalModelResponse {
	return api.MentalModelResponse{
		ID:               m.ID,
		BankID:           m.BankID,
		Name:             m.Name,
		SourceQuery:      optString(m.SourceQuery),
		Content:          optString(m.Content),
		Tags:             m.Tags,
		MaxTokens:        optIntZero(m.MaxTokens),
		LastRefreshedAt:  optString(m.LastRefreshed),
		LastMemorySeenAt: optString(m.LastSeen),
		CreatedAt:        optString(m.CreatedAt),
	}
}

// normalizeTags guarantees a non-nil slice for wire fields.
func normalizeTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

// optIntZero maps 0 to unset (the wire treats 0 as "not specified").
func optIntZero(v int) api.OptInt {
	if v == 0 {
		return api.OptInt{}
	}
	return api.OptInt{Set: true, Value: v}
}
