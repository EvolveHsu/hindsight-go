package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/memory"
)

// newEngineServer wires a fresh in-memory engine through the real generated
// router, so these tests cover the wire format end to end.
func newEngineServer(t *testing.T) *httptest.Server {
	t.Helper()
	h, err := AsHTTPHandler(NewEngine(memory.New()))
	if err != nil {
		t.Fatalf("AsHTTPHandler: %v", err)
	}
	return httptest.NewServer(h)
}

func doJSON(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
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

func TestBankLifecycle(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	// PUT creates
	code, body := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/test-bank", map[string]any{
		"reflect_mission": "remember what matters",
	})
	if code != http.StatusOK {
		t.Fatalf("PUT bank status = %d (body: %v)", code, body)
	}
	if body["bank_id"] != "test-bank" {
		t.Errorf("bank_id = %v", body["bank_id"])
	}

	// PUT is idempotent
	code, _ = doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/test-bank", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("idempotent PUT status = %d", code)
	}

	// list shows one bank
	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks", nil)
	if code != http.StatusOK {
		t.Fatalf("GET banks status = %d", code)
	}
	banks, _ := body["banks"].([]any)
	if len(banks) != 1 || body["total"] != float64(1) {
		t.Errorf("banks = %v, total = %v", banks, body["total"])
	}
}

func TestRetainAndRecallRoundTrip(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/rt", map[string]any{}); code != 200 {
		t.Fatalf("bank setup failed: %d", code)
	}

	// retain with the plain-string content form (what the official Python
	// client sends, and what the raw generated decoder rejects).
	retainBody := map[string]any{
		"items": []any{
			map[string]any{"content": "Alice moved to Berlin in 2023.", "context": "introduction"},
			map[string]any{"content": "Alice plays the cello.", "tags": []string{"music"}},
		},
	}
	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/rt/memories", retainBody)
	if code != http.StatusOK {
		t.Fatalf("retain status = %d (body: %v)", code, body)
	}
	if body["success"] != true || body["items_count"] != float64(2) {
		t.Errorf("retain response = %v", body)
	}

	// recall finds a stored fact by query
	recallBody := map[string]any{"query": "Where does Alice live?"}
	code, body = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/rt/memories/recall", recallBody)
	if code != http.StatusOK {
		t.Fatalf("recall status = %d (body: %v)", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("recall returned no results (body: %v)", body)
	}
	// Both facts mention Alice, so both can rank; assert membership and that
	// each carries scores, rather than pinning position 0.
	wantBerlin := false
	for _, r := range results {
		m, _ := r.(map[string]any)
		if txt, _ := m["text"].(string); txt == "Alice moved to Berlin in 2023." {
			wantBerlin = true
		}
		if _, ok := m["scores"].(map[string]any); !ok {
			t.Errorf("result missing scores: %v", m)
		}
	}
	if !wantBerlin {
		t.Errorf("Berlin fact missing from results")
	}

	// tag filtering path via recall with fact types
	recallBody2 := map[string]any{"query": "Alice cello", "types": []string{"world", "experience"}}
	code, body = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/rt/memories/recall", recallBody2)
	if code != http.StatusOK {
		t.Fatalf("recall(2) status = %d", code)
	}
	if r, _ := body["results"].([]any); len(r) == 0 {
		t.Errorf("second recall returned nothing: %v", body)
	}
}

func TestRetainIdempotentReplay(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/idem", map[string]any{}); code != 200 {
		t.Fatalf("bank setup: %d", code)
	}
	payload := map[string]any{
		"items": []any{map[string]any{"content": "The team shipped v2 on Friday.", "document_id": "doc-1"}},
	}
	if code, b := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/idem/memories", payload); code != 200 {
		t.Fatalf("first retain: %d %v", code, b)
	}
	// same document_id + same content -> no-op success, no duplicate units
	if code, b := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/idem/memories", payload); code != 200 {
		t.Fatalf("replay retain: %d %v", code, b)
	}

	// verify unit count via recall: the fact must appear exactly once
	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/idem/memories/recall", map[string]any{"query": "team shipped v2"})
	if code != 200 {
		t.Fatalf("recall: %d", code)
	}
	results, _ := body["results"].([]any)
	n := 0
	for _, r := range results {
		if m, _ := r.(map[string]any)["text"]; m == "The team shipped v2 on Friday." {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly 1 unit after replay, got %d", n)
	}
}

func TestRetainRequiresExistingBank(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/ghost/memories", map[string]any{
		"items": []any{map[string]any{"content": "hello"}},
	})
	if code != http.StatusNotFound {
		t.Fatalf("retain into missing bank status = %d, want 404 (body: %v)", code, body)
	}
}

func TestListTagsCountsUnits(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/tags-bank", map[string]any{}); code != 200 {
		t.Fatalf("bank setup: %d", code)
	}
	payload := map[string]any{
		"items": []any{
			map[string]any{"content": "one", "tags": []string{"a", "b"}},
			map[string]any{"content": "two", "tags": []string{"b"}},
		},
	}
	if code, b := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/tags-bank/memories", payload); code != 200 {
		t.Fatalf("retain: %d %v", code, b)
	}

	code, body := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/tags-bank/tags", nil)
	if code != 200 {
		t.Fatalf("list tags: %d", code)
	}
	items, _ := body["items"].([]any)
	counts := map[string]float64{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		counts[m["tag"].(string)] = m["count"].(float64)
	}
	if counts["a"] != 1 || counts["b"] != 2 {
		t.Errorf("tag counts = %v, want a=1 b=2", counts)
	}
	if body["total"] != float64(2) {
		t.Errorf("total = %v, want 2", body["total"])
	}
}

func TestUnknownBankRecallIs404(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	code, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/nope/memories/recall", map[string]any{"query": "x"})
	if code != http.StatusNotFound {
		t.Fatalf("recall into missing bank status = %d, want 404", code)
	}
}
