// Package storepg: main store implementation. Clean rewrite after sed corruption.
package storepg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EvolveHsu/hindsight-go/internal/embeddings"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// Store is the PostgreSQL-backed storage engine.
type Store struct {
	pool     *pgxpool.Pool
	upstream bool
	readOnly bool
	embedder embeddings.Embedder
	pgvector bool
}

type Option func(*Store)

func WithUpstreamSchema() Option { return func(s *Store) { s.upstream = true } }

// WithEmbedder enables real embedding generation for retain and recall.
// Without it the lite token-overlap arm remains the fallback.
func WithEmbedder(e embeddings.Embedder) Option {
	return func(s *Store) { s.embedder = e }
}

// WithReadOnlySession puts every pooled connection into a read-only session.
// Diagnostic tools use it so a write cannot reach a live database by mistake.
func WithReadOnlySession() Option { return func(s *Store) { s.readOnly = true } }

// Schema helpers: return the correct table/column names per mode.
func (s *Store) bankPK() string { return "bank_id" }
func (s *Store) unitsTable() string {
	if s.upstream {
		return "memory_units"
	}
	return "units"
}
func (s *Store) docsTable() string { return "documents" }

// Upstream tables are owned by the Python migrations, so a handful of column
// names differ from the lite DDL. These helpers keep both modes on one code
// path instead of forking every statement.
func (s *Store) mmRefreshColumn() string {
	if s.upstream {
		return "last_refreshed_at"
	}
	return "last_refreshed"
}

func (s *Store) mmSeenColumn() string {
	if s.upstream {
		return "last_memory_seen_at"
	}
	return "last_seen"
}

// mmConflictTarget is the mental-model primary key: upstream keys on
// (bank_id, id), the lite DDL keys on id alone.
func (s *Store) mmConflictTarget() string {
	if s.upstream {
		return "(bank_id, id)"
	}
	return "(id)"
}

// mmMaxTokens keeps upstream's NOT NULL default while the lite column stays
// nullable when the caller did not ask for a budget.
func (s *Store) mmMaxTokens(v int) any {
	if s.upstream && v <= 0 {
		return 2048
	}
	return nilInt(v)
}

// sourceIDsColumn names the array that links an observation back to the facts
// it was synthesised from.
func (s *Store) sourceIDsColumn() string {
	if s.upstream {
		return "source_memory_ids"
	}
	return "source_fact_ids"
}

// sourceIDsCast lets a text[] parameter land in upstream's uuid[] column.
func (s *Store) sourceIDsCast() string {
	if s.upstream {
		return "::text[]::uuid[]"
	}
	return "::text[]"
}

// observationDocumentID is NULL upstream because memory_units.document_id has
// an FK to documents; the lite table has no such FK and keeps its marker.
func (s *Store) observationDocumentID() string {
	if s.upstream {
		return "NULL"
	}
	return "'consolidated'"
}

// observationChunkID is NULL upstream: memory_units.chunk_id carries an FK to
// chunks, and the lite store writes no chunk rows.
func (s *Store) observationChunkID() string {
	if s.upstream {
		return "NULL"
	}
	return "''"
}

// docTouchSet refreshes documents.updated_at, a column only the upstream
// schema has.
func (s *Store) docTouchSet() string {
	if s.upstream {
		return ", updated_at = now()"
	}
	return ""
}

// mmTouchSet refreshes mental_models.last_updated, a column only the upstream
// schema has.
func (s *Store) mmTouchSet() string {
	if s.upstream {
		return ", last_updated = now()"
	}
	return ""
}

