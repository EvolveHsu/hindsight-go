package server

import (
	"context"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// Directive is the storage-level directive row (shared by both backends via
// their own types; the seam converts to model.Directive).
type Directive = model.Directive

// DirectiveSource extends Store with the reflect surface. Both backends
// implement it; the reflect agent depends only on this slice.
type DirectiveSource interface {
	Store
	CreateDirective(ctx context.Context, bankID, name, content string) (*Directive, error)
	ListDirectives(ctx context.Context, bankID string) ([]Directive, error)
	BankMission(ctx context.Context, bankID string) (string, error)
}
