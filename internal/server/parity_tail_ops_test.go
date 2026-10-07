package server

import (
	"net/http"
	"testing"
)

// TestDeleteDirectiveEndpoint covers delete_directive end to end: create,
// delete, then a 404 for the second delete.
func TestDeleteDirectiveEndpoint(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/dir-bank", map[string]any{}); code != http.StatusOK {
		t.Fatalf("bank setup failed")
	}
	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/dir-bank/directives", map[string]any{
		"name":    "tone",
		"content": "be terse",
	})
	if code != http.StatusOK {
		t.Fatalf("create directive status = %d (body: %v)", code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("directive id missing: %v", body)
	}

	code, body = doJSON(t, http.MethodDelete, ts.URL+"/v1/default/banks/dir-bank/directives/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("delete directive status = %d (body: %v)", code, body)
	}
	if body["status"] != "deleted" {
		t.Errorf("delete body = %v, want status deleted", body)
	}

	code, _ = doJSON(t, http.MethodDelete, ts.URL+"/v1/default/banks/dir-bank/directives/"+id, nil)
	if code != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", code)
	}
}

// TestClearMentalModelEndpoint covers clear_mental_model: the model survives,
// its content is dropped, and a missing id is a 404.
func TestClearMentalModelEndpoint(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/mm-bank", map[string]any{}); code != http.StatusOK {
		t.Fatalf("bank setup failed")
	}
	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/mm-bank/mental-models", map[string]any{
		"name":         "weekly",
		"source_query": "what changed this week?",
	})
	if code != http.StatusOK {
		t.Fatalf("create mental model status = %d (body: %v)", code, body)
	}
	id, _ := body["mental_model_id"].(string)
	if id == "" {
		t.Fatalf("mental_model_id missing: %v", body)
	}

	code, body = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/mm-bank/mental-models/"+id+"/clear", nil)
	if code != http.StatusOK {
		t.Fatalf("clear status = %d (body: %v)", code, body)
	}
	if body["id"] != id {
		t.Errorf("clear body = %v, want the model back", body)
	}
	if content, ok := body["content"].(string); ok && content != "" {
		t.Errorf("content after clear = %q, want empty", content)
	}

	code, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/mm-bank/mental-models/ghost/clear", nil)
	if code != http.StatusNotFound {
		t.Errorf("clear missing model status = %d, want 404", code)
	}
}

// TestCancelOperationIsConflict pins the honest lite behaviour: operations run
// inline, so there is never a cancellable one and the endpoint says so instead
// of reporting a cancellation that did not happen.
func TestCancelOperationIsConflict(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()

	if code, _ := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/ops-bank", map[string]any{}); code != http.StatusOK {
		t.Fatalf("bank setup failed")
	}
	code, body := doJSON(t, http.MethodDelete, ts.URL+"/v1/default/banks/ops-bank/operations/op-1", nil)
	if code != http.StatusConflict {
		t.Fatalf("cancel status = %d (body: %v), want 409", code, body)
	}
	if detail, _ := body["detail"].(string); detail == "" {
		t.Errorf("cancel body = %v, want an explanation", body)
	}
	code, _ = doJSON(t, http.MethodDelete, ts.URL+"/v1/default/banks/ghost/operations/op-1", nil)
	if code != http.StatusNotFound {
		t.Errorf("cancel on missing bank = %d, want 404", code)
	}
}
