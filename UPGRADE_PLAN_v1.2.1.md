# Hindsight Go v1.2.1 Sync

Date: 2026-11-10

Upstream:

- Repository: `vectorize-io/hindsight`
- Release: `v1.2.1`
- Commit: `0eb02d8c418eac6b489bc63883c993b2257dfac4`

Previous Go contract: `1.2.0`, 101 operations.

Target Go contract: `1.2.1`, 109 operations.

Go service version: `0.1.4`.

## API

- Added the eight canonical `/pages/...` operations.
- Kept `/knowledge-base/...` working as deprecated aliases.
- Added `add_tags` / `remove_tags` document PATCH modes.
- Added `is_page`, `paused_mental_models`, and the candidate-rejected refresh
  states.
- Added a separate `page_default_trigger` config key while retaining the
  deprecated `knowledge_page_default_trigger` alias.
- Bounded page `max_tokens` to 256..8192.

## Implementation

- Generated `internal/api` from the normalized 1.2.1 contract.
- Reused the existing PostgreSQL/in-memory document and page storage.
- Added atomic incremental document tag updates in both storage backends.
- Added v1.2.1 MCP names while retaining all previous tool aliases.
- Applied the shared bank/alias/mental-model ID rules.

## Known Lite Boundaries

The bounded differences documented in the 1.2.0 audit remain unless a feature
is explicitly listed above. Hindsight Go remains a Lite data plane: it does not
provide the official Python worker/decision-model/extension stack, complete
OCR/Office/audio parsing, or the full graph-maintenance implementation.

## Verification

- OpenAPI method/path set matches upstream v1.2.1: 109/109.
- Coverage assertion: 109/109 Handler methods.
- `go test ./... -count=1`
- `go test -race ./... -count=1`
- `go vet ./...`
- Linux amd64 static build.
- NAS candidate health/version/MCP verification.
- NAS production REST Pages, MCP, control-plane, and data-preservation checks.

NAS backup before deployment:

`/vol1/1000/docker/hindsight-go-backup-v1.2.1-20261110`
