package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTransferServer builds a server whose blob store is a per-test directory,
// so export artifacts never leak between runs.
func newTransferServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("HINDSIGHT_GO_FILE_ROOT", t.TempDir())
	resetOperations()
	t.Cleanup(resetOperations)
	return newEngineServer(t)
}

func createBank(t *testing.T, ts *httptest.Server, id string) {
	t.Helper()
	code, body := doJSON(t, http.MethodPut, ts.URL+"/v1/default/banks/"+id, map[string]any{"name": id})
	if code != http.StatusOK {
		t.Fatalf("create bank %s: status %d (body %v)", id, code, body)
	}
}

func retain(t *testing.T, ts *httptest.Server, bankID, docID, text string) {
	t.Helper()
	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/"+bankID+"/memories", map[string]any{
		"items": []map[string]any{{"content": text, "document_id": docID, "tags": []string{"topic:test"}}},
	})
	if code != http.StatusOK {
		t.Fatalf("retain: status %d (body %v)", code, body)
	}
}

// doRaw performs a request with a caller-supplied body and content type.
func doRaw(t *testing.T, method, url string, body io.Reader, contentType string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, raw
}

// multipartArchive builds the multipart body the import endpoints expect.
func multipartArchive(t *testing.T, field string, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// TestDocumentTransferExportImportRoundTrip walks the async-shaped export:
// submit, poll the operation for its download_url, fetch the archive, then
// import it into a fresh bank without an LLM in the loop.
func TestDocumentTransferExportImportRoundTrip(t *testing.T) {
	ts := newTransferServer(t)
	defer ts.Close()

	createBank(t, ts, "alpha")
	retain(t, ts, "alpha", "notes", "Ada prefers tea over coffee")

	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/alpha/document-transfer/export", nil)
	if code != http.StatusAccepted {
		t.Fatalf("export status = %d (body %v), want 202", code, body)
	}
	opID, _ := body["operation_id"].(string)
	if opID == "" {
		t.Fatalf("export returned no operation_id: %v", body)
	}

	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/alpha/operations/"+opID, nil)
	if code != http.StatusOK {
		t.Fatalf("operation status = %d (body %v)", code, body)
	}
	if body["status"] != "completed" {
		t.Errorf("operation status = %v, want completed", body["status"])
	}
	meta, _ := body["result_metadata"].(map[string]any)
	downloadURL, _ := meta["download_url"].(string)
	if downloadURL == "" {
		t.Fatalf("result_metadata has no download_url: %v", body)
	}

	code, archive := doRaw(t, http.MethodGet, ts.URL+downloadURL, nil, "")
	if code != http.StatusOK {
		t.Fatalf("download status = %d", code)
	}
	if !bytes.HasPrefix(archive, []byte("PK")) {
		t.Fatalf("download is not a ZIP: %q", archive[:min(4, len(archive))])
	}

	createBank(t, ts, "beta")
	bodyReader, contentType := multipartArchive(t, "file", "documents.zip", archive)
	code, raw := doRaw(t, http.MethodPost, ts.URL+"/v1/default/banks/beta/document-transfer", bodyReader, contentType)
	if code != http.StatusAccepted {
		t.Fatalf("import status = %d (body %s)", code, raw)
	}

	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/beta/documents", nil)
	if code != http.StatusOK || body["total"].(float64) != 1 {
		t.Fatalf("beta documents = %d %v", code, body)
	}
	items, _ := body["items"].([]any)
	first, _ := items[0].(map[string]any)
	if first["id"] != "notes" {
		t.Errorf("imported document id = %v, want notes", first["id"])
	}

	code, body = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/beta/memories/recall", map[string]any{"query": "tea"})
	if code != http.StatusOK {
		t.Fatalf("recall status = %d (body %v)", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("imported bank recalls nothing: %v", body)
	}
	hit, _ := results[0].(map[string]any)
	if !strings.Contains(toStr(hit["text"]), "tea") {
		t.Errorf("recalled text = %v, want the imported fact", hit["text"])
	}
}

// TestBankTransferRestoreCarriesConfigAndDirectives covers the whole-bank
// restore path: a fresh target bank receives the source's config, directives
// and documents.
func TestBankTransferRestoreCarriesConfigAndDirectives(t *testing.T) {
	ts := newTransferServer(t)
	defer ts.Close()

	createBank(t, ts, "source")
	code, body := doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/source", map[string]any{
		"reflect_mission": "answer briefly",
	})
	if code != http.StatusOK {
		t.Fatalf("patch bank: %d %v", code, body)
	}
	code, body = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/source/directives", map[string]any{
		"name": "tone", "content": "be warm",
	})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create directive: %d %v", code, body)
	}
	retain(t, ts, "source", "notes", "Ada prefers tea over coffee")

	code, body = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/source/transfer/export", nil)
	if code != http.StatusAccepted {
		t.Fatalf("bank export: %d %v", code, body)
	}
	opID, _ := body["operation_id"].(string)
	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/source/operations/"+opID, nil)
	if code != http.StatusOK {
		t.Fatalf("poll: %d %v", code, body)
	}
	meta, _ := body["result_metadata"].(map[string]any)
	downloadURL, _ := meta["download_url"].(string)
	code, archive := doRaw(t, http.MethodGet, ts.URL+downloadURL, nil, "")
	if code != http.StatusOK {
		t.Fatalf("download: %d", code)
	}

	// Restore into a fresh bank id; the target must not exist yet.
	bodyReader, contentType := multipartArchive(t, "file", "bank.zip", archive)
	code, raw := doRaw(t, http.MethodPost,
		ts.URL+"/v1/default/banks/source/transfer/import?mode=restore&target_bank_id=clone", bodyReader, contentType)
	if code != http.StatusAccepted {
		t.Fatalf("restore status = %d (body %s)", code, raw)
	}

	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/clone/profile", nil)
	if code != http.StatusOK {
		t.Fatalf("clone profile: %d %v", code, body)
	}
	if body["mission"] != "answer briefly" {
		t.Errorf("clone mission = %v, want the restored config", body["mission"])
	}
	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/clone/directives", nil)
	if code != http.StatusOK {
		t.Fatalf("clone directives: %d %v", code, body)
	}
	directives, _ := body["items"].([]any)
	if len(directives) != 1 {
		t.Fatalf("clone directives = %v, want the restored directive", body)
	}
	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/clone/documents", nil)
	if code != http.StatusOK || body["total"].(float64) != 1 {
		t.Fatalf("clone documents = %d %v", code, body)
	}

	// Restoring again onto an existing bank is a caller error.
	bodyReader, contentType = multipartArchive(t, "file", "bank.zip", archive)
	code, raw = doRaw(t, http.MethodPost,
		ts.URL+"/v1/default/banks/source/transfer/import?mode=restore&target_bank_id=clone", bodyReader, contentType)
	if code != http.StatusBadRequest {
		t.Errorf("restore onto an existing bank = %d (body %s), want 400", code, raw)
	}
}

