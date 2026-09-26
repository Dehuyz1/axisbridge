# AGENTS.md — axisbridge (split)

## Overview

- **Purpose**: split rewrite of `axisbridgev2`. AXIS renewal + gift bridge,
  now decoupled into isolated services so one bug does not stop the world.
- **Stack**: Go 1.22, PostgreSQL 16, embedded goose migrations, plain HTML +
  Alpine.js + Tailwind CDN for the UI.
- **Runtime**: 4 binaries. `axis-core` owns Postgres + REST + the renew
  scanner; `register-worker` consumes `axis_otp_request` jobs; `gift-worker`
  consumes `axis_gift` jobs; `axis-ui` serves the 4 dashboards and
  reverse-proxies `/api/*` to core.
- **Package manager**: `go mod` (`go.mod` at repo root).

## Commands

```bash
# dev, without docker (needs local postgres)
go run ./cmd/axis-core
go run ./cmd/register-worker
go run ./cmd/gift-worker
go run ./cmd/axis-ui

# build all
go build -o bin/ ./cmd/...

# docker
docker compose up -d --build
docker compose logs -f axis-core

# migration is embedded and auto-runs on axis-core boot.
```

## Conventions

- One binary per `cmd/<name>/`, package `main`, `main.go` only.
- Shared code lives under `internal/`. Nothing else imports `internal/*`.
- Every binary reads `DATABASE_URL` from env. Migration runs from
  `axis-core` only.
- Runtime config = `settings` table. `.env` holds bootstrap secrets only.
- Job handlers return `jobq.Result{Done|Retry|Err|Data}`. Panics are
  recovered and turned into failures.
- All timestamps are `TIMESTAMPTZ`; container timezone is Asia/Jakarta.
- Never touch `axisbridgev2/` in this repo tree — that lives under
  `D:/PROJECT/axisbridgev2` and is the reference source to port from.

## Boundaries

**NEVER touch**:

- `.env` (real secrets)
- `OVO_MASTER_KEY` values in commits or logs — losing it orphans every
  stored wallet
- OVO PIN and access token plaintext — always `ovo.PinCipher` ciphertext in
  `pin_enc` / `token_enc`, and never selected into an API response
- `pgdata/` volume
- Production Postgres data unless explicitly asked

## Dependencies

- `github.com/jackc/pgx/v5` — Postgres driver + pool + LISTEN/NOTIFY
- `github.com/pressly/goose/v3` — embedded SQL migrations
- stdlib `crypto/aes` + `crypto/cipher` — AES-256-GCM vault for OVO secrets

## Configuration

Bootstrap env (see `.env.example`):

```
DATABASE_URL, CORE_ADDR, UI_ADDR, CORE_URL, OVO_MASTER_KEY,
REGISTER_CONCURRENCY, GIFT_CONCURRENCY, LOG_LEVEL
```

Runtime settings live in the `settings` table (key TEXT PK, value JSONB).
Defaults inserted by `0001_init.sql`.

## Error handling

- `db.Migrate` → fatal on start.
- `jobq` handler error with `Done=false` → retry with backoff until
  `attempts >= max_attempts`, then `failed`.
- REST API returns `500` for pg errors; `404` on `pgx.ErrNoRows`.

## Troubleshooting

- **`no rows` on ClaimJobSQL**: normal — the worker just found nothing to
  do. Real errors bubble up as `claim failed` in logs.
- **`LISTEN` never wakes**: check `postgres` logs for `NOTIFY` reaching the
  channel. Fallback poll still runs every 5s so nothing stalls.
- **UI shows `502 core upstream unreachable`**: `axis-core` is down or
  `CORE_URL` is wrong. Logs: `docker compose logs axis-core` (docker compose
  deploy) or `journalctl -u axis-core -n 50 --no-pager` (systemd/Kainode).
- **`ERROR: cannot change name of input parameter` on migrate**: schema
  changed — bump migration file, do not edit `0001_init.sql` in place.
- **`/ovo` returns `412 OVO_MASTER_KEY not configured`**: the key is missing
  from the environment `axis-core` actually reads. On Kainode that is
  `/opt/axisbridge/.env`; generate with `openssl rand -hex 32`, then
  `systemctl restart axis-core`.
- **OVO `[OV00006]` on verify**: wrong or expired OTP code. The wallet stays
  in `otp_sent`, so request a new code and retry.
- **OVO `[OV00612]` on login**: OVO reported an existing account that turns
  out not to exist. `ovoVerifyOTP` already falls back to `Register`, so this
  only surfaces if registration itself also fails.
- **Wallet stuck in `login_needed`**: the cached token was rejected. Send a
  fresh OTP; the PIN on file is reused, no re-entry needed.
- **Renew scanner enqueues nothing**: check `renew.enabled` is `true` and
  `renew.minus_days` is negative. A number is also skipped when it is
  `dead`, `gift_only`, `paused`, or was charged within `renew.cooldown`.