func New(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	s := &Store{}
	for _, o := range opts {
		o(s)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if s.readOnly {
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, "SET default_transaction_read_only = on")
			return err
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	s.pool = pool
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if s.upstream {
		if err := s.ensureUpstreamReadable(ctx); err != nil {
			pool.Close()
			return nil, err
		}
	} else {
		if err := s.migrate(ctx); err != nil {
			pool.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return s, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS banks (
    bank_id TEXT PRIMARY KEY,
    name    TEXT NOT NULL DEFAULT '',
    mission TEXT NOT NULL DEFAULT ''
);
ALTER TABLE banks ADD COLUMN IF NOT EXISTS config JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE banks ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
CREATE TABLE IF NOT EXISTS bank_aliases (
    alias      TEXT PRIMARY KEY,
    bank_id    TEXT NOT NULL REFERENCES banks(bank_id) ON DELETE CASCADE,
    is_primary BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_bank_aliases_bank ON bank_aliases (bank_id);
CREATE TABLE IF NOT EXISTS documents (
    bank_id       TEXT NOT NULL,
    id            TEXT NOT NULL,
    original_text TEXT NOT NULL DEFAULT '',
    content_hash  TEXT NOT NULL,
    tags          TEXT[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (bank_id, id)
);
CREATE TABLE IF NOT EXISTS units (
    id           TEXT PRIMARY KEY,
    bank_id      TEXT NOT NULL,
    document_id  TEXT NOT NULL,
    fact_type    TEXT NOT NULL,
    text         TEXT NOT NULL,
    context      TEXT NOT NULL DEFAULT '',
    chunk_id     TEXT NOT NULL DEFAULT '',
    tags         TEXT[] NOT NULL DEFAULT '{}',
    embedding    TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE units ADD COLUMN IF NOT EXISTS embedding TEXT;`)
	return err
}

func (s *Store) ensureUpstreamReadable(ctx context.Context) error {
	for _, q := range []string{
		`SELECT bank_id, name FROM banks LIMIT 0`,
		`SELECT id, bank_id, content_hash FROM documents LIMIT 0`,
		`SELECT id::text, bank_id, fact_type, text FROM memory_units LIMIT 0`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("upstream schema check: %w", err)
		}
	}
	var embeddingType string
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(format_type(a.atttypid, a.atttypmod), '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema()
		  AND c.relname = 'memory_units'
		  AND a.attname = 'embedding'
		  AND NOT a.attisdropped`).Scan(&embeddingType); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("upstream embedding column check: %w", err)
		}
	}
	s.pgvector = strings.Contains(embeddingType, "vector")
	return nil
}

func (s *Store) EnsureBank(ctx context.Context, id, name, mission string) (*model.Bank, error) {
	pk := s.bankPK()
	_, err := s.pool.Exec(ctx,
		fmt.Sprintf("INSERT INTO banks (%s, name, mission) VALUES ($1,$2,$3) ON CONFLICT (%s) DO NOTHING", pk, pk),
		id, name, mission)
	if err != nil {
		return nil, err
	}
	return s.GetBank(ctx, id)
}

func (s *Store) GetBank(ctx context.Context, id string) (*model.Bank, error) {
	pk := s.bankPK()
	row := s.pool.QueryRow(ctx,
		fmt.Sprintf("SELECT %s AS id, COALESCE(name,''), COALESCE(mission,'') FROM banks WHERE %s = $1", pk, pk), id)
	var b model.Bank
	if err := row.Scan(&b.ID, &b.Name, &b.Mission); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

func (s *Store) BankIDs(ctx context.Context) ([]string, error) {
	pk := s.bankPK()
	rows, err := s.pool.Query(ctx,
		fmt.Sprintf("SELECT %s AS id FROM banks ORDER BY %s", pk, pk))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) Retain(ctx context.Context, bankID string, items []model.RetainItem) (*model.RetainResult, error) {
	if b, err := s.GetBank(ctx, bankID); err != nil {
		return nil, err
	} else if b == nil {
		return nil, ErrBankNotFound
	}

	res := &model.RetainResult{BankID: bankID, ItemsCount: len(items)}
	groups := map[string][]model.RetainItem{}
	var order []string
	for _, it := range items {
		if it.DocumentID == "" {
			it.DocumentID = "auto-" + model.ContentHash(it.Content)[:12]
		}
		if _, seen := groups[it.DocumentID]; !seen {
			order = append(order, it.DocumentID)
		}
		groups[it.DocumentID] = append(groups[it.DocumentID], it)
	}

	for _, docID := range order {
		its := groups[docID]
		var b strings.Builder
		for _, it := range its {
			b.WriteString(it.Content)
			b.WriteByte('\n')
		}
		text := strings.TrimRight(b.String(), "\n")
		hash := model.ContentHash(text)

		ut := s.unitsTable()
		dt := s.docsTable()

		var existing string
		err := s.pool.QueryRow(ctx,
			fmt.Sprintf("SELECT COALESCE(content_hash,'') FROM %s WHERE bank_id = $1 AND id = $2 FOR UPDATE", dt),
			bankID, docID).Scan(&existing)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return nil, err
		case existing == hash:
			continue
		}

		vectors := make([][]float32, len(its))
		if s.embedder != nil {
			texts := make([]string, len(its))
			for i, it := range its {
				texts[i] = it.Content
			}
			var err error
			vectors, err = s.embedder.Embed(ctx, texts, embeddings.InputDocument)
			if err != nil {
				return nil, fmt.Errorf("embed retain document %q: %w", docID, err)
			}
			if len(vectors) != len(its) {
				return nil, fmt.Errorf("embed retain document %q: got %d vectors for %d items", docID, len(vectors), len(its))
			}
		}

		if existing != "" {
			if _, err := s.pool.Exec(ctx,
				fmt.Sprintf("DELETE FROM %s WHERE bank_id = $1 AND document_id = $2", ut), bankID, docID); err != nil {
				return nil, err
			}
		}

		if _, err := s.pool.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %s (id, bank_id, original_text, content_hash, tags) VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (bank_id, id) DO UPDATE SET original_text = EXCLUDED.original_text, content_hash = EXCLUDED.content_hash, tags = EXCLUDED.tags%s`, dt, s.docTouchSet()),
			docID, bankID, text, hash, normalizeTags(mergeTags(its))); err != nil {
			return nil, err
		}

		chunkID := ""
		if s.upstream {
			chunkID = "chunk-" + hash[:12]
			if _, err := s.pool.Exec(ctx, `
				INSERT INTO chunks (chunk_id, document_id, bank_id, chunk_index, chunk_text, content_hash)
				VALUES ($1,$2,$3,0,$4,$5)
				ON CONFLICT (chunk_id) DO UPDATE SET chunk_text=EXCLUDED.chunk_text, content_hash=EXCLUDED.content_hash`,
				chunkID, docID, bankID, text, hash); err != nil {
				return nil, err
			}
		}

		insertedIDs := make([]string, 0, len(its))
		prevID := ""
		for i, it := range its {
			id := model.UnitID(bankID, docID, it.Content)
			columns := "id, bank_id, document_id, fact_type, text, context, tags"
			values := "$1,$2,$3,$4,$5,$6,$7"
			args := []any{id, bankID, docID, string(it.FactType), it.Content, it.Context, normalizeTags(it.Tags)}
			if chunkID != "" {
				columns += ", chunk_id"
				values += fmt.Sprintf(",$%d", len(args)+1)
				args = append(args, chunkID)
			}
			if i < len(vectors) && len(vectors[i]) > 0 {
				columns += ", embedding"
				literal := embeddingLiteral(vectors[i])
				if s.upstream && s.pgvector {
					values += fmt.Sprintf(",'%s'::vector", literal)
				} else {
					values += fmt.Sprintf(",'%s'", literal)
				}
			}
			if _, err := s.pool.Exec(ctx,
				fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (id) DO NOTHING`, ut, columns, values),
				args...); err != nil {
				return nil, err
			}
			insertedIDs = append(insertedIDs, id)
			if s.upstream {
				if err := s.linkEntities(ctx, bankID, id, it.Entities); err != nil {
					return nil, err
				}
				if prevID != "" {
					_, _ = s.pool.Exec(ctx, `
						INSERT INTO memory_links (from_unit_id,to_unit_id,link_type,bank_id,weight)
						VALUES ($1,$2,'temporal',$3,1.0) ON CONFLICT DO NOTHING`, prevID, id, bankID)
				}
				prevID = id
			}
			res.UnitsStored++
		}
		if s.upstream {
			for i := 0; i < len(insertedIDs); i++ {
				for j := i + 1; j < len(insertedIDs); j++ {
					if i < len(vectors) && j < len(vectors) && embeddings.Cosine(vectors[i], vectors[j]) >= 0.75 {
						_, _ = s.pool.Exec(ctx, `
							INSERT INTO memory_links (from_unit_id,to_unit_id,link_type,bank_id,weight)
							VALUES ($1,$2,'semantic',$3,0.75) ON CONFLICT DO NOTHING`, insertedIDs[i], insertedIDs[j], bankID)
					}
				}
			}
		}
		res.DocumentIDs = append(res.DocumentIDs, docID)
	}
	return res, nil
}

