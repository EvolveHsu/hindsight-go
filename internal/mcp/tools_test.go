package mcp

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestBuildCallMapsToolsOntoRestOperations(t *testing.T) {
	cases := []struct {
		name      string
		tool      string
		args      map[string]any
		wantVerb  string
		wantPath  string
		wantQuery map[string]string
		wantBody  map[string]any
	}{
		{
			name:     "retain defaults context to general",
			tool:     "retain",
			args:     map[string]any{"content": "remember this"},
			wantVerb: "POST",
			wantPath: "/v1/default/banks/example-bank/memories",
			wantBody: map[string]any{
				"items": []any{map[string]any{"content": "remember this", "context": "general"}},
			},
		},
		{
			name:     "sync_retain uses the same synchronous endpoint",
			tool:     "sync_retain",
			args:     map[string]any{"content": "sync me", "context": "work"},
			wantVerb: "POST",
			wantPath: "/v1/default/banks/example-bank/memories",
			wantBody: map[string]any{
				"items": []any{map[string]any{"content": "sync me", "context": "work"}},
			},
		},
		{
			name:     "recall carries the query and types",
			tool:     "recall",
			args:     map[string]any{"query": "what happened", "types": []any{"world"}, "budget": "low"},
			wantVerb: "POST",
			wantPath: "/v1/default/banks/example-bank/memories/recall",
			wantBody: map[string]any{"query": "what happened", "types": []any{"world"}, "budget": "low"},
		},
		{
			name:      "list_memories turns arguments into query parameters",
			tool:      "list_memories",
			args:      map[string]any{"type": "world", "limit": float64(5)},
			wantVerb:  "GET",
			wantPath:  "/v1/default/banks/example-bank/memories/list",
			wantQuery: map[string]string{"type": "world", "limit": "5"},
		},
		{
			name:     "invalidate_memory defaults to the invalidated state",
			tool:     "invalidate_memory",
			args:     map[string]any{"memory_id": "m1", "reason": "superseded"},
			wantVerb: "PATCH",
			wantPath: "/v1/default/banks/example-bank/memories/m1",
			wantBody: map[string]any{"state": "invalidated", "reason": "superseded"},
		},
		{
			name:     "invalidate_memory with restore maps to the valid state",
			tool:     "invalidate_memory",
			args:     map[string]any{"memory_id": "m1", "restore": true},
			wantVerb: "PATCH",
			wantPath: "/v1/default/banks/example-bank/memories/m1",
			wantBody: map[string]any{"state": "valid"},
		},
		{
			name:      "search_knowledge_base renames query to q",
			tool:      "search_knowledge_base",
			args:      map[string]any{"query": "deploy notes", "limit": float64(3)},
			wantVerb:  "GET",
			wantPath:  "/v1/default/banks/example-bank/knowledge-base/search",
			wantQuery: map[string]string{"q": "deploy notes", "limit": "3"},
		},
		{
			name: "create_mental_model folds the id and trigger flags",
			tool: "create_mental_model",
			args: map[string]any{
				"name":                                "Weekly",
				"source_query":                        "what changed",
				"mental_model_id":                     "weekly",
				"trigger_refresh_after_consolidation": true,
				"tags_match":                          "any",
			},
			wantVerb: "POST",
			wantPath: "/v1/default/banks/example-bank/mental-models",
			wantBody: map[string]any{
				"name":         "Weekly",
				"source_query": "what changed",
				"id":           "weekly",
				"trigger":      map[string]any{"refresh_after_consolidation": true, "tags_match": "any"},
			},
		},
		{
			name:      "clear_memories passes the fact-type filter",
			tool:      "clear_memories",
			args:      map[string]any{"type": "world"},
			wantVerb:  "DELETE",
			wantPath:  "/v1/default/banks/example-bank/memories",
			wantQuery: map[string]string{"type": "world"},
		},
		{
			name:     "get_operation reads the operation status route",
			tool:     "get_operation",
			args:     map[string]any{"operation_id": "op-1"},
			wantVerb: "GET",
			wantPath: "/v1/default/banks/example-bank/operations/op-1",
		},
		{
			name:     "get_bank reads the profile route",
			tool:     "get_bank",
			args:     map[string]any{},
			wantVerb: "GET",
			wantPath: "/v1/default/banks/example-bank/profile",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, err := buildCall("example-bank", tc.tool, tc.args)
			if err != nil {
				t.Fatalf("buildCall: %v", err)
			}
			if call.method != tc.wantVerb {
				t.Errorf("method = %q, want %q", call.method, tc.wantVerb)
			}
			if call.path != tc.wantPath {
				t.Errorf("path = %q, want %q", call.path, tc.wantPath)
			}
			for key, want := range tc.wantQuery {
				if got := call.query.Get(key); got != want {
					t.Errorf("query %q = %q, want %q", key, got, want)
				}
			}
			if tc.wantQuery == nil && len(call.query) > 0 {
				t.Errorf("unexpected query %v", call.query)
			}
			if tc.wantBody != nil {
				got, err := json.Marshal(call.body)
				if err != nil {
					t.Fatalf("marshal body: %v", err)
				}
				want, err := json.Marshal(tc.wantBody)
				if err != nil {
					t.Fatalf("marshal want: %v", err)
				}
				if string(got) != string(want) {
					t.Errorf("body = %s, want %s", got, want)
				}
			}
		})
	}
}

