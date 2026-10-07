package memory

import (
	"context"
	"github.com/EvolveHsu/hindsight-go/internal/embeddings"
	"github.com/EvolveHsu/hindsight-go/internal/model"
	"sort"
)

// ListMemories returns units for a bank with pagination.
func (s *Store) ListMemories(ctx context.Context, bankID string, limit, offset int) ([]model.Unit, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var all []model.Unit
	for _, u := range s.units {
		if u.BankID == bankID {
			all = append(all, *u)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	total := len(all)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}
	return all[offset:end], total, nil
}

func (s *Store) GetMemory(ctx context.Context, bankID, memoryID string) (*model.Unit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.units[memoryID]
	if u == nil || u.BankID != bankID {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}

func (s *Store) UpdateMemory(ctx context.Context, bankID, memoryID string, fn func(*model.Unit)) (*model.Unit, error) {
	s.mu.RLock()
	u := s.units[memoryID]
	if u == nil || u.BankID != bankID {
		s.mu.RUnlock()
		return nil, nil
	}
	candidate := *u
	s.mu.RUnlock()

	fn(&candidate)
	if s.embedder != nil {
		vectors, err := s.embedder.Embed(ctx, []string{candidate.Text}, embeddings.InputDocument)
		if err != nil {
			return nil, err
		}
		if len(vectors) != 1 || len(vectors[0]) == 0 {
			return nil, ErrEmbeddingCount
		}
		candidate.Embedding = vectors[0]
	}

	s.mu.Lock()
	current := s.units[memoryID]
	if current == nil || current.BankID != bankID {
		s.mu.Unlock()
		return nil, nil
	}
	*current = candidate
	s.mu.Unlock()
	return &candidate, nil
}

func (s *Store) InvalidateMemory(ctx context.Context, bankID, memoryID, reason string) error {
	return nil
}

func (s *Store) ClearBankMemories(ctx context.Context, bankID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, u := range s.units {
		if u.BankID == bankID {
			delete(s.units, id)
			n++
		}
	}
	return n, nil
}

func (s *Store) ClearMemoryObservations(ctx context.Context, bankID, memoryID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, u := range s.units {
		if u.BankID == bankID && u.FactType == model.FactObservation {
			for _, srcID := range u.SourceMemoryIds {
				if srcID == memoryID {
					delete(s.units, id)
					n++
				}
			}
		}
	}
	return n, nil
}

func (s *Store) ListDocuments(ctx context.Context, bankID string, limit, offset int) ([]model.DocumentInfo, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var all []model.DocumentInfo
	for _, d := range s.documents {
		if d.BankID != bankID {
			continue
		}
		all = append(all, model.DocumentInfo{ID: d.ID, BankID: d.BankID, OriginalText: d.OriginalText, ContentHash: d.ContentHash, CreatedAt: d.ID, Tags: d.Tags, UnitCount: len(s.byDoc[bankID+"/"+d.ID])})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	total := len(all)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}
	return all[offset:end], total, nil
}

func (s *Store) GetDocument(ctx context.Context, bankID, docID string) (*model.DocumentInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.documents[bankID+"/"+docID]
	if d == nil {
		return nil, nil
	}
	return &model.DocumentInfo{ID: d.ID, BankID: d.BankID, OriginalText: d.OriginalText, ContentHash: d.ContentHash, CreatedAt: d.ID, Tags: d.Tags, UnitCount: len(s.byDoc[bankID+"/"+docID])}, nil
}
