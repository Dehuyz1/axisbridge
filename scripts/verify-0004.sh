#!/usr/bin/env bash
# Verify migration 0004 produced the schema the renew/pay pipeline relies on.
# Runs against a scratch database, never production.
set -eu
DB=${1:-mig_test}
q() { docker exec axisbridge-pg psql -U axis -d "$DB" -tAc "$1"; }

echo "== job_kind enum labels =="
q "SELECT string_agg(enumlabel, ' ' ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid = 'job_kind'::regtype"

echo "== ovo_state labels =="
q "SELECT string_agg(enumlabel, ' ' ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid = 'ovo_state'::regtype"

echo "== tx_state labels =="
q "SELECT string_agg(enumlabel, ' ' ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid = 'tx_state'::regtype"

echo "== transactions columns =="
q "SELECT string_agg(column_name, ' ' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_name = 'transactions'"

echo "== ovo_accounts columns =="
q "SELECT string_agg(column_name, ' ' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_name = 'ovo_accounts'"

echo "== accounts.last_renew_at present (expect 1) =="
q "SELECT count(*) FROM information_schema.columns WHERE table_name = 'accounts' AND column_name = 'last_renew_at'"

echo "== dedup index =="
q "SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_jobs_active_per_account'"

echo "== renew/ovo settings =="
q "SELECT key || '=' || value::text FROM settings WHERE key LIKE 'renew.%' OR key LIKE 'ovo.%' OR key = 'worker.pay_enabled' ORDER BY key"

echo
echo "== dedup index blocks a duplicate active job =="
q "INSERT INTO accounts(msisdn, status) VALUES('6283100000001','new') ON CONFLICT (msisdn) DO NOTHING" >/dev/null
ACC=$(q "SELECT id FROM accounts WHERE msisdn = '6283100000001'")
q "INSERT INTO jobs(kind, account_id) VALUES('axis_renew', $ACC) ON CONFLICT DO NOTHING" >/dev/null
q "INSERT INTO jobs(kind, account_id) VALUES('axis_renew', $ACC) ON CONFLICT DO NOTHING" >/dev/null
N=$(q "SELECT count(*) FROM jobs WHERE account_id = $ACC AND kind = 'axis_renew' AND state IN ('pending','running')")
echo "active axis_renew after two inserts: $N (must be 1)"

echo "== a different kind for the same account is still allowed =="
q "INSERT INTO jobs(kind, account_id) VALUES('axis_gift', $ACC) ON CONFLICT DO NOTHING" >/dev/null
M=$(q "SELECT count(*) FROM jobs WHERE account_id = $ACC AND state IN ('pending','running')")
echo "active jobs across kinds: $M (must be 2)"

echo "== after completion a fresh job may be enqueued again =="
q "UPDATE jobs SET state='done' WHERE account_id = $ACC AND kind = 'axis_renew'" >/dev/null
q "INSERT INTO jobs(kind, account_id) VALUES('axis_renew', $ACC) ON CONFLICT DO NOTHING" >/dev/null
K=$(q "SELECT count(*) FROM jobs WHERE account_id = $ACC AND kind = 'axis_renew' AND state IN ('pending','running')")
echo "active axis_renew after completing the first: $K (must be 1)"
