// Package memory is the in-process backend for the lite storage seam.
//
// It delegates the scoring and fusion vocabulary to internal/model and owns
// only persistence: maps guarded by one RWMutex. The Postgres backend
// (internal/storepg) implements the same Store interface against real tables.
package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/EvolveHsu/hindsight-go/internal/embeddings"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// Store is the in-process storage engine.
type Store struct {
	mu              sync.RWMutex
	banks           map[string]*model.Bank
	documents       map[string]*model.Document // key: bankID + "/" + docID
	units           map[string]*model.Unit     // key: unit ID
	byDoc           map[string][]string        // key: bankID + "/" + docID -> unit IDs
	directives      map[string][]*Directive    // key: bankID
	consolidatedSet map[string]bool            // unit ids folded into observations
	mentalModels    map[string]*model.MentalModel
	bankConfigs     map[string]map[string]any // key: bankID -> config overrides
	aliases         map[string][]bankAliasRow // key: bankID -> ordered aliases
	invalidated     map[string]*model.Unit    // key: unit ID
	now             func() time.Time
	embedder        embeddings.Embedder
}

// Option configures the in-process backend.
type Option func(*Store)

// WithEmbedder enables real embedding generation in the in-memory backend.
func WithEmbedder(e embeddings.Embedder) Option {
	return func(s *Store) { s.embedder = e }
}

