#!/usr/bin/env bash
set -eu
echo "== systemd status =="
for s in axis-core axis-register-worker axis-gift-worker axis-ui; do
  printf "%-25s " "$s"
  systemctl is-active "$s" || true
done
echo
echo "== listening ports =="
ss -tlnp 2>/dev/null | grep -E ':(5000|5001|5002)\s' || echo "none"
echo
echo "== /healthz core =="
curl -sS http://127.0.0.1:5001/healthz && echo
echo
UP=$(grep '^UI_PASS=' /opt/axisbridge/.env | cut -d= -f2)
echo "== /api/accounts via UI =="
curl -sS -u "admin:$UP" http://127.0.0.1:5002/api/accounts && echo
echo
echo "== POST /api/register =="
curl -sS -u "admin:$UP" -H 'Content-Type: application/json' \
  -X POST http://127.0.0.1:5002/api/register \
  -d '{"msisdn":"083131234567"}' && echo
echo
echo "== wait 3s (register-worker + gift-worker cycle) =="
sleep 3
echo
echo "== /api/accounts (after register) =="
curl -sS -u "admin:$UP" http://127.0.0.1:5002/api/accounts && echo
echo
echo "== /api/gifts =="
curl -sS -u "admin:$UP" http://127.0.0.1:5002/api/gifts && echo
echo
echo "== recent logs =="
journalctl -u axis-register-worker -n 10 --no-pager 2>&1 | tail -12
echo
journalctl -u axis-gift-worker -n 10 --no-pager 2>&1 | tail -12
