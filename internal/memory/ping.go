package memory

import "context"

func (s *Store) Ping(ctx context.Context) error { return ctx.Err() }
