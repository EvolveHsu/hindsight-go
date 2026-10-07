package consolidate

import (
	"context"
	"strings"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// fakeSource records ApplyBatch calls.
type fakeSource struct {
	facts []model.Unit
	obs   []model.Unit
	batch *Batch
}

func (f *fakeSource) UnconsolidatedFacts(ctx context.Context, bankID string, limit int) ([]model.Unit, error) {
	out := f.facts
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	// simulate drain: second Run call returns nothing
	f.facts = nil
	return out, nil
}

func (f *fakeSource) Observations(ctx context.Context, bankID string, limit int) ([]model.Unit, error) {
	return f.obs, nil
}

func (f *fakeSource) ApplyBatch(ctx context.Context, bankID string, b Batch) error {
	f.batch = &b
	return nil
}

// scripted serves raw model content strings in order.
type scripted struct {
	responses []string
	gotUser   string
	calls     int
}

func (s *scripted) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.gotUser = req.Messages[len(req.Messages)-1].Content
	idx := s.calls
	if idx >= len(s.responses) {
		idx = len(s.responses) - 1
	}
	s.calls++
	return &llm.ChatResponse{
		Choices: []llm.Choice{{Message: llm.Message{Content: s.responses[idx]}}},
		Usage:   llm.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}, nil
}

const validJSON = `{"creates":[{"text":"Alice lives in Berlin","source_fact_ids":["f1","f2"],"reason":"merge"}],"updates":[{"text":"Alice owns 3 cats","observation_id":"o1","source_fact_ids":["f3"]}],"deletes":[{"observation_id":"o2","reason":"restated"}]}`

func TestRunMergesBatch(t *testing.T) {
	src := &fakeSource{
		facts: []model.Unit{
			{ID: "f1", FactType: model.FactExperience, Text: "Alice moved to Berlin"},
			{ID: "f2", FactType: model.FactExperience, Text: "Alice loves it there"},
			{ID: "f3", FactType: model.FactExperience, Text: "Alice got a third cat"},
		},
		obs: []model.Unit{
			{ID: "o1", FactType: model.FactObservation, Text: "Alice owns 2 cats"},
			{ID: "o2", FactType: model.FactObservation, Text: "duplicate observation"},
		},
	}
	prov := &scripted{responses: []string{validJSON}}
	c := &Consolidator{Provider: prov, Cfg: DefaultConfig()}

	stats, err := c.Run(context.Background(), src, "b")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Batches != 1 || stats.Creates != 1 || stats.Updates != 1 || stats.Deletes != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.DocsSeen != 3 || stats.UsageIn != 100 || stats.UsageOut != 50 {
		t.Errorf("stats detail = %+v", stats)
	}

	b := src.batch
	if b == nil {
		t.Fatal("ApplyBatch was not called")
	}
	if len(b.Creates) != 1 || b.Creates[0].Text != "Alice lives in Berlin" {
		t.Errorf("creates = %+v", b.Creates)
	}
	if len(b.Updates) != 1 || b.Updates[0].ObservationID != "o1" {
		t.Errorf("updates = %+v", b.Updates)
	}
	if len(b.Deletes) != 1 || b.Deletes[0].ObservationID != "o2" {
		t.Errorf("deletes = %+v", b.Deletes)
	}
	// the input carried the fact ids so the model could reference them
	if !strings.Contains(prov.gotUser, "[f1]") || !strings.Contains(prov.gotUser, "[f3]") {
		t.Errorf("user prompt missing fact ids:\n%s", prov.gotUser)
	}
	// and the observation context
	if !strings.Contains(prov.gotUser, `"o1"`) {
		t.Errorf("user prompt missing observation ids:\n%s", prov.gotUser)
	}
}

func TestRunFenceStripped(t *testing.T) {
	src := &fakeSource{
		facts: []model.Unit{{ID: "f1", FactType: model.FactWorld, Text: "x"}},
	}
	fenced := "```json\n" + `{"creates":[{"text":"t","source_fact_ids":["f1"]}]}` + "\n```"
	prov := &scripted{responses: []string{fenced}}
	c := &Consolidator{Provider: prov, Cfg: DefaultConfig()}
	stats, err := c.Run(context.Background(), src, "b")
	if err != nil {
		t.Fatalf("fenced JSON rejected: %v", err)
	}
	if stats.Creates != 1 {
		t.Errorf("creates = %d", stats.Creates)
	}
}

func TestRunMalformedJSONErrors(t *testing.T) {
	src := &fakeSource{facts: []model.Unit{{ID: "f1", FactType: model.FactWorld, Text: "x"}}}
	prov := &scripted{responses: []string{"not json at all"}}
	c := &Consolidator{Provider: prov, Cfg: DefaultConfig()}
	if _, err := c.Run(context.Background(), src, "b"); err == nil {
		t.Fatal("expected malformed error")
	}
}

func TestRunEmptySourceIsNoop(t *testing.T) {
	src := &fakeSource{}
	prov := &scripted{responses: []string{validJSON}}
	c := &Consolidator{Provider: prov, Cfg: DefaultConfig()}
	stats, err := c.Run(context.Background(), src, "b")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Batches != 0 || stats.DocsSeen != 0 {
		t.Errorf("stats = %+v, want no work", stats)
	}
}
