// Package server wires the generated HTTP surface to a storage backend.
//
// Store is the seam: internal/memory and internal/storepg both satisfy it.
// The context parameters mirror database reality — every real backend call
// needs cancellation — and the in-memory backend simply ignores them.
package server

import (
	"context"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// Store is the persistence seam.
type Store interface {
	EnsureBank(ctx context.Context, id, name, mission string) (*model.Bank, error)
	GetBank(ctx context.Context, id string) (*model.Bank, error)
	BankIDs(ctx context.Context) ([]string, error)

	Retain(ctx context.Context, bankID string, items []model.RetainItem) (*model.RetainResult, error)
	DeleteDocument(ctx context.Context, bankID, docID string) (bool, error)
	Recall(ctx context.Context, bankID string, opt model.RecallOptions) ([]model.RecallHit, error)
	ListTags(ctx context.Context, bankID string) (map[string]int, error)
}