// New creates an empty store.
func New(opts ...Option) *Store {
	s := &Store{
		banks:           map[string]*model.Bank{},
		documents:       map[string]*model.Document{},
		units:           map[string]*model.Unit{},
		byDoc:           map[string][]string{},
		directives:      map[string][]*Directive{},
		consolidatedSet: map[string]bool{},
		mentalModels:    map[string]*model.MentalModel{},
		bankConfigs:     map[string]map[string]any{},
		aliases:         map[string][]bankAliasRow{},
		invalidated:     map[string]*model.Unit{},
		now:             time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// EnsureBank creates the bank if missing and returns it.
func (s *Store) EnsureBank(ctx context.Context, id, name, mission string) (*model.Bank, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.banks[id]; ok {
		return b, nil
	}
	b := &model.Bank{ID: id, Name: name, Mission: mission}
	s.banks[id] = b
	return b, nil
}

// GetBank returns nil when absent.
func (s *Store) GetBank(ctx context.Context, id string) (*model.Bank, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.banks[id], nil
}

// BankIDs lists all bank IDs in stable order.
func (s *Store) BankIDs(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.banks))
	for id := range s.banks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// Retain writes items grouped by document with content-hash idempotency.
func (s *Store) Retain(ctx context.Context, bankID string, items []model.RetainItem) (*model.RetainResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prepared := make([]model.RetainItem, len(items))
	for i, it := range items {
		if it.DocumentID == "" {
			it.DocumentID = "auto-" + model.ContentHash(it.Content)[:12]
		}
		prepared[i] = it
	}
	vectors := make([][]float32, len(prepared))
	if s.embedder != nil {
		texts := make([]string, len(prepared))
		for i := range prepared {
			texts[i] = prepared[i].Content
		}
		var err error
		vectors, err = s.embedder.Embed(ctx, texts, embeddings.InputDocument)
		if err != nil {
			return nil, err
		}
		if len(vectors) != len(prepared) {
			return nil, ErrEmbeddingCount
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.banks[bankID]; !ok {
		return nil, ErrBankNotFound
	}

	res := &model.RetainResult{BankID: bankID, ItemsCount: len(items)}
	groups := map[string][]int{}
	var order []string
	for i, it := range prepared {
		if _, seen := groups[it.DocumentID]; !seen {
			order = append(order, it.DocumentID)
		}
		groups[it.DocumentID] = append(groups[it.DocumentID], i)
	}

	usage := 0
	for _, docID := range order {
		idxs := groups[docID]
		its := make([]model.RetainItem, len(idxs))
		docVectors := make([][]float32, len(idxs))
		for j, idx := range idxs {
			its[j] = prepared[idx]
			docVectors[j] = vectors[idx]
		}
		var b strings.Builder
		for _, it := range its {
			b.WriteString(it.Content)
			b.WriteByte('\n')
		}
		text := strings.TrimRight(b.String(), "\n")
		hash := model.ContentHash(text)

		key := bankID + "/" + docID
		if prev, ok := s.documents[key]; ok && prev.ContentHash == hash {
			continue // idempotent replay
		}

		s.documents[key] = &model.Document{
			ID: docID, BankID: bankID, OriginalText: text, ContentHash: hash,
			Tags: mergeTags(its),
		}

		ids := s.byDoc[key]
		for j, it := range its {
			usage += model.TokenCount(it.Content)
			id := model.UnitID(bankID, docID, it.Content)
			mentioned := it.MentionedAt
			if mentioned == "" {
				mentioned = s.now().UTC().Format(time.RFC3339)
			}
			s.units[id] = &model.Unit{
				ID: id, BankID: bankID,
				FactType:    it.FactType,
				Text:        it.Content,
				Context:     it.Context,
				DocumentID:  docID,
				ChunkID:     "chunk-" + id,
				Tags:        it.Tags,
				Entities:    it.Entities,
				MentionedAt: mentioned,
				CreatedAt:   s.now().UTC().Format(time.RFC3339),
				Embedding:   docVectors[j],
			}
			ids = append(ids, id)
			res.UnitsStored++
		}
		s.byDoc[key] = ids
		res.DocumentIDs = append(res.DocumentIDs, docID)
	}
	res.UsageTokens = usage
	return res, nil
}

// DeleteDocument removes a document and its units.
func (s *Store) DeleteDocument(ctx context.Context, bankID, docID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bankID + "/" + docID
	if _, ok := s.documents[key]; !ok {
		return false, nil
	}
	for _, uid := range s.byDoc[key] {
		delete(s.units, uid)
	}
	delete(s.byDoc, key)
	delete(s.documents, key)
	return true, nil
}

// Recall runs the two lite arms and fuses them with RRF.
func (s *Store) Recall(ctx context.Context, bankID string, opt model.RecallOptions) ([]model.RecallHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var queryVector []float32
	if s.embedder != nil {
		vectors, err := s.embedder.Embed(ctx, []string{opt.Query}, embeddings.InputQuery)
		if err != nil {
			return nil, err
		}
		if len(vectors) != 1 || len(vectors[0]) == 0 {
			return nil, ErrEmbeddingCount
		}
		queryVector = vectors[0]
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.banks[bankID]; !ok {
		return nil, ErrBankNotFound
	}

	want := map[model.FactType]bool{}
	for _, t := range opt.Types {
		want[t] = true
	}
	if len(want) == 0 {
		for _, t := range model.FactTypesAll {
			want[t] = true
		}
	}

	q := model.Tokenize(opt.Query)
	semanticMin := opt.MinSemantic
	if queryVector != nil && !opt.MinSemanticSet && semanticMin <= 0 {
		semanticMin = 0.1
	}
	var semantic, keyword []model.ArmResult
	for _, u := range s.units {
		if u.BankID != bankID || !want[u.FactType] {
			continue
		}
		if queryVector != nil {
			if len(u.Embedding) > 0 {
				if sim := embeddings.Cosine(queryVector, u.Embedding); sim > 0 && sim >= semanticMin {
					semantic = append(semantic, model.ArmResult{Unit: u, Score: sim})
				}
			}
		} else if sem := model.SemanticScore(q, u.Text); sem > 0 && sem >= opt.MinSemantic {
			semantic = append(semantic, model.ArmResult{Unit: u, Score: sem})
		}
		if kw := model.KeywordScore(q, u.Text); kw > 0 && kw >= opt.MinKeyword {
			keyword = append(keyword, model.ArmResult{Unit: u, Score: kw})
		}
	}
	armScore := func(list []model.ArmResult) {
		sort.Slice(list, func(i, j int) bool { return list[i].Score > list[j].Score })
	}
	armScore(semantic)
	armScore(keyword)

	hits := model.FuseArms(semantic, keyword)
	limit := opt.Budget
	if limit <= 0 {
		limit = 20
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// ListTags returns the distinct tags in the bank with unit counts.
func (s *Store) ListTags(ctx context.Context, bankID string) (map[string]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{}
	for _, u := range s.units {
		if u.BankID != bankID {
			continue
		}
		for _, t := range u.Tags {
			out[t]++
		}
	}
	return out, nil
}

// mergeTags is the order-stable union of item tags.
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
