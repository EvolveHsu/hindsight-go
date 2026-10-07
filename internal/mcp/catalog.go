package mcp

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// catalogJSON is the live tools/list payload captured from the upstream Python
// server (vectorize-io/hindsight 0.10.2) at /mcp/{bank}/, single-bank mode.
//
// Serving the captured payload verbatim is deliberate: the client that already
// talks to the Python deployment keeps seeing byte-identical names,
// descriptions, annotations, and input schemas, so retiring Python does not
// change what the model is told the tools are.
//
//go:embed catalog.json
var catalogJSON []byte

// catalog is the parsed tool list: raw entries for serving, plus a name set so
// tools/call can reject unknown names without re-scanning JSON.
type catalog struct {
	raw   []json.RawMessage
	names map[string]struct{}
}

func loadCatalog() (*catalog, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(catalogJSON, &entries); err != nil {
		return nil, fmt.Errorf("parse embedded tool catalog: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("embedded tool catalog is empty")
	}
	names := make(map[string]struct{}, len(entries))
	for i, entry := range entries {
		var meta struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(entry, &meta); err != nil {
			return nil, fmt.Errorf("parse embedded tool %d: %w", i, err)
		}
		if meta.Name == "" {
			return nil, fmt.Errorf("embedded tool %d has no name", i)
		}
		if _, dup := names[meta.Name]; dup {
			return nil, fmt.Errorf("embedded tool catalog repeats %q", meta.Name)
		}
		names[meta.Name] = struct{}{}
	}
	return &catalog{raw: entries, names: names}, nil
}

// has reports whether the catalog exposes a tool by that name.
func (c *catalog) has(name string) bool {
	_, ok := c.names[name]
	return ok
}

// list returns the tools/list result payload.
func (c *catalog) list() map[string]any {
	tools := make([]any, 0, len(c.raw))
	for _, entry := range c.raw {
		tools = append(tools, entry)
	}
	return map[string]any{"tools": tools}
}
