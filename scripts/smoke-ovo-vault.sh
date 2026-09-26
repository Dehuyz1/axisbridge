#!/usr/bin/env bash
# Prove the OVO wallet vault end-to-end against a running axis-core:
#   1. the PIN round-trips into the database as ciphertext,
#   2. neither the PIN nor the encrypted token ever appears in an API response,
#   3. the wallet can be removed again.
#
# No OVO network call is made: login needs a real OTP. Everything up to the
# login boundary is exercised.
set -eu

CORE=${CORE:-http://127.0.0.1:5001}
MSISDN_IN=081299887766
MSISDN_DB=6281299887766
PIN=135790

psql() { docker exec axisbridge-pg psql -U axis -d axisbridge -tAc "$1"; }
fail=0
check() { # check <label> <actual> <expected>
  if [ "$2" = "$3" ]; then echo "ok   $1 = $2"; else echo "FAIL $1 = $2 (want $3)"; fail=$((fail + 1)); fi
}

echo "== vault enabled at startup =="
unset_warn=$(journalctl -u axis-core --since '5 min ago' --no-pager | grep -c 'OVO_MASTER_KEY unset' || true)
check "startup warnings about missing key" "$unset_warn" 0

echo
echo "== POST /api/ovo =="
code=$(curl -sS -o /tmp/ovo-create.json -w '%{http_code}' -X POST "$CORE/api/ovo" \
  -H 'Content-Type: application/json' \
  -d "{\"msisdn\":\"$MSISDN_IN\",\"pin\":\"$PIN\",\"name\":\"Vault Probe\"}")
check "create status" "$code" 201
cat /tmp/ovo-create.json
echo
ID=$(sed -n 's/.*"id":\([0-9]*\).*/\1/p' /tmp/ovo-create.json)

echo
echo "== msisdn normalised to 62 form =="
stored=$(psql "SELECT msisdn FROM ovo_accounts WHERE id = $ID")
check "stored msisdn" "$stored" "$MSISDN_DB"

echo
echo "== PIN stored as ciphertext, not plaintext =="
enc_len=$(psql "SELECT length(pin_enc) FROM ovo_accounts WHERE id = $ID")
echo "     pin_enc length: $enc_len (AES-GCM + base64, so well above 6)"
if [ "$enc_len" -gt 20 ]; then echo "ok   pin_enc is not a 6-digit plaintext"; else
  echo "FAIL pin_enc too short to be ciphertext"; fail=$((fail + 1)); fi
leak=$(psql "SELECT count(*) FROM ovo_accounts WHERE id = $ID AND pin_enc LIKE '%$PIN%'")
check "rows whose pin_enc contains the plaintext PIN" "$leak" 0

echo
echo "== GET /api/ovo never exposes the PIN or the encrypted token =="
curl -sS "$CORE/api/ovo" -o /tmp/ovo-list.json
check "plaintext PIN occurrences in response" "$(grep -c "$PIN" /tmp/ovo-list.json || true)" 0
check "pin_enc occurrences in response" "$(grep -c 'pin_enc' /tmp/ovo-list.json || true)" 0
check "token_enc occurrences in response" "$(grep -c 'token_enc' /tmp/ovo-list.json || true)" 0
echo "     exposed fields:"
python3 - "$MSISDN_DB" <<'PY'
import json, sys
rows = json.load(open('/tmp/ovo-list.json'))
row = next(r for r in rows if r['msisdn'] == sys.argv[1])
print('     ', ', '.join(sorted(row)))
PY

echo
echo "== fresh wallet reports state=new and has_token=false =="
state=$(psql "SELECT state FROM ovo_accounts WHERE id = $ID")
check "state" "$state" new
has=$(python3 - "$MSISDN_DB" <<'PY'
import json, sys
rows = json.load(open('/tmp/ovo-list.json'))
print(str(next(r for r in rows if r['msisdn'] == sys.argv[1])['has_token']).lower())
PY
)
check "has_token" "$has" false

echo
echo "== verify without an OTP in flight is rejected, not attempted =="
code=$(curl -sS -o /tmp/ovo-verify.txt -w '%{http_code}' -X POST "$CORE/api/ovo/$ID/verify" \
  -H 'Content-Type: application/json' -d '{"code":"000000"}')
check "verify-before-otp status" "$code" 409
echo "     body: $(cat /tmp/ovo-verify.txt)"

echo
echo "== DELETE /api/ovo/{id} =="
code=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$CORE/api/ovo/$ID")
check "delete status" "$code" 204
check "rows left" "$(psql "SELECT count(*) FROM ovo_accounts WHERE id = $ID")" 0

rm -f /tmp/ovo-create.json /tmp/ovo-list.json /tmp/ovo-verify.txt
echo
if [ "$fail" -gt 0 ]; then echo "RESULT: $fail failure(s)"; exit 1; fi
echo "RESULT: all assertions passed"
