package memory

import (
	"context"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/embeddings"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

type staticEmbedder map[string][]float32

func (s staticEmbedder) Embed(_ context.Context, texts []string, _ embeddings.InputType) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = append([]float32(nil), s[text]...)
	}
	return out, nil
}

func TestRecallUsesInMemoryEmbeddings(t *testing.T) {
	ctx := context.Background()
	s := New(WithEmbedder(staticEmbedder{
		"alpha fact":  {1, 0},
		"beta fact":   {0, 1},
		"alpha query": {1, 0},
	}))
	if _, err := s.EnsureBank(ctx, "b", "b", ""); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}
	if _, err := s.Retain(ctx, "b", []model.RetainItem{
		{Content: "alpha fact", FactType: model.FactWorld},
		{Content: "beta fact", FactType: model.FactWorld},
	}); err != nil {
		t.Fatalf("retain: %v", err)
	}
	hits, err := s.Recall(ctx, "b", model.RecallOptions{Query: "alpha query"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 || hits[0].Unit.Text != "alpha fact" {
		t.Fatalf("hits = %+v, want alpha fact first", hits)
	}
	if hits[0].Semantic < 0.99 {
		t.Fatalf("semantic score = %v, want ~1", hits[0].Semantic)
	}
}

func TestUpdateMemoryReembeds(t *testing.T) {
	ctx := context.Background()
	s := New(WithEmbedder(staticEmbedder{
		"before": {1, 0},
		"after":  {0, 1},
	}))
	if _, err := s.EnsureBank(ctx, "b", "b", ""); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}
	if _, err := s.Retain(ctx, "b", []model.RetainItem{
		{Content: "before", FactType: model.FactWorld, DocumentID: "doc"},
	}); err != nil {
		t.Fatalf("retain: %v", err)
	}
	id := model.UnitID("b", "doc", "before")
	updated, err := s.UpdateMemory(ctx, "b", id, func(u *model.Unit) { u.Text = "after" })
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated == nil || len(updated.Embedding) != 2 || updated.Embedding[1] != 1 {
		t.Fatalf("updated embedding = %v, want [0 1]", updated)
	}
}
