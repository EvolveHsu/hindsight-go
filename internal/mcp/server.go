// Package mcp serves the Model Context Protocol over the Streamable HTTP
// transport, backed by the same REST handler as the /v1 surface.
//
// It exists so the Go build can take over the endpoint the Python deployment
// owns today (POST /mcp/{bank}/, plus the GET probe clients send first) without
// a second implementation of the memory engine: the tool catalog is the live
// payload captured from the upstream server, and every tool call is replayed
// against the generated REST handler in process.
package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// DefaultServerName matches the upstream serverInfo name so clients that key
	// on it see no change.
	DefaultServerName = "hindsight-mcp-server"

	// latestProtocolVersion is what we answer when a client asks for a version
	// we do not know.
	latestProtocolVersion = "2025-06-18"

	// pathPrefix is the mount point; the bank id is the next path segment.
	pathPrefix = "/mcp"

	// maxBodyBytes bounds a single JSON-RPC request.
	maxBodyBytes = 4 << 20
)

// supportedProtocolVersions are the MCP revisions this server speaks. The
// transport is identical across them, so a client that asks for one of these
// gets it echoed back.
var supportedProtocolVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// Config wires the MCP surface.
type Config struct {
	// API is the generated REST handler. Tool calls replay through it.
	API http.Handler

	// Version is reported as serverInfo.version. Defaults to "0.1.0".
	Version string

	// ServerName overrides serverInfo.name. Defaults to DefaultServerName.
	ServerName string

	// DefaultBank backs the bare /mcp endpoint (no bank in the path).
	DefaultBank string

	// Instructions, when set, is returned from initialize.
	Instructions string
}

// Server is the MCP endpoint. It is safe for concurrent use.
type Server struct {
	cfg    Config
	tools  *catalog
	bridge *bridge
}

// New builds the MCP surface around an already-constructed REST handler.
func New(cfg Config) (*Server, error) {
	if cfg.API == nil {
		return nil, errors.New("mcp: API handler is required")
	}
	if cfg.Version == "" {
		cfg.Version = "0.1.0"
	}
	if cfg.ServerName == "" {
		cfg.ServerName = DefaultServerName
	}
	if cfg.DefaultBank == "" {
		cfg.DefaultBank = "default"
	}
	tools, err := loadCatalog()
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, tools: tools, bridge: &bridge{api: cfg.API}}, nil
}

// ToolNames returns the exposed tool names in catalog order. Tests and the
// cutover check use it to assert the surface did not shrink.
func (s *Server) ToolNames() []string {
	names := make([]string, 0, len(s.tools.raw))
	for _, entry := range s.tools.raw {
		var meta struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(entry, &meta); err != nil {
			continue
		}
		names = append(names, meta.Name)
	}
	return names
}

// Wrap routes the MCP paths to this server and everything else to next, so the
// REST surface keeps its own routing and behavior.
func (s *Server) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := bankFromPath(r.URL.Path, s.cfg.DefaultBank); !ok {
			next.ServeHTTP(w, r)
			return
		}
		s.ServeHTTP(w, r)
	})
}

// bankFromPath extracts the bank id from /mcp/{bank}[/...]. The bare /mcp and
// /mcp/ endpoints fall back to the default bank.
func bankFromPath(path, fallback string) (string, bool) {
	if path != pathPrefix && !strings.HasPrefix(path, pathPrefix+"/") {
		return "", false
	}
	rest := strings.TrimPrefix(path, pathPrefix)
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return fallback, true
	}
	segment := rest
	if idx := strings.IndexByte(rest, '/'); idx >= 0 {
		segment = rest[:idx]
	}
	if segment == "" {
		return fallback, true
	}
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return "", false
	}
	return decoded, true
}

// ServeHTTP implements the Streamable HTTP transport:
//
//	POST  JSON-RPC request (SSE or JSON response, per Accept)
//	GET   client probe: 200 {} without a session, 405 with one (no SSE stream)
//	DELETE session teardown
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bank, ok := bankFromPath(r.URL.Path, s.cfg.DefaultBank)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r, bank)
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodDelete:
		writeJSON(w, http.StatusOK, map[string]any{})
	case http.MethodOptions:
		w.Header().Set("Allow", "POST, GET, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Mcp-Session-Id, Authorization, X-Bank-Id")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, DELETE, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGet answers the pre-initialize probe clients send (200, empty JSON).
