package server

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/memory"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// fakeGraphStore layers an entity registry over the in-memory store so the
// entity endpoints can be exercised without PostgreSQL.
type fakeGraphStore struct {
	*memory.Store
	entities []model.EntityInfo
	detail   *model.EntityDetail
	graph    *model.EntityGraph
	err      error
}

func (f *fakeGraphStore) GraphData(ctx context.Context, bankID string, opt model.GraphOptions) (*model.GraphData, error) {
	return f.Store.GraphData(ctx, bankID, opt)
}

func (f *fakeGraphStore) ListEntities(ctx context.Context, bankID string, opt model.EntityListOptions) ([]model.EntityInfo, int, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.entities, len(f.entities), nil
}

func (f *fakeGraphStore) GetEntity(ctx context.Context, bankID, entityID string, scope model.TagFilter) (*model.EntityDetail, error) {
	if f.detail != nil && f.detail.ID == entityID {
		return f.detail, nil
	}
	return nil, nil
}

func (f *fakeGraphStore) EntityGraph(ctx context.Context, bankID string, opt model.EntityGraphOptions) (*model.EntityGraph, error) {
	return f.graph, nil
}

func TestMemoryGraphEndpoint(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/g", map[string]any{"name": "g"}); code != 200 {
		t.Fatalf("create bank: %d", code)
	}
	if code, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/g/memories", map[string]any{
		"items": []map[string]any{
			{"content": "alice likes tea", "document_id": "doc1"},
			{"content": "bob likes coffee", "document_id": "doc1"},
		},
	}); code != 200 {
		t.Fatalf("retain: %d", code)
	}

	code, body := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/g/graph", nil)
	if code != 200 {
		t.Fatalf("graph status = %d (%v)", code, body)
	}
	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (%v)", len(nodes), body)
	}
	rows, _ := body["table_rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("table_rows = %d, want 2", len(rows))
	}
	if got := body["total_units"]; got != float64(2) {
		t.Fatalf("total_units = %v, want 2", got)
	}

	// Filtering by an unknown type yields an empty page but keeps the total.
	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/g/graph?type=observation", nil)
	if code != 200 {
		t.Fatalf("filtered graph status = %d", code)
	}
	if nodes, _ := body["nodes"].([]any); len(nodes) != 0 {
		t.Fatalf("filtered nodes = %d, want 0", len(nodes))
	}

	if code, _ := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/missing/graph", nil); code != 404 {
		t.Fatalf("missing bank graph status = %d, want 404", code)
	}
}

func TestEntityEndpoints(t *testing.T) {
	const entityID = "11111111-2222-3333-4444-555555555555"
	store := &fakeGraphStore{
		Store:    memory.New(),
		entities: []model.EntityInfo{{ID: entityID, CanonicalName: "Alice", MentionCount: 3}},
		detail: &model.EntityDetail{
			EntityInfo:   model.EntityInfo{ID: entityID, CanonicalName: "Alice", MentionCount: 3},
			Observations: []model.EntityObservation{{Text: "Alice likes tea"}},
		},
		graph: &model.EntityGraph{
			Nodes: []model.EntityGraphNode{{ID: entityID, Label: "Alice", MentionCount: 3}},
			Edges: []model.EntityGraphEdge{},
		},
	}
	ts := newEngineServerWithStore(t, store)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/e", map[string]any{"name": "e"}); code != 200 {
		t.Fatalf("create bank failed")
	}

	code, body := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/e/entities", nil)
	if code != 200 {
		t.Fatalf("list entities status = %d (%v)", code, body)
	}
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Fatalf("entities = %v, want 1 item", body)
	}

	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/e/entities/"+entityID, nil)
	if code != 200 {
		t.Fatalf("get entity status = %d (%v)", code, body)
	}
	if body["canonical_name"] != "Alice" {
		t.Fatalf("canonical_name = %v", body["canonical_name"])
	}
	obs, _ := body["observations"].([]any)
	if len(obs) != 1 {
		t.Fatalf("observations = %v, want 1", body["observations"])
	}

	if code, _ := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/e/entities/not-a-uuid", nil); code != 400 {
		t.Fatalf("invalid entity id status = %d, want 400", code)
	}
	if code, _ := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/e/entities/99999999-2222-3333-4444-555555555555", nil); code != 404 {
		t.Fatalf("missing entity status = %d, want 404", code)
	}

	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/e/entities/graph", nil)
	if code != 200 {
		t.Fatalf("entity graph status = %d (%v)", code, body)
	}
	if nodes, _ := body["nodes"].([]any); len(nodes) != 1 {
		t.Fatalf("entity graph nodes = %v", body["nodes"])
	}
	if got := body["total_entities"]; got != float64(1) {
		t.Fatalf("total_entities = %v, want 1", got)
	}

	code, body = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/e/entities/"+entityID+"/regenerate", nil)
	if code != 410 {
		t.Fatalf("regenerate status = %d, want 410 (%v)", code, body)
	}
}

