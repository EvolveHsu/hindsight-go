package server

import (
	"net/http"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
)

// AsHTTPHandler builds the generated ogen server around an Engine with the
// shared error mapping and the plain-string content bridge. main.go calls this;
// tests use the same path, so the wire behavior under test is the wire behavior
// in production.
func AsHTTPHandler(e *Engine) (http.Handler, error) {
	h, err := api.NewServer(e, api.WithErrorHandler(ErrorHandler))
	if err != nil {
		return nil, err
	}
	return NewContentRewriter(h), nil
}
