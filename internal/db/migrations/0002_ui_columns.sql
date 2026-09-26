-- +goose Up
-- +goose StatementBegin

-- accounts: kolom untuk UI Management (masa aktif, paket, dan tampilan token).
-- `masa_aktif_until` = tanggal SIM hangus (dari refresh AXIS). Kalau NULL,
-- worker akan menandai `dead=true` setelah refresh gagal berulang.
ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS masa_aktif_until DATE,
    ADD COLUMN IF NOT EXISTS paket            TEXT,
    ADD COLUMN IF NOT EXISTS pulsa            BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dead             BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS idx_accounts_dead ON accounts(dead);
CREATE INDEX IF NOT EXISTS idx_accounts_masa ON accounts(masa_aktif_until);

-- gifts: tambah kolom untuk rincian OVO (nomor OVO yang membayar + trx id +
-- nominal). Tetap boleh NULL supaya gift lama (stub) tidak error.
ALTER TABLE gifts
    ADD COLUMN IF NOT EXISTS paket       TEXT,
    ADD COLUMN IF NOT EXISTS trx_id      TEXT,
    ADD COLUMN IF NOT EXISTS amount      BIGINT,
    ADD COLUMN IF NOT EXISTS ovo_msisdn  TEXT,
    ADD COLUMN IF NOT EXISTS ovo_trx_id  TEXT,
    ADD COLUMN IF NOT EXISTS ovo_amount  BIGINT;

-- Settings tambahan: minus berapa hari sebelum masa aktif habis stack renew.
-- Contoh: renew.minus_days=3 → nomor dengan masa_aktif_until <= today+3 masuk
-- antrian renew.
INSERT INTO settings(key, value) VALUES
    ('renew.minus_days', '3'),
    ('renew.enabled',    'false')
ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE gifts
    DROP COLUMN IF EXISTS ovo_amount,
    DROP COLUMN IF EXISTS ovo_trx_id,
    DROP COLUMN IF EXISTS ovo_msisdn,
    DROP COLUMN IF EXISTS amount,
    DROP COLUMN IF EXISTS trx_id,
    DROP COLUMN IF EXISTS paket;

ALTER TABLE accounts
    DROP COLUMN IF EXISTS dead,
    DROP COLUMN IF EXISTS pulsa,
    DROP COLUMN IF EXISTS paket,
    DROP COLUMN IF EXISTS masa_aktif_until;

DELETE FROM settings WHERE key IN ('renew.minus_days','renew.enabled');
-- +goose StatementEnd
