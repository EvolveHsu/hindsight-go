package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/extract"
	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/memory"
)

type retryProvider struct {
	calls   int
	succeed bool
}

func (p *retryProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls++
	content, finish := `[{"text":"Partial fact","entities":["Alice"]`, "length"
	if p.succeed && p.calls == 2 {
		content = `[{"text":"Final fact","fact_type":"world","entities":["Alice"]}]`
		finish = "stop"
	}
	return &llm.ChatResponse{Choices: []llm.Choice{{
		Message: llm.Message{Content: content}, FinishReason: finish,
	}}}, nil
}

func TestRetainExtractionFailurePreservesDocument(t *testing.T) {
	engine := NewEngine(memory.New())
	handler, err := AsHTTPHandler(engine)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()
	path := ts.URL + "/v1/default/banks/extraction"
	if code, body := doJSON(t, http.MethodPut, path, map[string]any{}); code != 200 {
		t.Fatalf("create bank: %d %v", code, body)
	}
	payload := func(text string) map[string]any {
		return map[string]any{"items": []any{map[string]any{"content": text, "document_id": "same-doc"}}}
	}
	if code, body := doJSON(t, http.MethodPost, path+"/memories", payload("Original fact")); code != 200 {
		t.Fatalf("seed: %d %v", code, body)
	}
	p := &retryProvider{}
	engine.SetExtractor(extract.New(p, ""))
	if code, body := doJSON(t, http.MethodPost, path+"/memories", payload("Replacement")); code != 500 {
		t.Fatalf("truncation: %d %v", code, body)
	}
	if p.calls != 3 {
		t.Fatalf("model calls=%d", p.calls)
	}
	assertFacts := func(want ...string) {
		t.Helper()
		code, body := doJSON(t, http.MethodPost, path+"/memories/recall", map[string]any{"query": "fact", "max_tokens": 4096})
		results, _ := body["results"].([]any)
		if code != 200 || len(results) != len(want) {
			t.Fatalf("stored facts: %d %v want=%v", code, body, want)
		}
		found := make(map[string]bool)
		for _, result := range results {
			found[result.(map[string]any)["text"].(string)] = true
		}
		for _, text := range want {
			if !found[text] {
				t.Fatalf("missing %q from %v", text, found)
			}
		}
	}
	assertFacts("Original fact")
	p.calls, p.succeed = 0, true
	if code, body := doJSON(t, http.MethodPost, path+"/memories", payload("Replacement")); code != 200 || body["items_count"] != float64(1) {
		t.Fatalf("successful retry: %d %v", code, body)
	}
	if p.calls != 2 {
		t.Fatalf("model calls=%d", p.calls)
	}
	assertFacts("Original fact", "Final fact")
}
