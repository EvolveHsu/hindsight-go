package mcp

import "encoding/json"

// JSON-RPC 2.0 error codes, plus the MCP convention that protocol-level
// failures come back as errors while tool-level failures ride inside a normal
// result with isError=true.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// rpcRequest is one JSON-RPC request. ID is kept raw so a numeric id comes back
// as a number and a string id as a string, which is what clients compare on.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the request carries no id. JSON-RPC
// notifications get no response body at all; `"id": null` counts as one too,
// which is how several clients send notifications/initialized.
func (r *rpcRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// rpcResponse is one JSON-RPC response. MarshalJSON is hand-rolled because the
// struct tags cannot express "exactly one of result/error, and result may be an
// empty object" - omitempty would drop `"result":{}`.
type rpcResponse struct {
	JSONRPC string
	ID      json.RawMessage
	Result  any
	Error   *rpcError
}

func (r rpcResponse) MarshalJSON() ([]byte, error) {
	payload := map[string]any{"jsonrpc": "2.0", "id": r.ID}
	if r.Error != nil {
		payload["error"] = r.Error
	} else {
		payload["result"] = r.Result
	}
	return json.Marshal(payload)
}

func newResult(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func newError(id json.RawMessage, code int, message string, data any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}}
}
