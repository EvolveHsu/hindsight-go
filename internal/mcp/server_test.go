package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/memory"
	"github.com/EvolveHsu/hindsight-go/internal/server"
)

// newMCP builds the real stack - in-memory engine, generated REST handler, MCP
// wrapper - so the tests exercise the same wiring main.go uses.
func newMCP(t *testing.T) *httptest.Server {
	t.Helper()
	api, err := server.AsHTTPHandler(server.NewEngine(memory.New()))
	if err != nil {
		t.Fatalf("build REST handler: %v", err)
	}
	handler, err := New(Config{API: api, Version: server.Version})
	if err != nil {
		t.Fatalf("build MCP server: %v", err)
	}
	ts := httptest.NewServer(handler.Wrap(api))
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, url string, payload any, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	return request(t, http.MethodPost, url, payload, headers)
}

func request(t *testing.T, method, url string, payload any, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

// rpcResult posts one JSON-RPC request and returns the decoded result, failing
// the test on a JSON-RPC error.
func rpcResult(t *testing.T, base, bank, method string, params any) map[string]any {
	t.Helper()
	status, _, body := post(t, base+"/mcp/"+bank+"/", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("%s status = %d, body %s", method, status, body)
	}
	envelope := decodeSSE(t, body)
	if errObj, ok := envelope["error"]; ok {
		t.Fatalf("%s returned error: %v", method, errObj)
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s result is %T, want object: %s", method, envelope["result"], body)
	}
	return result
}

// decodeSSE pulls the JSON-RPC envelope out of an SSE body, falling back to a
// plain JSON body so the same helper works for both content types.
func decodeSSE(t *testing.T, body []byte) map[string]any {
	t.Helper()
	text := strings.TrimSpace(string(body))
	var envelope map[string]any
	if strings.HasPrefix(text, "{") {
		if err := json.Unmarshal([]byte(text), &envelope); err != nil {
			t.Fatalf("decode JSON body: %v (%s)", err, text)
		}
		return envelope
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "{}" {
			continue
		}
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			t.Fatalf("decode SSE data: %v (%s)", err, payload)
		}
		return envelope
	}
	t.Fatalf("no data line in body: %s", text)
	return nil
}

func putBank(t *testing.T, base, bank string) {
	t.Helper()
	status, _, body := request(t, http.MethodPut, base+"/v1/default/banks/"+bank, map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT bank status = %d, body %s", status, body)
	}
}

func TestGetProbeWithoutSessionAnswers200(t *testing.T) {
	ts := newMCP(t)

	resp, err := http.Get(ts.URL + "/mcp/default/")
	if err != nil {
		t.Fatalf("GET probe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET probe status = %d", resp.StatusCode)
	}
	if strings.TrimSpace(string(body)) != "{}" {
		t.Errorf("GET probe body = %q, want {}", body)
	}
}

func TestGetWithSessionDeclinesStream(t *testing.T) {
	ts := newMCP(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mcp/default/", nil)
	req.Header.Set("Mcp-Session-Id", "abc123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET with session status = %d, want 405", resp.StatusCode)
	}
}

func TestInitializeReturnsSessionAndServerInfo(t *testing.T) {
	ts := newMCP(t)

	status, headers, body := post(t, ts.URL+"/mcp/default/", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "1.0"},
		},
	}, nil)

	if status != http.StatusOK {
		t.Fatalf("initialize status = %d, body %s", status, body)
	}
	if got := headers.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if headers.Get("Mcp-Session-Id") == "" {
		t.Errorf("initialize did not return an Mcp-Session-Id")
	}

	result := decodeSSE(t, body)["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != DefaultServerName {
		t.Errorf("serverInfo.name = %v, want %q", info["name"], DefaultServerName)
	}
	if info["version"] != server.Version {
		t.Errorf("serverInfo.version = %v, want %q", info["version"], server.Version)
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("capabilities.tools missing: %v", result["capabilities"])
	}
}

func TestInitializeEchoesSessionHeader(t *testing.T) {
	ts := newMCP(t)

	_, headers, _ := post(t, ts.URL+"/mcp/default/", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-06-18"},
	}, map[string]string{"Mcp-Session-Id": "client-chosen-session"})

	if got := headers.Get("Mcp-Session-Id"); got != "client-chosen-session" {
		t.Errorf("session id = %q, want the client's own id echoed back", got)
	}
}

func TestNotificationGets202WithNoBody(t *testing.T) {
	ts := newMCP(t)

	status, _, body := post(t, ts.URL+"/mcp/default/", map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}, nil)

	if status != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", status)
	}
	if len(bytes.TrimSpace(body)) != 0 {
		t.Errorf("notification body = %q, want empty", body)
	}
}

func TestToolsListServesTheCapturedCatalog(t *testing.T) {
	ts := newMCP(t)

	result := rpcResult(t, ts.URL, "example-bank", "tools/list", map[string]any{})
	tools, _ := result["tools"].([]any)
	if len(tools) != 36 {
		t.Fatalf("tool count = %d, want 36 (the captured upstream surface)", len(tools))
	}

	seen := map[string]bool{}
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		name, _ := tool["name"].(string)
		seen[name] = true
		if _, ok := tool["inputSchema"]; !ok {
			t.Errorf("tool %q has no inputSchema", name)
		}
	}
	for _, want := range []string{"retain", "sync_retain", "recall", "reflect", "list_memories", "get_bank"} {
		if !seen[want] {
			t.Errorf("tools/list is missing %q", want)
		}
	}
}

