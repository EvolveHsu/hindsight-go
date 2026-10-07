package server

import (
	"context"
	"net/http"
	"strings"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/model"
	"github.com/EvolveHsu/hindsight-go/internal/reflect"
)

// reflectSource adapts a DirectiveSource to the reflect.MemorySource slice.
type reflectSource struct {
	store DirectiveSource
}

var _ reflect.MemorySource = (*reflectSource)(nil)

func (r *reflectSource) Recall(ctx context.Context, bankID string, opt model.RecallOptions) ([]model.RecallHit, error) {
	return r.store.Recall(ctx, bankID, opt)
}

func (r *reflectSource) ListDirectives(ctx context.Context, bankID string) ([]reflect.Directive, error) {
	dirs, err := r.store.ListDirectives(ctx, bankID)
	if err != nil {
		return nil, err
	}
	out := make([]reflect.Directive, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, reflect.Directive{ID: d.ID, Name: d.Name, Content: d.Content})
	}
	return out, nil
}

func (r *reflectSource) BankMission(ctx context.Context, bankID string) (string, error) {
	return r.store.BankMission(ctx, bankID)
}

// reflectProvider is the pluggable model provider. Tests inject a fake; main
// injects an llm.Client built from environment variables.
type reflectProvider interface {
	Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error)
}

// SetReflectProvider wires the model provider used by Reflect. Without one,
// Reflect answers 501 with instructions on which env vars to set.
func (e *Engine) SetReflectProvider(p reflectProvider) { e.provider = p }

// Reflect implements reflect (POST /banks/{bank_id}/reflect).
func (e *Engine) Reflect(ctx context.Context, req *api.ReflectRequest, params api.ReflectParams) (api.ReflectRes, error) {
	if req == nil || strings.TrimSpace(req.Query) == "" {
		return nil, badRequest("query is required")
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	if e.provider == nil {
		return nil, &statusError{
			status: http.StatusNotImplemented,
			body: map[string]any{
				"detail": "no LLM provider configured; set HINDSIGHT_GO_LLM_BASE_URL / HINDSIGHT_GO_LLM_API_KEY / HINDSIGHT_GO_LLM_MODEL",
			},
		}
	}

	agent := &reflect.Agent{Provider: e.provider, Cfg: reflect.DefaultConfig()}
	if req.MaxTokens.Set && req.MaxTokens.Value > 0 {
		agent.Cfg.MaxTokens = req.MaxTokens.Value
	}

	ds, dsOK := e.store.(DirectiveSource)
	if !dsOK {
		return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support directives (reflect requires them)"}}
	}
	res, err := agent.Run(ctx, &reflectSource{store: ds}, params.BankID, req.Query)
	if err != nil {
		return nil, internal(err)
	}

	out := &api.ReflectResponse{Text: res.Text}
	out.Usage = api.OptTokenUsage{
		Set: true,
		Value: api.TokenUsage{
			InputTokens:  api.OptInt{Set: true, Value: res.Usage.PromptTokens},
			OutputTokens: api.OptInt{Set: true, Value: res.Usage.CompletionTokens},
			TotalTokens:  api.OptInt{Set: true, Value: res.Usage.TotalTokens},
		},
	}
	if req.Include.Set && req.Include.Value.Facts != nil {
		basedOn := api.ReflectBasedOn{}
		for _, m := range res.Evidence.Memories {
			basedOn.Memories = append(basedOn.Memories, reflectFactFromUnit(m))
		}
		for _, m := range res.Evidence.MentalModels {
			basedOn.MentalModels = append(basedOn.MentalModels, reflectMentalModelFromUnit(m))
		}
		for _, d := range res.Evidence.Directives {
			basedOn.Directives = append(basedOn.Directives, api.ReflectDirective{
				ID: d.ID, Name: d.Name, Content: d.Content,
			})
		}
		out.BasedOn = api.OptReflectBasedOn{Set: true, Value: basedOn}
	}
	return out, nil
}

// reflectFactFromUnit converts a stored unit into the wire fact shape.
func reflectFactFromUnit(u model.Unit) api.ReflectFact {
	return api.ReflectFact{
		ID:          optString(u.ID),
		Text:        u.Text,
		Type:        optString(string(u.FactType)),
		Context:     optString(u.Context),
		DocumentID:  optString(u.DocumentID),
		ChunkID:     optString(u.ChunkID),
		MentionedAt: optString(u.MentionedAt),
		Tags:        u.Tags,
	}
}

// reflectMentalModelFromUnit converts an observation unit into the wire shape.
func reflectMentalModelFromUnit(u model.Unit) api.ReflectMentalModel {
	return api.ReflectMentalModel{
		ID:   u.ID,
		Text: u.Text,
	}
}

// CreateDirective implements create_directive.
func (e *Engine) CreateDirective(ctx context.Context, req *api.CreateDirectiveRequest, params api.CreateDirectiveParams) (api.CreateDirectiveRes, error) {
	if req == nil || strings.TrimSpace(req.Content) == "" {
		return nil, badRequest("content is required")
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	ds, ok := e.store.(DirectiveSource)
	if !ok {
		return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support directives"}}
	}
	d, err := ds.CreateDirective(ctx, params.BankID, req.Name, req.Content)
	if err != nil {
		return nil, internal(err)
	}
	return directiveResponse(d), nil
}

// ListDirectives implements list_directives.
func (e *Engine) ListDirectives(ctx context.Context, params api.ListDirectivesParams) (api.ListDirectivesRes, error) {
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	ds, ok := e.store.(DirectiveSource)
	if !ok {
		return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support directives"}}
	}
	dirs, err := ds.ListDirectives(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	items := make([]api.DirectiveResponse, 0, len(dirs))
	for _, d := range dirs {
		items = append(items, *directiveResponse(&d))
	}
	return &api.DirectiveListResponse{Items: items, Total: len(items), Limit: len(items), Offset: 0}, nil
}

func directiveResponse(d *model.Directive) *api.DirectiveResponse {
	return &api.DirectiveResponse{
		ID:       d.ID,
		Name:     d.Name,
		Content:  d.Content,
		IsActive: api.OptBool{Set: true, Value: true},
	}
}
