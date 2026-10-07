package mcp

import (
	"encoding/json"
	"net/http"
	"testing"
)

// toolText returns the first text block of a tools/call result.
func toolText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tool result carries no content: %v", result)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

// TestBankToolsReachTheRestHandlers proves the MCP bridge picks up the new
// bank-admin handlers: update_bank patches the profile, get_bank reads it back,
// and delete_bank removes the bank instead of answering "not implemented".
func TestBankToolsReachTheRestHandlers(t *testing.T) {
	ts := newMCP(t)
	putBank(t, ts.URL, "example-bank")

	result := rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name":      "update_bank",
		"arguments": map[string]any{"name": "Codex Bank", "mission": "remember the thread"},
	})
	if result["isError"] == true {
		t.Fatalf("update_bank reported an error: %s", toolText(t, result))
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(toolText(t, result)), &profile); err != nil {
		t.Fatalf("decode update_bank body: %v", err)
	}
	if profile["name"] != "Codex Bank" || profile["mission"] != "remember the thread" {
		t.Errorf("update_bank body = %v", profile)
	}

	result = rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name":      "get_bank",
		"arguments": map[string]any{},
	})
	if result["isError"] == true {
		t.Fatalf("get_bank reported an error: %s", toolText(t, result))
	}
	var readBack map[string]any
	if err := json.Unmarshal([]byte(toolText(t, result)), &readBack); err != nil {
		t.Fatalf("decode get_bank body: %v", err)
	}
	if readBack["name"] != "Codex Bank" || readBack["mission"] != "remember the thread" {
		t.Errorf("get_bank body = %v", readBack)
	}

	// config_updates must persist in full, including keys the bank PATCH
	// schema does not carry: those are routed through the config endpoint.
	result = rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name": "update_bank",
		"arguments": map[string]any{
			"config_updates": map[string]any{
				"retain_extraction_mode": "custom",
				"mcp_enabled_tools":      []any{"recall"},
			},
		},
	})
	if result["isError"] == true {
		t.Fatalf("update_bank config_updates reported an error: %s", toolText(t, result))
	}
	status, _, raw := request(t, http.MethodGet, ts.URL+"/v1/default/banks/example-bank/config", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET config status = %d, body %s", status, raw)
	}
	var cfgBody map[string]any
	if err := json.Unmarshal(raw, &cfgBody); err != nil {
		t.Fatalf("decode config body: %v", err)
	}
	overrides, _ := cfgBody["overrides"].(map[string]any)
	if overrides["retain_extraction_mode"] != "custom" {
		t.Errorf("overrides = %v, want the extraction mode", overrides)
	}
	if _, ok := overrides["mcp_enabled_tools"]; !ok {
		t.Errorf("off-schema config key was dropped: %v", overrides)
	}

	result = rpcResult(t, ts.URL, "example-bank", "tools/call", map[string]any{
		"name":      "delete_bank",
		"arguments": map[string]any{},
	})
	if result["isError"] == true {
		t.Fatalf("delete_bank reported an error: %s", toolText(t, result))
	}
	var deleted map[string]any
	if err := json.Unmarshal([]byte(toolText(t, result)), &deleted); err != nil {
		t.Fatalf("decode delete_bank body: %v", err)
	}
	if deleted["success"] != true {
		t.Errorf("delete_bank body = %v", deleted)
	}
}