// TestBankTemplateRoundTrip covers export → import of a template, plus the
// dry-run path and the published schema.
func TestBankTemplateRoundTrip(t *testing.T) {
	ts := newTransferServer(t)
	defer ts.Close()

	createBank(t, ts, "tpl-source")
	if code, body := doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/tpl-source", map[string]any{
		"reflect_mission": "keep it short",
	}); code != http.StatusOK {
		t.Fatalf("patch bank: %d %v", code, body)
	}
	if code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/tpl-source/directives", map[string]any{
		"name": "tone", "content": "be warm",
	}); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create directive: %d %v", code, body)
	}
	if code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/tpl-source/mental-models", map[string]any{
		"id": "mm-profile", "name": "Profile", "source_query": "who is Ada?",
	}); code != http.StatusOK {
		t.Fatalf("create mental model: %d %v", code, body)
	}

	code, manifest := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/tpl-source/export", nil)
	if code != http.StatusOK {
		t.Fatalf("export template: %d %v", code, manifest)
	}
	if manifest["version"] != "1" {
		t.Errorf("template version = %v", manifest["version"])
	}
	bank, _ := manifest["bank"].(map[string]any)
	if bank["reflect_mission"] != "keep it short" {
		t.Errorf("template bank = %v", bank)
	}

	// Dry run reports what would happen without creating the bank.
	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/tpl-dry/import?dry_run=true", manifest)
	if code != http.StatusOK {
		t.Fatalf("dry-run import: %d %v", code, body)
	}
	if body["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", body["dry_run"])
	}
	if code, _ := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/tpl-dry/profile", nil); code != http.StatusNotFound {
		t.Errorf("dry run created the bank (status %d)", code)
	}

	// A real import creates the bank and applies everything.
	code, body = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/tpl-target/import", manifest)
	if code != http.StatusOK {
		t.Fatalf("import: %d %v", code, body)
	}
	if body["config_applied"] != true {
		t.Errorf("config_applied = %v", body["config_applied"])
	}
	if created, _ := body["mental_models_created"].([]any); len(created) != 1 {
		t.Errorf("mental_models_created = %v", body["mental_models_created"])
	}
	if created, _ := body["directives_created"].([]any); len(created) != 1 {
		t.Errorf("directives_created = %v", body["directives_created"])
	}

	code, profile := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/tpl-target/profile", nil)
	if code != http.StatusOK || profile["mission"] != "keep it short" {
		t.Errorf("imported profile = %d %v", code, profile)
	}

	// Importing the same manifest again updates rather than duplicating.
	code, body = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/tpl-target/import", manifest)
	if code != http.StatusOK {
		t.Fatalf("second import: %d %v", code, body)
	}
	if updated, _ := body["mental_models_updated"].([]any); len(updated) != 1 {
		t.Errorf("second import mental_models_updated = %v", body["mental_models_updated"])
	}

	// The published schema is the upstream JSON Schema document.
	code, raw := doRaw(t, http.MethodGet, ts.URL+"/v1/bank-template-schema", nil, "")
	if code != http.StatusOK {
		t.Fatalf("schema status = %d", code)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if schema["title"] != "BankTemplateManifest" {
		t.Errorf("schema title = %v", schema["title"])
	}
}

