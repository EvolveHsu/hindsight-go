package model

// Transfer vocabulary: the storage-level projection of the upstream transfer
// archive (hindsight_api/engine/transfer/schema.py). The archive carries no
// embeddings and no database ids — an import re-embeds with the target bank's
// model, exactly like upstream.

// TransferChunk is one raw text chunk of a document.
type TransferChunk struct {
	ChunkIndex int
	ChunkText  string
}

// TransferFact is one extracted fact without its embedding or row id.
type TransferFact struct {
	Text           string
	SourceID       string
	FactType       FactType
	Context        string
	EventDate      string
	OccurredStart  string
	OccurredEnd    string
	MentionedAt    string
	Metadata       map[string]string
	Tags           []string
	ChunkIndex     int // -1 when the source unit names no chunk
	Entities       []string
	CausalSources  []string // source ids of earlier facts this fact causes
	CreatedAt      string
	ConsolidatedAt string
}

// TransferDocument is one document plus its chunks and extracted facts.
type TransferDocument struct {
	ID           string
	OriginalText string
	Tags         []string
	CreatedAt    string
	Chunks       []TransferChunk
	Facts        []TransferFact
}

// TransferObservation is a consolidated observation, carried only when the
// caller asked for observations.
type TransferObservation struct {
	Text        string
	SourceID    string
	Context     string
	Tags        []string
	MentionedAt string
	CreatedAt   string
	ProofCount  int
	Sources     []string // source ids of the facts it consolidated
}

// TransferOutcome reports what restoring one archive document did.
type TransferOutcome struct {
	CreatedFacts int
	Skipped      bool
}
