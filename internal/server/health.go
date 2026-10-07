package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-faster/jx"
)

type pinger interface {
	Ping(ctx context.Context) error
}

// HealthEndpointHealthGet overrides the embedded monitoring handler so health
// reflects the real storage connection instead of a startup flag.
func (e *Engine) HealthEndpointHealthGet(ctx context.Context) (jx.Raw, error) {
	database := "connected"
	status := "healthy"
	if p, ok := e.store.(pinger); ok {
		if err := p.Ping(ctx); err != nil {
			database = "disconnected"
			status = "unhealthy"
			raw, _ := json.Marshal(map[string]any{"status": status, "version": Version, "database": database})
			return jx.Raw{}, &statusError{status: http.StatusServiceUnavailable, body: map[string]any{
				"status": status, "version": Version, "database": database, "detail": err.Error(), "_raw": string(raw),
			}}
		}
	}
	raw, err := json.Marshal(map[string]any{"status": status, "version": Version, "database": database})
	if err != nil {
		return jx.Raw{}, err
	}
	return jx.Raw(raw), nil
}

// GetReadiness overrides the embedded readiness handler with a live ping.
func (e *Engine) GetReadiness(ctx context.Context) (jx.Raw, error) {
	if p, ok := e.store.(pinger); ok {
		if err := p.Ping(ctx); err != nil {
			return jx.Raw{}, &statusError{status: http.StatusServiceUnavailable, body: map[string]any{
				"status": "not ready", "detail": err.Error(),
			}}
		}
	}
	raw, err := json.Marshal(map[string]any{"status": "ready"})
	if err != nil {
		return jx.Raw{}, err
	}
	return jx.Raw(raw), nil
}

// MetricsEndpointMetricsGet returns a valid Prometheus exposition payload.
func (e *Engine) MetricsEndpointMetricsGet(ctx context.Context) (jx.Raw, error) {
	ready := 1
	if p, ok := e.store.(pinger); ok {
		if err := p.Ping(ctx); err != nil {
			ready = 0
		}
	}
	body := "# HELP hindsight_up Whether the Hindsight API is ready to serve traffic.\n" +
		"# TYPE hindsight_up gauge\n" +
		"hindsight_up " + itoa(ready) + "\n"
	return jx.Raw(body), nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	return "1"
}
