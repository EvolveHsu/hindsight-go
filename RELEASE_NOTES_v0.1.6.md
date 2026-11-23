# Hindsight Go 0.1.6

Date: 2026-11-23

Upstream: `vectorize-io/hindsight` v1.2.2

Commit: `b4b4652949d5a8c3ac3fe9fa572c99c74ecb2b9c`

## Changes

- Upgrade the normalized OpenAPI contract from 1.2.1 to 1.2.2.
- Add `GET /v1/default/banks/{bank_id}/observations/tree` with deterministic
  tag-grouped markdown topics.
- Add `knowledge_banks`, `reflect_kb_search_apply_tags`, and
  `reflect_kb_search_media_types` bank configuration fields.
- Add `FeaturesInfo.knowledge_banks` and report it as disabled in the Go Lite
  data plane.
- Add reflect/mental-model trace `started_at_ms` fields.
- Add the `mental_model.refreshed` webhook event documentation.

## Validation

- 110/110 OpenAPI operation coverage.
- Full unit tests, race tests, `go vet`, and Linux amd64 build.
- NAS Observation Tree REST smoke test and MCP exact-retain check.

The NAS LLM endpoint returned HTTP 401 during the normal retain smoke test
with the unchanged model/base-URL/API-key configuration. Exact-retain and
Observation Tree remain functional; the backup must be kept until the
provider credential is renewed and normal retain is re-verified.

The Lite boundaries remain unchanged. Knowledge Banks storage, media ingest,
and the official Python worker stack are not implemented.