// With a session the spec allows either an SSE stream or 405; this server
// answers every result inline on the POST, so it declines the stream.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Mcp-Session-Id") == "" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
		"jsonrpc": "2.0",
		"error": map[string]any{
			"code":    codeInvalidRequest,
			"message": "this server does not offer a standalone SSE stream; send requests with POST",
		},
		"id": nil,
	})
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, bank string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		s.writeRPC(w, r, newError(nil, codeParseError, "cannot read request body", nil), "")
		return
	}
	_ = r.Body.Close()

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeRPC(w, r, newError(nil, codeParseError, "invalid JSON", nil), "")
		return
	}
	if req.Method == "" {
		s.writeRPC(w, r, newError(req.ID, codeInvalidRequest, "method is required", nil), "")
		return
	}

	session := s.sessionFor(r, req.Method)
	if req.isNotification() {
		// Notifications carry no response. 202 with no body is the shape the
		// spec and the upstream server both use.
		if session != "" {
			w.Header().Set("Mcp-Session-Id", session)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := s.dispatch(r, bank, &req)
	s.writeRPC(w, r, resp, session)
}

// sessionFor returns the session id to echo: the client's own if it sent one,
// a fresh id for initialize otherwise.
//
// Sessions are not enforced. The Python deployment issues ids, and some clients
// send the id on every later request, so we echo it; but requiring it would
// break the simple POST-only clients (and the cutover check) that never
// initialize. Nothing server-side is keyed on the id, so accepting both is
// strictly more compatible.
func (s *Server) sessionFor(r *http.Request, method string) string {
	if existing := r.Header.Get("Mcp-Session-Id"); existing != "" {
		return existing
	}
	if method != "initialize" {
		return ""
	}
	return newSessionID()
}

func newSessionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(buf[:])
}

// dispatch runs one JSON-RPC request.
func (s *Server) dispatch(r *http.Request, bank string, req *rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return newResult(req.ID, s.initializeResult(req.Params))
	case "ping":
		return newResult(req.ID, map[string]any{})
	case "tools/list":
		return newResult(req.ID, s.tools.list())
	case "tools/call":
		return s.toolsCall(r, bank, req)
	case "prompts/list":
		return newResult(req.ID, map[string]any{"prompts": []any{}})
	case "resources/list":
		return newResult(req.ID, map[string]any{"resources": []any{}})
	case "resources/templates/list":
		return newResult(req.ID, map[string]any{"resourceTemplates": []any{}})
	case "logging/setLevel":
		return newResult(req.ID, map[string]any{})
	default:
		return newError(req.ID, codeMethodNotFound, "Method not found: "+req.Method, nil)
	}
}

func (s *Server) initializeResult(params json.RawMessage) map[string]any {
	var request struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &request)

	version := request.ProtocolVersion
	if !supportedProtocolVersions[version] {
		version = latestProtocolVersion
	}

	result := map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools":     map[string]any{"listChanged": false},
			"logging":   map[string]any{},
			"prompts":   map[string]any{"listChanged": false},
			"resources": map[string]any{"subscribe": false, "listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    s.cfg.ServerName,
			"version": s.cfg.Version,
		},
	}
	if s.cfg.Instructions != "" {
		result["instructions"] = s.cfg.Instructions
	}
	return result
}

func (s *Server) toolsCall(r *http.Request, bank string, req *rpcRequest) rpcResponse {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return newError(req.ID, codeInvalidParams, "invalid tools/call params", nil)
	}
	if params.Name == "" {
		return newError(req.ID, codeInvalidParams, "tools/call needs a tool name", nil)
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}

	result, rpcErr := s.callTool(r.Context(), bank, params.Name, params.Arguments)
	if rpcErr != nil {
		return newError(req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
	}

	text := result.text
	if text == "" {
		encoded, err := json.Marshal(result.payload)
		if err != nil {
			return newError(req.ID, codeInternalError, "cannot encode tool result", nil)
		}
		text = string(encoded)
	}

	content := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": result.isError,
	}
	// structuredContent is only legal for object payloads; a scalar or array
	// result still reaches the model through the text block.
	if _, ok := result.payload.(map[string]any); ok {
		content["structuredContent"] = result.payload
	}
	return newResult(req.ID, content)
}

// writeRPC writes a response, as SSE when the client accepts it. The upstream
// server always answers SSE, and the client that is being migrated already
// handles that, so SSE is the default when Accept is absent.
func (s *Server) writeRPC(w http.ResponseWriter, r *http.Request, resp rpcResponse, session string) {
	if session != "" {
		w.Header().Set("Mcp-Session-Id", session)
	}
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")

	if !acceptsSSE(r) {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	payload, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "cannot encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// acceptsSSE reports whether the request accepts text/event-stream. An absent
// Accept header counts as yes, matching the upstream server.
func acceptsSSE(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return true
	}
	return strings.Contains(accept, "text/event-stream") || strings.Contains(accept, "*/*")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
