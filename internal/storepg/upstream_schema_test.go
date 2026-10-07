package storepg

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EvolveHsu/hindsight-go/internal/consolidate"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// upstreamTestDB keeps the upstream-shaped fixture away from the lite tables
// the other tests create in the default database.
const upstreamTestDB = "hindsight_go_upstream"

// upstreamDDL mirrors the production tables hindsight-go writes to: key
// constraints, defaults and NOT NULLs come from the live Python schema. The
// pgvector columns are TEXT stand-ins because the embedded test server has no
// pgvector build, and the store never selects them.
const upstreamDDL = `
CREATE TABLE IF NOT EXISTS banks (
    bank_id           TEXT PRIMARY KEY,
    name              TEXT,
    disposition       JSONB NOT NULL DEFAULT '{"empathy": 3, "literalism": 3, "skepticism": 3}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    mission           TEXT,
    last_consolidated_at TIMESTAMPTZ,
    mission_changed_at   TIMESTAMPTZ,
    config            JSONB NOT NULL DEFAULT '{}'::jsonb,
    internal_id       UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE
);

CREATE TABLE IF NOT EXISTS documents (
    id            TEXT NOT NULL,
    bank_id       TEXT NOT NULL,
    original_text TEXT,
    content_hash  TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    retain_params JSONB,
    tags          VARCHAR(255)[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (id, bank_id)
);

CREATE TABLE IF NOT EXISTS chunks (
    chunk_id    TEXT PRIMARY KEY,
    bank_id     TEXT NOT NULL,
    document_id TEXT NOT NULL,
    chunk_index INTEGER NOT NULL,
    chunk_text  TEXT NOT NULL,
    content_hash TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (document_id, bank_id) REFERENCES documents(id, bank_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS memory_units (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bank_id        TEXT NOT NULL,
    document_id    TEXT,
    text           TEXT NOT NULL,
    embedding      TEXT,
    context        TEXT,
    event_date     TIMESTAMPTZ,
    occurred_start TIMESTAMPTZ,
    occurred_end   TIMESTAMPTZ,
    mentioned_at   TIMESTAMPTZ,
    fact_type      TEXT NOT NULL DEFAULT 'world' CHECK (fact_type IN ('world','experience','observation')),
    metadata       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    chunk_id       TEXT REFERENCES chunks(chunk_id) ON DELETE CASCADE,
    tags           VARCHAR(255)[] NOT NULL DEFAULT '{}',
    proof_count    INTEGER,
    source_memory_ids UUID[],
    consolidated_at   TIMESTAMPTZ,
    search_vector     TSVECTOR,
    edited_at         TIMESTAMPTZ,
    attachment_ids    TEXT[] NOT NULL DEFAULT '{}',
    FOREIGN KEY (document_id, bank_id) REFERENCES documents(id, bank_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS mental_models (
    id                 TEXT NOT NULL DEFAULT gen_random_uuid()::text,
    bank_id            TEXT NOT NULL REFERENCES banks(bank_id) ON DELETE CASCADE,
    name               VARCHAR(255) NOT NULL,
    source_query       TEXT NOT NULL,
    content            TEXT NOT NULL,
    embedding          TEXT,
    tags               VARCHAR(255)[] DEFAULT '{}',
    last_refreshed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    search_vector      TSVECTOR,
    reflect_response   JSONB,
    max_tokens         INTEGER NOT NULL DEFAULT 2048,
    trigger            JSONB NOT NULL DEFAULT '{"refresh_after_consolidation": false}'::jsonb,
    structured_content JSONB,
    subtype            VARCHAR(32) NOT NULL DEFAULT 'structural'
                       CHECK (subtype IN ('structural','emergent','pinned','learned')),
    description        TEXT NOT NULL DEFAULT '',
    entity_id          UUID,
    observations       JSONB,
    links              VARCHAR(255)[],
    last_updated       TIMESTAMPTZ,
    last_memory_seen_at TIMESTAMPTZ,
    last_refresh_failed_at TIMESTAMPTZ,
    PRIMARY KEY (bank_id, id)
);

CREATE TABLE IF NOT EXISTS directives (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bank_id    TEXT NOT NULL REFERENCES banks(bank_id) ON DELETE CASCADE,
    name       VARCHAR(255) NOT NULL,
    content    TEXT NOT NULL,
    priority   INTEGER NOT NULL DEFAULT 0,
    is_active  BOOLEAN NOT NULL DEFAULT true,
    tags       VARCHAR(255)[] DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bank_aliases (
    alias      TEXT PRIMARY KEY,
    bank_id    TEXT NOT NULL REFERENCES banks(bank_id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    is_primary BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS entities (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    canonical_name TEXT NOT NULL,
    bank_id        TEXT NOT NULL
);`

