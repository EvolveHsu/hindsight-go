package model

import "errors"

// ErrAliasConflict is returned when a new alias already names a bank or
// another bank's alias. The HTTP layer maps it to 409.
var ErrAliasConflict = errors.New("alias already names a bank or alias")

// BankPatch is the partial update the bank-admin seam applies to a bank row.
// Nil fields are left untouched, mirroring upstream PATCH semantics where an
// omitted field never clears a value.
type BankPatch struct {
	Name    *string
	Mission *string
	// Config carries bank-level configuration overrides (Python field names).
	// Upstream merges them into banks.config; an empty map is a no-op.
	Config map[string]any
}

// BankAlias is one extra id a bank also answers to. Primary is display-only:
// the bank keeps its own id everywhere else.
type BankAlias struct {
	Alias   string
	Primary bool
}

// BankDeleteCounts reports what a bank deletion removed, so the REST response
// can carry the same deleted_count the upstream DeleteResponse does.
type BankDeleteCounts struct {
	MemoryUnits int
	Documents   int
	Entities    int
}

// Total is the upstream deleted_count: units + documents + entities.
func (c BankDeleteCounts) Total() int {
	return c.MemoryUnits + c.Documents + c.Entities
}
