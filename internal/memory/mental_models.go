package memory

import (
	"context"
	"sort"
	"time"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// MentalModel is the lite mental model row.
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

// CreateMentalModel stores a new model definition (content empty until refresh).
func (s *Store) CreateMentalModel(ctx context.Context, bankID string, m *model.MentalModel) (*model.MentalModel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.banks[bankID]; !ok {
		return nil, ErrBankNotFound
	}
	if s.mentalModels == nil {
		s.mentalModels = map[string]*model.MentalModel{}
	}
	now := s.now().UTC().Format(time.RFC3339)
	m.BankID = bankID
	m.CreatedAt = now
	s.mentalModels[m.ID] = m
	return m, nil
}

// GetMentalModel returns nil when absent.
func (s *Store) GetMentalModel(ctx context.Context, bankID, id string) (*model.MentalModel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.mentalModels[id]
	if m == nil || m.BankID != bankID {
		return nil, nil
	}
	cp := *m
	return &cp, nil
}

// ListMentalModels returns all models for the bank, oldest first.
func (s *Store) ListMentalModels(ctx context.Context, bankID string) ([]*model.MentalModel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.MentalModel
	for _, m := range s.mentalModels {
		if m.BankID == bankID {
			cp := *m
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// UpdateMentalModel applies an update func in place; nil when absent.
func (s *Store) UpdateMentalModel(ctx context.Context, bankID, id string, fn func(*model.MentalModel)) (*model.MentalModel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mentalModels[id]
	if m == nil || m.BankID != bankID {
		return nil, nil
	}
	fn(m)
	cp := *m
	return &cp, nil
}

// DeleteMentalModel removes one; false when absent.
func (s *Store) DeleteMentalModel(ctx context.Context, bankID, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mentalModels[id]
	if m == nil || m.BankID != bankID {
		return false, nil
	}
	delete(s.mentalModels, id)
	return true, nil
}