// upstreamDSN returns a DSN for a second database on the embedded server.
func upstreamDSN(t *testing.T) string {
	t.Helper()
	base := testDSN(t)
	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	admin, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(context.Background())

	var exists bool
	if err := admin.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, upstreamTestDB).Scan(&exists); err != nil {
		t.Fatalf("probe database: %v", err)
	}
	if !exists {
		if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+upstreamTestDB); err != nil {
			t.Fatalf("create database: %v", err)
		}
	}
	// pgx.ConnConfig.ConnString() returns the original DSN, so rebuild the
	// URL with the fixture database swapped in.
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))),
		Path:     "/" + upstreamTestDB,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// newUpstreamStore provisions the upstream-shaped fixture and returns a store
// running in upstream mode with empty tables.
func newUpstreamStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	dsn := upstreamDSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fixture: %v", err)
	}
	var dbName string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil || dbName != upstreamTestDB {
		conn.Close(ctx)
		t.Fatalf("fixture connected to %q (%v), want %q", dbName, err, upstreamTestDB)
	}
	if _, err := conn.Exec(ctx, upstreamDDL); err != nil {
		conn.Close(ctx)
		t.Fatalf("apply upstream ddl: %v", err)
	}
	var hasUpdatedAt bool
	if err := conn.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM information_schema.columns
  WHERE table_name = 'documents' AND column_name = 'updated_at')`).Scan(&hasUpdatedAt); err != nil || !hasUpdatedAt {
		conn.Close(ctx)
		t.Fatalf("fixture documents table is not upstream-shaped (updated_at present=%v, err=%v)", hasUpdatedAt, err)
	}
	conn.Close(ctx)

	s, err := New(ctx, dsn, WithUpstreamSchema())
	if err != nil {
		t.Fatalf("open upstream store: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(ctx,
		`TRUNCATE directives, mental_models, memory_units, chunks, documents, banks CASCADE`); err != nil {
		t.Fatalf("truncate fixture: %v", err)
	}
	return s, ctx
}

// TestUpstreamSchemaHasNoLiteDDL is the P0 guard: opening the store and
// touching mental models must not create lite-only tables or columns.
func TestUpstreamSchemaHasNoLiteDDL(t *testing.T) {
	s, ctx := newUpstreamStore(t)

	if _, err := s.EnsureBank(ctx, "up-null", "", ""); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO banks (bank_id) VALUES ('up-null2')`); err != nil {
		t.Fatalf("insert null-name bank: %v", err)
	}

	b, err := s.GetBank(ctx, "up-null2")
	if err != nil {
		t.Fatalf("get bank: %v", err)
	}
	if b == nil || b.Name != "" || b.Mission != "" {
		t.Fatalf("NULL name/mission bank = %+v, want empty strings", b)
	}

	mission, err := s.BankMission(ctx, "up-null2")
	if err != nil || mission != "" {
		t.Fatalf("mission = %q, err = %v; want empty string", mission, err)
	}
	if _, err := s.BankMission(ctx, "missing-bank"); err != ErrBankNotFound {
		t.Fatalf("missing bank mission err = %v, want ErrBankNotFound", err)
	}

	if _, err := s.ListMentalModels(ctx, "up-null"); err != nil {
		t.Fatalf("list mental models: %v", err)
	}

	var liteCols int
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'mental_models'
  AND column_name IN ('last_refreshed','last_seen')`).Scan(&liteCols); err != nil {
		t.Fatalf("count lite columns: %v", err)
	}
	if liteCols != 0 {
		t.Errorf("upstream mental_models gained %d lite-only columns", liteCols)
	}

	var unitsRegclass *string
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.units')::text`).Scan(&unitsRegclass); err != nil {
		t.Fatalf("probe lite units table: %v", err)
	}
	if unitsRegclass != nil {
		t.Errorf("upstream mode created a lite units table: %s", *unitsRegclass)
	}
}

