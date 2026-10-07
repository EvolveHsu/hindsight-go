package memory

import (
	"context"
	"fmt"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

func (s *Store) ClearBankMemoriesByType(ctx context.Context, bankID, factType string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, u := range s.units {
		if u.BankID == bankID && string(u.FactType) == factType {
			delete(s.units, id)
			n++
		}
	}
	return n, nil
}

func (s *Store) ArchiveMemory(ctx context.Context, bankID, memoryID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.units[memoryID]
	if u == nil || u.BankID != bankID || (u.FactType != model.FactWorld && u.FactType != model.FactExperience) {
		return fmt.Errorf("memory %s is missing or not curatable", memoryID)
	}
	copy := *u
	s.invalidated[memoryID] = &copy
	delete(s.units, memoryID)
	for key, ids := range s.byDoc {
		filtered := ids[:0]
		for _, id := range ids {
			if id != memoryID {
				filtered = append(filtered, id)
			}
		}
		s.byDoc[key] = filtered
	}
	return nil
}

func (s *Store) RestoreMemory(ctx context.Context, bankID, memoryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.invalidated[memoryID]
	if u == nil || u.BankID != bankID {
		return fmt.Errorf("invalidated memory %s not found", memoryID)
	}
	copy := *u
	s.units[memoryID] = &copy
	s.byDoc[bankID+"/"+u.DocumentID] = append(s.byDoc[bankID+"/"+u.DocumentID], memoryID)
	delete(s.invalidated, memoryID)
	return nil
}
