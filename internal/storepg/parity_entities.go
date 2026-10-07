package storepg

import (
	"context"
	"strings"
)

// linkEntities upserts entity rows and connects them to a memory unit. It is
// intentionally small: canonical names are matched case-insensitively by the
// upstream expression index, and repeated mentions increment the counter.
func (s *Store) linkEntities(ctx context.Context, bankID, unitID string, names []string) error {
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		var entityID string
		if err := s.pool.QueryRow(ctx, `
INSERT INTO entities (bank_id, canonical_name, last_seen)
VALUES ($1,$2,now())
ON CONFLICT (bank_id, lower(canonical_name))
DO UPDATE SET mention_count = entities.mention_count + 1, last_seen = now()
RETURNING id::text`, bankID, name).Scan(&entityID); err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO unit_entities (unit_id, entity_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			unitID, entityID); err != nil {
			return err
		}
	}
	return nil
}