func (s *Store) Recall(ctx context.Context, bankID string, opt model.RecallOptions) ([]model.RecallHit, error) {
	if b, err := s.GetBank(ctx, bankID); err != nil {
		return nil, err
	} else if b == nil {
		return nil, ErrBankNotFound
	}

	want := map[model.FactType]bool{}
	for _, t := range opt.Types {
		want[t] = true
	}
	if len(want) == 0 {
		want = map[model.FactType]bool{model.FactWorld: true, model.FactExperience: true, model.FactObservation: true}
	}
	types := make([]string, 0, len(want))
	for t := range want {
		types = append(types, string(t))
	}
	sort.Strings(types)

	limit := opt.Budget
	if limit <= 0 {
		limit = 20 // upstream BudgetLow
	}

	var queryVector []float32
	if s.embedder != nil {
		vectors, err := s.embedder.Embed(ctx, []string{opt.Query}, embeddings.InputQuery)
		if err != nil {
			return nil, fmt.Errorf("embed recall query: %w", err)
		}
		if len(vectors) != 1 || len(vectors[0]) == 0 {
			return nil, fmt.Errorf("embed recall query: provider returned no query vector")
		}
		queryVector = vectors[0]
	}

	semanticMin := opt.MinSemantic
	if queryVector != nil && !opt.MinSemanticSet && semanticMin <= 0 {
		semanticMin = 0.1 // upstream DEFAULT_SEMANTIC_MIN_SIMILARITY
	}

	var semantic []model.ArmResult
	var semanticErr error
	if queryVector != nil && s.upstream && s.pgvector {
		semantic, semanticErr = s.semanticVectorCandidates(ctx, bankID, types, queryVector, limit, semanticMin)
		if semanticErr != nil {
			return nil, semanticErr
		}
	}

	ut := s.unitsTable()
	embeddingExpr := "COALESCE(embedding::text,'')"
	if queryVector != nil && s.upstream && s.pgvector {
		embeddingExpr = "''"
	}
	rows, err := s.pool.Query(ctx,
		fmt.Sprintf(`SELECT id::text, bank_id, COALESCE(document_id,''), fact_type, text, COALESCE(context,''), COALESCE(chunk_id,''), COALESCE(tags::text[],'{}'), COALESCE(to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), ''), %s FROM %s WHERE bank_id = $1 AND fact_type = ANY($2)`, embeddingExpr, ut),
		bankID, types)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	q := model.Tokenize(opt.Query)
	var keyword []model.ArmResult
	for rows.Next() {
		u := &model.Unit{}
		var tags []string
		var embeddingText string
		if err := rows.Scan(&u.ID, &u.BankID, &u.DocumentID, &u.FactType, &u.Text,
			&u.Context, &u.ChunkID, &tags, &u.CreatedAt, &embeddingText); err != nil {
			return nil, err
		}
		u.Tags = tags

		switch {
		case queryVector == nil:
			if sem := model.SemanticScore(q, u.Text); sem > 0 && sem >= opt.MinSemantic {
				semantic = append(semantic, model.ArmResult{Unit: u, Score: sem})
			}
		case !s.upstream || !s.pgvector:
			if stored := parseEmbeddingLiteral(embeddingText); len(stored) > 0 {
				if sim := embeddings.Cosine(queryVector, stored); sim > 0 && sim >= semanticMin {
					semantic = append(semantic, model.ArmResult{Unit: u, Score: sim})
				}
			}
		}

		if kw := model.KeywordScore(q, u.Text); kw > 0 && kw >= opt.MinKeyword {
			keyword = append(keyword, model.ArmResult{Unit: u, Score: kw})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(semantic, func(i, j int) bool { return semantic[i].Score > semantic[j].Score })
	sort.Slice(keyword, func(i, j int) bool { return keyword[i].Score > keyword[j].Score })

	hits := model.FuseArms(semantic, keyword)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// semanticVectorCandidates runs one ANN arm per fact type against the
// upstream pgvector column. Each arm has its own ORDER BY/LIMIT so the
// per-bank partial HNSW indexes created by Python remain usable.
func (s *Store) semanticVectorCandidates(ctx context.Context, bankID string, types []string, queryVector []float32, limit int, minSimilarity float64) ([]model.ArmResult, error) {
	if limit <= 0 {
		limit = 20
	}
	ut := s.unitsTable()
	const columns = `id::text, bank_id, COALESCE(document_id,''), fact_type, text, COALESCE(context,''), COALESCE(chunk_id,''), COALESCE(tags::text[],'{}'), COALESCE(to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), '')`
	vectorLiteral := embeddingLiteral(queryVector)
	arms := make([]string, 0, len(types))
	for _, factType := range types {
		switch model.FactType(factType) {
		case model.FactWorld, model.FactExperience, model.FactObservation:
		default:
			continue
		}
		arms = append(arms, fmt.Sprintf(`(
			SELECT %s, 1 - (embedding <=> '%s'::vector) AS score
			FROM %s
			WHERE bank_id = $1
			  AND fact_type = '%s'
			  AND embedding IS NOT NULL
			  AND 1 - (embedding <=> '%s'::vector) >= $3
			ORDER BY embedding <=> '%s'::vector
			LIMIT $2
		)`, columns, vectorLiteral, ut, factType, vectorLiteral, vectorLiteral))
	}
	if len(arms) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, strings.Join(arms, " UNION ALL "), bankID, limit, minSimilarity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ArmResult
	for rows.Next() {
		u := &model.Unit{}
		var tags []string
		var score float64
		if err := rows.Scan(&u.ID, &u.BankID, &u.DocumentID, &u.FactType, &u.Text,
			&u.Context, &u.ChunkID, &tags, &u.CreatedAt, &score); err != nil {
			return nil, err
		}
		u.Tags = tags
		out = append(out, model.ArmResult{Unit: u, Score: score})
	}
	return out, rows.Err()
}

func embeddingLiteral(vector []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, value := range vector {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(value), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func parseEmbeddingLiteral(raw string) []float32 {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]float32, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 32)
		if err != nil {
			return nil
		}
		out = append(out, float32(value))
	}
	return out
}

func (s *Store) ListTags(ctx context.Context, bankID string) (map[string]int, error) {
	ut := s.unitsTable()
	rows, err := s.pool.Query(ctx,
		fmt.Sprintf("SELECT unnest(tags::text[]) AS tag, count(*) FROM %s WHERE bank_id = $1 GROUP BY unnest(tags::text[])", ut), bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var tag string
		var n int
		if err := rows.Scan(&tag, &n); err != nil {
			return nil, err
		}
		out[tag] = n
	}
	return out, rows.Err()
}

// graphCandidates expands the top semantic/keyword seeds through upstream
// memory_links. It is deliberately bounded: recall latency must not grow
// with the bank's full graph.
func (s *Store) graphCandidates(ctx context.Context, bankID string, types []string, limit int, semantic, keyword []model.ArmResult) []model.ArmResult {
	if !s.upstream || limit <= 0 {
		return nil
	}
	seeds := make([]string, 0, 10)
	seen := map[string]bool{}
	for _, arm := range [][]model.ArmResult{semantic, keyword} {
		for _, hit := range arm {
			if len(seeds) >= 10 {
				break
			}
			if !seen[hit.Unit.ID] {
				seen[hit.Unit.ID] = true
				seeds = append(seeds, hit.Unit.ID)
			}
		}
	}
	if len(seeds) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT %[1]s, GREATEST(ml.weight, 0.1) AS score
		FROM memory_links ml
		JOIN %[2]s mu ON mu.id = ml.to_unit_id AND mu.bank_id = ml.bank_id
		WHERE ml.bank_id=$1 AND ml.from_unit_id = ANY($2::uuid[])
		  AND mu.fact_type = ANY($3::text[]) AND mu.id <> ALL($2::uuid[])
		UNION ALL
		SELECT %[1]s, GREATEST(ml.weight, 0.1) AS score
		FROM memory_links ml
		JOIN %[2]s mu ON mu.id = ml.from_unit_id AND mu.bank_id = ml.bank_id
		WHERE ml.bank_id=$1 AND ml.to_unit_id = ANY($2::uuid[])
		  AND mu.fact_type = ANY($3::text[]) AND mu.id <> ALL($2::uuid[])
		LIMIT $4`,
		`mu.id::text, mu.bank_id, COALESCE(mu.document_id,''), mu.fact_type, mu.text, COALESCE(mu.context,''), COALESCE(mu.chunk_id,''), COALESCE(mu.tags::text[],'{}'), COALESCE(to_char(mu.created_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), '')`,
		s.unitsTable()), bankID, seeds, types, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []model.ArmResult{}
	for rows.Next() {
		u := &model.Unit{}
		var tags []string
		var score float64
		if err := rows.Scan(&u.ID, &u.BankID, &u.DocumentID, &u.FactType, &u.Text, &u.Context, &u.ChunkID, &tags, &u.CreatedAt, &score); err != nil {
			return nil
		}
		u.Tags = tags
		out = append(out, model.ArmResult{Unit: u, Score: score})
	}
	return out
}

func normalizeTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

func mergeTags(items []model.RetainItem) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		for _, t := range it.Tags {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}