// TestExportDocumentsSyncRemovedIs410 keeps the removed synchronous endpoint
// honest: it points at the async pair instead of exporting.
func TestExportDocumentsSyncRemovedIs410(t *testing.T) {
	ts := newTransferServer(t)
	defer ts.Close()
	createBank(t, ts, "alpha")

	code, raw := doRaw(t, http.MethodGet, ts.URL+"/v1/default/banks/alpha/document-transfer", nil, "")
	if code != http.StatusGone {
		t.Fatalf("status = %d (body %s), want 410", code, raw)
	}
	if !strings.Contains(string(raw), "document-transfer/export") {
		t.Errorf("body = %s, want a pointer at the async endpoint", raw)
	}
}

// TestFileRetainConvertsTextFiles covers the file→memory path for formats the
// lite build parses itself.
func TestFileRetainConvertsTextFiles(t *testing.T) {
	ts := newTransferServer(t)
	defer ts.Close()
	createBank(t, ts, "files")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("files", "notes.md")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	if _, err := part.Write([]byte("Ada prefers tea over coffee")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.WriteField("request", `{"tags":["topic:test"],"context":"a note"}`); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	code, raw := doRaw(t, http.MethodPost, ts.URL+"/v1/default/banks/files/files/retain", &buf, mw.FormDataContentType())
	if code != http.StatusOK {
		t.Fatalf("file retain status = %d (body %s)", code, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids, _ := body["operation_ids"].([]any)
	if len(ids) != 1 {
		t.Fatalf("operation_ids = %v", body["operation_ids"])
	}

	code, body = doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/files/documents", nil)
	if code != http.StatusOK || body["total"].(float64) != 1 {
		t.Fatalf("documents = %d %v", code, body)
	}
	items, _ := body["items"].([]any)
	doc, _ := items[0].(map[string]any)
	if !strings.HasPrefix(toStr(doc["id"]), "file_") {
		t.Errorf("document id = %v, want a file_ id", doc["id"])
	}

	code, body = doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/files/memories/recall", map[string]any{"query": "tea"})
	if code != http.StatusOK || len(body["results"].([]any)) == 0 {
		t.Fatalf("recall of the converted file = %d %v", code, body)
	}

	// A format this build cannot parse is refused with a clear error.
	var deck bytes.Buffer
	mw = multipart.NewWriter(&deck)
	part, err = mw.CreateFormFile("files", "deck.pptx")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	if _, err := part.Write([]byte("PK\x03\x04")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.WriteField("request", `{}`); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	code, raw = doRaw(t, http.MethodPost, ts.URL+"/v1/default/banks/files/files/retain", &deck, mw.FormDataContentType())
	if code != http.StatusBadRequest {
		t.Fatalf("unsupported file status = %d (body %s), want 400", code, raw)
	}
	if !strings.Contains(string(raw), ".pdf") {
		t.Errorf("error does not list the supported formats: %s", raw)
	}
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}

// TestCloneBankCopiesIntoFreshBank covers clone_bank: one call exports the
// bank and restores it under a new id.
func TestCloneBankCopiesIntoFreshBank(t *testing.T) {
	ts := newTransferServer(t)
	defer ts.Close()

	createBank(t, ts, "origin")
	if code, body := doJSON(t, http.MethodPatch, ts.URL+"/v1/default/banks/origin", map[string]any{
		"reflect_mission": "copy me",
	}); code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, body)
	}
	retain(t, ts, "origin", "notes", "Ada prefers tea over coffee")

	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/origin/clone?target_bank_id=copy", nil)
	if code != http.StatusAccepted {
		t.Fatalf("clone status = %d (body %v)", code, body)
	}
	if opID, _ := body["operation_id"].(string); opID == "" {
		t.Fatalf("clone returned no operation_id: %v", body)
	}

	code, profile := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/copy/profile", nil)
	if code != http.StatusOK || profile["mission"] != "copy me" {
		t.Fatalf("clone profile = %d %v", code, profile)
	}
	code, docs := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/copy/documents", nil)
	if code != http.StatusOK || docs["total"].(float64) != 1 {
		t.Fatalf("clone documents = %d %v", code, docs)
	}

	// Cloning onto an existing bank, or onto itself, is a caller error.
	if code, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/origin/clone?target_bank_id=copy", nil); code != http.StatusBadRequest {
		t.Errorf("cloning onto an existing bank = %d, want 400", code)
	}
	if code, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/origin/clone?target_bank_id=origin", nil); code != http.StatusBadRequest {
		t.Errorf("cloning onto itself = %d, want 400", code)
	}
}

