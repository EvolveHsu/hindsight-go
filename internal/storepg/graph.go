package storepg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// The graph and entity reads sit on the upstream schema: entities /
// unit_entities / entity_cooccurrences / memory_links. The lite schema ships
// none of those tables, so its graph carries the memory nodes only and the
// entity endpoints answer empty pages instead of failing.

const graphTSFormat = `'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'`

// tagsMatchSQL renders the upstream tags filter for one column. It returns the
// fragment (no leading AND) and whether the caller must bind the tags value at
// the given parameter index.
func tagsMatchSQL(column string, tags []string, match string, param int) (string, bool) {
	if match == "exact" && len(tags) == 0 {
		return fmt.Sprintf("(%s IS NULL OR %s = '{}')", column, column), false
	}
	if len(tags) == 0 {
		return "", false
	}
	p := fmt.Sprintf("$%d::varchar[]", param)
	switch match {
	case "exact":
		return fmt.Sprintf("(%s @> %s AND %s <@ %s)", column, p, column, p), true
	case "all":
		return fmt.Sprintf("(%s IS NULL OR %s = '{}' OR %s @> %s)", column, column, column, p), true
	case "all_strict":
		return fmt.Sprintf("(%s IS NOT NULL AND %s != '{}' AND %s @> %s)", column, column, column, p), true
	case "any_strict":
		return fmt.Sprintf("(%s IS NOT NULL AND %s != '{}' AND %s && %s)", column, column, column, p), true
	default: // "any" and unknown modes: OR matching, untagged rows included.
		return fmt.Sprintf("(%s IS NULL OR %s = '{}' OR %s && %s)", column, column, column, p), true
	}
}

// tagFilterActive mirrors upstream: exact with no tags still selects the
// untagged (global) scope, every other mode with no tags is no filter.
func tagFilterActive(tags []string, match string) bool {
	return len(tags) > 0 || match == "exact"
}

