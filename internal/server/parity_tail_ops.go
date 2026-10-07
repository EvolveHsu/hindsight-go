package server

import (
	"context"
	"encoding/json"
	"net/http"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// DirectiveDeleteSource is the storage slice delete_directive needs. It is a
// separate seam so the reflect surface does not grow for one endpoint.
type DirectiveDeleteSource interface {
	DeleteDirective(ctx context.Context, bankID, directiveID string) (bool, error)
}

func (e *Engine) directiveDeleteSource() (DirectiveDeleteSource, error) {
	if s, ok := e.store.(DirectiveDeleteSource); ok {
		return s, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support deleting directives"}}
}

// DeleteDirective implements delete_directive
// (DELETE /banks/{bank_id}/directives/{directive_id}).
func (e *Engine) DeleteDirective(ctx context.Context, params api.DeleteDirectiveParams) (api.DeleteDirectiveRes, error) {
	src, err := e.directiveDeleteSource()
	if err != nil {
		return nil, err
	}
	removed, err := src.DeleteDirective(ctx, params.BankID, params.DirectiveID)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	if !removed {
		return nil, notFound("Directive '%s' not found", params.DirectiveID)
	}
	raw, err := json.Marshal(map[string]any{"status": "deleted"})
	if err != nil {
		return nil, internal(err)
	}
	out := api.DeleteDirectiveOKApplicationJSON(raw)
	return &out, nil
}

// ClearMentalModel implements clear_mental_model
// (POST /banks/{bank_id}/mental-models/{mental_model_id}/clear): content is
// dropped so the next refresh re-synthesises from scratch.
func (e *Engine) ClearMentalModel(ctx context.Context, params api.ClearMentalModelParams) (api.ClearMentalModelRes, error) {
	mms, err := e.mmStore()
	if err != nil {
		return nil, err
	}
	m, err := mms.UpdateMentalModel(ctx, params.BankID, params.MentalModelID, func(m *model.MentalModel) {
		m.Content = ""
		m.LastRefreshed = ""
	})
	if err != nil {
		return nil, internal(err)
	}
	if m == nil {
		return nil, notFound("Mental model '%s' not found", params.MentalModelID)
	}
	resp := mentalModelResponse(m)
	return &resp, nil
}

// CancelOperation implements cancel_operation
// (DELETE /banks/{bank_id}/operations/{operation_id}).
//
// Upstream flips a pending/processing row to 'cancelled'. The lite build runs
// every operation inline, so by the time a caller holds an operation id the
// work is already terminal — the same state upstream answers 409 for. It
// answers that instead of inventing a cancellation it cannot perform.
func (e *Engine) CancelOperation(ctx context.Context, params api.CancelOperationParams) (api.CancelOperationRes, error) {
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	return nil, conflict("Operation %s cannot be cancelled: hindsight-go runs operations synchronously, "+
		"so only 'pending' and 'processing' operations can be cancelled and this build has none",
		params.OperationID)
}
