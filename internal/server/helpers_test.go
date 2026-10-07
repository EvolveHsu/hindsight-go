package server

import (
	"net/http/httptest"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/memory"
)

// newEngineServerWithStore exposes the store handle for tests that need to
// assert on storage state directly.
func newEngineServerWithStore(t *testing.T, store Store) *httptest.Server {
	t.Helper()
	h, err := AsHTTPHandler(NewEngine(store))
	if err != nil {
		t.Fatalf("AsHTTPHandler: %v", err)
	}
	return httptest.NewServer(h)
}

var _ = memory.New
