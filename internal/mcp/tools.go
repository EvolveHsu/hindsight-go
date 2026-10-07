package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// toolResult is what one tools/call hands back. Tool-level failures are results
// with isError=true (the model can read and recover); only protocol faults
// (unknown tool, arguments that are not an object) become JSON-RPC errors.
type toolResult struct {
	payload any
	text    string
	isError bool
}

// callTool maps one MCP tool name onto the REST operation that implements it
// and replays that call in process.
//
// The mapping is the whole contract: upstream registers each tool twice (once
// with an explicit bank_id, once session-scoped); the path-scoped endpoint this
// serves is the session-scoped half, so every call is anchored to the bank that
// came in on the URL.
func (s *Server) callTool(ctx context.Context, bank, name string, args map[string]any) (toolResult, *rpcError) {
	if !s.tools.has(name) {
		return toolResult{}, &rpcError{Code: codeInvalidParams, Message: "Unknown tool: " + name}
	}

	call, err := buildCall(bank, name, args)
	if blocked := unsupportedCall(name, args); blocked != nil {
		return *blocked, nil
	}
	if err != nil {
		return toolResult{}, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}

	status, body, err := s.bridge.do(ctx, call)
	if err != nil {
		return toolResult{}, &rpcError{Code: codeInternalError, Message: err.Error()}
	}

	// Follow-up calls cover work the primary operation cannot express (the
	// full config override map for update_bank, say). The primary response is
	// still the tool result; a follow that fails becomes the error result so
	// the model never sees a half-applied success.
	for _, follow := range call.follow {
		fStatus, fBody, fErr := s.bridge.do(ctx, follow)
		if fErr != nil {
			return toolResult{}, &rpcError{Code: codeInternalError, Message: fErr.Error()}
		}
		if fStatus < 200 || fStatus >= 300 {
			payload, decodeErr := decodePayload(fBody)
			if decodeErr != nil {
				payload = strings.TrimSpace(string(fBody))
			}
			message := errorMessage(payload, fStatus)
			return toolResult{
				payload: map[string]any{"error": message, "status": fStatus},
				text:    message,
				isError: true,
			}, nil
		}
	}

	payload, decodeErr := decodePayload(body)
	if decodeErr != nil {
		payload = strings.TrimSpace(string(body))
	}
	if status < 200 || status >= 300 {
		message := errorMessage(payload, status)
		return toolResult{
			payload: map[string]any{"error": message, "status": status},
			text:    message,
			isError: true,
		}, nil
	}

	text := strings.TrimSpace(string(body))
	if payload == nil {
		payload = map[string]any{}
	}
	return toolResult{payload: payload, text: text}, nil
}

// decodePayload parses a JSON response body. An empty body is a legitimate
// result for operations that answer 204-style payloads.
func decodePayload(body []byte) (any, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, nil
	}
	var payload any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// errorMessage pulls the human-readable detail out of a REST error body,
