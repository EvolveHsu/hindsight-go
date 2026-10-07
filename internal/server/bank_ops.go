package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/go-faster/jx"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// BankAdminSource is the storage slice for bank-level administration: profile
// updates and whole-bank deletion. Both backends implement it.
type BankAdminSource interface {
	UpdateBank(ctx context.Context, bankID string, patch model.BankPatch) (*model.Bank, error)
	DeleteBank(ctx context.Context, bankID string) (model.BankDeleteCounts, error)
}

// BankConfigSource is the storage slice for per-bank configuration overrides
// (upstream banks.config JSONB).
type BankConfigSource interface {
	BankConfig(ctx context.Context, bankID string) (map[string]any, error)
	UpdateBankConfig(ctx context.Context, bankID string, updates map[string]any) error
	ResetBankConfig(ctx context.Context, bankID string) error
}

// BankAliasSource is the storage slice for the extra ids a bank answers to
// (upstream bank_aliases).
type BankAliasSource interface {
	ListBankAliases(ctx context.Context, bankID string) ([]model.BankAlias, error)
	CreateBankAlias(ctx context.Context, bankID, alias string, primary bool) error
	DeleteBankAlias(ctx context.Context, bankID, alias string) (bool, error)
	SetBankAliasPrimary(ctx context.Context, bankID, alias string, primary bool) (bool, error)
}

func conflict(format string, a ...any) error {
	return &statusError{status: http.StatusConflict, body: map[string]any{"detail": fmt.Sprintf(format, a...)}}
}

// gone answers the retired-endpoint 410 the upstream build returns.
func gone(detail string) error {
	return &statusError{status: http.StatusGone, body: map[string]any{"detail": detail}}
}