// GraphData implements the storage side of GET /graph.
func (s *Store) GraphData(ctx context.Context, bankID string, opt model.GraphOptions) (*model.GraphData, error) {
	if !s.upstream {
		return s.liteGraphData(ctx, bankID, opt)
	}
	conds := []string{"bank_id = $1"}
	params := []any{bankID}
	add := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}
	if opt.FactType != "" {
		conds = append(conds, "fact_type = "+add(opt.FactType))
	}
	if opt.DocumentID != "" {
		p := add(opt.DocumentID)
		conds = append(conds, fmt.Sprintf(
			"(document_id = %s OR (fact_type = 'observation' AND source_memory_ids && (SELECT COALESCE(array_agg(id), '{}'::uuid[]) FROM memory_units WHERE document_id = %s AND bank_id = $1)))",
			p, p))
	}
	if opt.ChunkID != "" {
		p := add(opt.ChunkID)
		conds = append(conds, fmt.Sprintf(
			"(chunk_id = %s OR (fact_type = 'observation' AND source_memory_ids && (SELECT COALESCE(array_agg(id), '{}'::uuid[]) FROM memory_units WHERE chunk_id = %s AND bank_id = $1)))",
			p, p))
	}
	if opt.Query != "" {
		p := add("%" + opt.Query + "%")
		conds = append(conds, fmt.Sprintf("(text ILIKE %s OR context ILIKE %s)", p, p))
	}
	if clause, bind := tagsMatchSQL("tags", opt.Tags, opt.TagsMatch, len(params)+1); clause != "" {
		conds = append(conds, clause)
		if bind {
			params = append(params, opt.Tags)
		}
	}
	where := "WHERE " + strings.Join(conds, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM memory_units "+where, params...).Scan(&total); err != nil {
		return nil, err
	}

	limit := opt.Limit
	if limit <= 0 {
		limit = 1000
	}
	pageParams := append(append([]any{}, params...), limit)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT id::text, text, COALESCE(context,''), fact_type, COALESCE(document_id,''), COALESCE(chunk_id,''),
		       COALESCE(tags::text[], '{}'),
		       COALESCE(to_char(created_at, %[1]s), ''),
		       COALESCE(to_char(mentioned_at, %[1]s), ''),
		       COALESCE(to_char(occurred_start, %[1]s), ''),
		       COALESCE(to_char(occurred_end, %[1]s), ''),
		       COALESCE(to_char(event_date, %[1]s), ''),
		       COALESCE(proof_count, 0),
		       COALESCE(source_memory_ids::text[], '{}')
		FROM memory_units
		%[2]s
		ORDER BY mentioned_at DESC NULLS LAST, event_date DESC
		LIMIT $%[3]d`, graphTSFormat, where, len(pageParams)), pageParams...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := &model.GraphData{TotalUnits: total}
	for rows.Next() {
		u := model.GraphUnit{}
		if err := rows.Scan(&u.ID, &u.Text, &u.Context, &u.FactType, &u.DocumentID, &u.ChunkID, &u.Tags,
			&u.CreatedAt, &u.MentionedAt, &u.OccurredStart, &u.OccurredEnd, &u.EventDate, &u.ProofCount, &u.SourceMemoryIDs); err != nil {
			return nil, err
		}
		data.Units = append(data.Units, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Entity postings and memory links cover the visible units *and* the source
	// memories observations inherit from, exactly like upstream passes both.
	allIDs := make([]string, 0, len(data.Units))
	for _, u := range data.Units {
		allIDs = append(allIDs, u.ID)
		allIDs = append(allIDs, u.SourceMemoryIDs...)
	}
	allIDs = dedupe(allIDs)
	if len(allIDs) > 0 {
		erows, err := s.pool.Query(ctx, `
			SELECT ue.unit_id::text, e.canonical_name
			FROM unit_entities ue
			JOIN entities e ON e.id = ue.entity_id
			WHERE ue.unit_id = ANY($1::text[]::uuid[])
			ORDER BY ue.unit_id, e.canonical_name`, allIDs)
		if err != nil {
			return nil, err
		}
		defer erows.Close()
		for erows.Next() {
			var row model.GraphEntityRow
			if err := erows.Scan(&row.UnitID, &row.CanonicalName); err != nil {
				return nil, err
			}
			data.EntityRows = append(data.EntityRows, row)
		}
		if err := erows.Err(); err != nil {
			return nil, err
		}

		lrows, err := s.pool.Query(ctx, `
			SELECT from_unit_id::text, to_unit_id::text, link_type, COALESCE(weight, 1.0)
			FROM memory_links
			WHERE from_unit_id = ANY($1::text[]::uuid[]) AND to_unit_id = ANY($1::text[]::uuid[])
			ORDER BY weight DESC NULLS LAST
			LIMIT 10000`, allIDs)
		if err != nil {
			return nil, err
		}
		defer lrows.Close()
		for lrows.Next() {
			var l model.GraphLink
			if err := lrows.Scan(&l.FromUnitID, &l.ToUnitID, &l.LinkType, &l.Weight); err != nil {
				return nil, err
			}
			data.Links = append(data.Links, l)
		}
		if err := lrows.Err(); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// liteGraphData is the same read against the lite `units` table: nodes and
// table rows work, entity-derived edges do not exist in that schema.
func (s *Store) liteGraphData(ctx context.Context, bankID string, opt model.GraphOptions) (*model.GraphData, error) {
	conds := []string{"bank_id = $1"}
	params := []any{bankID}
	if opt.FactType != "" {
		params = append(params, opt.FactType)
		conds = append(conds, fmt.Sprintf("fact_type = $%d", len(params)))
	}
	if opt.DocumentID != "" {
		params = append(params, opt.DocumentID)
		conds = append(conds, fmt.Sprintf("document_id = $%d", len(params)))
	}
	if opt.ChunkID != "" {
		params = append(params, opt.ChunkID)
		conds = append(conds, fmt.Sprintf("chunk_id = $%d", len(params)))
	}
	if opt.Query != "" {
		params = append(params, "%"+opt.Query+"%")
		conds = append(conds, fmt.Sprintf("(text ILIKE $%d OR context ILIKE $%d)", len(params), len(params)))
	}
	if clause, bind := tagsMatchSQL("tags", opt.Tags, opt.TagsMatch, len(params)+1); clause != "" {
		conds = append(conds, clause)
		if bind {
			params = append(params, opt.Tags)
		}
	}
	where := "WHERE " + strings.Join(conds, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM units "+where, params...).Scan(&total); err != nil {
		return nil, err
	}
	limit := opt.Limit
	if limit <= 0 {
		limit = 1000
	}
	pageParams := append(append([]any{}, params...), limit)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT id::text, text, COALESCE(context,''), fact_type, COALESCE(document_id,''), COALESCE(chunk_id,''),
		       COALESCE(tags::text[], '{}'),
		       COALESCE(to_char(created_at, %[1]s), '')
		FROM units
		%[2]s
		ORDER BY created_at DESC
		LIMIT $%[3]d`, graphTSFormat, where, len(pageParams)), pageParams...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := &model.GraphData{TotalUnits: total}
	for rows.Next() {
		u := model.GraphUnit{}
		if err := rows.Scan(&u.ID, &u.Text, &u.Context, &u.FactType, &u.DocumentID, &u.ChunkID, &u.Tags, &u.CreatedAt); err != nil {
			return nil, err
		}
		data.Units = append(data.Units, u)
	}
	return data, rows.Err()
}

