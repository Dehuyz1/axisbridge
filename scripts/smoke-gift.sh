#!/usr/bin/env bash
set -eu

echo "== migrations state =="
docker exec axisbridge-pg psql -U axis -d axisbridge -c "SELECT version_id, is_applied FROM goose_db_version ORDER BY version_id"

echo
echo "== upload gift bulk (2 nomor, langsung skip OTP) =="
curl -sS -H "Content-Type: text/plain" \
  -X POST http://127.0.0.1:5002/api/gifts/bulk \
  --data-binary "083198765432 083199999999"
echo

echo
echo "== wait 4s gift-worker cycle =="
sleep 4

echo
echo "== /api/gifts (harus muncul 2 nomor baru) =="
curl -sS http://127.0.0.1:5002/api/gifts | head -c 500
echo

echo
echo "== /api/accounts (Management — TIDAK boleh ada 083198765432/999) =="
curl -sS http://127.0.0.1:5002/api/accounts | head -c 500
echo

echo
echo "== accounts by gift_only + status =="
docker exec axisbridge-pg psql -U axis -d axisbridge -c \
  "SELECT gift_only, status::text, COUNT(*) FROM accounts GROUP BY 1,2 ORDER BY 1,2"

echo
echo "== delete single gift (id terkecil di list) =="
FIRST_ID=$(curl -sS http://127.0.0.1:5002/api/gifts | python3 -c 'import sys,json;print(json.load(sys.stdin)[0]["id"])')
echo "delete id=$FIRST_ID"
curl -sS -X DELETE "http://127.0.0.1:5002/api/gifts/$FIRST_ID" -w "HTTP %{http_code}\n"

echo
echo "== /api/gifts count sekarang =="
curl -sS http://127.0.0.1:5002/api/gifts | python3 -c 'import sys,json;print(len(json.load(sys.stdin)),"rows")'
