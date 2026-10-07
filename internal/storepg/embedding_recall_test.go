package storepg

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/embeddings"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// fakeEmbedder returns a deterministic vector per exact input string.
type fakeEmbedder map[string][]float32

func (f fakeEmbedder) Embed(_ context.Context, texts []string, _ embeddings.InputType) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		vector, ok := f[text]
		if !ok {
			vector = []float32{0, 0}
		}
		out[i] = append([]float32(nil), vector...)
	}
	return out, nil
}

func TestEmbeddingLiteralRoundTrip(t *testing.T) {
	in := []float32{0.25, -1.5, 3}
	got := parseEmbeddingLiteral(embeddingLiteral(in))
	if len(got) != len(in) {
		t.Fatalf("round trip length = %d, want %d", len(got), len(in))
	}
	for i := range in {
		if math.Abs(float64(got[i]-in[i])) > 1e-6 {
			t.Errorf("value %d = %v, want %v", i, got[i], in[i])
		}
	}
}

func TestPgvectorRecallAgainstLiveDatabase(t *testing.T) {
	dsn := os.Getenv("TEST_PGVECTOR_URL")
	if dsn == "" {
		t.Skip("set TEST_PGVECTOR_URL to exercise the pgvector recall arm")
	}
	ctx := context.Background()
	query := make([]float32, 384)
	query[0] = 1
	s, err := New(ctx, dsn, WithUpstreamSchema(), WithReadOnlySession(), WithEmbedder(fakeEmbedder{"probe query": query}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer s.Close()
	banks, err := s.BankIDs(ctx)
	if err != nil {
		t.Fatalf("list banks: %v", err)
	}
	if len(banks) == 0 {
		t.Skip("live database has no banks")
	}
	if _, err := s.Recall(ctx, banks[0], model.RecallOptions{Query: "probe query"}); err != nil {
		t.Fatalf("pgvector recall: %v", err)
	}
}

func TestPostgresRecallUsesStoredEmbeddings(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, testDSN(t), WithEmbedder(fakeEmbedder{
		"alpha fact":  {1, 0},
		"beta fact":   {0, 1},
		"alpha query": {1, 0},
	}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	const bank = "it-embedding-recall"
	if _, err := s.EnsureBank(ctx, bank, bank, ""); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}
	if _, err := s.Retain(ctx, bank, []model.RetainItem{
		{Content: "alpha fact", FactType: model.FactWorld, DocumentID: "doc-embed"},
		{Content: "beta fact", FactType: model.FactWorld, DocumentID: "doc-embed"},
	}); err != nil {
		t.Fatalf("retain: %v", err)
	}

	var stored string
	if err := s.pool.QueryRow(ctx,
		"SELECT embedding FROM units WHERE bank_id = $1 AND text = 'alpha fact'", bank).Scan(&stored); err != nil {
		t.Fatalf("read stored embedding: %v", err)
	}
	if got := parseEmbeddingLiteral(stored); len(got) != 2 || got[0] != 1 {
		t.Fatalf("stored embedding = %q (parsed %v)", stored, got)
	}

	hits, err := s.Recall(ctx, bank, model.RecallOptions{Query: "alpha query"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("recall returned no hits")
	}
	if hits[0].Unit.Text != "alpha fact" {
		t.Fatalf("top hit = %q, want alpha fact", hits[0].Unit.Text)
	}
	if hits[0].Semantic < 0.99 {
		t.Fatalf("semantic score = %v, want ~1", hits[0].Semantic)
	}
}
