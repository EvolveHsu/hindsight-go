package server

import (
	"context"
	"net/http"
	"os"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/consolidate"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// ConsolidatorSource is the storage slice consolidation needs. Both backends
// implement it.
type ConsolidatorSource interface {
	Store
	UnconsolidatedFacts(ctx context.Context, bankID string, limit int) ([]model.Unit, error)
	Observations(ctx context.Context, bankID string, limit int) ([]model.Unit, error)
	ApplyBatch(ctx context.Context, bankID string, b consolidate.Batch) error
}

// SetConsolidationProvider wires the model used for observation synthesis.
func (e *Engine) SetConsolidationProvider(p consolidate.Provider) { e.consolProvider = p }

// TriggerConsolidation implements trigger_consolidation.
//
// Upstream runs consolidation as an async operation; lite executes it
// synchronously and returns a synthetic operation id (the contract keeps its
// shape so async can slot in later without breaking clients).
func (e *Engine) TriggerConsolidation(ctx context.Context, req api.OptConsolidationRequest, params api.TriggerConsolidationParams) (api.TriggerConsolidationRes, error) {
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	if e.consolProvider == nil {
		return nil, &statusError{
			status: http.StatusNotImplemented,
			body: map[string]any{
				"detail": "no LLM provider configured; set HINDSIGHT_GO_LLM_BASE_URL / HINDSIGHT_GO_LLM_API_KEY / HINDSIGHT_GO_LLM_MODEL",
			},
		}
	}
	src, ok := e.store.(ConsolidatorSource)
	if !ok {
		return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support consolidation"}}
	}

	cfg := consolidate.DefaultConfig()
	if m := os.Getenv("HINDSIGHT_GO_LLM_MODEL"); m != "" {
		cfg.Model = m
	}
	cons := &consolidate.Consolidator{Provider: e.consolProvider, Cfg: cfg}
	stats, err := cons.Run(ctx, src, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	_ = stats // surfaced via logs; the contract only wants an operation id

	return &api.ConsolidationResponse{
		OperationID:  "consol-sync-" + params.BankID,
		Deduplicated: api.OptBool{Set: true, Value: false},
	}, nil
}

// RecoverConsolidation implements recover_consolidation. Lite has no failed
// consolidation state to recover (batches commit atomically), so it reports
// zero retried - the honest no-op.
func (e *Engine) RecoverConsolidation(ctx context.Context, params api.RecoverConsolidationParams) (api.RecoverConsolidationRes, error) {
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	return &api.RecoverConsolidationResponse{RetriedCount: 0}, nil
}
