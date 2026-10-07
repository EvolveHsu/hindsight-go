package storepg

import (
	"context"
)

// DeleteDirective removes one directive row; false when it does not exist. The
// id column is a uuid upstream and text in the lite table, so the comparison
// goes through its text form in both modes.
func (s *Store) DeleteDirective(ctx context.Context, bankID, directiveID string) (bool, error) {
	if err := s.ensureDirectivesTable(ctx); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM directives WHERE bank_id = $1 AND id::text = $2", bankID, directiveID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
