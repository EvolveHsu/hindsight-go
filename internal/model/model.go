// Package model holds the storage vocabulary shared by every backend: the
// row types, the retain/recall request shapes, and the scoring + fusion
// helpers. Backends (in-memory, PostgreSQL) own persistence; this package owns
// the nouns so neither implementation depends on the other.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"unicode"
)

// FactType is one of the three upstream fact types.
type FactType string

const (
	FactWorld       FactType = "world"
	FactExperience  FactType = "experience"
	FactObservation FactType = "observation"
)

// FactTypesAll is the default recall scope when no types are requested.
var FactTypesAll = []FactType{FactWorld, FactExperience, FactObservation}

// Bank mirrors the upstream banks row (lite columns).
type Bank struct {
	ID      string
	Name    string
	Mission string
}

// Document mirrors the upstream documents row (lite columns).
type Document struct {
	ID           string
	BankID       string
	OriginalText string
	ContentHash  string
	Tags         []string
}

// Unit is one memory row (upstream memory_units). Timestamps are RFC3339
// strings on the wire; backends convert from their native types.
type Unit struct {
	ID              string
	BankID          string
	FactType        FactType
	Text            string
	Context         string
	DocumentID      string
	ChunkID         string
	Tags            []string
	Entities        []string
	OccurredStart   string
	OccurredEnd     string
	MentionedAt     string
	CreatedAt       string
	SourceMemoryIds []string
	Embedding       []float32
}

// DocumentInfo is the wire projection of a document row.
type DocumentInfo struct {
	ID           string
	BankID       string
	OriginalText string
	ContentHash  string
	CreatedAt    string
	UpdatedAt    string
	Tags         []string
	UnitCount    int
}

// ErrDocumentUnchanged is returned when a document with the same content_hash
// is re-submitted: the write is a no-op, matching upstream idempotency.
var ErrDocumentUnchanged = errors.New("document unchanged (content_hash matches existing)")

// Directive is a bank-level rule injected into prompts.
type Directive struct {
	BankID   string
	ID       string
	Name     string
	Content  string
	Priority int
	IsActive bool
	Tags     []string
}

// DirectivePatch is a sparse directive update. Nil fields are unchanged.
type DirectivePatch struct {
	Name     *string
	Content  *string
	Priority *int
	IsActive *bool
	Tags     *[]string
}

// ErrBankNotFound is returned when an operation names a bank that does not exist.
var ErrBankNotFound = errors.New("bank not found")

// RetainItem is one validated input fact.
type RetainItem struct {
	Content       string
	DocumentID    string
	Tags          []string
	Entities      []string
	Context       string
	MentionedAt   string
	FactType      FactType
	SourceContent string
	OccurredStart string
	OccurredEnd   string
}

// RetainResult reports what one retain call did.
type RetainResult struct {
	BankID      string
	ItemsCount  int
	UnitsStored int
	DocumentIDs []string
	UsageTokens int
}

// RecallOptions carries the request knobs the lite recall implements.
type RecallOptions struct {
	Query          string
	Types          []FactType
	Budget         int // max results
	MinSemantic    float64
	MinSemanticSet bool
	MinKeyword     float64
	MinKeywordSet  bool
}

// ArmResult is one unit scored by one retrieval arm, already filtered and
// ranked best-first by the backend.
type ArmResult struct {
	Unit  *Unit
	Score float64
}

// RecallHit is one fused result.
type RecallHit struct {
	Unit       *Unit
	Semantic   float64
	Keyword    float64
	Graph      float64
	FusedScore float64
}

// RRFK is the upstream reciprocal-rank-fusion constant.
const RRFK = 60.0

// FuseArms fuses the semantic and keyword arms with reciprocal rank fusion
// (k=60, the upstream default) and attaches per-arm scores to the hits.
// Arm lists must already be ranked best-first.
func FuseArms(arms ...[]ArmResult) []RecallHit {
	fused := map[string]*RecallHit{}
	for armIndex, arm := range arms {
		for i, ar := range arm {
			h, ok := fused[ar.Unit.ID]
			if !ok {
				h = &RecallHit{Unit: ar.Unit}
				fused[ar.Unit.ID] = h
			}
			h.FusedScore += 1.0 / (RRFK + float64(i+1))
			switch armIndex {
			case 0:
				h.Semantic = ar.Score
			case 1:
				h.Keyword = ar.Score
			case 2:
				h.Graph = ar.Score
			}
		}
	}

	out := make([]RecallHit, 0, len(fused))
	for _, h := range fused {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FusedScore > out[j].FusedScore })
	return out
}

// ContentHash is the document-level idempotency key (upstream content_hash).
func ContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// UnitID is deterministic: same content in the same document yields the same
// id (upstream's deterministic chunk_id layer).
func UnitID(bankID, docID, content string) string {
	sum := sha256.Sum256([]byte(bankID + "\x00" + docID + "\x00" + content))
	return hex.EncodeToString(sum[:16])
}

// TokenCount is a cheap word-ish count for usage metrics; chunk boundaries do
// not depend on it in the lite build.
func TokenCount(s string) int {
	n := 0
	inWord := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			inWord = false
			continue
		}
		if !inWord {
			n++
			inWord = true
		}
	}
	return n
}

// Tokenize lowercases and splits on non-letters/digits; CJK runs become single
// tokens (good enough for the lite keyword arm).
func Tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
				if cur.Len() > 0 {
					out = append(out, cur.String())
					cur.Reset()
				}
				out = append(out, string(r))
			} else {
				cur.WriteRune(r)
			}
		default:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// SemanticScore is token-overlap Jaccard in [0,1]: the lite stand-in for the
// embedding cosine arm. Swapping in real embeddings changes this one function
// and the storage column, not the fusion logic.
func SemanticScore(queryTokens []string, text string) float64 {
	doc := Tokenize(text)
	if len(doc) == 0 || len(queryTokens) == 0 {
		return 0
	}
	docs := map[string]bool{}
	for _, t := range doc {
		docs[t] = true
	}
	inter := 0
	seen := map[string]bool{}
	for _, t := range queryTokens {
		if docs[t] && !seen[t] {
			seen[t] = true
			inter++
		}
	}
	union := len(docs) + len(queryTokens) - inter
	if union <= 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// KeywordScore is a BM25-ish coverage score: fraction of query tokens present,
// scaled by length normalization.
func KeywordScore(queryTokens []string, text string) float64 {
	doc := Tokenize(text)
	if len(doc) == 0 || len(queryTokens) == 0 {
		return 0
	}
	docs := map[string]int{}
	for _, t := range doc {
		docs[t]++
	}
	hits := 0
	for _, t := range queryTokens {
		if docs[t] > 0 {
			hits++
		}
	}
	if hits == 0 {
		return 0
	}
	coverage := float64(hits) / float64(len(queryTokens))
	norm := 1.0 / (1.0 + 0.0015*float64(len(doc)))
	return coverage * norm * 10
}

// ErrUnknownFact / ErrUnknownObservation are returned when a consolidation
// batch references ids the model invented.
var (
	ErrUnknownFact        = errors.New("consolidation references unknown fact")
	ErrUnknownObservation = errors.New("consolidation references unknown observation")
)

// MentalModel is a stored, auto-refreshed reflect result.
type MentalModel struct {
	ID            string
	BankID        string
	Name          string
	SourceQuery   string
	Content       string
	Tags          []string
	MaxTokens     int
	LastRefreshed string
	LastSeen      string
	CreatedAt     string
}
