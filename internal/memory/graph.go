package memory

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// The in-memory backend keeps no entity registry: entities only exist in the
// upstream schema, where the extractor writes them. The memory graph is still
// served from units, and the entity endpoints answer empty pages rather than
// pretending rows exist.

// GraphData implements the memory-graph read.
func (s *Store) GraphData(ctx context.Context, bankID string, opt model.GraphOptions) (*model.GraphData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var units []model.GraphUnit
	for _, u := range s.units {
		if u.BankID != bankID {
			continue
		}
		if opt.FactType != "" && string(u.FactType) != opt.FactType {
			continue
		}
		if opt.DocumentID != "" && u.DocumentID != opt.DocumentID {
			continue
		}
		if opt.ChunkID != "" && u.ChunkID != opt.ChunkID {
			continue
		}
		if opt.Query != "" {
			q := strings.ToLower(opt.Query)
			if !strings.Contains(strings.ToLower(u.Text), q) && !strings.Contains(strings.ToLower(u.Context), q) {
				continue
			}
		}
		if !tagsMatch(u.Tags, opt.Tags, opt.TagsMatch) {
			continue
		}
		units = append(units, model.GraphUnit{
			ID:          u.ID,
			Text:        u.Text,
			Context:     u.Context,
			FactType:    string(u.FactType),
			DocumentID:  u.DocumentID,
			ChunkID:     u.ChunkID,
			Tags:        u.Tags,
			CreatedAt:   u.CreatedAt,
			MentionedAt: u.MentionedAt,
		})
	}
	sort.Slice(units, func(i, j int) bool {
		if units[i].MentionedAt != units[j].MentionedAt {
			return units[i].MentionedAt > units[j].MentionedAt
		}
		return units[i].CreatedAt > units[j].CreatedAt
	})
	total := len(units)
	if opt.Limit > 0 && len(units) > opt.Limit {
		units = units[:opt.Limit]
	}
	if units == nil {
		units = []model.GraphUnit{}
	}
	return &model.GraphData{Units: units, TotalUnits: total}, nil
}

// ListEntities answers the empty entity page the lite schema supports.
func (s *Store) ListEntities(ctx context.Context, bankID string, opt model.EntityListOptions) ([]model.EntityInfo, int, error) {
	return []model.EntityInfo{}, 0, nil
}

// GetEntity always misses in the lite schema.
func (s *Store) GetEntity(ctx context.Context, bankID, entityID string, scope model.TagFilter) (*model.EntityDetail, error) {
	return nil, nil
}

// EntityGraph answers an empty co-occurrence graph.
func (s *Store) EntityGraph(ctx context.Context, bankID string, opt model.EntityGraphOptions) (*model.EntityGraph, error) {
	return &model.EntityGraph{Nodes: []model.EntityGraphNode{}, Edges: []model.EntityGraphEdge{}}, nil
}

// DocumentChunks lists the document's retained units as chunks, in creation
// order (the lite retain stores one unit per retained item).
func (s *Store) DocumentChunks(ctx context.Context, bankID, docID string, limit, offset int) (*model.ChunkPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := bankID + "/" + docID
	if _, ok := s.documents[key]; !ok {
		return nil, nil
	}
	ids := s.byDoc[key]
	page := &model.ChunkPage{Total: len(ids), Limit: limit, Offset: offset}
	if offset < 0 {
		offset = 0
	}
	if offset > len(ids) {
		offset = len(ids)
	}
	end := len(ids)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	for i, uid := range ids[offset:end] {
		u := s.units[uid]
		if u == nil {
			continue
		}
		chunkID := u.ChunkID
		if chunkID == "" {
			chunkID = "chunk-" + u.ID
		}
		page.Items = append(page.Items, model.ChunkInfo{
			ChunkID:    chunkID,
			DocumentID: u.DocumentID,
			BankID:     u.BankID,
			ChunkIndex: offset + i,
			ChunkText:  u.Text,
			CreatedAt:  u.CreatedAt,
		})
	}
	if page.Items == nil {
		page.Items = []model.ChunkInfo{}
	}
	return page, nil
}

// Chunk looks a single chunk up by its id.
func (s *Store) Chunk(ctx context.Context, chunkID string) (*model.ChunkInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.units {
		id := u.ChunkID
		if id == "" {
			id = "chunk-" + u.ID
		}
		if id != chunkID && u.ID != chunkID {
			continue
		}
		idx := 0
		for i, uid := range s.byDoc[u.BankID+"/"+u.DocumentID] {
			if uid == u.ID {
				idx = i
				break
			}
		}
		return &model.ChunkInfo{
			ChunkID:    id,
			DocumentID: u.DocumentID,
			BankID:     u.BankID,
			ChunkIndex: idx,
			ChunkText:  u.Text,
			CreatedAt:  u.CreatedAt,
		}, nil
	}
	return nil, nil
}

// UpdateDocumentTags replaces the document's tags, its units' tags and drops
// observations derived from those units.
func (s *Store) UpdateDocumentTags(ctx context.Context, bankID, docID string, tags []string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bankID + "/" + docID
	d, ok := s.documents[key]
	if !ok {
		return false, nil
	}
	d.Tags = normalize(tags)
	sourceIDs := map[string]bool{}
	for _, uid := range s.byDoc[key] {
		if u := s.units[uid]; u != nil {
			u.Tags = normalize(tags)
			if u.FactType != model.FactObservation {
				sourceIDs[uid] = true
			}
		}
	}
	for id, u := range s.units {
		if u.BankID != bankID || u.FactType != model.FactObservation {
			continue
		}
		for _, src := range u.SourceMemoryIds {
			if sourceIDs[src] {
				delete(s.units, id)
				break
			}
		}
	}
	return true, nil
}

// ReprocessDocument reports the unit count for the document. The lite extractor
// is content-deterministic, so a re-run over unchanged content yields exactly
// the units that already exist; see the PostgreSQL backend for the rationale.
func (s *Store) ReprocessDocument(ctx context.Context, bankID, docID string) (*model.ReprocessResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := bankID + "/" + docID
	d, ok := s.documents[key]
	if !ok || strings.TrimSpace(d.OriginalText) == "" {
		return nil, nil
	}
	return &model.ReprocessResult{
		OperationID: "reprocess-" + docID + "-" + time.Now().UTC().Format("20060102150405.000000000"),
		ItemsCount:  len(s.byDoc[key]),
	}, nil
}

// tagsMatch applies the upstream tags matching modes to one row's tags.
func tagsMatch(rowTags, tags []string, match string) bool {
	if match == "exact" && len(tags) == 0 {
		return len(rowTags) == 0
	}
	if len(tags) == 0 {
		return true
	}
	has := func(tag string) bool {
		for _, t := range rowTags {
			if t == tag {
				return true
			}
		}
		return false
	}
	strict := match == "any_strict" || match == "all_strict"
	if strict && len(rowTags) == 0 {
		return false
	}
	switch match {
	case "all", "all_strict":
		for _, t := range tags {
			if !has(t) {
				return false
			}
		}
		return true
	case "exact":
		if len(rowTags) != len(tags) {
			return false
		}
		for _, t := range tags {
			if !has(t) {
				return false
			}
		}
		return true
	default: // any / any_strict
		for _, t := range tags {
			if has(t) {
				return true
			}
		}
		return false
	}
}

func normalize(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}
