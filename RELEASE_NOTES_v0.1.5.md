# Hindsight Go 0.1.5

Date: 2026-11-17

## Fixes

- Retain extraction now sends the JSON output instruction in the user
  message and requests OpenAI `response_format={"type":"json_object"}`.
- The parser accepts a JSON array, an object with a `facts` array, fenced
  JSON, and a single JSON value surrounded by short prose.
- `exact`, `verbatim`, and `chunks` retain strategies bypass the LLM
  extractor and store the original content directly.
- Added `HINDSIGHT_GO_LLM_CONTEXT_TOKENS`; NAS is configured to `1000000`.
  Oversized retain input is split by the context budget.

## Validation

- Full unit tests, race tests, `go vet`, 109/109 operation coverage.
- Synthetic real-model probe returns a parseable JSON object.
- NAS production retain/recall and exact-strategy smoke tests.

The v1.2.1 API contract and existing storage schema are unchanged.
