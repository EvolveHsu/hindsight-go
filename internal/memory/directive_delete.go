package memory

import (
	"context"
)

// DeleteDirective removes one directive; false when the bank has no directive
// with that id.
func (s *Store) DeleteDirective(ctx context.Context, bankID, directiveID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.directives[bankID]
	for i, d := range rows {
		if d.ID == directiveID {
			s.directives[bankID] = append(rows[:i:i], rows[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}
