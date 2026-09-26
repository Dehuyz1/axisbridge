# CLAUDE.md

Mirror of AGENTS.md for Claude Code. See AGENTS.md for the source of
truth.

## Fast facts

- Repo: split rewrite of `axisbridgev2`. Go + Postgres.
- 4 binaries: `cmd/axis-core`, `cmd/register-worker`, `cmd/gift-worker`,
  `cmd/axis-ui`.
- Shared code under `internal/`.
- UI is 3 pages: `/management`, `/gifts`, `/setting`. No built-in auth —
  protect with a reverse proxy if exposed publicly.
- Job queue is Postgres — LISTEN/NOTIFY + `FOR UPDATE SKIP LOCKED`.
- Runtime config in `settings` table, editable from `/setting`.

## Do not

- Edit `0001_init.sql` after it has been applied. Add a new migration.
- Store OVO PIN plaintext.
- Import `internal/*` from outside the module.
- Add SQLite or any second DB.

## When adding a job kind

1. Extend `job_kind` enum in a new migration.
2. Add the kind to the worker's `Kinds` slice.
3. Add a handler branch in that worker's `main.go`.
4. Add an enqueue endpoint in `internal/api/server.go` if the UI triggers it.
