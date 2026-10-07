package memory

import (
	"errors"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// ErrBankNotFound re-exports the shared sentinel so callers can errors.Is
// against either name.
var ErrBankNotFound = model.ErrBankNotFound

// ErrUnknownFact / ErrUnknownObservation re-export the shared sentinels for
// consolidation batches that reference ids the model invented.
var (
	ErrUnknownFact        = model.ErrUnknownFact
	ErrUnknownObservation = model.ErrUnknownObservation
)

// ErrEmbeddingCount reports a provider that returned the wrong number of
// vectors for a batch.
var ErrEmbeddingCount = errors.New("embedding provider returned the wrong number of vectors")