// ListEntities implements GET /entities.
func (s *Store) ListEntities(ctx context.Context, bankID string, opt model.EntityListOptions) ([]model.EntityInfo, int, error) {
	if !s.upstream {
		return nil, 0, nil
	}
	params := []any{bankID}
	if entityTagFilterActive(opt.Tags, opt.TagsMatch, opt.TagGroups) {
		return s.listEntitiesTagFiltered(ctx, bankID, opt)
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT id::text, canonical_name, mention_count,
		       COALESCE(to_char(first_seen, %[1]s), ''), COALESCE(to_char(last_seen, %[1]s), '')
		FROM entities
		WHERE bank_id = $1
		ORDER BY mention_count DESC, last_seen DESC, id ASC
		LIMIT $2 OFFSET $3`, graphTSFormat), bankID, opt.Limit, opt.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.EntityInfo
	for rows.Next() {
		var e model.EntityInfo
		if err := rows.Scan(&e.ID, &e.CanonicalName, &e.MentionCount, &e.FirstSeen, &e.LastSeen); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM entities WHERE bank_id = $1", params...).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// listEntitiesTagFiltered recomputes counts and dates from the memories the tag
// filter lets through, so a scoped reader never sees out-of-scope totals.
func (s *Store) listEntitiesTagFiltered(ctx context.Context, bankID string, opt model.EntityListOptions) ([]model.EntityInfo, int, error) {
	clause, extra, err := memoryTagFilter(opt.Tags, opt.TagsMatch, opt.TagGroups)
	if err != nil {
		return nil, 0, err
	}
	if clause == "" {
		clause = "true"
	}
	params := append([]any{bankID}, extra...)
	stats := fmt.Sprintf(`
		SELECT ue.entity_id,
		       COUNT(*) AS mention_count,
		       MIN(COALESCE(mu.occurred_start, mu.mentioned_at, mu.event_date)) AS first_seen,
		       MAX(COALESCE(mu.occurred_start, mu.mentioned_at, mu.event_date)) AS last_seen
		FROM unit_entities ue
		JOIN memory_units mu ON mu.id = ue.unit_id
		WHERE mu.bank_id = $1 AND %s
		GROUP BY ue.entity_id`, clause)
	from := `FROM entities e JOIN (` + stats + `) s ON s.entity_id = e.id WHERE e.bank_id = $1`

	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) "+from, params...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pageParams := append(append([]any{}, params...), opt.Limit, opt.Offset)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT e.id::text, e.canonical_name, s.mention_count,
		       COALESCE(to_char(s.first_seen, %[1]s), ''), COALESCE(to_char(s.last_seen, %[1]s), '')
		%[2]s
		ORDER BY s.mention_count DESC, s.last_seen DESC, e.id ASC
		LIMIT $%[3]d OFFSET $%[4]d`, graphTSFormat, from, len(pageParams)-1, len(pageParams)), pageParams...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.EntityInfo
	for rows.Next() {
		var e model.EntityInfo
		if err := rows.Scan(&e.ID, &e.CanonicalName, &e.MentionCount, &e.FirstSeen, &e.LastSeen); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// GetEntity implements GET /entities/{entity_id}.
func (s *Store) GetEntity(ctx context.Context, bankID, entityID string, scope model.TagFilter) (*model.EntityDetail, error) {
	if !s.upstream {
		return nil, nil
	}
	if entityTagFilterActive(scope.Tags, scope.TagsMatch, scope.TagGroups) {
		clause, extra, err := memoryTagFilter(scope.Tags, scope.TagsMatch, scope.TagGroups)
		if err != nil {
			return nil, err
		}
		if clause == "" {
			clause = "true"
		}
		params := append([]any{bankID}, extra...)
		params = append(params, entityID)
		stats := fmt.Sprintf(`
			SELECT ue.entity_id,
			       COUNT(*) AS mention_count,
			       MIN(COALESCE(mu.occurred_start, mu.mentioned_at, mu.event_date)) AS first_seen,
			       MAX(COALESCE(mu.occurred_start, mu.mentioned_at, mu.event_date)) AS last_seen
			FROM unit_entities ue
			JOIN memory_units mu ON mu.id = ue.unit_id
			WHERE mu.bank_id = $1 AND %s
			GROUP BY ue.entity_id`, clause)
		row := s.pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT e.id::text, e.canonical_name, s.mention_count,
			       COALESCE(to_char(s.first_seen, %[1]s), ''), COALESCE(to_char(s.last_seen, %[1]s), '')
			FROM entities e JOIN (%[2]s) s ON s.entity_id = e.id
			WHERE e.bank_id = $1 AND e.id = $%[3]d::uuid`, graphTSFormat, stats, len(params)), params...)
		return scanEntityDetail(row)
	}
	row := s.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT id::text, canonical_name, mention_count,
		       COALESCE(to_char(first_seen, %[1]s), ''), COALESCE(to_char(last_seen, %[1]s), '')
		FROM entities WHERE bank_id = $1 AND id = $2::uuid`, graphTSFormat), bankID, entityID)
	return scanEntityDetail(row)
}

func scanEntityDetail(row pgx.Row) (*model.EntityDetail, error) {
	e := &model.EntityDetail{}
	if err := row.Scan(&e.ID, &e.CanonicalName, &e.MentionCount, &e.FirstSeen, &e.LastSeen); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	e.Observations = []model.EntityObservation{}
	return e, nil
}

// EntityGraph implements GET /entities/graph.
func (s *Store) EntityGraph(ctx context.Context, bankID string, opt model.EntityGraphOptions) (*model.EntityGraph, error) {
	if !s.upstream {
		return &model.EntityGraph{}, nil
	}
	if entityTagFilterActive(opt.Tags, opt.TagsMatch, opt.TagGroups) {
		return s.entityGraphTagFiltered(ctx, bankID, opt)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ec.entity_id_1::text, ec.entity_id_2::text, ec.cooccurrence_count,
		       COALESCE(to_char(ec.last_cooccurred, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), ''),
		       e1.canonical_name, e1.mention_count,
		       e2.canonical_name, e2.mention_count
		FROM entity_cooccurrences ec
		JOIN entities e1 ON e1.id = ec.entity_id_1
		JOIN entities e2 ON e2.id = ec.entity_id_2
		WHERE e1.bank_id = $1 AND e2.bank_id = $1 AND ec.cooccurrence_count >= $2
		ORDER BY ec.cooccurrence_count DESC, ec.last_cooccurred DESC
		LIMIT $3`, bankID, opt.MinCount, opt.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edgeRows []entityEdgeRow
	for rows.Next() {
		var r entityEdgeRow
		if err := rows.Scan(&r.ID1, &r.ID2, &r.Count, &r.LastCooccurred, &r.Name1, &r.Count1, &r.Name2, &r.Count2); err != nil {
			return nil, err
		}
		edgeRows = append(edgeRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return renderEntityEdgeRows(edgeRows), nil
}

// entityGraphTagFiltered recomputes pairs from the memories the filter lets
// through; the materialised co-occurrence table carries no tags.
func (s *Store) entityGraphTagFiltered(ctx context.Context, bankID string, opt model.EntityGraphOptions) (*model.EntityGraph, error) {
	clause, extra, err := memoryTagFilter(opt.Tags, opt.TagsMatch, opt.TagGroups)
	if err != nil {
		return nil, err
	}
	if clause == "" {
		clause = "true"
	}
	params := append([]any{bankID}, extra...)
	params = append(params, opt.MinCount, opt.Limit)
	minParam, limitParam := len(params)-1, len(params)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		WITH ve AS (
			SELECT ue.unit_id, ue.entity_id, COALESCE(mu.occurred_start, mu.mentioned_at, mu.event_date) AS seen_at
			FROM unit_entities ue
			JOIN memory_units mu ON mu.id = ue.unit_id
			WHERE mu.bank_id = $1 AND %[1]s
		),
		pairs AS (
			SELECT a.entity_id AS entity_id_1, b.entity_id AS entity_id_2,
			       COUNT(*) AS cooccurrence_count, MAX(a.seen_at) AS last_cooccurred
			FROM ve a JOIN ve b ON b.unit_id = a.unit_id AND a.entity_id < b.entity_id
			GROUP BY a.entity_id, b.entity_id
			HAVING COUNT(*) >= $%[2]d
		),
		stats AS (SELECT entity_id, COUNT(*) AS mention_count FROM ve GROUP BY entity_id)
		SELECT p.entity_id_1::text, p.entity_id_2::text, p.cooccurrence_count,
		       COALESCE(to_char(p.last_cooccurred, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'), ''),
		       e1.canonical_name, s1.mention_count, e2.canonical_name, s2.mention_count
		FROM pairs p
		JOIN entities e1 ON e1.id = p.entity_id_1
		JOIN entities e2 ON e2.id = p.entity_id_2
		JOIN stats s1 ON s1.entity_id = p.entity_id_1
		JOIN stats s2 ON s2.entity_id = p.entity_id_2
		ORDER BY p.cooccurrence_count DESC, p.last_cooccurred DESC
		LIMIT $%[3]d`, clause, minParam, limitParam), params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edgeRows []entityEdgeRow
	for rows.Next() {
		var r entityEdgeRow
		if err := rows.Scan(&r.ID1, &r.ID2, &r.Count, &r.LastCooccurred, &r.Name1, &r.Count1, &r.Name2, &r.Count2); err != nil {
			return nil, err
		}
		edgeRows = append(edgeRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return renderEntityEdgeRows(edgeRows), nil
}

type entityEdgeRow struct {
	ID1, ID2              string
	Name1, Name2          string
	Count1, Count2, Count int
	LastCooccurred        string
}

// renderEntityEdgeRows mirrors upstream _render_entity_graph.
func renderEntityEdgeRows(rows []entityEdgeRow) *model.EntityGraph {
	g := &model.EntityGraph{}
	seen := map[string]bool{}
	for _, r := range rows {
		for _, n := range []model.EntityGraphNode{
			{ID: r.ID1, Label: r.Name1, MentionCount: r.Count1},
			{ID: r.ID2, Label: r.Name2, MentionCount: r.Count2},
		} {
			if !seen[n.ID] {
				seen[n.ID] = true
				g.Nodes = append(g.Nodes, n)
			}
		}
		g.Edges = append(g.Edges, model.EntityGraphEdge{
			Source: r.ID1, Target: r.ID2, Count: r.Count, LastCooccurred: r.LastCooccurred,
		})
	}
	if g.Nodes == nil {
		g.Nodes = []model.EntityGraphNode{}
	}
	if g.Edges == nil {
		g.Edges = []model.EntityGraphEdge{}
	}
	return g
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
