package memory

import (
	"context"
	"sort"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// Directive is the shared model row (lite columns).
type Directive = model.Directive

// Store directive surface (extends the base Store struct via methods here so
// engine.go's base file stays focused on the retain/recall core).

// CreateDirective inserts a directive.
func (s *Store) CreateDirective(ctx context.Context, bankID, name, content string) (*Directive, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.banks[bankID]; !ok {
		return nil, ErrBankNotFound
	}
	id := model.UnitID(bankID, "directive", name+"\x00"+content)
	d := &Directive{ID: "dir-" + id[:12], Name: name, Content: content}
	if s.directives == nil {
		s.directives = map[string][]*Directive{}
	}
	s.directives[bankID] = append(s.directives[bankID], d)
	return d, nil
}

// ListDirectives returns active directives, insertion order.
func (s *Store) ListDirectives(ctx context.Context, bankID string) ([]Directive, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Directive
	for _, d := range s.directives[bankID] {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// BankMission returns the bank's mission string.
func (s *Store) BankMission(ctx context.Context, bankID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if b, ok := s.banks[bankID]; ok {
		return b.Mission, nil
	}
	return "", ErrBankNotFound
}