func TestRetainThenRecallThroughTheBridge(t *testing.T) {
	ts := newMCP(t)
	putBank(t, ts.URL, "example-bank")

	retain := rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name": "retain",
		"arguments": map[string]any{
			"content": "The Go rewrite keeps the port numbers.",
			"context": "project",
		},
	})
	if retain["isError"] == true {
		t.Fatalf("retain failed: %v", retain)
	}
	if _, ok := retain["structuredContent"]; !ok {
		t.Errorf("retain returned no structuredContent: %v", retain)
	}
	content, _ := retain["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("retain content = %v", retain["content"])
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Errorf("content block type = %v", block["type"])
	}

	recall := rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name":      "recall",
		"arguments": map[string]any{"query": "port numbers"},
	})
	structured, _ := recall["structuredContent"].(map[string]any)
	results, _ := structured["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("recall returned no results: %v", structured)
	}
	hit, _ := results[0].(map[string]any)
	if text, _ := hit["text"].(string); !strings.Contains(text, "port numbers") {
		t.Errorf("recall hit = %v, want the retained memory", hit)
	}
}

func TestGetBankProfileToolAnswers(t *testing.T) {
	ts := newMCP(t)
	putBank(t, ts.URL, "example-bank")

	result := rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name":      "get_bank",
		"arguments": map[string]any{},
	})
	if result["isError"] == true {
		t.Fatalf("get_bank failed: %v", result)
	}
	structured, _ := result["structuredContent"].(map[string]any)
	if structured["bank_id"] != "example-bank" {
		t.Errorf("get_bank structuredContent = %v", structured)
	}
}

func TestUnknownToolIsAProtocolError(t *testing.T) {
	ts := newMCP(t)

	status, _, body := post(t, ts.URL+"/mcp/default/", map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/call",
		"params":  map[string]any{"name": "no_such_tool", "arguments": map[string]any{}},
	}, nil)

	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	envelope := decodeSSE(t, body)
	errObj, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error, got %s", body)
	}
	if code, _ := errObj["code"].(float64); int(code) != codeInvalidParams {
		t.Errorf("error code = %v, want %d", errObj["code"], codeInvalidParams)
	}
}

// TestRestErrorSurfacesAsToolError pins the error-propagation contract: a REST
// failure comes back as an error result carrying the status, never as a false
// success. (The earlier version of this test used an unimplemented operation,
// which stops being unimplemented as parity work lands; a missing memory id
// keeps the contract pinned without depending on the 501 list.)
func TestRestErrorSurfacesAsToolError(t *testing.T) {
	ts := newMCP(t)
	putBank(t, ts.URL, "example-bank")

	result := rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name":      "get_memory",
		"arguments": map[string]any{"memory_id": "00000000-0000-0000-0000-000000000000"},
	})
	if result["isError"] != true {
		t.Fatalf("get_memory isError = %v, want true for a missing memory", result["isError"])
	}
	structured, _ := result["structuredContent"].(map[string]any)
	if status, _ := structured["status"].(float64); int(status) != http.StatusNotFound {
		t.Errorf("structuredContent = %v, want status 404", structured)
	}
}

// TestClearMemoriesWithTypeFilterIsRefused pins the guard around the case
// where the REST layer would delete more than the tool asked for.
func TestClearMemoriesWithTypeFilterIsRefused(t *testing.T) {
	ts := newMCP(t)
	putBank(t, ts.URL, "example-bank")

	rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name":      "retain",
		"arguments": map[string]any{"content": "keep me", "context": "project"},
	})

	result := rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name":      "clear_memories",
		"arguments": map[string]any{"type": "world"},
	})
	if result["isError"] != true {
		t.Fatalf("clear_memories with a filter must be refused, got %v", result)
	}

	// Refusing is only correct if nothing ran, so the memory has to still be there.
	listed := rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name":      "list_memories",
		"arguments": map[string]any{},
	})
	structured, _ := listed["structuredContent"].(map[string]any)
	if total, _ := structured["total"].(float64); total < 1 {
		t.Errorf("list_memories total = %v, want the retained memory to survive", structured["total"])
	}
}

// TestInvalidateMemoryIsRefused pins the guard around a write the storage layer
// drops on the floor; a false success would leave the memory in recall.
func TestInvalidateMemoryIsRefused(t *testing.T) {
	ts := newMCP(t)
	putBank(t, ts.URL, "example-bank")

	result := rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name":      "invalidate_memory",
		"arguments": map[string]any{"memory_id": "whatever"},
	})
	if result["isError"] != true {
		t.Fatalf("invalidate_memory must be refused while state is unsupported, got %v", result)
	}
}

func TestNonMCPPathIsPassedThrough(t *testing.T) {
	ts := newMCP(t)

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health status = %d", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte(`"status":"healthy"`)) {
		t.Errorf("/health body = %s", body)
	}
}

func TestBankFromPath(t *testing.T) {
	cases := map[string]string{
		"/mcp":                "default",
		"/mcp/":               "default",
		"/mcp/custom":          "custom",
		"/mcp/default/":         "default",
		"/mcp/default/messages": "default",
		"/mcp/a%20b/":         "a b",
	}
	for path, want := range cases {
		got, ok := bankFromPath(path, "default")
		if !ok || got != want {
			t.Errorf("bankFromPath(%q) = %q, %v; want %q, true", path, got, ok, want)
		}
	}
	for _, path := range []string{"/", "/v1/default/banks", "/mcpx/example-bank"} {
		if got, ok := bankFromPath(path, "default"); ok {
			t.Errorf("bankFromPath(%q) matched with bank %q; want no match", path, got)
		}
	}
}
