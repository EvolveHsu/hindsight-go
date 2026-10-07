package storepg

import (
	"context"
	"os"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// testDSN comes from TEST_DATABASE_URL. When unset, the suite skips: the unit
// coverage for the seam lives in internal/memory; these tests prove the SQL
// against a real PostgreSQL.
func testDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
	// Fall back to an embedded PostgreSQL so the SQL is actually exercised on
	// every dev machine without Docker or a local install.
	return startEmbedded(t)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestEnsureBankAndGetBank(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.EnsureBank(ctx, "it-bank", "it-bank", "integration"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	b, err := s.GetBank(ctx, "it-bank")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if b == nil || b.Mission != "integration" {
		t.Fatalf("bank = %+v", b)
	}
}

func TestRetainAndRecall(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const bank = "it-retain"

	if _, err := s.EnsureBank(ctx, bank, bank, ""); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	res, err := s.Retain(ctx, bank, []model.RetainItem{
		{Content: "The quarterly report shipped on Friday.", FactType: model.FactExperience},
		{Content: "Alice owns the cello.", FactType: model.FactExperience, Tags: []string{"music"}},
	})
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if res.UnitsStored != 2 {
		t.Fatalf("units stored = %d, want 2", res.UnitsStored)
	}

	hits, err := s.Recall(ctx, bank, model.RecallOptions{Query: "quarterly report"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("recall returned no hits")
	}
	found := false
	for _, h := range hits {
		if h.Unit.Text == "The quarterly report shipped on Friday." {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the report fact among hits")
	}

	// replay is a no-op
	if _, err := s.Retain(ctx, bank, []model.RetainItem{
		{Content: "The quarterly report shipped on Friday.", FactType: model.FactExperience},
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	hits2, err := s.Recall(ctx, bank, model.RecallOptions{Query: "quarterly report"})
	if err != nil {
		t.Fatalf("recall 2: %v", err)
	}
	n := 0
	for _, h := range hits2 {
		if h.Unit.Text == "The quarterly report shipped on Friday." {
			n++
		}
	}
	if n != 1 {
		t.Errorf("after replay, report fact appears %d times, want 1", n)
	}
}

func TestListTagsCounts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const bank = "it-tags"

	if _, err := s.EnsureBank(ctx, bank, bank, ""); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := s.Retain(ctx, bank, []model.RetainItem{
		{Content: "one", FactType: model.FactWorld, Tags: []string{"a", "b"}},
		{Content: "two", FactType: model.FactWorld, Tags: []string{"b"}},
	}); err != nil {
		t.Fatalf("retain: %v", err)
	}
	counts, err := s.ListTags(ctx, bank)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if counts["a"] != 1 || counts["b"] != 2 {
		t.Errorf("counts = %v, want a=1 b=2", counts)
	}
}

func TestRetainUnknownBankFails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, err := s.Retain(ctx, "no-such-bank", []model.RetainItem{
		{Content: "x", FactType: model.FactWorld},
	})
	if err == nil {
		t.Fatalf("expected error for unknown bank")
	}
}
