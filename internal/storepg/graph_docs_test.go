package storepg

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// upstreamEntityDDL adds the registry tables the graph/entity reads use. The
// shared fixture starts with banks/documents/chunks/memory_units only.
const upstreamEntityDDL = `
-- The shared fixture already creates a minimal entities table; widen it to the
-- upstream shape the graph/entity reads select from.
ALTER TABLE entities ADD COLUMN IF NOT EXISTS entity_kind TEXT;
ALTER TABLE entities ADD COLUMN IF NOT EXISTS first_seen TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE entities ADD COLUMN IF NOT EXISTS last_seen TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE entities ADD COLUMN IF NOT EXISTS mention_count INTEGER NOT NULL DEFAULT 1;
ALTER TABLE entities ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE TABLE IF NOT EXISTS unit_entities (
    unit_id   UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    entity_id UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    PRIMARY KEY (unit_id, entity_id)
);

CREATE TABLE IF NOT EXISTS entity_cooccurrences (
    entity_id_1        UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    entity_id_2        UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    cooccurrence_count INTEGER NOT NULL DEFAULT 1,
    last_cooccurred    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (entity_id_1, entity_id_2),
    CHECK (entity_id_1 < entity_id_2)
);

CREATE TABLE IF NOT EXISTS memory_links (
    from_unit_id UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    to_unit_id   UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    link_type    TEXT NOT NULL,
    entity_id    UUID,
    weight       DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);`

