package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/reflect"
)

type historySource interface {
	AuditLogsJSON(ctx context.Context, bankID, action, transport, startDate, endDate string, limit, offset int) ([]byte, error)
	AuditStatsJSON(ctx context.Context, bankID, action, period string) ([]byte, error)
	LLMRequestsJSON(ctx context.Context, bankID, status, operation, scope, provider, traceID, startDate, endDate string, limit, offset int) ([]byte, error)
	LLMStatsJSON(ctx context.Context, bankID, operation, period string) ([]byte, error)
	MentalModelHistoryJSON(ctx context.Context, bankID, modelID string) ([]byte, error)
	ObservationHistoryJSON(ctx context.Context, bankID, observationID string) ([]byte, error)
}

func (e *Engine) historySource() (historySource, error) {
	if src, ok := e.store.(historySource); ok {
		return src, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support history"}}
}

func (e *Engine) ListAuditLogs(ctx context.Context, params api.ListAuditLogsParams) (api.ListAuditLogsRes, error) {
	src, err := e.historySource()
	if err != nil {
		return nil, err
	}
	raw, err := src.AuditLogsJSON(ctx, params.BankID, optValue(params.Action), optValue(params.Transport),
		optValue(params.StartDate), optValue(params.EndDate), params.Limit.Or(50), params.Offset.Or(0))
	if err != nil {
		return nil, internal(err)
	}
	var out api.AuditLogListResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, internal(err)
	}
	return &out, nil
}

func (e *Engine) AuditLogStats(ctx context.Context, params api.AuditLogStatsParams) (api.AuditLogStatsRes, error) {
	src, err := e.historySource()
	if err != nil {
		return nil, err
	}
	period := "7d"
	if params.Period.Set && params.Period.Value != "" {
		period = params.Period.Value
	}
	raw, err := src.AuditStatsJSON(ctx, params.BankID, optValue(params.Action), period)
	if err != nil {
		return nil, internal(err)
	}
	var out api.AuditLogStatsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, internal(err)
	}
	return &out, nil
}

func (e *Engine) ListLlmRequests(ctx context.Context, params api.ListLlmRequestsParams) (api.ListLlmRequestsRes, error) {
	src, err := e.historySource()
	if err != nil {
		return nil, err
	}
	raw, err := src.LLMRequestsJSON(ctx, params.BankID, optValue(params.Status), optValue(params.Operation),
		optValue(params.Scope), optValue(params.Provider), optValue(params.TraceID),
		optValue(params.StartDate), optValue(params.EndDate), params.Limit.Or(50), params.Offset.Or(0))
	if err != nil {
		return nil, internal(err)
	}
	var out api.LLMRequestListResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, internal(err)
	}
	return &out, nil
}

func (e *Engine) LlmRequestStats(ctx context.Context, params api.LlmRequestStatsParams) (api.LlmRequestStatsRes, error) {
	src, err := e.historySource()
	if err != nil {
		return nil, err
	}
	period := "7d"
	if params.Period.Set && params.Period.Value != "" {
		period = params.Period.Value
	}
	raw, err := src.LLMStatsJSON(ctx, params.BankID, optValue(params.Operation), period)
	if err != nil {
		return nil, internal(err)
	}
	var out api.LLMRequestStatsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, internal(err)
	}
	return &out, nil
}

func (e *Engine) GetMentalModelHistory(ctx context.Context, params api.GetMentalModelHistoryParams) (api.GetMentalModelHistoryRes, error) {
	src, err := e.historySource()
	if err != nil {
		return nil, err
	}
	raw, err := src.MentalModelHistoryJSON(ctx, params.BankID, params.MentalModelID)
	if err != nil {
		return nil, internal(err)
	}
	res := api.GetMentalModelHistoryOKApplicationJSON(raw)
	return &res, nil
}

func (e *Engine) GetObservationHistory(ctx context.Context, params api.GetObservationHistoryParams) (api.GetObservationHistoryRes, error) {
	src, err := e.historySource()
	if err != nil {
		return nil, err
	}
	raw, err := src.ObservationHistoryJSON(ctx, params.BankID, params.MemoryID)
	if err != nil {
		return nil, internal(err)
	}
	res := api.GetObservationHistoryOKApplicationJSON(raw)
	return &res, nil
}

func (e *Engine) DryRunExtractMemories(ctx context.Context, req *api.DryRunExtractRequest, params api.DryRunExtractMemoriesParams) (api.DryRunExtractMemoriesRes, error) {
	if req == nil {
		return nil, badRequest("request is required")
	}
	text, err := contentText(req.Content)
	if err != nil {
		return nil, badRequest("content: %v", err)
	}
	paragraphs := splitParagraphs(text)
	chunks := make([]api.ExtractionChunk, 0, len(paragraphs))
	facts := make([]api.ExtractedFact, 0, len(paragraphs))
	for i, paragraph := range paragraphs {
		if strings.TrimSpace(paragraph) == "" {
			continue
		}
		chunks = append(chunks, api.ExtractionChunk{Text: paragraph, FactCount: 1})
		facts = append(facts, api.ExtractedFact{
			Text:        paragraph,
			FactType:    "experience",
			Entities:    []string{},
			Attachments: []api.ExtractedFactAttachment{},
			ChunkIndex:  api.OptInt{Set: true, Value: i},
		})
	}
	return &api.DryRunExtractionResult{Chunks: chunks, Facts: facts}, nil
}

