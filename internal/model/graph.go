package model

import "errors"

// ErrFuzzyTagGroups marks a compound tag filter whose fuzzy leaves this build
// cannot resolve (there is no trigram resolver here); the server maps it to 422.
var ErrFuzzyTagGroups = errors.New("fuzzy tag resolution is not available in this build; use resolve=\"exact\"")

// This file holds the vocabulary of the graph, entity and chunk surfaces. The
// shapes mirror upstream Hindsight: entities live in their own registry table,
// the memory graph is built from memory_units plus unit_entities/memory_links,
// and chunks are the document slices retain produced.

// EntityInfo is one row of the entity registry as the list endpoint renders it.
type EntityInfo struct {
	ID            string
	CanonicalName string
	MentionCount  int
	FirstSeen     string
	LastSeen      string
}

// EntityObservation is one observation attached to an entity.
type EntityObservation struct {
	Text        string
	MentionedAt string
}

// EntityDetail is an entity row plus its observations.
type EntityDetail struct {
	EntityInfo
	Observations []EntityObservation
}

// EntityListOptions carries the read knobs of GET /entities.
type EntityListOptions struct {
	Limit     int
	Offset    int
	Tags      []string
	TagsMatch string
	TagGroups string
}

// TagFilter is the optional tags/tags_match scope a single-entity read takes.
type TagFilter struct {
	Tags      []string
	TagsMatch string
	TagGroups string
}

// EntityGraphOptions carries the read knobs of GET /entities/graph.
type EntityGraphOptions struct {
	Limit     int
	MinCount  int
	Tags      []string
	TagsMatch string
	TagGroups string
}

// EntityGraphNode is one entity node rendered with its mention count.
type EntityGraphNode struct {
	ID           string
	Label        string
	MentionCount int
}

// EntityGraphEdge is one co-occurrence edge between two entities.
type EntityGraphEdge struct {
	Source         string
	Target         string
	Count          int
	LastCooccurred string
}

// EntityGraph is the renderable co-occurrence graph.
type EntityGraph struct {
	Nodes []EntityGraphNode
	Edges []EntityGraphEdge
}

// GraphOptions carries the query knobs of GET /graph.
type GraphOptions struct {
	FactType   string
	Limit      int
	Query      string
	DocumentID string
	ChunkID    string
	Tags       []string
	TagsMatch  string
}

// GraphUnit is one memory unit as the graph view reads it: the node payload,
// the flat table row, and the source links that observations inherit from.
type GraphUnit struct {
	ID              string
	Text            string
	Context         string
	FactType        string
	DocumentID      string
	ChunkID         string
	Tags            []string
	CreatedAt       string
	MentionedAt     string
	OccurredStart   string
	OccurredEnd     string
	EventDate       string
	ProofCount      int
	SourceMemoryIDs []string
}

// GraphLink is one memory-to-memory edge between two units.
type GraphLink struct {
	FromUnitID string
	ToUnitID   string
	LinkType   string
	EntityName string
	Weight     float64
}

// GraphEntityRow is one (unit, entity) posting; the server derives entity edges
// from these rather than materialising them as memory_links rows.
type GraphEntityRow struct {
	UnitID        string
	CanonicalName string
}

// GraphData is the storage-level answer behind GET /graph. The server turns it
// into the Cytoscape payload so both backends share one renderer.
type GraphData struct {
	Units      []GraphUnit
	Links      []GraphLink
	EntityRows []GraphEntityRow
	TotalUnits int
}

// ChunkInfo is one row of a document's chunks.
type ChunkInfo struct {
	ChunkID    string
	DocumentID string
	BankID     string
	ChunkIndex int
	ChunkText  string
	CreatedAt  string
}

// ChunkPage is a page of chunks plus its paging envelope.
type ChunkPage struct {
	Items  []ChunkInfo
	Total  int
	Limit  int
	Offset int
}

// ReprocessResult reports what one document reprocess produced.
type ReprocessResult struct {
	OperationID string
	ItemsCount  int
}
