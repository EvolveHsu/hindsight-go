# Hindsight Go

[English](README.md) | [简体中文](README.zh-CN.md)

A Go implementation of the [Hindsight](https://github.com/vectorize-io/hindsight) agent-memory API, generated from the upstream OpenAPI 3.1 contract. It provides an independent Go service for the REST API and MCP protocol, and is intended to run alongside the official Hindsight Control Plane UI.

## Features

- **99 / 99 OpenAPI operations**: every operation in the normalized upstream contract has a generated handler and a Go implementation.
- **REST + MCP on one port**: `/v1/...` REST routes and `/mcp/{bank}/` Streamable HTTP MCP share the same implementation, so tool calls and API calls behave identically.
- **Agent memory workflow**: retain, recall, reflect, consolidation, entities, temporal and semantic links, directives, bank aliases, operations, knowledge base, webhooks, transfer/import/export, and file retention.
- **PostgreSQL + pgvector storage**: an upstream-schema-compatible PostgreSQL backend is included for durable deployments and migration from existing Hindsight data.
- **Real semantic recall**: embeddings are generated through any OpenAI-compatible embeddings endpoint (TEI works well), stored in pgvector, and fused with keyword results using Reciprocal Rank Fusion.
- **Pluggable storage**: use PostgreSQL for production, or the in-process backend for tests and local evaluation.
- **Monitoring endpoints**: `/health`, `/health/live`, `/health/ready`, `/version`, and Prometheus-style `/metrics`.

## Architecture

Hindsight Go focuses on the API and MCP data plane. The official [Hindsight Control Plane](https://github.com/vectorize-io/hindsight/tree/main/hindsight-control-plane) can run as a separate frontend service and point to this Go backend.

```text
Client / Agent / MCP
        |
        +--> Hindsight Go :8890
              |-> REST /v1/...
              |-> MCP  /mcp/{bank}/
              |-> PostgreSQL + pgvector
              |-> OpenAI-compatible LLM
              |-> OpenAI-compatible embeddings
```

The deploy compose file also runs the official control plane as `hindsight-control-plane`.

## Quick Start

### 1. Run a minimal Go API

```bash
go run ./cmd/hindsight-go
```

This starts an in-memory backend on `http://0.0.0.0:8890`:

```bash
curl http://127.0.0.1:8890/health
```

### 2. Run with PostgreSQL and embeddings

Create `deploy/.env`:

```bash
HINDSIGHT_DB_PASSWORD=change-me
HINDSIGHT_GO_PORT=8890
HINDSIGHT_CP_PORT=9999
HINDSIGHT_MCP_BANK=default
HINDSIGHT_LLM_NETWORK=hindsight-go-llm

HINDSIGHT_GO_LLM_BASE_URL=https://api.example.com/v1
HINDSIGHT_GO_LLM_API_KEY=replace-with-api-key
HINDSIGHT_GO_LLM_MODEL=your-model

HINDSIGHT_GO_EMBEDDINGS_BASE_URL=http://hindsight-embedding:80/v1
HINDSIGHT_GO_EMBEDDINGS_MODEL=BAAI/bge-small-en-v1.5
HINDSIGHT_GO_EMBEDDINGS_DIMENSIONS=384
HINDSIGHT_GO_EMBEDDINGS_BATCH_SIZE=64
```

Then build and start the full stack:

```bash
docker compose -f deploy/docker-compose.yml --env-file deploy/.env up -d --build
```

Services:

- Hindsight Go API / MCP: `http://127.0.0.1:8890`
- Official Control Plane UI: `http://127.0.0.1:9999`
- PostgreSQL and TEI embedding sidecar remain internal to the compose network.

For production, keep PostgreSQL and embedding services inside a private network and expose only the Go API and Control Plane ports you actually need. The Go container also joins `HINDSIGHT_LLM_NETWORK`; create that network or change the variable if your LLM endpoint is on another Docker network.

## API And MCP

REST paths follow the upstream contract. Common operations include:

```text
PUT    /v1/default/banks/{bank_id}
POST   /v1/default/banks/{bank_id}/memories
POST   /v1/default/banks/{bank_id}/memories/recall
GET    /v1/default/banks/{bank_id}/memories
GET    /v1/default/banks/{bank_id}/documents
POST   /v1/default/banks/{bank_id}/reflect
POST   /v1/default/banks/{bank_id}/consolidate
GET    /v1/default/banks
GET    /v1/default/banks/{bank_id}/tags
```

MCP is served at:

```text
http://127.0.0.1:8890/mcp/{bank}/
```

The MCP catalog currently contains 36 tools, including `retain`, `sync_retain`, `recall`, and `reflect`. MCP calls are bridged in process to the same REST handlers, so a feature is either available consistently on both surfaces or reports an explicit not-supported result.

## Data And Migration

The PostgreSQL backend can operate on the existing Hindsight upstream schema:

```bash
HINDSIGHT_GO_UPSTREAM_SCHEMA=1
```

This lets an existing Python Hindsight database be handed over to the Go backend without changing the data model. `deploy/migrate-from-python.sh` contains a cutover workflow that snapshots the old database, restores it into a standalone PostgreSQL instance, and starts the Go API and Control Plane.

Never delete the old data directory or database backup before you have verified the Go deployment.

## Development

Requirements:

- Go 1.25 or newer
- Python 3.12 for the coverage assertion script
- [ogen](https://github.com/ogen-go/ogen) v1.24.0 only if regenerating API code

Run checks:

```bash
go build ./...
go vet ./...
go test ./... -count=1
python scripts/coverage_assert.py
```

On Windows PowerShell:

```powershell
pwsh scripts/check.ps1
```

The generated API package in `internal/api` is committed intentionally so a normal build does not require regeneration. If you intentionally change `openapi.go.json`, regenerate it and run the full check.

## Configuration

### Required for production

| Variable | Purpose |
|---|---|
| `HINDSIGHT_GO_DATABASE_URL` | PostgreSQL DSN. Unset means in-memory storage. |
| `HINDSIGHT_DB_PASSWORD` | Compose-only password for PostgreSQL. |
| `HINDSIGHT_GO_LLM_BASE_URL` | OpenAI-compatible LLM endpoint. |
| `HINDSIGHT_GO_LLM_API_KEY` | LLM API key. |
| `HINDSIGHT_GO_LLM_MODEL` | LLM model name. |

### Embeddings

| Variable | Purpose |
|---|---|
| `HINDSIGHT_GO_EMBEDDINGS_BASE_URL` | OpenAI-compatible embeddings endpoint. |
| `HINDSIGHT_GO_EMBEDDINGS_MODEL` | Embedding model name. |
| `HINDSIGHT_GO_EMBEDDINGS_DIMENSIONS` | Vector dimensions. |
| `HINDSIGHT_GO_EMBEDDINGS_BATCH_SIZE` | Batch size for embedding requests. |
| `HINDSIGHT_GO_EMBEDDINGS_API_KEY` | Optional API key. |
| `HINDSIGHT_GO_EMBEDDINGS_QUERY_PREFIX` | Optional query prefix. |
| `HINDSIGHT_GO_EMBEDDINGS_DOCUMENT_PREFIX` | Optional document prefix. |

Without an embeddings endpoint, recall falls back to token overlap instead of pretending that the semantic vector arm ran.

### Ports and paths

| Variable | Default | Purpose |
|---|---|---|
| `HINDSIGHT_GO_PORT` | `8890` | Published Go API / MCP port. |
| `HINDSIGHT_CP_PORT` | `9999` | Published Control Plane port. |
| `HINDSIGHT_MCP_BANK` | `default` | Bank name used by cutover verification. |
| `HINDSIGHT_LLM_NETWORK` | `hindsight-go-llm` | Docker network used to reach an external LLM service. |
| `HINDSIGHT_GO_MCP_BANK_ID` | unset | Default bank for the bare `/mcp` endpoint. |
| `HINDSIGHT_GO_MCP_INSTRUCTIONS` | unset | MCP instructions returned during initialize. |

### Runtime

| Variable | Purpose |
|---|---|
| `HINDSIGHT_GO_UPSTREAM_SCHEMA` | Set to `1` to use the upstream-compatible PostgreSQL tables. |
| `HINDSIGHT_GO_FILE_ROOT` | Root for transfer archives and stored files. |

## Project Layout

```text
cmd/hindsight-go/       main binary
internal/api/           generated OpenAPI handlers and types
internal/server/        Engine, HTTP routes, Store seam
internal/memory/        in-process storage backend
internal/storepg/       PostgreSQL + pgvector backend
internal/model/         shared rows, scoring, and fusion vocabulary
internal/mcp/           Streamable HTTP MCP and REST bridge
internal/embeddings/    OpenAI-compatible embeddings client
internal/extract/       retain-time LLM fact extraction
internal/reflect/       reflect agent
internal/consolidate/   consolidation agent
deploy/                 production compose, migration, and verification
scripts/                checks, smoke tests, and coverage assertion
```

## Upstream

This project reimplements the service side of the Hindsight agent-memory API in Go and depends on the upstream API contract.

- Upstream repository: [vectorize-io/hindsight](https://github.com/vectorize-io/hindsight)
- Upstream Control Plane: [vectorize-io/hindsight-control-plane](https://github.com/vectorize-io/hindsight/tree/main/hindsight-control-plane)
- Upstream license: MIT

Hindsight Go is an independent Go implementation, not an official Vectorize project.

## Limitations

Known bounded differences from the current Python implementation:

- Office document, OCR, and audio parsing in file retention is not complete.
- Attachment bytes are not fully represented inside transfer archives.
- Background consolidation, webhook delivery, and mental-model cron refresh still need a dedicated long-running worker.
- Optional recall reranking and some graph/temporal recall strategies remain simplified.

Core retain, recall, reflect, PostgreSQL persistence, MCP, and the public REST surface are implemented and tested.

## License

MIT License. See [LICENSE](LICENSE).

This repository includes code derived from the MIT-licensed Hindsight project by Vectorize AI, Inc. The upstream license notice is preserved in `LICENSE`.