func TestDocumentChunksPatchAndReprocess(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/d", map[string]any{"name": "d"}); code != 200 {
		t.Fatalf("create bank failed")
	}
	if code, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/d/memories", map[string]any{
		"items": []map[string]any{
			{"content": "first fact", "document_id": "doc1", "tags": []string{"old"}},
			{"content": "second fact", "document_id": "doc1"},
		},
	}); code != 200 {
		t.Fatalf("retain failed")
	}

	code, body := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/d/documents/doc1/chunks", nil)
	if code != 200 {
		t.Fatalf("chunks status = %d (%v)", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("chunks = %v, want 2", body["items"])
	}
	if got := body["total"]; got != float64(2) {
		t.Fatalf("chunks total = %v, want 2", got)
	}
	first, _ := items[0].(map[string]any)
	chunkID, _ := first["chunk_id"].(string)
	if chunkID == "" || first["chunk_text"] != "first fact" {
		t.Fatalf("unexpected first chunk: %v", first)
	}

	if code, body := doJSON(t, http.MethodGet, ts.URL+"/v1/default/chunks/"+chunkID, nil); code != 200 || body["chunk_text"] != "first fact" {
		t.Fatalf("get chunk = %d (%v)", code, body)
	}
	if code, _ := doJSON(t, http.MethodGet, ts.URL+"/v1/default/chunks/nope", nil); code != 404 {
		t.Fatalf("missing chunk status = %d, want 404", code)
	}
	if code, _ := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/d/documents/missing/chunks", nil); code != 404 {
		t.Fatalf("missing document chunks status = %d, want 404", code)
	}

	// Tags replace, not merge.
	if code, body := doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/d/documents/doc1", map[string]any{"tags": []string{"new"}}); code != 200 || body["success"] != true {
		t.Fatalf("patch document = %d (%v)", code, body)
	}
	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/d/documents/doc1", nil)
	if code != 200 {
		t.Fatalf("get document = %d", code)
	}
	tags, _ := body["tags"].([]any)
	if len(tags) != 1 || tags[0] != "new" {
		t.Fatalf("document tags = %v, want [new]", body["tags"])
	}

	if code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/d/documents/doc1/reprocess", nil); code != 200 {
		t.Fatalf("reprocess = %d (%v)", code, body)
	} else if body["success"] != true || body["items_count"] != float64(2) {
		t.Fatalf("reprocess body = %v", body)
	}
	if code, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/d/documents/missing/reprocess", nil); code != 404 {
		t.Fatalf("missing document reprocess status = %d, want 404", code)
	}
	if code, _ := doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/d/documents/doc1", map[string]any{}); code != 422 {
		t.Fatalf("patch without tags status = %d, want 422", code)
	}
}

func TestEntityTagGroupsFuzzyMapsTo422(t *testing.T) {
	store := &fakeGraphStore{Store: memory.New(), err: model.ErrFuzzyTagGroups}
	ts := newEngineServerWithStore(t, store)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/e", map[string]any{"name": "e"}); code != 200 {
		t.Fatalf("create bank failed")
	}
	groups := url.QueryEscape(`[{"tags":["x"],"resolve":"fuzzy"}]`)
	code, body := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/e/entities?tag_groups="+groups, nil)
	if code != 422 {
		t.Fatalf("fuzzy tag_groups status = %d, want 422 (%v)", code, body)
	}
}

func TestRenderGraphDataDerivesEntityEdges(t *testing.T) {
	data := &model.GraphData{
		Units: []model.GraphUnit{
			{ID: "u1", Text: "alice likes tea", FactType: "experience"},
			{ID: "u2", Text: "alice likes coffee", FactType: "experience"},
			{ID: "u3", Text: "she likes both", FactType: "observation", SourceMemoryIDs: []string{"u1", "u2"}},
		},
		EntityRows: []model.GraphEntityRow{
			{UnitID: "u1", CanonicalName: "Alice"},
			{UnitID: "u2", CanonicalName: "Alice"},
		},
		TotalUnits: 3,
	}
	res := renderGraphData(data, 1000)
	var entityEdges, semanticEdges int
	for _, e := range res.Edges {
		switch e.Data.LinkType.Value {
		case "entity":
			entityEdges++
			if e.Data.EntityName.Value != "Alice" {
				t.Fatalf("entity edge name = %q", e.Data.EntityName.Value)
			}
		case "semantic":
			semanticEdges++
		}
	}
	if entityEdges != 3 {
		t.Fatalf("entity edges = %d, want 3", entityEdges)
	}
	// The observation inherits Alice, so all three nodes pair up under that name.
	if got := len(res.Edges); got != 3 {
		t.Fatalf("edges = %d, want 3 (%+v)", got, res.Edges)
	}
	if len(res.Nodes) != 3 || len(res.TableRows) != 3 {
		t.Fatalf("nodes=%d rows=%d, want 3/3", len(res.Nodes), len(res.TableRows))
	}
	for _, n := range res.Nodes {
		if n.Data.ID == "u3" && n.Data.Entities.Value != "Alice" {
			t.Fatalf("observation entities = %q, want Alice", n.Data.Entities.Value)
		}
	}
	if semanticEdges != 0 {
		t.Fatalf("semantic edges = %d, want 0", semanticEdges)
	}
}
