package storepg

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/server"
)

// TestWireRoundTripOnPostgres is the end-to-end proof: embedded PostgreSQL
// backend behind the full generated HTTP stack, retain and recall over the wire
// with the plain-string content form the official client sends.
func TestWireRoundTripOnPostgres(t *testing.T) {
	ctx := context.Background()
	dsn := startEmbedded(t)

	pg, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)

	h, err := server.AsHTTPHandler(server.NewEngine(pg))
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
		req, err := http.NewRequestWithContext(ctx, method, ts.URL+path, rdr)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	if code, b := do("PUT", "/v1/default/banks/pg-e2e", map[string]any{}); code != 200 {
		t.Fatalf("PUT bank: %d %v", code, b)
	}

	code, b := do("POST", "/v1/default/banks/pg-e2e/memories", map[string]any{
		"items": []any{
			map[string]any{"content": "The pg-e2e round trip works."},
			map[string]any{"content": "Remember the cello recital.", "tags": []string{"music"}},
		},
	})
	if code != 200 {
		t.Fatalf("retain: %d %v", code, b)
	}

	code, b = do("POST", "/v1/default/banks/pg-e2e/memories/recall", map[string]any{"query": "round trip"})
	if code != 200 {
		t.Fatalf("recall: %d %v", code, b)
	}
	results, _ := b["results"].([]any)
	found := false
	for _, r := range results {
		m, _ := r.(map[string]any)
		if txt, _ := m["text"].(string); txt == "The pg-e2e round trip works." {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the round-trip fact in recall results: %v", b)
	}

	// idempotent replay on Postgres: same document content, no duplicate
	if code, b := do("POST", "/v1/default/banks/pg-e2e/memories", map[string]any{
		"items": []any{map[string]any{"content": "The pg-e2e round trip works."}},
	}); code != 200 {
		t.Fatalf("replay retain: %d %v", code, b)
	}
	code, b = do("POST", "/v1/default/banks/pg-e2e/memories/recall", map[string]any{"query": "round trip works"})
	if code != 200 {
		t.Fatalf("recall 2: %d", code)
	}
	n := 0
	for _, r := range b["results"].([]any) {
		m, _ := r.(map[string]any)
		if txt, _ := m["text"].(string); txt == "The pg-e2e round trip works." {
			n++
		}
	}
	if n != 1 {
		t.Errorf("after replay, fact appears %d times, want 1", n)
	}
}
