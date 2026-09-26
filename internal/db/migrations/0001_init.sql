-- +goose Up
-- +goose StatementBegin

-- accounts: master list nomor. Status memakai enum ketat supaya UI + worker
-- tidak salah paham antara "belum register" vs "otp gagal" vs "sudah login".
CREATE TYPE account_status AS ENUM (
    'new',           -- baru masuk, belum minta OTP
    'otp_pending',   -- OTP sudah dikirim, menunggu SMS balasan
    'otp_failed',    -- semua percobaan OTP habis
    'available',     -- login sukses, punya token aktif
    'gift_wait',     -- OTP gagal / login mati, antre di gift worker
    'gift_done',     -- gift sukses (arsip; row juga muncul di tabel gifts)
    'dead',          -- SIM hangus / dinyatakan mati
    'paused'         -- manual pause dari dashboard
);

CREATE TABLE accounts (
    id              BIGSERIAL PRIMARY KEY,
    msisdn          TEXT UNIQUE NOT NULL,          -- format 62xxx
    status          account_status NOT NULL DEFAULT 'new',
    otp_attempts    INT  NOT NULL DEFAULT 0,       -- percobaan sejak reset terakhir
    otp_last_at     TIMESTAMPTZ,                   -- kapan OTP terakhir diminta
    otp_enc_msisdn  TEXT,                          -- payload enc dari AXIS RequestOTP
    axis_token      TEXT,
    axis_refresh    TEXT,
    last_login_at   TIMESTAMPTZ,
    note            TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_accounts_status ON accounts(status);

-- gifts: satu baris per gift sukses. Sengaja sederhana: nomor + bukti sukses.
-- account_id boleh NULL kalau nomor sudah dihapus dari master.
CREATE TABLE gifts (
    id          BIGSERIAL PRIMARY KEY,
    account_id  BIGINT REFERENCES accounts(id) ON DELETE SET NULL,
    msisdn      TEXT NOT NULL,
    proof       JSONB NOT NULL,                    -- payload sukses (trx_id, msg, dll)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_gifts_created ON gifts(created_at DESC);
CREATE INDEX idx_gifts_msisdn  ON gifts(msisdn);

-- jobs: single queue table dishared antar worker. Setiap worker LISTEN kind
-- yang dia handle saja.
CREATE TYPE job_kind AS ENUM (
    'axis_otp_request',   -- worker register: minta OTP satu kali
    'axis_gift'           -- worker gift: proses nomor di gift_wait
);
CREATE TYPE job_state AS ENUM ('pending','running','done','failed','cancelled');

CREATE TABLE jobs (
    id           BIGSERIAL PRIMARY KEY,
    kind         job_kind  NOT NULL,
    state        job_state NOT NULL DEFAULT 'pending',
    account_id   BIGINT REFERENCES accounts(id) ON DELETE CASCADE,
    payload      JSONB NOT NULL DEFAULT '{}'::jsonb,
    result       JSONB,
    error        TEXT,
    attempts     INT  NOT NULL DEFAULT 0,
    max_attempts INT  NOT NULL DEFAULT 1,           -- retry di-drive oleh worker, bukan queue
    scheduled_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_jobs_claim      ON jobs(kind, scheduled_at) WHERE state = 'pending';
CREATE INDEX idx_jobs_account    ON jobs(account_id);
CREATE INDEX idx_jobs_kind_state ON jobs(kind, state);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by TEXT
);

CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,
    ts         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    kind       TEXT NOT NULL,
    account_id BIGINT,
    job_id     BIGINT,
    data       JSONB
);
CREATE INDEX idx_events_ts      ON events(ts DESC);
CREATE INDEX idx_events_account ON events(account_id) WHERE account_id IS NOT NULL;

-- Default settings. Angka OTP diselaraskan dengan axisbridgev2.
INSERT INTO settings(key, value) VALUES
    ('otp.max_attempt',      '3'),         -- total percobaan OTP per nomor
    ('otp.retry_gap',        '"90s"'),     -- jeda antar percobaan OTP
    ('otp.debounce',         '"10m"'),     -- debounce push nomor duplikat
    ('gift.quota_daily',     '5'),
    ('worker.register_enabled', 'true'),
    ('worker.gift_enabled',     'true'),
    ('log.level',            '"info"');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION touch_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_accounts_updated
    BEFORE UPDATE ON accounts
    FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_settings_updated
    BEFORE UPDATE ON settings
    FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notify_new_job() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('jobs_' || NEW.kind::text, NEW.id::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_jobs_notify
    AFTER INSERT ON jobs
    FOR EACH ROW EXECUTE FUNCTION notify_new_job();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS gifts;
DROP TABLE IF EXISTS accounts;
DROP TYPE  IF EXISTS job_state;
DROP TYPE  IF EXISTS job_kind;
DROP TYPE  IF EXISTS account_status;
DROP FUNCTION IF EXISTS notify_new_job();
DROP FUNCTION IF EXISTS touch_updated_at();
-- +goose StatementEnd