func TestUpstreamMentalModelLifecycle(t *testing.T) {
	s, ctx := newUpstreamStore(t)
	if _, err := s.EnsureBank(ctx, "mm-bank", "MM", "mission"); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}

	created, err := s.CreateMentalModel(ctx, "mm-bank", &model.MentalModel{
		ID: "mm-1", Name: "Deploys", SourceQuery: "how do deploys work?", Content: "blue/green",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.CreatedAt == "" {
		t.Fatalf("created_at not stamped")
	}

	got, err := s.GetMentalModel(ctx, "mm-bank", "mm-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.Name != "Deploys" || got.MaxTokens != 2048 {
		t.Fatalf("get = %+v, want name Deploys and default max_tokens 2048", got)
	}
	if got.LastRefreshed == "" {
		t.Errorf("last_refreshed_at not projected from upstream default")
	}

	if _, err := s.CreateMentalModel(ctx, "mm-bank", &model.MentalModel{
		ID: "mm-2", Name: "Owners", SourceQuery: "who owns what?", Content: "alice",
	}); err != nil {
		t.Fatalf("create second: %v", err)
	}
	list, err := s.ListMentalModels(ctx, "mm-bank")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}

	stamp := time.Now().UTC().Format(time.RFC3339)
	updated, err := s.UpdateMentalModel(ctx, "mm-bank", "mm-1", func(m *model.MentalModel) {
		m.Name = "Deploys v2"
		m.MaxTokens = 512
		m.LastSeen = stamp
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Deploys v2" || updated.MaxTokens != 512 {
		t.Fatalf("updated = %+v", updated)
	}
	reloaded, err := s.GetMentalModel(ctx, "mm-bank", "mm-1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.LastSeen == "" {
		t.Errorf("last_memory_seen_at did not round-trip")
	}

	var subtype, description string
	var triggerRefreshAfter bool
	if err := s.pool.QueryRow(ctx, `
SELECT subtype, description, (trigger->>'refresh_after_consolidation')::boolean
FROM mental_models WHERE bank_id = $1 AND id = $2`, "mm-bank", "mm-1").
		Scan(&subtype, &description, &triggerRefreshAfter); err != nil {
		t.Fatalf("read upstream defaults: %v", err)
	}
	if subtype != "structural" || description != "" || triggerRefreshAfter {
		t.Errorf("upstream defaults = %q/%q/%v", subtype, description, triggerRefreshAfter)
	}

	ok, err := s.DeleteMentalModel(ctx, "mm-bank", "mm-1")
	if err != nil || !ok {
		t.Fatalf("delete = %v, %v; want true", ok, err)
	}
	ok, err = s.DeleteMentalModel(ctx, "mm-bank", "mm-1")
	if err != nil || ok {
		t.Fatalf("second delete = %v, %v; want false", ok, err)
	}
}

func TestUpstreamDirectivesAreUuidAndIdempotent(t *testing.T) {
	s, ctx := newUpstreamStore(t)
	if _, err := s.EnsureBank(ctx, "dir-bank", "D", ""); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}

	first, err := s.CreateDirective(ctx, "dir-bank", "tone", "be terse")
	if err != nil {
		t.Fatalf("create directive: %v", err)
	}
	if !strings.Contains(first.ID, "-") {
		t.Errorf("directive id %q does not look like a uuid", first.ID)
	}
	again, err := s.CreateDirective(ctx, "dir-bank", "tone", "be terse")
	if err != nil {
		t.Fatalf("create directive twice: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("duplicate create returned %q, want %q", again.ID, first.ID)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM directives WHERE bank_id = 'dir-bank'`).Scan(&count); err != nil {
		t.Fatalf("count directives: %v", err)
	}
	if count != 1 {
		t.Errorf("directive rows = %d, want 1", count)
	}

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO directives (bank_id, name, content, priority) VALUES ('dir-bank','priority','always check',5)`); err != nil {
		t.Fatalf("insert high-priority directive: %v", err)
	}
	list, err := s.ListDirectives(ctx, "dir-bank")
	if err != nil {
		t.Fatalf("list directives: %v", err)
	}
	if len(list) != 2 || list[0].Name != "priority" {
		t.Fatalf("directives = %+v, want priority first", list)
	}
}

func TestUpstreamRetainRecallAndDocuments(t *testing.T) {
	s, ctx := newUpstreamStore(t)
	const bank = "up-retain"
	if _, err := s.EnsureBank(ctx, bank, bank, ""); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}

	items := []model.RetainItem{
		{Content: "The quarterly report shipped on Friday.", FactType: model.FactExperience, Tags: []string{"work"}},
		{Content: "Alice owns the cello.", FactType: model.FactExperience, Tags: []string{"music"}},
	}
	res, err := s.Retain(ctx, bank, items)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if res.UnitsStored != 2 {
		t.Fatalf("units stored = %d, want 2", res.UnitsStored)
	}
	// Replaying the same payload must stay idempotent through content_hash.
	if _, err := s.Retain(ctx, bank, items); err != nil {
		t.Fatalf("replay retain: %v", err)
	}

	units, total, err := s.ListMemories(ctx, bank, 50, 0)
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	if total != 2 || len(units) != 2 {
		t.Fatalf("memories = %d/%d, want 2/2", len(units), total)
	}

	var target model.Unit
	for _, u := range units {
		if strings.HasPrefix(u.Text, "The quarterly") {
			target = u
		}
	}
	if target.ID == "" {
		t.Fatalf("quarterly fact not found in %+v", units)
	}
	got, err := s.GetMemory(ctx, bank, target.ID)
	if err != nil || got == nil || got.Text != target.Text {
		t.Fatalf("get memory = %+v, %v", got, err)
	}

	updated, err := s.UpdateMemory(ctx, bank, target.ID, func(u *model.Unit) {
		u.Text = "The quarterly report shipped on Friday afternoon."
	})
	if err != nil || updated == nil {
		t.Fatalf("update memory: %v", err)
	}
	reloaded, err := s.GetMemory(ctx, bank, target.ID)
	if err != nil || reloaded.Text != updated.Text {
		t.Fatalf("reload memory = %+v, %v", reloaded, err)
	}
	var edited bool
	if err := s.pool.QueryRow(ctx, `SELECT edited_at IS NOT NULL FROM memory_units WHERE id = $1`, target.ID).Scan(&edited); err != nil {
		t.Fatalf("probe edited_at: %v", err)
	}
	if !edited {
		t.Errorf("upstream edit did not stamp memory_units.edited_at")
	}

	hits, err := s.Recall(ctx, bank, model.RecallOptions{Query: "quarterly report"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("recall returned no hits")
	}

	tags, err := s.ListTags(ctx, bank)
	if err != nil || tags["work"] != 1 || tags["music"] != 1 {
		t.Fatalf("tags = %v, err = %v", tags, err)
	}

	docs, docTotal, err := s.ListDocuments(ctx, bank, 50, 0)
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	// Items without an explicit document_id each become their own
	// content-addressed document, so the retain above created two.
	if docTotal != 2 || len(docs) != 2 {
		t.Fatalf("documents = %d/%d, want 2/2", len(docs), docTotal)
	}
	for i, d := range docs {
		doc, err := s.GetDocument(ctx, bank, d.ID)
		if err != nil || doc == nil {
			t.Fatalf("get document %d = %+v, %v", i, doc, err)
		}
		ok, err := s.DeleteDocument(ctx, bank, d.ID)
		if err != nil || !ok {
			t.Fatalf("delete document %d = %v, %v", i, ok, err)
		}
	}
	if _, total, err := s.ListMemories(ctx, bank, 50, 0); err != nil || total != 0 {
		t.Fatalf("after document delete total = %d, err = %v", total, err)
	}
}

func TestUpstreamConsolidationUsesSourceMemoryIds(t *testing.T) {
	s, ctx := newUpstreamStore(t)
	const bank = "up-consolidate"
	if _, err := s.EnsureBank(ctx, bank, bank, ""); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}
	if _, err := s.Retain(ctx, bank, []model.RetainItem{
		{Content: "Alice ships the report on Friday.", FactType: model.FactExperience},
	}); err != nil {
		t.Fatalf("retain: %v", err)
	}

	facts, err := s.UnconsolidatedFacts(ctx, bank, 10)
	if err != nil {
		t.Fatalf("unconsolidated: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("unconsolidated = %d, want 1", len(facts))
	}
	factID := facts[0].ID

	if err := s.ApplyBatch(ctx, bank, consolidate.Batch{
		Creates: []consolidate.CreateAction{{
			Text:          "Alice ships reports on Fridays.",
			SourceFactIDs: []string{factID},
		}},
	}); err != nil {
		t.Fatalf("apply batch: %v", err)
	}

	obs, err := s.Observations(ctx, bank, 10)
	if err != nil {
		t.Fatalf("observations: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("observations = %d, want 1", len(obs))
	}

	var documentID *string
	var sources []string
	if err := s.pool.QueryRow(ctx,
		`SELECT document_id, source_memory_ids::text[] FROM memory_units WHERE id = $1`, obs[0].ID).
		Scan(&documentID, &sources); err != nil {
		t.Fatalf("read observation row: %v", err)
	}
	if documentID != nil {
		t.Errorf("observation document_id = %q, want NULL (FK to documents)", *documentID)
	}
	if len(sources) != 1 || sources[0] != factID {
		t.Errorf("source_memory_ids = %v, want [%s]", sources, factID)
	}

	remaining, err := s.UnconsolidatedFacts(ctx, bank, 10)
	if err != nil {
		t.Fatalf("unconsolidated after batch: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("source fact still unconsolidated: %+v", remaining)
	}

	cleared, err := s.ClearMemoryObservations(ctx, bank, factID)
	if err != nil || cleared != 1 {
		t.Fatalf("clear observations = %d, %v; want 1", cleared, err)
	}
	if obs, err := s.Observations(ctx, bank, 10); err != nil || len(obs) != 0 {
		t.Fatalf("observations after clear = %d, %v", len(obs), err)
	}

	deleted, err := s.ClearBankMemories(ctx, bank)
	if err != nil || deleted != 1 {
		t.Fatalf("clear bank memories = %d, %v; want 1", deleted, err)
	}
}

// TestReadOnlySessionRejectsWrites pins the guard the diagnostic probe relies
// on: a store opened read-only must fail loudly instead of writing.
func TestReadOnlySessionRejectsWrites(t *testing.T) {
	_, ctx := newUpstreamStore(t)
	ro, err := New(ctx, upstreamDSN(t), WithUpstreamSchema(), WithReadOnlySession())
	if err != nil {
		t.Fatalf("open read-only store: %v", err)
	}
	defer ro.Close()

	if _, err := ro.EnsureBank(ctx, "readonly-bank", "RO", ""); err == nil {
		t.Fatalf("write on a read-only session succeeded")
	}
}

// TestUpstreamUuidRoundTrip pins the id encoding assumption: the 32-hex ids
// model.UnitID generates must bind to upstream uuid columns.
func TestUpstreamUuidRoundTrip(t *testing.T) {
	s, ctx := newUpstreamStore(t)
	if _, err := s.EnsureBank(ctx, "uuid-bank", "U", ""); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}
	if _, err := s.Retain(ctx, "uuid-bank", []model.RetainItem{
		{Content: "uuid round trip", FactType: model.FactWorld},
	}); err != nil {
		t.Fatalf("retain: %v", err)
	}
	units, _, err := s.ListMemories(ctx, "uuid-bank", 10, 0)
	if err != nil || len(units) != 1 {
		t.Fatalf("list = %d, %v", len(units), err)
	}
	var dashes int
	if err := s.pool.QueryRow(ctx,
		`SELECT length(id::text) FROM memory_units WHERE id = $1`, units[0].ID).Scan(&dashes); err != nil {
		t.Fatalf("select by raw id: %v", err)
	}
	if dashes != 36 {
		t.Errorf("uuid text length = %s, want 36", strconv.Itoa(dashes))
	}
}
