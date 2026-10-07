// Command hindsight-go serves the Hindsight HTTP API (lite scope).
//
// Backend selection:
//
//	HINDSIGHT_GO_DATABASE_URL  -> PostgreSQL (internal/storepg)
//	unset                        -> in-process memory (internal/memory)
//
// When HINDSIGHT_GO_UPSTREAM_SCHEMA=1 the PostgreSQL backend maps onto an
// existing upstream deployment's tables instead of creating its own.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/EvolveHsu/hindsight-go/internal/embeddings"
	"github.com/EvolveHsu/hindsight-go/internal/extract"
	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/mcp"
	"github.com/EvolveHsu/hindsight-go/internal/memory"
	"github.com/EvolveHsu/hindsight-go/internal/server"
	"github.com/EvolveHsu/hindsight-go/internal/storepg"
)

func main() {
	addr := flag.String("addr", "0.0.0.0:8890", "listen address")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var embedder embeddings.Embedder
	if base := os.Getenv("HINDSIGHT_GO_EMBEDDINGS_BASE_URL"); base != "" {
		model := os.Getenv("HINDSIGHT_GO_EMBEDDINGS_MODEL")
		if model == "" {
			log.Fatalf("embeddings: HINDSIGHT_GO_EMBEDDINGS_MODEL is required when the base URL is set")
		}
		client := embeddings.NewClient(base, os.Getenv("HINDSIGHT_GO_EMBEDDINGS_API_KEY"), model)
		if raw := os.Getenv("HINDSIGHT_GO_EMBEDDINGS_DIMENSIONS"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value <= 0 {
				log.Fatalf("embeddings: invalid HINDSIGHT_GO_EMBEDDINGS_DIMENSIONS %q", raw)
			}
			client.Dimensions = value
		}
		if raw := os.Getenv("HINDSIGHT_GO_EMBEDDINGS_BATCH_SIZE"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value <= 0 {
				log.Fatalf("embeddings: invalid HINDSIGHT_GO_EMBEDDINGS_BATCH_SIZE %q", raw)
			}
			client.BatchSize = value
		}
		client.QueryPrefix = os.Getenv("HINDSIGHT_GO_EMBEDDINGS_QUERY_PREFIX")
		client.DocumentPrefix = os.Getenv("HINDSIGHT_GO_EMBEDDINGS_DOCUMENT_PREFIX")
		embedder = client
		log.Printf("embeddings: %s (model %q, dimensions %d)", base, model, client.Dimensions)
	}

	var store server.Store
	if dsn := os.Getenv("HINDSIGHT_GO_DATABASE_URL"); dsn != "" {
		var opts []storepg.Option
		if os.Getenv("HINDSIGHT_GO_UPSTREAM_SCHEMA") == "1" {
			opts = append(opts, storepg.WithUpstreamSchema())
		}
		if embedder != nil {
			opts = append(opts, storepg.WithEmbedder(embedder))
		}
		pg, err := storepg.New(ctx, dsn, opts...)
		if err != nil {
			log.Fatalf("postgres backend: %v", err)
		}
		defer pg.Close()
		store = pg
		server.SetReady(true)
		log.Printf("storage: postgresql")
	} else {
		store = memory.New(memory.WithEmbedder(embedder))
		log.Printf("storage: in-memory (set HINDSIGHT_GO_DATABASE_URL for PostgreSQL)")
	}

	engine := server.NewEngine(store)

	// Optional LLM provider (any OpenAI-compatible endpoint) enables reflect,
	// consolidation, and mental-model refresh. Without it those operations
	// answer 501 with setup instructions.
	if base := os.Getenv("HINDSIGHT_GO_LLM_BASE_URL"); base != "" {
		client := llm.NewClient(base, os.Getenv("HINDSIGHT_GO_LLM_API_KEY"))
		engine.SetReflectProvider(client)
		engine.SetExtractor(extract.New(client, os.Getenv("HINDSIGHT_GO_LLM_MODEL")))
		engine.SetConsolidationProvider(client)
		log.Printf("LLM provider: %s (model %q)", base, os.Getenv("HINDSIGHT_GO_LLM_MODEL"))
	}

	httpHandler, err := server.AsHTTPHandler(engine)
	if err != nil {
		log.Fatalf("build server: %v", err)
	}

	// The MCP surface rides on the same REST handler: tool calls replay
	// through it in process, so both surfaces answer identically and the 501s
	// for operations that are not built yet show up from either one.
	mcpServer, err := mcp.New(mcp.Config{
		API:          httpHandler,
		Version:      server.Version,
		DefaultBank:  os.Getenv("HINDSIGHT_GO_MCP_BANK_ID"),
		Instructions: os.Getenv("HINDSIGHT_GO_MCP_INSTRUCTIONS"),
	})
	if err != nil {
		log.Fatalf("build mcp server: %v", err)
	}
	handler := mcpServer.Wrap(httpHandler)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	log.Printf("hindsight-go %s (upstream contract %s) listening on http://%s", server.Version, server.UpstreamAPIVersion, *addr)
	log.Printf("health:  http://%s/health", *addr)
	log.Printf("version: http://%s/version", *addr)
	log.Printf("mcp:     http://%s/mcp/{bank}/ (%d tools)", *addr, len(mcpServer.ToolNames()))

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}
}
