package storepg

import "github.com/EvolveHsu/hindsight-go/internal/model"

// ErrBankNotFound re-exports the shared sentinel.
var ErrBankNotFound = model.ErrBankNotFound

// ErrUnknownFact / ErrUnknownObservation mirror the memory backend sentinels.
var (
	ErrUnknownFact        = model.ErrUnknownFact
	ErrUnknownObservation = model.ErrUnknownObservation
)
