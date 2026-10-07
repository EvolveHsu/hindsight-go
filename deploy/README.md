# Hindsight Go Cutover

This directory contains the one-time migration from the all-in-one Python
Hindsight container to:

- `hindsight-go` on `${HINDSIGHT_GO_PORT}`
- the official Node/Next.js control plane on `${HINDSIGHT_CP_PORT}`
- standalone PostgreSQL 18 with pgvector on the internal Docker network
- `hindsight-embedding`, a CPU TEI sidecar serving `BAAI/bge-small-en-v1.5`
  so the Go recall path can keep using the existing 384-dimension vectors

The old `${HINDSIGHT_OLD_COMPOSE}` deployment directory, the old container, and the
old image are intentionally preserved for rollback. No recurring backup job is
created.

## Prerequisites

1. The Go image must pass `go build ./...`, `go vet ./...`, and `go test ./...`.
2. `/mcp/{bank_id}/` must answer `initialize` and `tools/list`.
   The host endpoint is `http://<nas-ip>:${HINDSIGHT_GO_PORT}/mcp/${HINDSIGHT_MCP_BANK}/`.
3. Create the runtime environment file:

   ```bash
   cp deploy/.env.example deploy/.env
   ```

   Set `HINDSIGHT_DB_PASSWORD`, the LLM URL/key/model. Do not commit this file.

## Cutover

Run from the checkout on NAS:

```bash
bash deploy/migrate-from-python.sh
```

The script:

1. Takes one `pg_dump -Fc` snapshot into `deploy/backups/`.
2. Stops the old container with a 45-second graceful timeout.
3. Creates a standalone `pgvector/pgvector:pg18` database.
4. Restores the snapshot and checks `banks|documents|memory_units` row counts.
5. Starts Go and the control plane, then checks both configured ports and the
   configured MCP endpoint.
6. Attempts rollback automatically if a step fails after the old container was
   stopped.

## Verify

```bash
bash deploy/verify-cutover.sh
```

Expected database counts for the current production snapshot are:

```text
banks|documents|memory_units = 2|50|691
```

## Rollback

The old data directory is never modified by this migration. Roll back with:

```bash
docker compose -f deploy/docker-compose.yml \
  --env-file deploy/.env down

docker compose -f "$HINDSIGHT_OLD_COMPOSE" up -d
```

The old all-in-one container can then own the same published ports again.

## Current Status

All 99 OpenAPI operations have Go handlers. The semantic recall arm uses the
TEI sidecar plus pgvector, and retain runs the LLM extractor, writes
embeddings/entities/chunks, and creates temporal/semantic links.

The remaining bounded differences are OCR/Office/audio file parsing, attachment
bytes inside transfer archives, a dedicated background cron/webhook-delivery
worker, and optional reranking.

The runtime image is built from a prebuilt Linux amd64 binary:

```powershell
New-Item -ItemType Directory -Force -Path deploy\runtime | Out-Null
$env:GOOS='linux'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'
go build -ldflags='-s -w' -o deploy\runtime\hindsight-go ./cmd/hindsight-go
```

Then sync the repository to NAS and run the runtime compose build/up.
