package server

import (
	"net/http"
	"testing"
)

// TestUpdateBankPatchesProfileAndConfig covers PATCH /banks/{id}: the request
// only changes the fields it mentions and the profile reads the config overlay
// back (reflect_mission wins over the legacy mission column).
func TestUpdateBankPatchesProfileAndConfig(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	code, body := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/admin", map[string]any{
		"name": "Initial",
	})
	if code != http.StatusOK {
		t.Fatalf("PUT bank status = %d (body: %v)", code, body)
	}

	code, body = doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/admin", map[string]any{
		"name":            "Renamed",
		"reflect_mission": "keep the point",
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH bank status = %d (body: %v)", code, body)
	}
	if body["name"] != "Renamed" {
		t.Errorf("name = %v, want Renamed", body["name"])
	}
	if body["mission"] != "keep the point" {
		t.Errorf("mission = %v, want the reflect_mission overlay", body["mission"])
	}

	// The profile read agrees with the PATCH response.
	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/admin/profile", nil)
	if code != http.StatusOK {
		t.Fatalf("GET profile status = %d", code)
	}
	if body["name"] != "Renamed" || body["mission"] != "keep the point" {
		t.Errorf("profile = %v, want the patched name and mission", body)
	}

	// Fields the PATCH did not mention survive a second PATCH.
	code, body = doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/admin", map[string]any{
		"retain_mission": "extract carefully",
	})
	if code != http.StatusOK {
		t.Fatalf("second PATCH status = %d", code)
	}
	if body["name"] != "Renamed" || body["mission"] != "keep the point" {
		t.Errorf("second PATCH dropped untouched fields: %v", body)
	}

	// The config API exposes the same overrides.
	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/admin/config", nil)
	if code != http.StatusOK {
		t.Fatalf("GET config status = %d", code)
	}
	overrides, _ := body["overrides"].(map[string]any)
	if overrides["reflect_mission"] != "keep the point" || overrides["retain_mission"] != "extract carefully" {
		t.Errorf("overrides = %v", overrides)
	}
}

// TestUpdateBankMissingIs404 pins the create_if_missing=false half of upstream
// PATCH: a bank nobody created must not be resurrected by an update.
func TestUpdateBankMissingIs404(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	code, _ := doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/ghost", map[string]any{
		"name": "Nope",
	})
	if code != http.StatusNotFound {
		t.Fatalf("PATCH missing bank status = %d, want 404", code)
	}
	code, _ = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks", nil)
	if code != http.StatusOK {
		t.Fatalf("list banks status = %d", code)
	}
}

// TestBankConfigUpdateResetAndCredentialGuard covers the config endpoints the
// console uses and the credential write refusal that mirrors upstream.
func TestBankConfigUpdateResetAndCredentialGuard(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/cfg", map[string]any{}); code != http.StatusOK {
		t.Fatalf("bank setup failed")
	}

	code, body := doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/cfg/config", map[string]any{
		"updates": map[string]any{
			"retain_extraction_mode":                   "custom",
			"HINDSIGHT_API_RETAIN_CUSTOM_INSTRUCTIONS": "be terse",
		},
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH config status = %d (body: %v)", code, body)
	}
	overrides, _ := body["overrides"].(map[string]any)
	if overrides["retain_extraction_mode"] != "custom" {
		t.Errorf("overrides = %v, want the mode", overrides)
	}
	// The environment-variable spelling lands under the Python field name.
	if overrides["retain_custom_instructions"] != "be terse" {
		t.Errorf("env-style key was not normalized: %v", overrides)
	}

	code, _ = doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/cfg/config", map[string]any{
		"updates": map[string]any{"openai_api_key": "sk-nope"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("credential update status = %d, want 400", code)
	}

	code, body = doJSON(t, http.MethodDelete, ts.URL+"/v1/default/banks/cfg/config", nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE config status = %d", code)
	}
	overrides, _ = body["overrides"].(map[string]any)
	if len(overrides) != 0 {
		t.Errorf("overrides after reset = %v, want empty", overrides)
	}
}

