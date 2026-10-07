package memory

import (
	"context"
	"sort"
	"time"

	"github.com/EvolveHsu/hindsight-go/internal/consolidate"
	"github.com/EvolveHsu/hindsight-go/internal/embeddings"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// consolidated tracks which unit ids have been folded into observations.
// (The upstream schema stamps consolidated_at on memory_units; the in-memory
// build keeps a set with the same semantics.)
func (s *Store) isConsolidated(id string) bool { return s.consolidatedSet[id] }

// UnconsolidatedFacts implements consolidate.Source.
func (s *Store) UnconsolidatedFacts(ctx context.Context, bankID string, limit int) ([]model.Unit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Unit
	for _, u := range s.units {
		if u.BankID != bankID || s.isConsolidated(u.ID) {
			continue
		}
		if u.FactType == model.FactObservation {
			continue
		}
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Observations implements consolidate.Source.
func (s *Store) Observations(ctx context.Context, bankID string, limit int) ([]model.Unit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Unit
	for _, u := range s.units {
		if u.BankID == bankID && u.FactType == model.FactObservation {
			out = append(out, *u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ApplyBatch implements consolidate.Source. The whole batch commits under one
// lock hold, mirroring the upstream single-transaction rule.
func (s *Store) ApplyBatch(ctx context.Context, bankID string, b consolidate.Batch) error {
	createVectors, err := s.embedDocuments(ctx, actionTexts(b.Creates))
	if err != nil {
		return err
	}
	updateVectors, err := s.embedDocuments(ctx, updateTexts(b.Updates))
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC().Format(time.RFC3339)
	if s.consolidatedSet == nil {
		s.consolidatedSet = map[string]bool{}
	}

	markFacts := func(ids []string) {
		for _, id := range ids {
			s.consolidatedSet[id] = true
		}
	}

	for i, c := range b.Creates {
		// verify the facts exist in this bank before trusting the model
		for _, fid := range c.SourceFactIDs {
			u, ok := s.units[fid]
			if !ok || u.BankID != bankID {
				return ErrUnknownFact
			}
		}
		id := model.UnitID(bankID, "observation", c.Text)
		if _, exists := s.units[id]; !exists {
			s.units[id] = &model.Unit{
				ID: id, BankID: bankID,
				FactType:    model.FactObservation,
				Text:        c.Text,
				DocumentID:  "consolidated",
				ChunkID:     "chunk-" + id,
				CreatedAt:   now,
				MentionedAt: now,
				Embedding:   vectorAt(createVectors, i),
			}
		}
		markFacts(c.SourceFactIDs)
	}
	for i, u := range b.Updates {
		unit, ok := s.units[u.ObservationID]
		if !ok || unit.BankID != bankID || unit.FactType != model.FactObservation {
			return ErrUnknownObservation
		}
		unit.Text = u.Text
		unit.MentionedAt = now
		unit.Embedding = vectorAt(updateVectors, i)
		markFacts(u.SourceFactIDs)
	}
	for _, d := range b.Deletes {
		unit, ok := s.units[d.ObservationID]
		if !ok || unit.BankID != bankID {
			return ErrUnknownObservation
		}
		delete(s.units, d.ObservationID)
	}
	return nil
}

func (s *Store) embedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if s.embedder == nil || len(texts) == 0 {
		return nil, nil
	}
	vectors, err := s.embedder.Embed(ctx, texts, embeddings.InputDocument)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, ErrEmbeddingCount
	}
	return vectors, nil
}

func actionTexts(creates []consolidate.CreateAction) []string {
	out := make([]string, len(creates))
	for i := range creates {
		out[i] = creates[i].Text
	}
	return out
}

func vectorAt(vectors [][]float32, index int) []float32 {
	if index < 0 || index >= len(vectors) {
		return nil
	}
	return vectors[index]
}

func updateTexts(updates []consolidate.UpdateAction) []string {
	out := make([]string, len(updates))
	for i := range updates {
		out[i] = updates[i].Text
	}
	return out
}
