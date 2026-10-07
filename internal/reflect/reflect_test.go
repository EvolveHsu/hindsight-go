package reflect

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// fakeSource is a deterministic MemorySource.
type fakeSource struct {
	hits       []model.RecallHit
	directives []Directive
	mission    string
}

func (f *fakeSource) Recall(ctx context.Context, bankID string, opt model.RecallOptions) ([]model.RecallHit, error) {
	var out []model.RecallHit
	for _, h := range f.hits {
		for _, want := range opt.Types {
			if h.Unit.FactType == want {
				out = append(out, h)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeSource) ListDirectives(ctx context.Context, bankID string) ([]Directive, error) {
	return f.directives, nil
}

func (f *fakeSource) BankMission(ctx context.Context, bankID string) (string, error) {
	return f.mission, nil
}

// scriptedProvider returns queued responses in order, verifying the request
// carries the tool definitions.
type scriptedProvider struct {
	responses []llm.ChatResponse
	calls     int
	err       error
}

func (p *scriptedProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if p.err != nil {
		return nil, p.err
	}
	if p.calls >= len(p.responses) {
		return nil, errors.New("script exhausted")
	}
	if len(req.Tools) == 0 {
		return nil, errors.New("provider expected tools on the request")
	}
	resp := p.responses[p.calls]
	p.calls++
	return &resp, nil
}

func toolCallReq(id, name, args string) llm.Message {
	return llm.Message{
		Role: llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{
			ID:       id,
			Type:     "function",
			Function: llm.FunctionCall{Name: name, Arguments: args},
		}},
	}
}

func TestAgentCallsRecallThenDone(t *testing.T) {
	src := &fakeSource{
		hits: []model.RecallHit{
			{Unit: &model.Unit{ID: "u1", FactType: model.FactExperience, Text: "Alice moved to Berlin in 2023."}},
			{Unit: &model.Unit{ID: "u2", FactType: model.FactExperience, Text: "Alice plays the cello."}},
		},
		mission: "be helpful",
	}
	prov := &scriptedProvider{responses: []llm.ChatResponse{
		{Choices: []llm.Choice{{Message: toolCallReq("t1", ToolRecall, `{"query":"Where does Alice live?"}`)}}},
		{Choices: []llm.Choice{{Message: toolCallReq("t2", ToolDone, `{"answer":"Alice lives in Berlin.","memory_ids":["u1"]}`)}}},
	}}

	agent := &Agent{Provider: prov, Cfg: DefaultConfig()}
	res, err := agent.Run(context.Background(), src, "b", "Where does Alice live?")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Text != "Alice lives in Berlin." {
		t.Errorf("text = %q", res.Text)
	}
	if len(res.Evidence.Memories) != 2 {
		t.Errorf("evidence memories = %d, want 2", len(res.Evidence.Memories))
	}
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2", res.Turns)
	}
	if prov.calls != 2 {
		t.Errorf("provider calls = %d, want 2", prov.calls)
	}
}

func TestAgentDirectivesInSystemPrompt(t *testing.T) {
	var gotSystem string
	src := &fakeSource{directives: []Directive{{ID: "d1", Name: "Always French", Content: "Reply in French"}}}
	prov := &scriptedProvider{responses: []llm.ChatResponse{
		{Choices: []llm.Choice{{Message: toolCallReq("t1", ToolDone, `{"answer":"Bonjour"}`)}}},
	}}
	agent := &Agent{
		Provider: &capturingProvider{inner: prov, onReq: func(r llm.ChatRequest) { gotSystem = r.Messages[0].Content }},
		Cfg:      DefaultConfig(),
	}
	res, err := agent.Run(context.Background(), src, "b", "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Text != "Bonjour" {
		t.Errorf("text = %q", res.Text)
	}
	if !strings.Contains(gotSystem, "Always French") || !strings.Contains(gotSystem, "DIRECTIVES") {
		t.Errorf("system prompt missing directives:\n%s", gotSystem)
	}
	if !strings.Contains(gotSystem, "be helpful") == false && src.mission != "" {
		// mission is empty on this source; nothing to assert
		_ = gotSystem
	}
}

type capturingProvider struct {
	inner Provider
	onReq func(llm.ChatRequest)
}

func (c *capturingProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	c.onReq(req)
	return c.inner.Chat(ctx, req)
}

func TestAgentMaxTurnsExhausted(t *testing.T) {
	src := &fakeSource{}
	// always call recall, never done
	prov := &scriptedProvider{}
	for i := 0; i < 20; i++ {
		prov.responses = append(prov.responses, llm.ChatResponse{
			Choices: []llm.Choice{{Message: toolCallReq("t", ToolRecall, `{"query":"x"}`)}},
		})
	}
	agent := &Agent{Provider: prov, Cfg: Config{MaxTurns: 3, Budget: 5}}
	_, err := agent.Run(context.Background(), src, "b", "q")
	if err == nil || !strings.Contains(err.Error(), "max turns") {
		t.Errorf("expected max-turns error, got %v", err)
	}
}

func TestAgentUnknownToolFeedsErrorBack(t *testing.T) {
	src := &fakeSource{}
	prov := &scriptedProvider{responses: []llm.ChatResponse{
		{Choices: []llm.Choice{{Message: toolCallReq("t1", "bogus_tool", `{}`)}}},
		{Choices: []llm.Choice{{Message: toolCallReq("t2", ToolDone, `{"answer":"recovered"}`)}}},
	}}
	agent := &Agent{Provider: prov, Cfg: DefaultConfig()}
	res, err := agent.Run(context.Background(), src, "b", "q")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("text = %q", res.Text)
	}
}

func TestAgentTypeFilteredObservationSearch(t *testing.T) {
	src := &fakeSource{
		hits: []model.RecallHit{
			{Unit: &model.Unit{ID: "o1", FactType: model.FactObservation, Text: "obs: Alice likes Berlin"}},
			{Unit: &model.Unit{ID: "w1", FactType: model.FactWorld, Text: "raw: Alice lives in Berlin"}},
		},
	}
	prov := &scriptedProvider{responses: []llm.ChatResponse{
		{Choices: []llm.Choice{{Message: toolCallReq("t1", ToolSearchObservations, `{"query":"Alice"}`)}}},
		{Choices: []llm.Choice{{Message: toolCallReq("t2", ToolDone, `{"answer":"ok"}`)}}},
	}}
	agent := &Agent{Provider: prov, Cfg: DefaultConfig()}
	res, err := agent.Run(context.Background(), src, "b", "q")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// observations tool must only see the observation fact
	if len(res.Evidence.MentalModels) != 1 || res.Evidence.MentalModels[0].ID != "o1" {
		t.Errorf("observations evidence = %+v", res.Evidence.MentalModels)
	}
	if len(res.Evidence.Memories) != 0 {
		t.Errorf("observations tool leaked raw facts: %+v", res.Evidence.Memories)
	}
}