func TestBuildCallRejectsBadArguments(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"retain", map[string]any{}, "content is required"},
		{"retain", map[string]any{"content": "   "}, "content must not be empty"},
		{"retain", map[string]any{"content": 42}, "content must be a string"},
		{"recall", map[string]any{}, "query is required"},
		{"get_memory", map[string]any{}, "memory_id is required"},
	}
	for _, tc := range cases {
		_, err := buildCall("example-bank", tc.tool, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("buildCall(%s, %v) error = %v, want %q", tc.tool, tc.args, err, tc.want)
		}
	}
}

// TestEveryCatalogToolHasAMapping guards the property the cutover depends on:
// a tool the client can see must never fall through to "no REST mapping".
func TestEveryCatalogToolHasAMapping(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	for _, entry := range cat.raw {
		var tool struct {
			Name        string `json:"name"`
			InputSchema struct {
				Required   []string `json:"required"`
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			} `json:"inputSchema"`
		}
		if err := json.Unmarshal(entry, &tool); err != nil {
			t.Fatalf("parse tool entry: %v", err)
		}

		args := map[string]any{}
		for _, key := range tool.InputSchema.Required {
			args[key] = "value"
		}
		// Update-style tools accept any subset of their optional fields; give
		// them something to update so the mapping is not rejected as a no-op.
		args["name"] = "value"
		args["content"] = "value"
		args["source_query"] = "value"
		args["query"] = "value"

		if _, err := buildCall("example-bank", tool.Name, args); err != nil {
			t.Errorf("tool %q has no usable mapping: %v", tool.Name, err)
		}
	}
}

func TestQueryEncodesArraysAsRepeatedParameters(t *testing.T) {
	values := query(
		map[string]any{"tags": []any{"a", "b"}, "limit": float64(10), "active_only": true},
		spec("tags", "tags"), spec("limit", "limit"), spec("active_only", "active_only"),
	)
	encoded := values.Encode()
	parsed, err := url.ParseQuery(encoded)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if got := parsed["tags"]; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("tags = %v, want [a b]", got)
	}
	if parsed.Get("limit") != "10" {
		t.Errorf("limit = %q, want 10 (no decimal point)", parsed.Get("limit"))
	}
	if parsed.Get("active_only") != "true" {
		t.Errorf("active_only = %q", parsed.Get("active_only"))
	}
}