// TestAsyncRetainReturnsPollableOperation pins the async retain contract: the
// lite build runs the work inline but still hands back an operation id that
// polls as completed.
func TestAsyncRetainReturnsPollableOperation(t *testing.T) {
	ts := newTransferServer(t)
	defer ts.Close()
	createBank(t, ts, "async")

	code, body := doJSON(t, http.MethodPost, ts.URL+"/v1/default/banks/async/memories", map[string]any{
		"async": true,
		"items": []map[string]any{{"content": "Ada prefers tea", "document_id": "notes"}},
	})
	if code != http.StatusOK {
		t.Fatalf("async retain status = %d (body %v)", code, body)
	}
	if body["async"] != true {
		t.Errorf("async = %v, want true", body["async"])
	}
	opID, _ := body["operation_id"].(string)
	if opID == "" {
		t.Fatalf("async retain returned no operation_id: %v", body)
	}

	code, status := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/async/operations/"+opID, nil)
	if code != http.StatusOK || status["status"] != "completed" {
		t.Fatalf("operation status = %d %v", code, status)
	}
	code, docs := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/async/documents", nil)
	if code != http.StatusOK || docs["total"].(float64) != 1 {
		t.Fatalf("documents after async retain = %d %v", code, docs)
	}

	// The operation also shows up in the bank's operation list.
	code, list := doJSON(t, http.MethodGet, ts.URL+"/v1/default/banks/async/operations", nil)
	if code != http.StatusOK || list["total"].(float64) < 1 {
		t.Fatalf("operations list = %d %v", code, list)
	}
}
