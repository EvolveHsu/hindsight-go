package server

import (
	"context"
	"fmt"
	"net/http"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

type statsOpsSource interface {
	BankStats(ctx context.Context, bankID string) (*model.BankStats, error)
	MemoryBuckets(ctx context.Context, bankID, period, timeField string) ([]model.MemoryBucket, error)
	ObservationScopeCounts(ctx context.Context, bankID string) ([]model.ScopeCount, error)
	GetDirectiveRow(ctx context.Context, bankID, id string) (*model.Directive, error)
	UpdateDirectiveRow(ctx context.Context, bankID, id string, patch model.DirectivePatch) (*model.Directive, error)
	ClearObservationRows(ctx context.Context, bankID string) (int, error)
	DeleteOperationRow(ctx context.Context, bankID, id string) (bool, error)
	RetryOperationRow(ctx context.Context, bankID, id string) (bool, error)
}

func (e *Engine) statsOpsSource() (statsOpsSource, error) {
	if src, ok := e.store.(statsOpsSource); ok {
		return src, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support this operation"}}
}

func (e *Engine) GetAgentStats(ctx context.Context, params api.GetAgentStatsParams) (api.GetAgentStatsRes, error) {
	src, err := e.statsOpsSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	stats, err := src.BankStats(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	out := &api.BankStatsResponse{
		BankID:            params.BankID,
		TotalNodes:        stats.TotalNodes,
		TotalLinks:        stats.TotalLinks,
		TotalDocuments:    stats.TotalDocuments,
		NodesByFactType:   api.BankStatsResponseNodesByFactType(stats.NodesByFactType),
		LinksByLinkType:   api.BankStatsResponseLinksByLinkType(stats.LinksByLinkType),
		LinksByFactType:   api.BankStatsResponseLinksByFactType{},
		LinksBreakdown:    api.BankStatsResponseLinksBreakdown{},
		PendingOperations: stats.OperationsByStatus["pending"] + stats.OperationsByStatus["processing"],
		FailedOperations:  stats.OperationsByStatus["failed"],
	}
	if len(stats.OperationsByStatus) > 0 {
		out.OperationsByStatus = api.OptBankStatsResponseOperationsByStatus{
			Set:   true,
			Value: api.BankStatsResponseOperationsByStatus(stats.OperationsByStatus),
		}
	}
	if stats.LastConsolidatedAt != "" {
		out.LastConsolidatedAt = optString(stats.LastConsolidatedAt)
	}
	if stats.LastMemoryWriteAt != "" {
		out.LastMemoryWriteAt = optString(stats.LastMemoryWriteAt)
	}
	out.PendingConsolidation = api.OptInt{Set: true, Value: stats.PendingConsolidation}
	out.FailedConsolidation = api.OptInt{Set: true, Value: stats.FailedConsolidation}
	out.TotalObservations = api.OptInt{Set: true, Value: stats.TotalObservations}
	return out, nil
}

func (e *Engine) GetMemoriesTimeseries(ctx context.Context, params api.GetMemoriesTimeseriesParams) (api.GetMemoriesTimeseriesRes, error) {
	src, err := e.statsOpsSource()
	if err != nil {
		return nil, err
	}
	period := "7d"
	if params.Period.Set && params.Period.Value != "" {
		period = params.Period.Value
	}
	timeField := ""
	if params.TimeField.Set {
		timeField = params.TimeField.Value
	}
	buckets, err := src.MemoryBuckets(ctx, params.BankID, period, timeField)
	if err != nil {
		return nil, internal(err)
	}
	out := &api.MemoriesTimeseriesResponse{
		BankID:  params.BankID,
		Period:  period,
		Trunc:   "day",
		Buckets: make([]api.MemoryTimeseriesBucket, 0, len(buckets)),
	}
	if timeField != "" {
		out.TimeField = optString(timeField)
	}
	for _, b := range buckets {
		out.Buckets = append(out.Buckets, api.MemoryTimeseriesBucket{
			Time:        b.Time,
			World:       api.OptInt{Set: true, Value: b.World},
			Experience:  api.OptInt{Set: true, Value: b.Experience},
			Observation: api.OptInt{Set: true, Value: b.Observation},
		})
	}
	return out, nil
}

func (e *Engine) ListObservationScopes(ctx context.Context, params api.ListObservationScopesParams) (api.ListObservationScopesRes, error) {
	src, err := e.statsOpsSource()
	if err != nil {
		return nil, err
	}
	scopes, err := src.ObservationScopeCounts(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	limit, offset := params.Limit.Or(50), params.Offset.Or(0)
	total := len(scopes)
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}
	items := make([]api.ObservationScope, 0, end-offset)
	for _, scope := range scopes[offset:end] {
		items = append(items, api.ObservationScope{Tags: scope.Tags, Count: scope.Count})
	}
	return &api.ObservationScopesResponse{Scopes: items, Total: total, Limit: limit, Offset: offset}, nil
}

func directiveResponseFromModel(d *model.Directive) *api.DirectiveResponse {
	return &api.DirectiveResponse{
		ID:       d.ID,
		BankID:   d.BankID,
		Name:     d.Name,
		Content:  d.Content,
		Priority: api.OptInt{Set: true, Value: d.Priority},
		IsActive: api.OptBool{Set: true, Value: d.IsActive},
		Tags:     d.Tags,
	}
}

func (e *Engine) GetDirective(ctx context.Context, params api.GetDirectiveParams) (api.GetDirectiveRes, error) {
	src, err := e.statsOpsSource()
	if err != nil {
		return nil, err
	}
	d, err := src.GetDirectiveRow(ctx, params.BankID, params.DirectiveID)
	if err != nil {
		return nil, internal(err)
	}
	if d == nil {
		return nil, notFound("directive %q not found", params.DirectiveID)
	}
	return directiveResponseFromModel(d), nil
}

func (e *Engine) UpdateDirective(ctx context.Context, req *api.UpdateDirectiveRequest, params api.UpdateDirectiveParams) (api.UpdateDirectiveRes, error) {
	src, err := e.statsOpsSource()
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, badRequest("request is required")
	}
	patch := model.DirectivePatch{}
	if req.Name.Set {
		patch.Name = &req.Name.Value
	}
	if req.Content.Set {
		patch.Content = &req.Content.Value
	}
	if req.Priority.Set {
		patch.Priority = &req.Priority.Value
	}
	if req.IsActive.Set {
		patch.IsActive = &req.IsActive.Value
	}
	if req.Tags != nil {
		patch.Tags = &req.Tags
	}
	d, err := src.UpdateDirectiveRow(ctx, params.BankID, params.DirectiveID, patch)
	if err != nil {
		return nil, internal(err)
	}
	if d == nil {
		return nil, notFound("directive %q not found", params.DirectiveID)
	}
	return directiveResponseFromModel(d), nil
}

func (e *Engine) ClearObservations(ctx context.Context, params api.ClearObservationsParams) (api.ClearObservationsRes, error) {
	src, err := e.statsOpsSource()
	if err != nil {
		return nil, err
	}
	n, err := src.ClearObservationRows(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	return &api.DeleteResponse{
		Success:      true,
		Message:      api.OptString{Set: true, Value: fmt.Sprintf("deleted %d observations", n)},
		DeletedCount: api.OptInt{Set: true, Value: n},
	}, nil
}

func (e *Engine) DeleteOperation(ctx context.Context, params api.DeleteOperationParams) (api.DeleteOperationRes, error) {
	src, err := e.statsOpsSource()
	if err != nil {
		return nil, err
	}
	ok, err := src.DeleteOperationRow(ctx, params.BankID, params.OperationID)
	if err != nil {
		return nil, internal(err)
	}
	if !ok {
		return nil, &statusError{status: http.StatusConflict, body: map[string]any{"detail": "operation is missing or still pending/processing"}}
	}
	return &api.DeleteOperationResponse{Success: true, Message: "deleted", OperationID: params.OperationID}, nil
}

func (e *Engine) RetryOperation(ctx context.Context, params api.RetryOperationParams) (api.RetryOperationRes, error) {
	src, err := e.statsOpsSource()
	if err != nil {
		return nil, err
	}
	ok, err := src.RetryOperationRow(ctx, params.BankID, params.OperationID)
	if err != nil {
		return nil, internal(err)
	}
	if !ok {
		return nil, &statusError{status: http.StatusConflict, body: map[string]any{"detail": "operation is missing or not in a retryable state"}}
	}
	return &api.RetryOperationResponse{Success: true, Message: "requeued", OperationID: params.OperationID}, nil
}