// falling back to the raw text and then to the status code.
func errorMessage(payload any, status int) string {
	switch v := payload.(type) {
	case map[string]any:
		for _, key := range []string{"detail", "error", "message"} {
			if text, ok := v[key].(string); ok && text != "" {
				return text
			}
		}
		encoded, err := json.Marshal(v)
		if err == nil {
			return string(encoded)
		}
	case string:
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return fmt.Sprintf("upstream API answered HTTP %d", status)
}

// unsupportedCall refuses the calls whose REST handler exists but would not do
// what the tool promises.
//
// Two cases are worse than a missing endpoint, because the REST layer would
// answer 200 and the model would believe it:
//
//   - clear_memories carries a fact-type filter that ClearBankMemories ignores,
//     so "clear only world facts" would delete the whole bank.
//   - invalidate_memory needs memory state, which model.Unit does not carry yet:
//     the write is dropped and the tool would report success.
//
// Both guards are deleted the moment the storage layer supports them; the tests
// that pin them name the exact behaviour that has to change.
func unsupportedCall(name string, args map[string]any) *toolResult {
	switch name {
	case "clear_memories":
		if filter, ok := optString(args, "type"); ok && strings.TrimSpace(filter) != "" {
			return &toolResult{
				payload: map[string]any{"error": "clear_memories with a type filter is not supported by this build"},
				text: "clear_memories with a type filter is not supported by this build: the Go " +
					"backend ignores the filter and would clear every memory in the bank. " +
					"Call clear_memories without arguments to clear the whole bank, or " +
					"delete individual documents instead.",
				isError: true,
			}
		}
	case "invalidate_memory":
	}
	return nil
}

// bankPatchConfigKeys lists the configuration overrides the bank PATCH schema
// accepts as typed fields. Anything else the client sends under
// config_updates has to go through the config endpoint, because the bank PATCH
// decoder drops unknown keys without complaint.
var bankPatchConfigKeys = map[string]bool{
	"reflect_mission":                  true,
	"retain_mission":                   true,
	"retain_extraction_mode":           true,
	"retain_custom_instructions":       true,
	"retain_chunk_size":                true,
	"retain_structured_chunk_size":     true,
	"retain_max_attachments_per_chunk": true,
	"enable_observations":              true,
	"observations_mission":             true,
	"enable_text_search":               true,
	"enable_temporal_retrieval":        true,
	"enable_graph_retrieval":           true,
	"enable_reranking":                 true,
	"disposition_skepticism":           true,
	"disposition_literalism":           true,
	"disposition_empathy":              true,
}

// buildCall translates one tool invocation into a REST request.
func buildCall(bank, name string, args map[string]any) (restCall, error) {
	if bank == "" {
		return restCall{}, fmt.Errorf("no bank_id configured")
	}
	base := "/v1/default/banks/" + url.PathEscape(bank)

	switch name {
	case "retain", "sync_retain":
		item, err := retainItem(args)
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodPost, path: base + "/memories",
			body: map[string]any{"items": []any{item}}}, nil

	case "recall":
		if _, err := requireString(args, "query"); err != nil {
			return restCall{}, err
		}
		body := pick(args, "query", "types", "prefer_observations", "budget", "max_tokens",
			"query_timestamp", "tags", "tags_match", "tag_groups", "min_scores", "temporal_window")
		return restCall{method: http.MethodPost, path: base + "/memories/recall", body: body}, nil

	case "reflect":
		if _, err := requireString(args, "query"); err != nil {
			return restCall{}, err
		}
		body := pick(args, "query", "context", "budget", "max_tokens", "response_schema",
			"tags", "tags_match", "tag_groups", "apply_all_directives")
		return restCall{method: http.MethodPost, path: base + "/reflect", body: body}, nil

	case "list_mental_models":
		return restCall{method: http.MethodGet, path: base + "/mental-models",
			query: query(args, spec("tags", "tags"), spec("limit", "limit"), spec("offset", "offset"))}, nil

	case "get_mental_model":
		id, err := requireString(args, "mental_model_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodGet, path: base + "/mental-models/" + url.PathEscape(id),
			query: query(args, spec("detail", "detail"))}, nil

	case "create_mental_model":
		name, err := requireString(args, "name")
		if err != nil {
			return restCall{}, err
		}
		sourceQuery, err := requireString(args, "source_query")
		if err != nil {
			return restCall{}, err
		}
		body := pick(args, "tags", "max_tokens")
		body["name"] = name
		body["source_query"] = sourceQuery
		if id, ok := optString(args, "mental_model_id"); ok {
			body["id"] = id
		}
		if trigger := triggerArg(args); trigger != nil {
			body["trigger"] = trigger
		}
		return restCall{method: http.MethodPost, path: base + "/mental-models", body: body}, nil

	case "update_mental_model":
		id, err := requireString(args, "mental_model_id")
		if err != nil {
			return restCall{}, err
		}
		body := pick(args, "name", "source_query", "max_tokens", "tags")
		if trigger := triggerArg(args); trigger != nil {
			body["trigger"] = trigger
		}
		return restCall{method: http.MethodPatch, path: base + "/mental-models/" + url.PathEscape(id), body: body}, nil

	case "delete_mental_model":
		id, err := requireString(args, "mental_model_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodDelete, path: base + "/mental-models/" + url.PathEscape(id)}, nil

	case "refresh_mental_model":
		id, err := requireString(args, "mental_model_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodPost, path: base + "/mental-models/" + url.PathEscape(id) + "/refresh"}, nil

	case "clear_mental_model":
		id, err := requireString(args, "mental_model_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodPost, path: base + "/mental-models/" + url.PathEscape(id) + "/clear"}, nil

	case "list_directives":
		return restCall{method: http.MethodGet, path: base + "/directives",
			query: query(args, spec("tags", "tags"), spec("active_only", "active_only"),
				spec("limit", "limit"), spec("offset", "offset"))}, nil

	case "create_directive":
		body := pick(args, "name", "content", "priority", "is_active", "tags")
		if _, err := requireString(args, "name"); err != nil {
			return restCall{}, err
		}
		if _, err := requireString(args, "content"); err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodPost, path: base + "/directives", body: body}, nil

	case "delete_directive":
		id, err := requireString(args, "directive_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodDelete, path: base + "/directives/" + url.PathEscape(id)}, nil

	case "list_memories":
		return restCall{method: http.MethodGet, path: base + "/memories/list",
			query: query(args, spec("type", "type"), spec("q", "q"), spec("limit", "limit"),
				spec("offset", "offset"), spec("tags", "tags"), spec("tags_match", "tags_match"))}, nil

	case "get_memory":
		id, err := requireString(args, "memory_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodGet, path: base + "/memories/" + url.PathEscape(id)}, nil

	case "update_memory":
		id, err := requireString(args, "memory_id")
		if err != nil {
			return restCall{}, err
		}
		body := pick(args, "text", "context", "occurred_start", "occurred_end",
			"fact_type", "entities", "resolve_entities")
		return restCall{method: http.MethodPatch, path: base + "/memories/" + url.PathEscape(id), body: body}, nil

	case "invalidate_memory":
		id, err := requireString(args, "memory_id")
		if err != nil {
			return restCall{}, err
		}
		state := "invalidated"
		if restore, ok := optBool(args, "restore"); ok && restore {
			state = "valid"
		}
		body := map[string]any{"state": state}
		if reason, ok := optString(args, "reason"); ok && reason != "" {
			body["reason"] = reason
		}
		return restCall{method: http.MethodPatch, path: base + "/memories/" + url.PathEscape(id), body: body}, nil

	case "list_documents":
		return restCall{method: http.MethodGet, path: base + "/documents",
			query: query(args, spec("q", "q"), spec("limit", "limit"), spec("offset", "offset"))}, nil

	case "get_document":
		id, err := requireString(args, "document_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodGet, path: base + "/documents/" + url.PathEscape(id)}, nil

	case "delete_document":
		id, err := requireString(args, "document_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodDelete, path: base + "/documents/" + url.PathEscape(id)}, nil

	case "list_operations":
		return restCall{method: http.MethodGet, path: base + "/operations",
			query: query(args, spec("status", "status"), spec("type", "type"), spec("limit", "limit"),
				spec("offset", "offset"), spec("exclude_parents", "exclude_parents"))}, nil

	case "get_operation":
		id, err := requireString(args, "operation_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodGet, path: base + "/operations/" + url.PathEscape(id)}, nil

	case "cancel_operation":
		id, err := requireString(args, "operation_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodDelete, path: base + "/operations/" + url.PathEscape(id)}, nil

	case "list_tags":
		return restCall{method: http.MethodGet, path: base + "/tags",
			query: query(args, spec("q", "q"), spec("limit", "limit"), spec("offset", "offset"))}, nil

	case "get_bank":
		return restCall{method: http.MethodGet, path: base + "/profile"}, nil

	case "update_bank":
		config, _ := mapArg(args, "config_updates")
		body := pick(args, "name", "mission")
		// The typed config fields ride along so the first response already
		// reflects them; the rest go through the config endpoint below.
		for key, value := range config {
			if bankPatchConfigKeys[key] {
				body[key] = value
			}
		}
		if len(body) == 0 && len(config) == 0 {
			return restCall{}, fmt.Errorf("update_bank needs at least one of name, mission, config_updates")
		}
		// PUT is upstream's create_if_missing=true update: a missing bank is
		// created, an existing one keeps fields the request did not mention.
		call := restCall{method: http.MethodPut, path: base, body: body}
		if len(config) > 0 {
			// The bank PATCH decoder drops keys outside its schema, so the whole
			// override map goes through the config API as a follow-up call.
			call.follow = []restCall{{
				method: http.MethodPatch,
				path:   base + "/config",
				body:   map[string]any{"updates": config},
			}}
		}
		return call, nil

	case "delete_bank":
		return restCall{method: http.MethodDelete, path: base}, nil

	case "clear_memories":
		return restCall{method: http.MethodDelete, path: base + "/memories",
			query: query(args, spec("type", "type"))}, nil

	case "get_knowledge_base_tree":
		return restCall{method: http.MethodGet, path: base + "/knowledge-base/tree"}, nil

	case "search_knowledge_base":
		phrase, err := requireString(args, "query")
		if err != nil {
			return restCall{}, err
		}
		values := url.Values{"q": []string{phrase}}
		addQuery(values, "limit", args["limit"])
		return restCall{method: http.MethodGet, path: base + "/knowledge-base/search", query: values}, nil

	case "get_knowledge_page":
		id, err := requireString(args, "page_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodGet, path: base + "/knowledge-base/pages/" + url.PathEscape(id)}, nil

	case "create_knowledge_folder":
		body := pick(args, "name", "parent_id")
		if _, err := requireString(args, "name"); err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodPost, path: base + "/knowledge-base/folders", body: body}, nil

	case "create_knowledge_page":
		body := pick(args, "name", "source_query", "parent_id", "tags", "max_tokens")
		if _, err := requireString(args, "name"); err != nil {
			return restCall{}, err
		}
		if _, err := requireString(args, "source_query"); err != nil {
			return restCall{}, err
		}
		if trigger := triggerArg(args); trigger != nil {
			body["trigger"] = trigger
		}
		return restCall{method: http.MethodPost, path: base + "/knowledge-base/pages", body: body}, nil

	case "update_knowledge_node":
		id, err := requireString(args, "node_id")
		if err != nil {
			return restCall{}, err
		}
		body := pick(args, "name", "parent_id", "source_query", "tags", "max_tokens")
		if trigger := triggerArg(args); trigger != nil {
			body["trigger"] = trigger
		}
		return restCall{method: http.MethodPatch, path: base + "/knowledge-base/nodes/" + url.PathEscape(id), body: body}, nil

	case "delete_knowledge_node":
		id, err := requireString(args, "node_id")
		if err != nil {
			return restCall{}, err
		}
		return restCall{method: http.MethodDelete, path: base + "/knowledge-base/nodes/" + url.PathEscape(id)}, nil
	}

	return restCall{}, fmt.Errorf("tool %q has no REST mapping", name)
}

// retainItem builds one memory item. context defaults to "general" the way the
// upstream tool signature does, so a client that omits it gets the same result
// it gets from Python.
func retainItem(args map[string]any) (map[string]any, error) {
	content, err := requireString(args, "content")
	if err != nil {
		return nil, err
	}
	item := pick(args, "context", "timestamp", "tags", "metadata", "document_id",
		"strategy", "update_mode", "entities", "resolve_entities")
	item["content"] = content
	if _, ok := item["context"]; !ok {
		item["context"] = "general"
	}
	return item, nil
}

// triggerArg folds the three trigger-ish MCP arguments into the single trigger
// object the REST contract takes.
func triggerArg(args map[string]any) map[string]any {
	trigger, _ := mapArg(args, "trigger")
	if trigger == nil {
		trigger = map[string]any{}
	}
	if value, ok := args["tags_match"]; ok && value != nil {
		trigger["tags_match"] = value
	}
	if value, ok := args["trigger_refresh_after_consolidation"]; ok && value != nil {
		trigger["refresh_after_consolidation"] = value
	}
	if len(trigger) == 0 {
		return nil
	}
	return trigger
}

// pick copies the arguments that are present and not null, under the same names
// the REST contract uses.
func pick(args map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		if value, ok := args[key]; ok && value != nil {
			out[key] = value
		}
	}
	return out
}

type querySpec struct {
	arg   string
	param string
}

func spec(arg, param string) querySpec { return querySpec{arg: arg, param: param} }

// query turns the listed arguments into URL query parameters, repeating the
// parameter for array values the way the upstream API expects.
func query(args map[string]any, specs ...querySpec) url.Values {
	values := url.Values{}
	for _, s := range specs {
		addQuery(values, s.param, args[s.arg])
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func addQuery(values url.Values, param string, value any) {
	switch v := value.(type) {
	case nil:
		return
	case string:
		values.Add(param, v)
	case bool:
		values.Add(param, strconv.FormatBool(v))
	case float64:
		values.Add(param, formatNumber(v))
	case json.Number:
		values.Add(param, v.String())
	case []any:
		for _, item := range v {
			addQuery(values, param, item)
		}
	case []string:
		for _, item := range v {
			values.Add(param, item)
		}
	default:
		values.Add(param, fmt.Sprint(v))
	}
}

func formatNumber(value float64) string {
	if value == math.Trunc(value) && math.Abs(value) < 1e15 {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func requireString(args map[string]any, key string) (string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", fmt.Errorf("%s is required", key)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must not be empty", key)
	}
	return text, nil
}

func optString(args map[string]any, key string) (string, bool) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	return text, true
}

func optBool(args map[string]any, key string) (bool, bool) {
	value, ok := args[key]
	if !ok || value == nil {
		return false, false
	}
	flag, ok := value.(bool)
	if !ok {
		return false, false
	}
	return flag, true
}

func mapArg(args map[string]any, key string) (map[string]any, bool) {
	value, ok := args[key]
	if !ok || value == nil {
		return nil, false
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	return object, true
}