func splitParagraphs(text string) []string {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n")
	out := make([]string, 0, len(raw))
	for _, part := range raw {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return []string{strings.TrimSpace(text)}
	}
	return out
}

func (e *Engine) DryRunRefreshMentalModel(ctx context.Context, params api.DryRunRefreshMentalModelParams) (api.DryRunRefreshMentalModelRes, error) {
	mms, err := e.mmStore()
	if err != nil {
		return nil, err
	}
	model, err := mms.GetMentalModel(ctx, params.BankID, params.MentalModelID)
	if err != nil {
		return nil, internal(err)
	}
	if model == nil {
		return nil, notFound("mental model %q not found", params.MentalModelID)
	}
	candidate := model.Content
	if e.provider != nil {
		agent := &reflect.Agent{Provider: e.provider, Cfg: reflect.DefaultConfig()}
		res, runErr := agent.Run(ctx, &reflectSource{store: e.store.(DirectiveSource)}, params.BankID, model.SourceQuery)
		if runErr == nil && res != nil && res.Text != "" {
			candidate = res.Text
		}
	}
	return &api.MentalModelDryRunRefreshResult{
		MentalModelID:    model.ID,
		Name:             model.Name,
		RequestedMode:    api.MentalModelDryRunRefreshResultRequestedModeFull,
		EffectiveMode:    api.MentalModelDryRunRefreshResultEffectiveModeFull,
		Outcome:          api.MentalModelDryRunRefreshResultOutcomeContentUnchanged,
		CurrentContent:   model.Content,
		CandidateContent: candidate,
		PreviewContent:   candidate,
		Warnings:         []string{},
	}, nil
}

func (e *Engine) PreviewPrompt(ctx context.Context, req *api.PromptPreviewRequest, params api.PreviewPromptParams) (api.PreviewPromptRes, error) {
	operation := "retain"
	if req != nil && req.Operation.Set {
		operation = string(req.Operation.Value)
	}
	return &api.PromptPreviewResponse{
		Messages:      []api.PromptMessageModel{},
		Strategies:    []string{},
		RunSettings:   []api.RunSettingModel{},
		SkippedReason: api.OptString{Set: true, Value: "prompt preview for " + operation + " is not configured in this build"},
	}, nil
}

func (e *Engine) PreviewConsolidationStrategies(ctx context.Context, req *api.ConsolidationStrategiesPreviewRequest, params api.PreviewConsolidationStrategiesParams) (api.PreviewConsolidationStrategiesRes, error) {
	if req == nil {
		return nil, badRequest("request is required")
	}
	return &api.ConsolidationStrategiesPreview{
		Strategies:    []api.StrategyPreview{},
		Default:       api.DefaultScopesPreview{Samples: []api.StrategyScopePreview{}},
		ScopesScanned: 0,
		Complete:      true,
	}, nil
}

func (e *Engine) TestBankLlm(ctx context.Context, params api.TestBankLlmParams) (api.TestBankLlmRes, error) {
	ops := []api.LlmOperationHealth{
		{Operation: api.LlmOperationHealthOperationRetain, Ok: false, Status: api.LlmOperationHealthStatusNotConfigured},
		{Operation: api.LlmOperationHealthOperationConsolidation, Ok: false, Status: api.LlmOperationHealthStatusNotConfigured},
		{Operation: api.LlmOperationHealthOperationReflect, Ok: false, Status: api.LlmOperationHealthStatusNotConfigured},
	}
	if e.provider != nil {
		start := time.Now()
		_, err := e.provider.Chat(ctx, llm.ChatRequest{
			Messages:  []llm.Message{{Role: "user", Content: "Reply with OK."}},
			MaxTokens: 8,
		})
		status := api.LlmOperationHealthStatusConnected
		ok := err == nil
		if err != nil {
			status = api.LlmOperationHealthStatusUnreachable
		}
		latency := float64(time.Since(start).Milliseconds())
		for i := range ops {
			ops[i].Ok = ok
			ops[i].Status = status
			ops[i].LatencyMs = api.OptFloat64{Set: true, Value: latency}
		}
	}
	return &api.BankLlmHealthResponse{BankID: params.BankID, Operations: ops}, nil
}

func optValue(v api.OptString) string {
	if v.Set {
		return v.Value
	}
	return ""
}

var _ = http.StatusOK
