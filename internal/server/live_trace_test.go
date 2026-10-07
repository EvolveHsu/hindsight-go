package server

import "testing"

// TestHealthOverWire exercises the monitoring surface through the full router
// (these handlers were verified green in server_test.go via api.NewServer; this
// test guards the AsHTTPHandler wrapper path too).
func TestHealthOverWire(t *testing.T) {
	ts := newEngineServer(t)
	defer ts.Close()
	code, body := doJSON(t, "GET", ts.URL+"/health", nil)
	t.Logf("/health -> %d %v", code, body)
	if code != 200 {
		t.Errorf("/health status = %d", code)
	}
	code, body = doJSON(t, "GET", ts.URL+"/version", nil)
	t.Logf("/version -> %d %v", code, body)
	if code != 200 {
		t.Errorf("/version status = %d", code)
	}
}
