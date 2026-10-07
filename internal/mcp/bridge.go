package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// bridge replays a tool call as an in-process HTTP request against the same
// generated REST handler that serves /v1/... .
//
// One request path, not two implementations: whatever the REST surface
// supports, the MCP surface supports, including the plain-string retain
// bridge, the error mapping, and the 501s for operations that are not built
// yet. That also keeps the two surfaces from drifting as handlers land.
type bridge struct {
	api http.Handler
}

// restCall is one replayed request.
type restCall struct {
	method string
	path   string
	query  url.Values
	body   any
	// follow carries side-effect calls a tool needs after the primary one,
	// for work the primary REST operation cannot express. The primary
	// response is still what the tool returns; a failing follow turns the
	// call into an error result.
	follow []restCall
}

// internalBase is the placeholder origin for the replayed request. The handler
// routes on path only, so the host never matters.
const internalBase = "http://hindsight-go.internal"

// do runs the call and returns the HTTP status plus the response body. A
// non-2xx status is not a transport error: it is a result the model should see.
func (b *bridge) do(ctx context.Context, call restCall) (int, []byte, error) {
	var payload *bytes.Reader
	if call.body != nil {
		raw, err := json.Marshal(call.body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(raw)
	} else {
		payload = bytes.NewReader(nil)
	}

	target := internalBase + call.path
	if len(call.query) > 0 {
		target += "?" + call.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, call.method, target, payload)
	if err != nil {
		return 0, nil, err
	}
	if call.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := &recorder{}
	b.api.ServeHTTP(rec, req)
	return rec.status(), rec.body.Bytes(), nil
}

// recorder is a minimal http.ResponseWriter so the replay needs no real socket
// and no test-only helper in the production binary.
type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(b)
}

func (r *recorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}
