# Retain Truncation Fix

Date: 2026-11-07

Base: `9b159017d9c63f1f205ae34c622876b23357b090`, upstream contract 1.2.0.

The extraction output budget now defaults to 8192 tokens and can expand to
32768 tokens, with at most three calls. Both budgets are configurable.
Length-limited output is never accepted as a complete extraction. Parsing
requires a complete JSON array; logs contain only bounded metadata and usage.
Reflect, consolidation, database schema and storage algorithms are unchanged.

Validation completed locally:

- `go test ./... -count=1`
- `go test -race ./... -count=1`
- `go vet ./...`
- 101 operation coverage assertion
- Invalid environment configuration rejected at startup
- Linux amd64 static build

Linux binary SHA-256:
`95b265630a89a53fbaa5d5aafe8500a0b9e9767eb1e477529cb10718afa1a3a3`.

NAS deployment uses image `hindsight-go:retain-fix-20261107`. The deployment
helper in `scripts/nas_retain_fix.py` preserves Docker configuration and keeps
secrets on the NAS. It verifies real model retries in an isolated in-memory
candidate before replacing the production container.

Deployment verified:

- Candidate: budget 2048 finished with `length`; retry at 4096 finished with
  `stop`, returning 24 facts. MCP recall succeeded.
- Production: real-model MCP retain/recall returned 26 canary facts; the canary
  was removed and pre-existing memory/document/bank row signatures matched.
- Previously failed muse2api update memory was retained (20 facts) and recalled.
  The Hindsight fix record was retained (30 facts) and recalled in bank `codex`.
- Backup: `/vol1/1000/docker/hindsight-go/backup-retain-fix-20261107`.
- Rollback container: `hindsight-go-rollback-20261107`.
- Existing deployment runtime binary and `hindsight-go:local` alias point to
  the fixed build, so the current runtime Compose workflow keeps the fix.
- Source changes are not committed or pushed.

Canonical patched source:
`/vol1/1000/docker/hindsight-go/source-retain-fix-20261107`.

The pre-existing NAS root source is an older snapshot; it is retained for
history, not used to build this patch. The existing production Compose stack
is not replaced by the repository's example stack.
