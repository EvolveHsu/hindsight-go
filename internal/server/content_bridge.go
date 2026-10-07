package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// contentRewriter wraps the generated server and rewrites retain request bodies
// so the plain-string content form ("content": "text") becomes the block-array
// form the generated decoder accepts.
//
// This bridge exists because the upstream contract declares content as a
// string|array union while ogen has no sum-type codegen; the normalization step
// collapses the spec to the array branch, and this shim restores the string
// half of the contract at the HTTP boundary.
type contentRewriter struct {
	next http.Handler
}

// NewContentRewriter wraps h.
func NewContentRewriter(h http.Handler) http.Handler { return &contentRewriter{next: h} }

func (c *contentRewriter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && isRetainPath(r.URL.Path) && r.Body != nil {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err == nil && hasPlainStringContent(body) {
			if fixed, ok := rewritePlainStringContent(body); ok {
				r.Body = io.NopCloser(bytes.NewReader(fixed))
				r.ContentLength = int64(len(fixed))
				r.Header.Set("Content-Length", strconv.Itoa(len(fixed)))
			} else {
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
		} else {
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
	}
	c.next.ServeHTTP(w, r)
}

// isRetainPath matches POST /v1/default/banks/{bank_id}/memories
func isRetainPath(p string) bool {
	const prefix = "/v1/default/banks/"
	const suffix = "/memories"
	if len(p) < len(prefix)+len(suffix) {
		return false
	}
	if p[:len(prefix)] != prefix {
		return false
	}
	return p[len(p)-len(suffix):] == suffix
}

// hasPlainStringContent is a cheap pre-check: a "content" key whose value is a
// JSON string (the block form starts with '[').
func hasPlainStringContent(body []byte) bool {
	idx := bytes.Index(body, []byte(`"content"`))
	if idx < 0 {
		return false
	}
	rest := body[idx+len(`"content"`):]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r') {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != ':' {
		return false
	}
	rest = rest[1:]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r') {
		rest = rest[1:]
	}
	return len(rest) > 0 && rest[0] == '"'
}

// rewritePlainStringContent converts {"content": "text"} items into
// {"content": [{"type":"text","text":"text"}]} via a generic JSON pass.
func rewritePlainStringContent(body []byte) ([]byte, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false
	}
	itemsRaw, ok := top["items"]
	if !ok {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(itemsRaw, &items); err != nil {
		return nil, false
	}
	changed := false
	for i, itemRaw := range items {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(itemRaw, &fields); err != nil {
			return nil, false
		}
		raw, ok := fields["content"]
		if !ok || len(raw) == 0 || raw[0] != '"' {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false
		}
		block, err := json.Marshal([]map[string]any{{"type": "text", "text": s}})
		if err != nil {
			return nil, false
		}
		fields["content"] = block
		fixedItem, err := json.Marshal(fields)
		if err != nil {
			return nil, false
		}
		items[i] = fixedItem
		changed = true
	}
	if !changed {
		return nil, false
	}
	fixedItems, err := json.Marshal(items)
	if err != nil {
		return nil, false
	}
	top["items"] = fixedItems
	out, err := json.Marshal(top)
	if err != nil {
		return nil, false
	}
	return out, true
}