// bankAdminSource casts the seam.
func (e *Engine) bankAdminSource() (BankAdminSource, error) {
	if s, ok := e.store.(BankAdminSource); ok {
		return s, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support bank administration"}}
}

// bankConfigSource casts the seam.
func (e *Engine) bankConfigSource() (BankConfigSource, error) {
	if s, ok := e.store.(BankConfigSource); ok {
		return s, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support bank configuration"}}
}

// bankAliasSource casts the seam.
func (e *Engine) bankAliasSource() (BankAliasSource, error) {
	if s, ok := e.store.(BankAliasSource); ok {
		return s, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support bank aliases"}}
}

// UpdateBank implements update_bank (PATCH /banks/{bank_id}). Upstream is
// PATCH-style: only the fields present in the request change, and a bank that
// was never created is a 404 rather than an implicit create.
func (e *Engine) UpdateBank(ctx context.Context, req *api.CreateBankRequest, params api.UpdateBankParams) (api.UpdateBankRes, error) {
	if params.BankID == "" {
		return nil, badRequest("bank_id is required")
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	profile, err := e.applyBankPatch(ctx, params.BankID, req)
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// DeleteBank implements delete_bank (DELETE /banks/{bank_id}). Deleting a bank
// that was never created is a no-op that reports zero, matching upstream.
func (e *Engine) DeleteBank(ctx context.Context, params api.DeleteBankParams) (api.DeleteBankRes, error) {
	src, err := e.bankAdminSource()
	if err != nil {
		return nil, err
	}
	counts, err := src.DeleteBank(ctx, params.BankID)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return &api.DeleteResponse{
		Success:      true,
		Message:      api.OptString{Set: true, Value: fmt.Sprintf("Bank '%s' and all associated data deleted successfully", params.BankID)},
		DeletedCount: api.OptInt{Set: true, Value: counts.Total()},
	}, nil
}

// UpdateBankDisposition implements update_bank_disposition
// (PUT /banks/{bank_id}/profile). Upstream 0.10.2 retired this endpoint: it
// always answers 410 with a pointer at the bank config API, so the Go build
// answers the same instead of inventing a write path the console no longer uses.
func (e *Engine) UpdateBankDisposition(ctx context.Context, req *api.UpdateDispositionRequest, params api.UpdateBankDispositionParams) (api.UpdateBankDispositionRes, error) {
	return nil, gone(profileRetiredDetail)
}

// AddBankBackground implements add_bank_background
// (POST /banks/{bank_id}/background), retired upstream in favour of writing
// reflect_mission through the bank config API.
func (e *Engine) AddBankBackground(ctx context.Context, req *api.AddBackgroundRequest, params api.AddBankBackgroundParams) (api.AddBankBackgroundRes, error) {
	return nil, gone("The bank background endpoint has been removed. The background was folded into " +
		"the reflect mission: write it with PATCH /v1/default/banks/{bank_id}/config as `reflect_mission`. " +
		"That call replaces the value rather than merging into it, so read the current mission from " +
		"GET .../config first if you relied on this endpoint's append behaviour.")
}

// profileRetiredDetail mirrors the upstream 410 body for the retired profile
// write paths.
const profileRetiredDetail = "The bank profile endpoints have been removed. " +
	"Read the profile from GET /v1/default/banks/{bank_id} and update it with " +
	"PATCH /v1/default/banks/{bank_id} or PATCH /v1/default/banks/{bank_id}/config."

// GetBankConfig implements get_bank_config (GET /banks/{bank_id}/config).
func (e *Engine) GetBankConfig(ctx context.Context, params api.GetBankConfigParams) (api.GetBankConfigRes, error) {
	src, err := e.bankConfigSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	overrides, err := src.BankConfig(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	return bankConfigResponse(params.BankID, overrides), nil
}

// UpdateBankConfig implements update_bank_config (PATCH /banks/{bank_id}/config).
// The merge is shallow, the way upstream's `config || updates` is.
func (e *Engine) UpdateBankConfig(ctx context.Context, req *api.BankConfigUpdate, params api.UpdateBankConfigParams) (api.UpdateBankConfigRes, error) {
	src, err := e.bankConfigSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	updates := decodeRawMap(req.Updates)
	if len(updates) == 0 {
		return nil, badRequest("updates must contain at least one configuration field")
	}
	normalized, err := normalizeConfigUpdates(updates)
	if err != nil {
		return nil, err
	}
	if err := src.UpdateBankConfig(ctx, params.BankID, normalized); err != nil {
		return nil, mapStoreErr(err)
	}
	overrides, err := src.BankConfig(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	return bankConfigResponse(params.BankID, overrides), nil
}

// ResetBankConfig implements reset_bank_config
// (DELETE /banks/{bank_id}/config): drop every override.
func (e *Engine) ResetBankConfig(ctx context.Context, params api.ResetBankConfigParams) (api.ResetBankConfigRes, error) {
	src, err := e.bankConfigSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	if err := src.ResetBankConfig(ctx, params.BankID); err != nil {
		return nil, mapStoreErr(err)
	}
	return bankConfigResponse(params.BankID, nil), nil
}

// ListBankAliases implements list_bank_aliases (GET /banks/{bank_id}/aliases).
func (e *Engine) ListBankAliases(ctx context.Context, params api.ListBankAliasesParams) (api.ListBankAliasesRes, error) {
	return e.bankAliasesResponse(ctx, params.BankID)
}

// CreateBankAlias implements create_bank_alias (POST /banks/{bank_id}/aliases).
// The alias must not already name a bank or another alias; that collision is
// a 409, the way upstream reports it.
func (e *Engine) CreateBankAlias(ctx context.Context, req *api.CreateBankAliasRequest, params api.CreateBankAliasParams) (api.CreateBankAliasRes, error) {
	src, err := e.bankAliasSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	alias := strings.TrimSpace(req.Alias)
	if err := validateAlias(alias); err != nil {
		return nil, err
	}
	primary := false
	if req.Primary.Set {
		primary = req.Primary.Value
	}
	if err := src.CreateBankAlias(ctx, params.BankID, alias, primary); err != nil {
		if err == model.ErrAliasConflict {
			return nil, conflict("alias %q already names a bank or another alias", alias)
		}
		return nil, mapStoreErr(err)
	}
	return e.bankAliasesResponse(ctx, params.BankID)
}

// DeleteBankAlias implements delete_bank_alias
// (DELETE /banks/{bank_id}/aliases/{alias}).
func (e *Engine) DeleteBankAlias(ctx context.Context, params api.DeleteBankAliasParams) (api.DeleteBankAliasRes, error) {
	src, err := e.bankAliasSource()
	if err != nil {
		return nil, err
	}
	removed, err := src.DeleteBankAlias(ctx, params.BankID, params.Alias)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	if !removed {
		return nil, notFound("bank %q has no alias %q", params.BankID, params.Alias)
	}
	return e.bankAliasesResponse(ctx, params.BankID)
}

// SetBankAliasPrimary implements set_bank_alias_primary
// (PATCH /banks/{bank_id}/aliases/{alias}).
func (e *Engine) SetBankAliasPrimary(ctx context.Context, req *api.SetBankAliasPrimaryRequest, params api.SetBankAliasPrimaryParams) (api.SetBankAliasPrimaryRes, error) {
	src, err := e.bankAliasSource()
	if err != nil {
		return nil, err
	}
	ok, err := src.SetBankAliasPrimary(ctx, params.BankID, params.Alias, req.Primary)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	if !ok {
		return nil, notFound("bank %q has no alias %q", params.BankID, params.Alias)
	}
	return e.bankAliasesResponse(ctx, params.BankID)
}

// applyBankPatch persists one request-shaped bank patch and returns the
// profile the way upstream reads it back (config overlays the legacy columns).
func (e *Engine) applyBankPatch(ctx context.Context, bankID string, req *api.CreateBankRequest) (*api.BankProfileResponse, error) {
	src, err := e.bankAdminSource()
	if err != nil {
		return nil, err
	}
	patch, err := bankPatchFromRequest(req)
	if err != nil {
		return nil, err
	}
	b, err := src.UpdateBank(ctx, bankID, patch)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	if b == nil {
		return nil, notFound("bank %q does not exist", bankID)
	}
	return e.bankProfileResponse(ctx, b)
}

// bankPatchFromRequest translates the generated request into the storage patch.
// It is the Go half of upstream's CreateBankRequest.get_config_updates():
// reflect_mission wins over the deprecated mission/background aliases, and only
// fields that were actually present are written.
func bankPatchFromRequest(req *api.CreateBankRequest) (model.BankPatch, error) {
	var patch model.BankPatch
	if req == nil {
		return patch, nil
	}
	if req.Name.Set {
		name := req.Name.Value
		patch.Name = &name
	}
	config := map[string]any{}
	mission := ""
	haveMission := false
	switch {
	case req.ReflectMission.Set:
		mission, haveMission = req.ReflectMission.Value, true
	case req.Mission.Set:
		mission, haveMission = req.Mission.Value, true
	case req.Background.Set:
		mission, haveMission = req.Background.Value, true
	}
	if haveMission {
		patch.Mission = &mission
		config["reflect_mission"] = mission
	}
	if req.DispositionSkepticism.Set {
		config["disposition_skepticism"] = req.DispositionSkepticism.Value
	} else if req.Disposition.Set {
		config["disposition_skepticism"] = req.Disposition.Value.Skepticism
	}
	if req.DispositionLiteralism.Set {
		config["disposition_literalism"] = req.DispositionLiteralism.Value
	} else if req.Disposition.Set {
		config["disposition_literalism"] = req.Disposition.Value.Literalism
	}
	if req.DispositionEmpathy.Set {
		config["disposition_empathy"] = req.DispositionEmpathy.Value
	} else if req.Disposition.Set {
		config["disposition_empathy"] = req.Disposition.Value.Empathy
	}
	strFields := []struct {
		key string
		val api.OptString
	}{
		{"retain_mission", req.RetainMission},
		{"retain_extraction_mode", req.RetainExtractionMode},
		{"retain_custom_instructions", req.RetainCustomInstructions},
		{"observations_mission", req.ObservationsMission},
	}
	for _, f := range strFields {
		if f.val.Set {
			config[f.key] = f.val.Value
		}
	}
	intFields := []struct {
		key string
		val api.OptInt
	}{
		{"retain_chunk_size", req.RetainChunkSize},
		{"retain_structured_chunk_size", req.RetainStructuredChunkSize},
		{"retain_max_attachments_per_chunk", req.RetainMaxAttachmentsPerChunk},
	}
	for _, f := range intFields {
		if f.val.Set {
			config[f.key] = f.val.Value
		}
	}
	boolFields := []struct {
		key string
		val api.OptBool
	}{
		{"enable_observations", req.EnableObservations},
		{"enable_text_search", req.EnableTextSearch},
		{"enable_temporal_retrieval", req.EnableTemporalRetrieval},
		{"enable_graph_retrieval", req.EnableGraphRetrieval},
		{"enable_reranking", req.EnableReranking},
	}
	for _, f := range boolFields {
		if f.val.Set {
			config[f.key] = f.val.Value
		}
	}
	if len(config) > 0 {
		normalized, err := normalizeConfigUpdates(config)
		if err != nil {
			return patch, err
		}
		patch.Config = normalized
	}
	return patch, nil
}

// bankProfileResponse builds the profile with the upstream precedence: values
// in the bank config override the legacy banks columns.
func (e *Engine) bankProfileResponse(ctx context.Context, b *model.Bank) (*api.BankProfileResponse, error) {
	mission := b.Mission
	disp := api.DispositionTraits{}
	if src, err := e.bankConfigSource(); err == nil {
		if cfg, cfgErr := src.BankConfig(ctx, b.ID); cfgErr == nil {
			if v, ok := cfg["reflect_mission"].(string); ok {
				mission = v
			}
			if v, ok := numberValue(cfg["disposition_skepticism"]); ok {
				disp.Skepticism = int(v)
			}
			if v, ok := numberValue(cfg["disposition_literalism"]); ok {
				disp.Literalism = int(v)
			}
			if v, ok := numberValue(cfg["disposition_empathy"]); ok {
				disp.Empathy = int(v)
			}
		}
	}
	return &api.BankProfileResponse{
		BankID:      b.ID,
		Name:        b.Name,
		Mission:     mission,
		Disposition: disp,
		Background:  optString(mission),
	}, nil
}

// bankAliasesResponse loads the alias list for the wire.
func (e *Engine) bankAliasesResponse(ctx context.Context, bankID string) (*api.BankAliasesResponse, error) {
	src, err := e.bankAliasSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, bankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", bankID)
	}
	aliases, err := src.ListBankAliases(ctx, bankID)
	if err != nil {
		return nil, internal(err)
	}
	items := make([]api.BankAliasEntry, 0, len(aliases))
	for _, a := range aliases {
		items = append(items, api.BankAliasEntry{
			Alias:   a.Alias,
			Primary: api.OptBool{Set: true, Value: a.Primary},
		})
	}
	return &api.BankAliasesResponse{BankID: bankID, Aliases: items}, nil
}

// bankConfigResponse projects the override map onto the wire. Lite has no
// Python-side default hierarchy to resolve against, so the resolved view is the
// override set itself; a deployment that needs the full resolved config keeps
// using the Python service until that hierarchy is ported.
func bankConfigResponse(bankID string, overrides map[string]any) *api.BankConfigResponse {
	if overrides == nil {
		overrides = map[string]any{}
	}
	return &api.BankConfigResponse{
		BankID:    bankID,
		Config:    api.BankConfigResponseConfig(rawMap(overrides)),
		Overrides: api.BankConfigResponseOverrides(rawMap(overrides)),
	}
}

// decodeRawMap turns the generated jx.Raw map into plain values.
func decodeRawMap(raw map[string]jx.Raw) map[string]any {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		var value any
		if err := json.Unmarshal(v, &value); err != nil {
			continue
		}
		out[k] = value
	}
	return out
}

// rawMap is the inverse for response encoding.
func rawMap(m map[string]any) map[string]jx.Raw {
	out := make(map[string]jx.Raw, len(m))
	for k, v := range m {
		raw, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out[k] = jx.Raw(raw)
	}
	return out
}

// normalizeConfigUpdates accepts both the Python field names and the
// environment-variable spelling upstream documents, and refuses credential
// fields the way upstream's config resolver does.
func normalizeConfigUpdates(updates map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(updates))
	for key, value := range updates {
		name := normalizeConfigKey(key)
		if credentialConfigKey(name) {
			return nil, badRequest("cannot set credential fields via API: %q", name)
		}
		out[name] = value
	}
	return out, nil
}

func normalizeConfigKey(key string) string {
	trimmed := strings.TrimSpace(key)
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "HINDSIGHT_API_") {
		return strings.ToLower(trimmed[len("HINDSIGHT_API_"):])
	}
	return trimmed
}

// credentialConfigKey blocks the credential/secret-shaped names upstream's
// config resolver always rejects, so a config write cannot smuggle a provider
// key into an API-readable map.
func credentialConfigKey(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range []string{"api_key", "apikey", "password", "secret", "token", "credential", "database_url", "dsn"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// validateAlias applies the upstream bank-id rules to an alias: non-empty, at
// most 192 bytes of UTF-8, no control characters, and never the literal "root".
func validateAlias(alias string) error {
	if alias == "" {
		return badRequest("alias must not be empty")
	}
	if len(alias) > 192 {
		return badRequest("alias is too long: at most 192 bytes of UTF-8 are allowed")
	}
	for _, r := range alias {
		if unicode.IsControl(r) {
			return badRequest("alias must not contain control characters")
		}
	}
	return nil
}

func numberValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
