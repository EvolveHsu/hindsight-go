package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/memory"
)

func TestMentalModelLifecycle(t *testing.T) {
	store := memory.New()
	engine := NewEngine(store)
	doneCall := llm.ToolCall{
		ID: "t1", Type: "function",
		Function: llm.FunctionCall{Name: "done", Arguments: `{"answer":"The user is a Berlin-based cellist."}`},
	}
	engine.SetReflectProvider(&fakeLLM{responses: []llm.ChatResponse{
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

	if code, _ := do("PUT", "/v1/default/banks/mm", map[string]any{}); code != 200 {
		t.Fatalf("bank: %d", code)
	}

	code, body := do("POST", "/v1/default/banks/mm/mental-models", map[string]any{
		"name": "user-profile", "source_query": "Who is the user?",
	})
	if code != 200 {
		t.Fatalf("create: %d %v", code, body)
	}
	mmID, _ := body["mental_model_id"].(string)
	if mmID == "" {
		t.Fatalf("missing mental_model_id: %v", body)
	}

	code, body = do("GET", "/v1/default/banks/mm/mental-models/"+mmID, nil)
	if code != 200 {
		t.Fatalf("get: %d %v", code, body)
	}
	if body["content"] != "The user is a Berlin-based cellist." {
		t.Errorf("content = %v", body["content"])
	}
	if body["last_refreshed_at"] == nil {
		t.Errorf("last_refreshed_at not stamped: %v", body)
	}

	code, body = do("GET", "/v1/default/banks/mm/mental-models", nil)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items = %d, want 1", len(items))
	}

	code, body = do("PATCH", "/v1/default/banks/mm/mental-models/"+mmID, map[string]any{
		"name": "renamed-profile",
	})
	if code != 200 {
		t.Fatalf("update: %d %v", code, body)
	}
	if body["name"] != "renamed-profile" {
		t.Errorf("name = %v", body["name"])
	}

	if code, _ := do("POST", "/v1/default/banks/mm/mental-models/"+mmID+"/refresh", nil); code != 200 {
		t.Fatalf("refresh: %d", code)
	}

	if code, _ := do("DELETE", "/v1/default/banks/mm/mental-models/"+mmID, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := do("GET", "/v1/default/banks/mm/mental-models/"+mmID, nil); code != 404 {
		t.Errorf("get after delete = %d, want 404", code)
	}
}

func TestRefreshWithoutProviderIs501(t *testing.T) {
	engine := NewEngine(memory.New())
	h, _ := AsHTTPHandler(engine)
	ts := httptest.NewServer(h)
	defer ts.Close()

	prep, _ := http.NewRequestWithContext(context.Background(), "PUT", ts.URL+"/v1/default/banks/mm2",
		bytes.NewReader([]byte(`{}`)))
	prep.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(prep)

	req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/default/banks/mm2/mental-models",
		bytes.NewReader([]byte(`{"name":"n","source_query":"q"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()

	req2, _ := http.NewRequestWithContext(context.Background(), "GET", ts.URL+"/v1/default/banks/mm2/mental-models", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp2.Body.Close()
	var list map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&list)
	items, _ := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 model, got %d", len(items))
	}
	m, _ := items[0].(map[string]any)
	id, _ := m["id"].(string)

	req3, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/default/banks/mm2/mental-models/"+id+"/refresh", nil)
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotImplemented {
		t.Errorf("refresh status = %d, want 501", resp3.StatusCode)
	}
}
