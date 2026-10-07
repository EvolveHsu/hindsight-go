package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	h, err := api.NewServer(&Server{}, api.WithErrorHandler(ErrorHandler))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return httptest.NewServer(h)
}

func get(t *testing.T, url string) (int, map[string]any, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body, raw
}

func TestLiveness(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	code, body, _ := get(t, ts.URL+"/health/live")
	if code != http.StatusOK {
		t.Fatalf("/health/live status = %d, want 200 (body: %s)", code, body)
	}
	if body["status"] != "alive" {
		t.Errorf("status = %v, want alive", body["status"])
	}
	if v, _ := body["version"].(string); v == "" {
		t.Errorf("version missing: %v", body)
	}
}

func TestVersionMatchesUpstreamContract(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	code, body, _ := get(t, ts.URL+"/version")
	if code != http.StatusOK {
		t.Fatalf("/version status = %d", code)
	}
	if body["api_version"] != UpstreamAPIVersion {
		t.Errorf("api_version = %v, want %q (contract freeze must match)", body["api_version"], UpstreamAPIVersion)
	}
	feats, _ := body["features"].(map[string]any)
	// mcp flipped on when the Streamable HTTP endpoint landed; the rest are
	// still stage-0 and must stay off so clients do not probe for them.
	if feats["mcp"] != true {
		t.Errorf("features.mcp = %v, want true (the /mcp endpoint is served)", feats["mcp"])
	}
	for _, k := range []string{"worker"} {
		if feats[k] == true {
			t.Errorf("stage-0 feature %q must be false", k)
		}
	}
	if feats["observations"] != true {
		t.Errorf("features.observations = %v, want true", feats["observations"])
	}
	if feats["worker"] == true {
		t.Errorf("features.worker = true, want false (no background worker yet)")
	}
}

func TestHealthReportsDisconnectedDB(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	code, body, _ := get(t, ts.URL+"/health")
	if code != http.StatusOK {
		t.Fatalf("/health status = %d", code)
	}
	if body["database"] != "disconnected" {
		t.Errorf("database = %v, want disconnected", body["database"])
	}
	// newTestServer uses the bare embedded Server, not an Engine; the
	// production path (Engine) is covered by the integration tests.
}

func TestReadinessIs503BeforeStorageConnects(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	code, body, raw := get(t, ts.URL+"/health/ready")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/health/ready status = %d, want 503 before storage connects (body: %s)", code, raw)
	}
	if body["detail"] != "storage layer not connected" {
		t.Errorf("detail = %v", body["detail"])
	}
}

func TestSetReadyFlipsReadiness(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	SetReady(true)
	defer SetReady(false)

	code, body, _ := get(t, ts.URL+"/health/ready")
	if code != http.StatusOK {
		t.Fatalf("/health/ready status = %d after SetReady(true)", code)
	}
	if body["status"] != "ready" {
		t.Errorf("status = %v", body["status"])
	}

	code, health, _ := get(t, ts.URL+"/health")
	if code != http.StatusOK {
		t.Fatalf("/health status = %d after SetReady(true)", code)
	}
	if health["database"] != "connected" {
		t.Errorf("database = %v, want connected after SetReady(true)", health["database"])
	}
}

func TestUnimplementedOperationReturns501(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	// list_banks has no engine behind it yet; the generated UnimplementedHandler
	// must answer with a typed 501, not a panic or a 404.
	code, _, raw := get(t, ts.URL+"/v1/default/banks")
	if code != http.StatusNotImplemented {
		t.Fatalf("list_banks status = %d, want 501 (body: %s)", code, raw)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	code, _, _ := get(t, ts.URL+"/definitely/not/a/route")
	if code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", code)
	}
}
