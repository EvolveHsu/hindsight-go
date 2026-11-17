package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/extract"
	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/memory"
)

type failingExtractProvider struct {
	calls int
}

func (p *failingExtractProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls++
	return nil, errors.New("extractor must not be called")
}

func TestExactRetainBypassesExtractor(t *testing.T) {
	store := memory.New()
	if _, err := store.EnsureBank(context.Background(), "exact-test", "", ""); err != nil {
		t.Fatal(err)
	}
	provider := &failingExtractProvider{}
	engine := NewEngine(store)
	engine.SetExtractor(extract.New(provider, "model"))
	handler, err := AsHTTPHandler(engine)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	code, body := doJSON(t, http.MethodPost, srv.URL+"/v1/default/banks/exact-test/memories", map[string]any{
		"items": []any{map[string]any{
			"content":     "ExactVerbatim20261117: preserve this text unchanged.",
			"document_id": "exact-doc",
			"strategy":    "exact",
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("exact retain returned %d: %v", code, body)
	}
	if provider.calls != 0 {
		t.Fatalf("extractor called %d times for exact strategy", provider.calls)
	}
	doc, err := store.GetDocument(context.Background(), "exact-test", "exact-doc")
	if err != nil {
		t.Fatal(err)
	}
	if doc == nil || doc.OriginalText != "ExactVerbatim20261117: preserve this text unchanged." {
		t.Fatalf("stored exact document = %+v", doc)
	}
	units, total, err := store.ListMemories(context.Background(), "exact-test", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(units) != 1 || units[0].Text != "ExactVerbatim20261117: preserve this text unchanged." {
		t.Fatalf("stored exact units = %+v total=%d", units, total)
	}
}
