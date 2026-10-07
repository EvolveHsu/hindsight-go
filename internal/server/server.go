// Package server holds the thin HTTP handlers for hindsight-go.
//
// The embedded generated Handler covers all 99 operations; handlers implemented
// here override the monitoring surface first (health/live/ready/version/metrics),
// which is also what the upstream deployment probes rely on. Everything else
// still returns 501 through the generated UnimplementedHandler, so the server
// is honest about what it does not do yet - the engine/storage layers are not
// implemented.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-faster/jx"
	"github.com/ogen-go/ogen/ogenerrors"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
)

// Version is the semantic version of this Go implementation.
const Version = "0.1.0"

// UpstreamAPIVersion mirrors the upstream OpenAPI info.version the generated
// types were frozen from.
const UpstreamAPIVersion = "0.10.2"

// startedAt backs the liveness uptime field.
var startedAt = time.Now()

// readiness is flipped by the storage layer once a DB connection exists. The
// monitoring surface reports 503 until then, mirroring upstream /health/ready
// semantics (readiness checks the database).
var readiness atomic.Bool

// SetReady flips the readiness gate (called by the storage layer at startup).
func SetReady(v bool) { readiness.Store(v) }

// statusCarrier is the contract for errors that know their own HTTP status and
// body. statusError implements it; ErrorHandler unwraps it.
type statusCarrier interface {
	StatusCode() int
	Body() map[string]any
}

func (e *statusError) StatusCode() int      { return e.status }
func (e *statusError) Body() map[string]any { return e.body }

// ErrorHandler maps handler errors to responses. Passed to api.NewServer via
// api.WithErrorHandler in main.go and tests: status carriers keep their status,
// everything else falls back to ogen's default (501/500).
func ErrorHandler(ctx context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	var sc statusCarrier
	if errors.As(err, &sc) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(sc.StatusCode())
		_ = json.NewEncoder(w).Encode(sc.Body())
		return
	}
	ogenerrors.DefaultErrorHandler(ctx, w, nil, err)
}

// Server implements the generated api.Handler interface.
type Server struct {
	api.UnimplementedHandler
}

// compile-time proof the handlers stay in lockstep with the generated interface.
var _ api.Handler = (*Server)(nil)

// statusError carries an HTTP status + JSON body through ogen's error path. The
// server wires ErrorHandler to honor it (see ErrorHandlerOption in main.go).
type statusError struct {
	status int
	body   map[string]any
}

func (e *statusError) Error() string {
	b, _ := json.Marshal(e.body)
	return string(b)
}

// GetLiveness implements get_liveness.
func (s *Server) GetLiveness(ctx context.Context) (*api.LivenessResponse, error) {
	return &api.LivenessResponse{
		Status:        "alive",
		Version:       Version,
		UptimeSeconds: time.Since(startedAt).Seconds(),
	}, nil
}

// GetReadiness implements get_readiness. The generated return type is jx.Raw
// because the spec carries no schema for this response; the readiness gate
// follows the upstream semantics (503 until the storage layer is connected).
func (s *Server) GetReadiness(ctx context.Context) (jx.Raw, error) {
	if !readiness.Load() {
		return jx.Raw{}, &statusError{
			status: http.StatusServiceUnavailable,
			body:   map[string]any{"detail": "storage layer not connected"},
		}
	}
	raw, err := json.Marshal(map[string]any{"status": "ready"})
	if err != nil {
		return jx.Raw{}, err
	}
	return jx.Raw(raw), nil
}

// HealthEndpointHealthGet implements health_endpoint_health_get.
func (s *Server) HealthEndpointHealthGet(ctx context.Context) (jx.Raw, error) {
	// Upstream /health reports component status; the storage layer flips the
	// readiness gate once a real backend connection exists.
	database := "disconnected"
	if readiness.Load() {
		database = "connected"
	}
	raw, err := json.Marshal(map[string]any{
		"status":   "healthy",
		"version":  Version,
		"database": database,
	})
	if err != nil {
		return jx.Raw{}, err
	}
	return jx.Raw(raw), nil
}

// GetVersion implements get_version.
func (s *Server) GetVersion(ctx context.Context) (*api.VersionResponse, error) {
	return &api.VersionResponse{
		APIVersion: UpstreamAPIVersion,
		Features: api.FeaturesInfo{
			Observations:      true,
			Mcp:               true,
			Worker:            false,
			BankConfigAPI:     true,
			BankLlmHealth:     true,
			FileUploadAPI:     true,
			DocumentExportAPI: true,
			DocumentImportAPI: true,
			AuditLog:          true,
			LlmTrace:          true,
			StoreDocumentText: true,
		},
	}, nil
}

// MetricsEndpointMetricsGet implements metrics_endpoint_metrics_get. Prometheus
// exposition is added when the first real workload lands; until then the
// endpoint answers with a minimal valid payload rather than 404.
func (s *Server) MetricsEndpointMetricsGet(ctx context.Context) (jx.Raw, error) {
	raw, err := json.Marshal(map[string]any{
		"implementation": "hindsight-go",
		"version":        Version,
	})
	if err != nil {
		return jx.Raw{}, err
	}
	return jx.Raw(raw), nil
}
