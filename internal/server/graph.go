package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// GraphSource is the storage surface behind the graph and entity endpoints.
// Both backends implement it; the lite schema has no entity registry, so its
// implementation answers the memory graph and reports empty entity pages.
type GraphSource interface {
	GraphData(ctx context.Context, bankID string, opt model.GraphOptions) (*model.GraphData, error)
	ListEntities(ctx context.Context, bankID string, opt model.EntityListOptions) ([]model.EntityInfo, int, error)
	GetEntity(ctx context.Context, bankID, entityID string, scope model.TagFilter) (*model.EntityDetail, error)
	EntityGraph(ctx context.Context, bankID string, opt model.EntityGraphOptions) (*model.EntityGraph, error)
}

func (e *Engine) graphSource() (GraphSource, error) {
	if s, ok := e.store.(GraphSource); ok {
		return s, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support graph reads"}}
}

// GetGraph implements get_graph (GET /banks/{bank_id}/graph). It returns the
// Cytoscape nodes/edges plus the flat table rows the control plane renders.
func (e *Engine) GetGraph(ctx context.Context, params api.GetGraphParams) (api.GetGraphRes, error) {
	src, err := e.graphSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	limit := params.Limit.Or(1000)
	if limit < 0 {
		return nil, badRequest("limit must be >= 0")
	}
	opt := model.GraphOptions{
		FactType:   params.Type.Or(""),
		Limit:      limit,
		Query:      params.Q.Or(""),
		DocumentID: params.DocumentID.Or(""),
		ChunkID:    params.ChunkID.Or(""),
		Tags:       params.Tags,
		TagsMatch:  params.TagsMatch.Or("all_strict"),
	}
	data, err := src.GraphData(ctx, params.BankID, opt)
	if err != nil {
		return nil, internal(err)
	}
	return renderGraphData(data, limit), nil
}

// ListEntities implements list_entities (GET /entities).
func (e *Engine) ListEntities(ctx context.Context, params api.ListEntitiesParams) (api.ListEntitiesRes, error) {
	src, err := e.graphSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	limit, offset := params.Limit.Or(100), params.Offset.Or(0)
	opt := model.EntityListOptions{
		Limit:     limit,
		Offset:    offset,
		Tags:      params.Tags,
		TagsMatch: string(params.TagsMatch.Or(api.ListEntitiesTagsMatchAny)),
		TagGroups: params.TagGroups.Or(""),
	}
	items, total, err := src.ListEntities(ctx, params.BankID, opt)
	if err != nil {
		return nil, mapTagErr(err)
	}
	out := make([]api.EntityListItem, 0, len(items))
	for _, it := range items {
		out = append(out, api.EntityListItem{
			ID:            it.ID,
			CanonicalName: it.CanonicalName,
			MentionCount:  it.MentionCount,
			FirstSeen:     optString(it.FirstSeen),
			LastSeen:      optString(it.LastSeen),
		})
	}
	return &api.EntityListResponse{Items: out, Total: total, Limit: limit, Offset: offset}, nil
}

// GetEntityGraph implements get_entity_graph (GET /entities/graph).
func (e *Engine) GetEntityGraph(ctx context.Context, params api.GetEntityGraphParams) (api.GetEntityGraphRes, error) {
	src, err := e.graphSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	limit, minCount := params.Limit.Or(1000), params.MinCount.Or(1)
	opt := model.EntityGraphOptions{
		Limit:     limit,
		MinCount:  minCount,
		Tags:      params.Tags,
		TagsMatch: string(params.TagsMatch.Or(api.GetEntityGraphTagsMatchAny)),
		TagGroups: params.TagGroups.Or(""),
	}
	g, err := src.EntityGraph(ctx, params.BankID, opt)
	if err != nil {
		return nil, mapTagErr(err)
	}
	return renderEntityGraph(g, limit), nil
}

// GetEntity implements get_entity (GET /entities/{entity_id}).
func (e *Engine) GetEntity(ctx context.Context, params api.GetEntityParams) (api.GetEntityRes, error) {
	src, err := e.graphSource()
	if err != nil {
		return nil, err
	}
	if !isUUID(params.EntityID) {
		return nil, badRequest("Invalid entity_id: '%s' is not a valid UUID", params.EntityID)
	}
	scope := model.TagFilter{
		Tags:      params.Tags,
		TagsMatch: string(params.TagsMatch.Or(api.GetEntityTagsMatchAny)),
		TagGroups: params.TagGroups.Or(""),
	}
	ent, err := src.GetEntity(ctx, params.BankID, params.EntityID, scope)
	if err != nil {
		return nil, mapTagErr(err)
	}
	if ent == nil {
		return nil, notFound("Entity %s not found", params.EntityID)
	}
	obs := make([]api.EntityObservationResponse, 0, len(ent.Observations))
	for _, o := range ent.Observations {
		obs = append(obs, api.EntityObservationResponse{Text: o.Text, MentionedAt: optString(o.MentionedAt)})
	}
	return &api.EntityDetailResponse{
		ID:            ent.ID,
		CanonicalName: ent.CanonicalName,
		MentionCount:  ent.MentionCount,
		FirstSeen:     optString(ent.FirstSeen),
		LastSeen:      optString(ent.LastSeen),
		Observations:  obs,
	}, nil
}

// RegenerateEntityObservations implements regenerate_entity_observations. The
// upstream endpoint is deprecated and always answers 410 Gone; this mirrors it.
func (e *Engine) RegenerateEntityObservations(ctx context.Context, params api.RegenerateEntityObservationsParams) (api.RegenerateEntityObservationsRes, error) {
	return nil, &statusError{
		status: http.StatusGone,
		body:   map[string]any{"detail": "This endpoint is deprecated. Entity observations are no longer supported."},
	}
}

// mapTagErr turns the compound-filter resolution failure into a 422, the status
// upstream uses for a fuzzy leaf it cannot resolve.
func mapTagErr(err error) error {
	if errors.Is(err, model.ErrFuzzyTagGroups) {
		return &statusError{status: http.StatusUnprocessableEntity, body: map[string]any{"detail": err.Error()}}
	}
	return internal(err)
}

// ---------------------------------------------------------------------------
// Rendering: memory graph
// ---------------------------------------------------------------------------

// renderGraphData reproduces upstream get_graph_data's derivation: nodes and
// table rows come from the units, entity edges are derived from unit_entities
// postings (observations inherit their sources' entities), and memory links are
// copied onto the observations built on their endpoints.
func renderGraphData(data *model.GraphData, limit int) *api.GraphDataResponse {
	if data == nil {
		data = &model.GraphData{}
	}
	units := data.Units
	unitIDs := make([]string, 0, len(units))
	unitSet := make(map[string]bool, len(units))
	for _, u := range units {
		unitIDs = append(unitIDs, u.ID)
		unitSet[u.ID] = true
	}

	// Entity map: direct postings first, then inherited from source memories for
	// observations that carry none of their own.
	entityMap := map[string][]string{}
	for _, row := range data.EntityRows {
		entityMap[row.UnitID] = append(entityMap[row.UnitID], row.CanonicalName)
	}
	for _, u := range units {
		if len(u.SourceMemoryIDs) == 0 || len(entityMap[u.ID]) > 0 {
			continue
		}
		var inherited []string
		for _, sid := range u.SourceMemoryIDs {
			inherited = append(inherited, entityMap[sid]...)
		}
		if len(inherited) > 0 {
			entityMap[u.ID] = dedupeStrings(inherited)
		}
	}

	nodes := make([]api.MemoryGraphNode, 0, len(units))
	rows := make([]api.MemoryGraphTableRow, 0, len(units))
	for _, u := range units {
		entities := entityMap[u.ID]
		// Upstream always emits these keys, with "" for the absent ones.
		node := api.MemoryGraphNodeData{ID: u.ID}
		node.Label = api.OptString{Set: true, Value: graphLabel(u.Text)}
		node.Text = api.OptString{Set: true, Value: u.Text}
		node.Date = api.OptString{Set: true, Value: u.EventDate}
		node.Context = api.OptString{Set: true, Value: u.Context}
		node.Entities = api.OptString{Set: true, Value: entitiesText(entities)}
		node.Color = api.OptString{Set: true, Value: nodeColor(len(entities))}
		nodes = append(nodes, api.MemoryGraphNode{Data: node})
		rows = append(rows, api.MemoryGraphTableRow{
			ID:            u.ID,
			Text:          optString(u.Text),
			Context:       optString(contextOrNA(u.Context)),
			OccurredStart: optString(u.OccurredStart),
			OccurredEnd:   optString(u.OccurredEnd),
			MentionedAt:   optString(u.MentionedAt),
			Date:          optString(eventDateLabel(u.EventDate)),
			Entities:      optString(entitiesText(entities)),
			DocumentID:    optString(u.DocumentID),
			ChunkID:       optString(u.ChunkID),
			FactType:      optString(u.FactType),
			Tags:          normalizeTags(u.Tags),
			CreatedAt:     optString(u.CreatedAt),
			ProofCount:    optIntZero(u.ProofCount),
		})
	}

	// Links visible to the caller: direct edges between visible units, links
	// copied onto observations from their sources, and derived entity/semantic
	// edges between visible units.
	sourceToObservations := map[string][]string{}
	for _, u := range units {
		for _, sid := range u.SourceMemoryIDs {
			sourceToObservations[sid] = append(sourceToObservations[sid], u.ID)
		}
	}
	var copied []model.GraphLink
	for _, l := range data.Links {
		fromObs, toObs := sourceToObservations[l.FromUnitID], sourceToObservations[l.ToUnitID]
		if len(fromObs) > 0 {
			for _, obsID := range fromObs {
				if !unitSet[l.ToUnitID] && len(toObs) == 0 {
					continue
				}
				target := l.ToUnitID
				if len(toObs) > 0 && !unitSet[l.ToUnitID] {
					target = toObs[0]
				}
				if unitSet[target] && obsID != target {
					copied = append(copied, model.GraphLink{FromUnitID: obsID, ToUnitID: target, LinkType: l.LinkType, Weight: l.Weight})
				}
			}
		}
		if len(toObs) > 0 && unitSet[l.FromUnitID] {
			for _, obsID := range toObs {
				if l.FromUnitID != obsID {
					copied = append(copied, model.GraphLink{FromUnitID: l.FromUnitID, ToUnitID: obsID, LinkType: l.LinkType, Weight: l.Weight})
				}
			}
		}
	}

	entityToUnits := map[string][]string{}
	for _, unitID := range unitIDs {
		for _, name := range entityMap[unitID] {
			entityToUnits[name] = append(entityToUnits[name], unitID)
		}
	}
	var inferred []model.GraphLink
	const maxNeighborsPerUnit = 10
	for _, name := range sortedKeys(entityToUnits) {
		ids := entityToUnits[name]
		for i, a := range ids {
			end := i + 1 + maxNeighborsPerUnit
			if end > len(ids) {
				end = len(ids)
			}
			for _, b := range ids[i+1 : end] {
				inferred = append(inferred, model.GraphLink{FromUnitID: a, ToUnitID: b, LinkType: "entity", EntityName: name, Weight: 1})
			}
		}
	}
	sourceToObsSemantic := map[string][]string{}
	for _, u := range units {
		if u.FactType != "observation" {
			continue
		}
		for _, sid := range u.SourceMemoryIDs {
			sourceToObsSemantic[sid] = append(sourceToObsSemantic[sid], u.ID)
		}
	}
	for _, src := range sortedKeys(sourceToObsSemantic) {
		obs := sourceToObsSemantic[src]
		for i, a := range obs {
			for _, b := range obs[i+1:] {
				inferred = append(inferred, model.GraphLink{FromUnitID: a, ToUnitID: b, LinkType: "semantic", Weight: 1})
			}
		}
	}

	var edges []api.MemoryGraphEdge
	seen := map[string]bool{}
	addEdge := func(from, to, linkType, entityName string, weight float64) {
		if !unitSet[from] || !unitSet[to] {
			return
		}
		key := from + "\x00" + to + "\x00" + linkType + "\x00" + entityName
		if seen[key] {
			return
		}
		seen[key] = true
		color, style := edgeStyle(linkType)
		edges = append(edges, api.MemoryGraphEdge{Data: api.MemoryGraphEdgeData{
			ID:         from + "-" + to + "-" + linkType,
			Source:     from,
			Target:     to,
			LinkType:   api.OptString{Set: true, Value: linkType},
			Weight:     api.OptFloat64{Set: true, Value: weight},
			EntityName: api.OptString{Set: true, Value: entityName},
			Color:      api.OptString{Set: true, Value: color},
			LineStyle:  api.OptString{Set: true, Value: style},
		}})
	}
	for _, l := range data.Links {
		if unitSet[l.FromUnitID] && unitSet[l.ToUnitID] {
			addEdge(l.FromUnitID, l.ToUnitID, l.LinkType, "", l.Weight)
		}
	}
	for _, l := range copied {
		addEdge(l.FromUnitID, l.ToUnitID, l.LinkType, "", l.Weight)
	}
	for _, l := range inferred {
		addEdge(l.FromUnitID, l.ToUnitID, l.LinkType, l.EntityName, l.Weight)
	}

	return &api.GraphDataResponse{
		Nodes:      nodes,
		Edges:      edges,
		TableRows:  rows,
		TotalUnits: data.TotalUnits,
		Limit:      limit,
	}
}

func renderEntityGraph(g *model.EntityGraph, limit int) *api.EntityGraphResponse {
	if g == nil {
		g = &model.EntityGraph{}
	}
	nodes := make([]api.EntityGraphNode, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		color := "#90caf9"
		if n.MentionCount > 1 {
			color = "#42a5f5"
		}
		nodes = append(nodes, api.EntityGraphNode{Data: api.EntityGraphNodeData{
			ID:           n.ID,
			Label:        api.OptString{Set: true, Value: n.Label},
			MentionCount: api.OptInt{Set: true, Value: n.MentionCount},
			Color:        api.OptString{Set: true, Value: color},
		}})
	}
	edges := make([]api.EntityGraphEdge, 0, len(g.Edges))
	for _, e := range g.Edges {
		edges = append(edges, api.EntityGraphEdge{Data: api.EntityGraphEdgeData{
			ID:             e.Source + "-" + e.Target,
			Source:         e.Source,
			Target:         e.Target,
			LinkType:       api.OptString{Set: true, Value: "cooccurrence"},
			Weight:         api.OptInt{Set: true, Value: e.Count},
			Color:          api.OptString{Set: true, Value: "#ffd700"},
			LineStyle:      api.OptString{Set: true, Value: "solid"},
			LastCooccurred: api.OptString{Set: true, Value: e.LastCooccurred},
		}})
	}
	return &api.EntityGraphResponse{
		Nodes:         nodes,
		Edges:         edges,
		TotalEntities: len(nodes),
		TotalEdges:    len(edges),
		Limit:         limit,
	}
}

// graphLabel truncates the node label the way upstream does (30 characters).
func graphLabel(text string) string {
	runes := []rune(text)
	if len(runes) > 30 {
		return string(runes[:30]) + "..."
	}
	return text
}

func contextOrNA(context string) string {
	if context == "" {
		return "N/A"
	}
	return context
}

func entitiesText(entities []string) string {
	if len(entities) == 0 {
		return "None"
	}
	return strings.Join(entities, ", ")
}

func nodeColor(entityCount int) string {
	switch {
	case entityCount == 0:
		return "#e0e0e0"
	case entityCount == 1:
		return "#90caf9"
	default:
		return "#42a5f5"
	}
}

func edgeStyle(linkType string) (color, style string) {
	switch linkType {
	case "temporal":
		return "#00bcd4", "dashed"
	case "semantic":
		return "#ff69b4", "solid"
	case "entity":
		return "#ffd700", "solid"
	default:
		return "#999999", "solid"
	}
}

// eventDateLabel renders the deprecated flat `date` column the way upstream
// does: "YYYY-MM-DD HH:MM", or "N/A" when the unit carries no event date.
func eventDateLabel(iso string) string {
	if iso == "" {
		return "N/A"
	}
	if t, err := time.Parse(time.RFC3339, iso); err == nil {
		return t.UTC().Format("2006-01-02 15:04")
	}
	if len(iso) >= 16 {
		return strings.Replace(iso[:16], "T", " ", 1)
	}
	return iso
}

func dedupeStrings(in []string) []string {
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

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isUUID reports whether s has the canonical UUID shape. Entity ids are UUID
// columns upstream, and a malformed id is a 400 - not a database error.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

var _ = json.Marshal
var _ = fmt.Sprintf