func seedUpstreamGraph(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx, upstreamEntityDDL); err != nil {
		t.Fatalf("create entity tables: %v", err)
	}
	// The shared fixture truncates its own tables between tests; these four are
	// not parents/children of those, so clear them explicitly.
	if _, err := s.pool.Exec(ctx, `TRUNCATE entities, unit_entities, entity_cooccurrences, memory_links CASCADE`); err != nil {
		t.Fatalf("truncate entity tables: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO banks (bank_id, name) VALUES ('b', 'B')`); err != nil {
		t.Fatalf("insert bank: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO documents (id, bank_id, original_text, content_hash) VALUES ('doc1', 'b', 'first fact' || chr(10) || 'second fact', 'h1');
		INSERT INTO chunks (chunk_id, bank_id, document_id, chunk_index, chunk_text) VALUES
			('b_doc1_0', 'b', 'doc1', 0, 'first fact'),
			('b_doc1_1', 'b', 'doc1', 1, 'second fact');
		INSERT INTO memory_units (id, bank_id, document_id, chunk_id, text, fact_type, tags, source_memory_ids) VALUES
			('00000000-0000-0000-0000-000000000001', 'b', 'doc1', 'b_doc1_0', 'first fact',  'experience', '{x}', NULL),
			('00000000-0000-0000-0000-000000000002', 'b', 'doc1', 'b_doc1_1', 'second fact', 'experience', '{}', NULL),
			('00000000-0000-0000-0000-000000000003', 'b', 'doc1', 'b_doc1_1', 'derived observation', 'observation', '{x}',
			 ARRAY['00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002']::uuid[]);
		INSERT INTO entities (id, canonical_name, bank_id, mention_count) VALUES
			('11111111-1111-1111-1111-111111111111', 'Alice', 'b', 2),
			('22222222-2222-2222-2222-222222222222', 'Bob', 'b', 1);
		INSERT INTO unit_entities (unit_id, entity_id) VALUES
			('00000000-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111'),
			('00000000-0000-0000-0000-000000000002', '11111111-1111-1111-1111-111111111111'),
			('00000000-0000-0000-0000-000000000002', '22222222-2222-2222-2222-222222222222');
		INSERT INTO entity_cooccurrences (entity_id_1, entity_id_2, cooccurrence_count) VALUES
			('11111111-1111-1111-1111-111111111111', '22222222-2222-2222-2222-222222222222', 3);
		INSERT INTO memory_links (from_unit_id, to_unit_id, link_type, weight) VALUES
			('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', 'temporal', 0.5);
	`); err != nil {
		t.Fatalf("seed upstream graph fixture: %v", err)
	}
}

func TestUpstreamGraphAndEntityReads(t *testing.T) {
	s, ctx := newUpstreamStore(t)
	seedUpstreamGraph(t, s)

	data, err := s.GraphData(ctx, "b", model.GraphOptions{Limit: 10, TagsMatch: "all_strict"})
	if err != nil {
		t.Fatalf("GraphData: %v", err)
	}
	if data.TotalUnits != 3 || len(data.Units) != 3 {
		t.Fatalf("units total=%d page=%d, want 3/3", data.TotalUnits, len(data.Units))
	}
	if len(data.EntityRows) != 3 {
		t.Fatalf("entity rows = %d, want 3", len(data.EntityRows))
	}
	if len(data.Links) != 1 || data.Links[0].LinkType != "temporal" {
		t.Fatalf("links = %+v", data.Links)
	}
	// The tag filter must exclude the untagged unit and its Bob posting.
	filtered, err := s.GraphData(ctx, "b", model.GraphOptions{Limit: 10, Tags: []string{"x"}, TagsMatch: "any_strict"})
	if err != nil {
		t.Fatalf("GraphData filtered: %v", err)
	}
	if len(filtered.Units) != 2 || filtered.TotalUnits != 2 {
		t.Fatalf("filtered units = %d/%d, want 2/2", len(filtered.Units), filtered.TotalUnits)
	}
	// The store returns links among visible *and source* units; the renderer drops
	// the ones whose far endpoint is not visible (verify via renderGraphData in the
	// server package). Here only the raw read is asserted.
	if len(filtered.Links) != 1 {
		t.Fatalf("filtered links = %+v, want 1 raw link", filtered.Links)
	}

	items, total, err := s.ListEntities(ctx, "b", model.EntityListOptions{Limit: 10, TagsMatch: "any"})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if total != 2 || len(items) != 2 || items[0].CanonicalName != "Alice" {
		t.Fatalf("entities total=%d items=%+v", total, items)
	}

	scoped, total, err := s.ListEntities(ctx, "b", model.EntityListOptions{Limit: 10, Tags: []string{"x"}, TagsMatch: "any_strict"})
	if err != nil {
		t.Fatalf("ListEntities scoped: %v", err)
	}
	if total != 1 || len(scoped) != 1 || scoped[0].CanonicalName != "Alice" || scoped[0].MentionCount != 1 {
		t.Fatalf("scoped entities total=%d items=%+v", total, scoped)
	}

	// Compound tag groups: a leaf, then an OR of a leaf and the untagged scope.
	grouped, gtotal, err := s.ListEntities(ctx, "b", model.EntityListOptions{Limit: 10, TagGroups: `[{"tags":["x"],"match":"any_strict"}]`})
	if err != nil || gtotal != 1 || len(grouped) != 1 || grouped[0].MentionCount != 1 {
		t.Fatalf("tag_groups leaf: total=%d items=%+v err=%v", gtotal, grouped, err)
	}
	compound := `[{"or":[{"tags":["x"],"match":"any_strict"},{"tags":[],"match":"exact"}]}]`
	all, atotal, err := s.ListEntities(ctx, "b", model.EntityListOptions{Limit: 10, TagGroups: compound})
	if err != nil || atotal != 2 || len(all) != 2 {
		t.Fatalf("tag_groups or: total=%d items=%+v err=%v", atotal, all, err)
	}
	if _, _, err := s.ListEntities(ctx, "b", model.EntityListOptions{Limit: 10, TagGroups: `[{"tags":["x"],"resolve":"fuzzy"}]`}); !errors.Is(err, model.ErrFuzzyTagGroups) {
		t.Fatalf("fuzzy tag group err = %v, want ErrFuzzyTagGroups", err)
	}
	// Nodes are derived from edges, so a scoped read with no co-occurring pair is empty.
	if g, err := s.EntityGraph(ctx, "b", model.EntityGraphOptions{Limit: 10, MinCount: 1, TagGroups: `[{"tags":["x"],"match":"any_strict"}]`}); err != nil || len(g.Edges) != 0 || len(g.Nodes) != 0 {
		t.Fatalf("tag_groups entity graph = %+v, %v", g, err)
	}
	ent, err := s.GetEntity(ctx, "b", "11111111-1111-1111-1111-111111111111", model.TagFilter{})
	if err != nil || ent == nil {
		t.Fatalf("GetEntity: %v %v", ent, err)
	}
	if ent.CanonicalName != "Alice" || ent.MentionCount != 2 {
		t.Fatalf("entity = %+v", ent)
	}
	if missing, err := s.GetEntity(ctx, "b", "33333333-3333-3333-3333-333333333333", model.TagFilter{}); err != nil || missing != nil {
		t.Fatalf("missing entity = %v, %v", missing, err)
	}

	g, err := s.EntityGraph(ctx, "b", model.EntityGraphOptions{Limit: 10, MinCount: 1})
	if err != nil {
		t.Fatalf("EntityGraph: %v", err)
	}
	if len(g.Nodes) != 2 || len(g.Edges) != 1 || g.Edges[0].Count != 3 {
		t.Fatalf("entity graph = %+v", g)
	}
	if g.Edges[0].Source > g.Edges[0].Target {
		t.Fatalf("edge order not normalised: %+v", g.Edges[0])
	}

	// A min_count above the stored count drops the edge but keeps the page shape.
	g, err = s.EntityGraph(ctx, "b", model.EntityGraphOptions{Limit: 10, MinCount: 9})
	if err != nil || len(g.Edges) != 0 {
		t.Fatalf("min_count graph = %+v, %v", g, err)
	}
}

func TestUpstreamDocumentChunksAndTags(t *testing.T) {
	s, ctx := newUpstreamStore(t)
	seedUpstreamGraph(t, s)

	page, err := s.DocumentChunks(ctx, "b", "doc1", 10, 0)
	if err != nil {
		t.Fatalf("DocumentChunks: %v", err)
	}
	if page == nil || page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("chunk page = %+v", page)
	}
	if page.Items[0].ChunkID != "b_doc1_0" || page.Items[1].ChunkIndex != 1 {
		t.Fatalf("chunk items = %+v", page.Items)
	}
	if missing, err := s.DocumentChunks(ctx, "b", "nope", 10, 0); err != nil || missing != nil {
		t.Fatalf("missing document page = %v, %v", missing, err)
	}

	c, err := s.Chunk(ctx, "b_doc1_1")
	if err != nil || c == nil || c.ChunkText != "second fact" || c.BankID != "b" {
		t.Fatalf("chunk = %+v, %v", c, err)
	}
	if missing, err := s.Chunk(ctx, "nope"); err != nil || missing != nil {
		t.Fatalf("missing chunk = %v, %v", missing, err)
	}

	ok, err := s.UpdateDocumentTags(ctx, "b", "doc1", []string{"fresh"})
	if err != nil || !ok {
		t.Fatalf("UpdateDocumentTags = %v, %v", ok, err)
	}
	if ok, err := s.UpdateDocumentTags(ctx, "b", "nope", []string{"fresh"}); err != nil || ok {
		t.Fatalf("UpdateDocumentTags missing = %v, %v", ok, err)
	}
	var docTags, unitTags []string
	if err := s.pool.QueryRow(ctx, `SELECT tags::text[] FROM documents WHERE id = 'doc1' AND bank_id = 'b'`).Scan(&docTags); err != nil {
		t.Fatalf("read document tags: %v", err)
	}
	if strings.Join(docTags, ",") != "fresh" {
		t.Fatalf("document tags = %v", docTags)
	}
	if err := s.pool.QueryRow(ctx, `SELECT tags::text[] FROM memory_units WHERE id = '00000000-0000-0000-0000-000000000001'`).Scan(&unitTags); err != nil {
		t.Fatalf("read unit tags: %v", err)
	}
	if strings.Join(unitTags, ",") != "fresh" {
		t.Fatalf("unit tags = %v", unitTags)
	}

	res, err := s.ReprocessDocument(ctx, "b", "doc1")
	// The retag dropped the derived observation, so two units are left.
	if err != nil || res == nil || res.ItemsCount != 2 || res.OperationID == "" {
		t.Fatalf("ReprocessDocument = %+v, %v", res, err)
	}
	if missing, err := s.ReprocessDocument(ctx, "b", "nope"); err != nil || missing != nil {
		t.Fatalf("missing reprocess = %v, %v", missing, err)
	}
}
