package server

import (
	"context"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
)

// GetBankProfile implements get_bank_profile
// (GET /v1/default/banks/{bank_id}/profile).
//
// The MCP tool get_bank maps onto this operation, so the Go build answers it
// from the same Store seam the rest of the surface uses. Disposition traits are
// not carried by model.Bank yet, so they come back zeroed rather than invented.
func (e *Engine) GetBankProfile(ctx context.Context, params api.GetBankProfileParams) (api.GetBankProfileRes, error) {
	if params.BankID == "" {
		return nil, badRequest("bank_id is required")
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	} else {
		return e.bankProfileResponse(ctx, b)
	}
}