// TestDeleteBankRemovesBankAndData covers DELETE /banks/{id}: the memories go
// with the bank and the profile 404s afterwards.
func TestDeleteBankRemovesBankAndData(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/doomed", map[string]any{}); code != http.StatusOK {
		t.Fatalf("bank setup failed")
	}
	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/doomed/memories", map[string]any{
		"items": []any{
			map[string]any{"content": "Alice keeps a notebook."},
			map[string]any{"content": "Alice walks to work."},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("retain status = %d (body: %v)", code, body)
	}

	code, body = doJSON(t, http.MethodDelete, ts.URL+"/v1/default/banks/doomed", nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE bank status = %d (body: %v)", code, body)
	}
	if body["success"] != true {
		t.Errorf("success = %v, want true", body["success"])
	}
	if count, _ := body["deleted_count"].(float64); count < 2 {
		t.Errorf("deleted_count = %v, want at least the two memories", body["deleted_count"])
	}

	code, _ = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/doomed/profile", nil)
	if code != http.StatusNotFound {
		t.Errorf("profile after delete = %d, want 404", code)
	}
	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks", nil)
	if code != http.StatusOK {
		t.Fatalf("list banks status = %d", code)
	}
	if total, _ := body["total"].(float64); total != 0 {
		t.Errorf("total after delete = %v, want 0", body["total"])
	}
}

// TestRetiredProfileWriteIs410 keeps the Go build on the upstream 0.10.2
// contract: the profile write endpoints are gone, not reimplemented.
func TestRetiredProfileWriteIs410(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/retired", map[string]any{}); code != http.StatusOK {
		t.Fatalf("bank setup failed")
	}
	code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/retired/profile", map[string]any{
		"disposition": map[string]any{"skepticism": 4, "literalism": 3, "empathy": 2},
	})
	if code != http.StatusGone {
		t.Errorf("PUT profile status = %d, want 410", code)
	}
	code, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/retired/background", map[string]any{
		"content": "extra context",
	})
	if code != http.StatusGone {
		t.Errorf("POST background status = %d, want 410", code)
	}
}

// TestBankAliasLifecycle covers create/list/promote/delete plus the two
// collision rules: an alias may not shadow a bank id or another alias.
func TestBankAliasLifecycle(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/bank-a", map[string]any{}); code != http.StatusOK {
		t.Fatalf("bank setup failed")
	}

	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/bank-a/aliases", map[string]any{
		"alias": "legacy-a",
	})
	if code != http.StatusCreated {
		t.Fatalf("create alias status = %d (body: %v)", code, body)
	}
	aliases, _ := body["aliases"].([]any)
	if len(aliases) != 1 {
		t.Fatalf("aliases = %v, want one", aliases)
	}
	if body["bank_id"] != "bank-a" {
		t.Errorf("bank_id = %v", body["bank_id"])
	}

	// An alias that already names a bank is a conflict.
	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/bank-b", map[string]any{}); code != http.StatusOK {
		t.Fatalf("second bank setup failed")
	}
	code, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/bank-a/aliases", map[string]any{
		"alias": "bank-b",
	})
	if code != http.StatusConflict {
		t.Fatalf("alias shadowing a bank id = %d, want 409", code)
	}
	// And an alias may not repeat another alias.
	code, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/bank-a/aliases", map[string]any{
		"alias": "legacy-a",
	})
	if code != http.StatusConflict {
		t.Fatalf("duplicate alias = %d, want 409", code)
	}

	// Promotion is display-only and round-trips.
	code, body = doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/bank-a/aliases/legacy-a", map[string]any{
		"primary": true,
	})
	if code != http.StatusOK {
		t.Fatalf("promote alias status = %d (body: %v)", code, body)
	}
	aliases, _ = body["aliases"].([]any)
	first, _ := aliases[0].(map[string]any)
	if first["primary"] != true || first["alias"] != "legacy-a" {
		t.Errorf("aliases after promote = %v", aliases)
	}

	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/bank-a/aliases", nil)
	if code != http.StatusOK {
		t.Fatalf("list aliases status = %d", code)
	}
	if len(body["aliases"].([]any)) != 1 {
		t.Errorf("aliases = %v", body["aliases"])
	}

	code, body = doJSON(t, http.MethodDelete, ts.URL+"/v1/default/banks/bank-a/aliases/legacy-a", nil)
	if code != http.StatusOK {
		t.Fatalf("delete alias status = %d (body: %v)", code, body)
	}
	if len(body["aliases"].([]any)) != 0 {
		t.Errorf("aliases after delete = %v", body["aliases"])
	}
	code, _ = doJSON(t, http.MethodDelete, ts.URL+"/v1/default/banks/bank-a/aliases/legacy-a", nil)
	if code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", code)
	}
}
