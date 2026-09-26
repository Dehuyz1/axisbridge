-- +goose Up

-- ALTER TYPE ... ADD VALUE cannot run inside a transaction block on older
-- PostgreSQL, so this migration opts out of goose's implicit transaction.
-- Every statement below is individually idempotent, which keeps a partial
-- failure safe to re-run.
-- +goose NO TRANSACTION

-- +goose StatementBegin
ALTER TYPE job_kind ADD VALUE IF NOT EXISTS 'axis_renew';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TYPE job_kind ADD VALUE IF NOT EXISTS 'axis_settle';
-- +goose StatementEnd

-- +goose StatementBegin
DO $$ BEGIN
    CREATE TYPE ovo_state AS ENUM (
        'new',           -- account row created, PIN stored, never logged in
        'otp_sent',      -- OTP challenge in flight, waiting for the code
        'active',        -- has a live access token; usable as a payer
        'login_needed',  -- token dead or login failed; needs operator action
        'disabled'       -- parked by the operator
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$ BEGIN
    CREATE TYPE tx_state AS ENUM (
        'pending',   -- row reserved, AXIS not charged yet
        'charged',   -- AXIS charge succeeded, QRIS payload captured
        'paid',      -- OVO checkout paid
        'settled',   -- verified against AXIS
        'failed'
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
-- +goose StatementEnd

-- ovo_accounts: one row per OVO wallet used as a payer.
--
-- PIN is never stored in plaintext: pin_enc holds AES-256-GCM ciphertext
-- keyed by OVO_MASTER_KEY. token_enc caches the access token the same way —
-- OVO tokens live 7 days, so caching skips a login round-trip on restart.
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS ovo_accounts (
    id             BIGSERIAL PRIMARY KEY,
    msisdn         TEXT UNIQUE NOT NULL,            -- 62xxx, any operator
    name           TEXT,
    device_id      TEXT NOT NULL,                   -- stable UUID per account
    pin_enc        TEXT NOT NULL,                   -- AES-256-GCM, never plaintext
    token_enc      TEXT,                            -- AES-256-GCM access token cache
    token_saved_at TIMESTAMPTZ,
    balance        BIGINT NOT NULL DEFAULT 0,
    balance_at     TIMESTAMPTZ,
    state          ovo_state NOT NULL DEFAULT 'new',
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    last_error     TEXT,
    otp_ref_id     TEXT,                            -- in-flight challenge ref
    otp_auth_type  TEXT,                            -- LOGIN | REGISTER
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_ovo_payable
    ON ovo_accounts(state, enabled, balance)
 WHERE enabled = TRUE AND state = 'active';
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS trg_ovo_accounts_updated ON ovo_accounts;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_ovo_accounts_updated
    BEFORE UPDATE ON ovo_accounts
    FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
-- +goose StatementEnd

-- transactions: one row per AXIS charge, whatever paid for it.
--
-- This is the shared contract between the charge side (renew/gift) and the
-- payment side. `kind` records intent so a settled row can be routed back to
-- either accounts.last_renew_at or a gifts row, but the payment logic itself
-- never branches on it.
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS transactions (
    id              BIGSERIAL PRIMARY KEY,
    kind            TEXT NOT NULL,                  -- 'renew' | 'gift'
    account_id      BIGINT REFERENCES accounts(id) ON DELETE SET NULL,
    msisdn          TEXT NOT NULL,                  -- AXIS number being topped up
    axis_trx_id     TEXT,                           -- PurchaseResult.ID
    paket           TEXT,
    amount          BIGINT,
    qris_emv        TEXT,                           -- EMV payload from the charge
    ovo_account_id  BIGINT REFERENCES ovo_accounts(id) ON DELETE SET NULL,
    ovo_checkout_id TEXT,
    ovo_order_id    TEXT,
    ovo_payment_id  TEXT,
    state           tx_state NOT NULL DEFAULT 'pending',
    error           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_tx_state   ON transactions(state);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_tx_msisdn  ON transactions(msisdn);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_tx_created ON transactions(created_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_tx_axis_trx ON transactions(axis_trx_id)
    WHERE axis_trx_id IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS trg_transactions_updated ON transactions;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_transactions_updated
    BEFORE UPDATE ON transactions
    FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
-- +goose StatementEnd

-- Renew cooldown needs to know when a number was last charged.
-- +goose StatementBegin
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS last_renew_at TIMESTAMPTZ;
-- +goose StatementEnd

-- Dedup guard: at most one active job per (account, kind). This is what makes
-- the renew scanner idempotent — it can tick repeatedly, or restart, and
-- INSERT ... ON CONFLICT DO NOTHING will never produce a duplicate job.
-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_active_per_account
    ON jobs(account_id, kind)
 WHERE state IN ('pending','running') AND account_id IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO settings(key, value) VALUES
    ('renew.cooldown',    '"20h"'),
    ('renew.scan_every',  '"5m"'),
    ('ovo.min_balance',   '10000'),
    ('worker.pay_enabled','true')
ON CONFLICT (key) DO NOTHING;
-- +goose StatementEnd

-- renew.minus_days is negative by design: -30 means "renew numbers whose
-- masa aktif lapsed 30 days ago or more", matching RENEW_STALE_DAYS in the
-- monolith. Migration 0002 seeded 3 under the old "days before expiry"
-- reading, so correct that value in place without clobbering a deliberate
-- operator setting.
-- +goose StatementBegin
UPDATE settings SET value = '-30'
 WHERE key = 'renew.minus_days' AND value::text = '3';
-- +goose StatementEnd

-- +goose Down
-- +goose NO TRANSACTION

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_jobs_active_per_account;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE accounts DROP COLUMN IF EXISTS last_renew_at;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS transactions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS ovo_accounts;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TYPE IF EXISTS tx_state;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TYPE IF EXISTS ovo_state;
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM settings
 WHERE key IN ('renew.cooldown','renew.scan_every','ovo.min_balance','worker.pay_enabled');
-- +goose StatementEnd

-- Note: 'axis_renew' and 'axis_settle' stay in the job_kind enum. PostgreSQL
-- has no ALTER TYPE ... DROP VALUE, and rebuilding the enum would require
-- rewriting every jobs row. Leaving unused labels behind is harmless.
