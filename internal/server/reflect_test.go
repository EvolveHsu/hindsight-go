package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/memory"
)

// fakeLLM scripts model responses for the wire-level reflect test.
type fakeLLM struct {
	responses []llm.ChatResponse
	calls     int
}

func (f *fakeLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if f.calls >= len(f.responses) {
		return nil, errors.New("script exhausted")
	}
	r := f.responses[f.calls]
	f.calls++
	return &r, nil
}

// TestReflectOverWire drives reflect through the full HTTP stack with a
// scripted provider: recall tool call, then done.
func TestReflectOverWire(t *testing.T) {
	store := memory.New()
	engine := NewEngine(store)
	recallCall := llm.ToolCall{
		ID: "t1", Type: "function",
		Function: llm.FunctionCall{Name: "recall", Arguments: `{"query":"launch"}`},
	}
	doneCall := llm.ToolCall{
		ID: "t2", Type: "function",
		Function: llm.FunctionCall{Name: "done", Arguments: `{"answer":"The launch went well.","memory_ids":["u1"]}`},
	}
	engine.SetReflectProvider(&fakeLLM{responses: []llm.ChatResponse{
		{Choices: []llm.Choice{{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{recallCall}}}}},
		{Choices: []llm.Choice{{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{doneCall}}}}},
	}})

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
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	if code, _ := do("PUT", "/v1/default/banks/refl", map[string]any{}); code != 200 {
		t.Fatalf("bank: %d", code)
	}
	if code, b := do("POST", "/v1/default/banks/refl/memories", map[string]any{
		"items": []any{map[string]any{"content": "The launch went well and users loved the demo."}},
	}); code != 200 {
		t.Fatalf("retain: %d %v", code, b)
	}

	code, body := do("POST", "/v1/default/banks/refl/reflect", map[string]any{"query": "How did the launch go?"})
	if code != 200 {
		t.Fatalf("reflect: %d %v", code, body)
	}
	if body["text"] != "The launch went well." {
		t.Errorf("reflect text = %v", body["text"])
	}
}

// TestReflectWithoutProviderIs501 checks the honest-no-config behavior: the
// bank must exist first (the handler checks storage before provider), then the
// missing provider yields 501 with setup instructions.
func TestReflectWithoutProviderIs501(t *testing.T) {
	engine := NewEngine(memory.New())
	h, err := AsHTTPHandler(engine)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	prep, _ := http.NewRequestWithContext(context.Background(), "PUT", ts.URL+"/v1/default/banks/any",
		bytes.NewReader([]byte(`{}`)))
	prep.Header.Set("Content-Type", "application/json")
	prepResp, err := http.DefaultClient.Do(prep)
	if err != nil {
		t.Fatalf("prep: %v", err)
	}
	prepRaw, _ := io.ReadAll(prepResp.Body)
	prepResp.Body.Close()
	if prepResp.StatusCode != http.StatusOK {
		t.Fatalf("prep status = %d body = %s", prepResp.StatusCode, prepRaw)
	}

	req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/default/banks/any/reflect",
		bytes.NewReader([]byte(`{"query":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (body: %s)", resp.StatusCode, raw)
	}
}
