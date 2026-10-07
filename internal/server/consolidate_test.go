package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/memory"
)

// scriptedConsol builds its response from the fact ids the consolidator sent,
// so the test does not need to predict deterministic ids.
type scriptedConsol struct{}

func (s *scriptedConsol) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	user := req.Messages[len(req.Messages)-1].Content
	var factID string
	for _, line := range strings.Split(user, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.Contains(line, "]") {
			factID = line[1:strings.Index(line, "]")]
			break
		}
	}
	content := fmt.Sprintf(
		`{"creates":[{"text":"User lives in Berlin","source_fact_ids":[%q]}],"updates":[],"deletes":[]}`,
		factID)
	return &llm.ChatResponse{
		Choices: []llm.Choice{{Message: llm.Message{Content: content}}},
		Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
	}, nil
}

// TestConsolidationOverWire drives the full loop: retain facts, trigger
// consolidation (which synthesizes an observation via the scripted model), then
// recall with type=observation and find it.
func TestConsolidationOverWire(t *testing.T) {
	store := memory.New()
	engine := NewEngine(store)
	engine.SetReflectProvider(&fakeLLM{})
	engine.SetConsolidationProvider(&scriptedConsol{})

	h, err := AsHTTPHandler(engine)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	do := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rdr = bytes.NewReader(raw)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, rdr)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	if code, _ := do("PUT", "/v1/default/banks/cons", map[string]any{}); code != 200 {
		t.Fatalf("bank: %d", code)
	}
	if code, b := do("POST", "/v1/default/banks/cons/memories", map[string]any{
		"items": []any{map[string]any{"content": "Alice moved to Berlin in 2023."}},
	}); code != 200 {
		t.Fatalf("retain: %d %v", code, b)
	}

	code, body := do("POST", "/v1/default/banks/cons/consolidate", nil)
	if code != 200 {
		t.Fatalf("consolidate: %d %v", code, body)
	}
	if body["operation_id"] == nil {
		t.Errorf("missing operation_id: %v", body)
	}

	code, body = do("POST", "/v1/default/banks/cons/memories/recall", map[string]any{
		"query": "Where does the user live", "types": []string{"observation"},
	})
	if code != 200 {
		t.Fatalf("recall: %d", code)
	}
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("no observations after consolidation: %v", body)
	}
	m, _ := results[0].(map[string]any)
	if m["text"] != "User lives in Berlin" {
		t.Errorf("observation text = %v", m["text"])
	}
}

// TestConsolidationWithoutProviderIs501 keeps the honest-error contract.
func TestConsolidationWithoutProviderIs501(t *testing.T) {
	engine := NewEngine(memory.New())
	h, _ := AsHTTPHandler(engine)
	ts := httptest.NewServer(h)
	defer ts.Close()

	prep, _ := http.NewRequestWithContext(context.Background(), "PUT", ts.URL+"/v1/default/banks/c",
		bytes.NewReader([]byte(`{}`)))
	prep.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(prep)

	req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/default/banks/c/consolidate", nil)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}
