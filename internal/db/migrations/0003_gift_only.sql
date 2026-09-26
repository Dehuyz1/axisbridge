-- +goose Up
-- +goose StatementBegin

-- gift_only: nomor yang diupload dari Gift page. Tidak muncul di Management
-- (Management hanya menampilkan register-flow nomor). Gift-worker tetap
-- memprosesnya seperti biasa dari `jobs`.
ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS gift_only BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS idx_accounts_gift_only ON accounts(gift_only);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_accounts_gift_only;
ALTER TABLE accounts DROP COLUMN IF EXISTS gift_only;
-- +goose StatementEnd
