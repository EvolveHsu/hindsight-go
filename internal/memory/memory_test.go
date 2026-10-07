package memory

import (
	"context"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

func TestRetainAndRecallDirect(t *testing.T) {
	s := New()
	s.EnsureBank(context.Background(), "b", "b", "")

	res, err := s.Retain(context.Background(), "b", []model.RetainItem{
		{Content: "Alice moved to Berlin in 2023.", FactType: model.FactExperience},
		{Content: "Alice plays the cello.", FactType: model.FactExperience, Tags: []string{"music"}},
	})
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if res.UnitsStored != 2 {
		t.Fatalf("units stored = %d, want 2", res.UnitsStored)
	}

	hits, err := s.Recall(context.Background(), "b", model.RecallOptions{Query: "Where does Alice live?"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("recall returned no hits")
	}
	found := false
	for _, h := range hits {
		if h.Unit.Text == "Alice moved to Berlin in 2023." {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the Berlin fact among hits")
	}
}

func TestRecallRespectsTypeFilter(t *testing.T) {
	s := New()
	s.EnsureBank(context.Background(), "b", "b", "")
	_, err := s.Retain(context.Background(), "b", []model.RetainItem{
		{Content: "plain world fact about cats", FactType: model.FactWorld},
	})
	if err != nil {
		t.Fatalf("retain: %v", err)
	}

	hits, err := s.Recall(context.Background(), "b", model.RecallOptions{
		Query: "cats", Types: []model.FactType{model.FactExperience},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("type filter leaked: got %d hits", len(hits))
	}
}

func TestDeterministicUnitIDs(t *testing.T) {
	a := model.UnitID("bank", "doc", "same content")
	b := model.UnitID("bank", "doc", "same content")
	c := model.UnitID("bank", "doc", "different")
	if a != b {
		t.Errorf("same input produced different ids")
	}
	if a == c {
		t.Errorf("different content collided")
	}
}

func TestRetainSameDocumentIsNoOp(t *testing.T) {
	s := New()
	s.EnsureBank(context.Background(), "b", "b", "")
	items := []model.RetainItem{{Content: "only fact", DocumentID: "d1", FactType: model.FactWorld}}

	if _, err := s.Retain(context.Background(), "b", items); err != nil {
		t.Fatalf("first retain: %v", err)
	}
	hits1, _ := s.Recall(context.Background(), "b", model.RecallOptions{Query: "only fact"})
	if len(hits1) != 1 {
		t.Fatalf("after first retain, hits = %d, want 1", len(hits1))
	}

	if _, err := s.Retain(context.Background(), "b", items); err != nil {
		t.Fatalf("replay retain: %v", err)
	}
	hits2, _ := s.Recall(context.Background(), "b", model.RecallOptions{Query: "only fact"})
	if len(hits2) != 1 {
		t.Errorf("after replay, hits = %d, want 1 (duplicate written)", len(hits2))
	}
}
