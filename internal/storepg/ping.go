package storepg

import "context"

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
