#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Smoke run inside WSL: a fresh kingresd creates a one-server cluster through the API.
# Usage: smoke.sh <kingresd-binary>   (uses ~/kw-cp/install and ports 56150-56151)
set -u
KD="$1"; INST="$HOME/kw-cp/install"; D="$HOME/kw-cp/test/smoke"; API=http://127.0.0.1:56151/api/v1
export LD_LIBRARY_PATH="$INST/lib/x86_64-linux-gnu"
rm -rf "$D"; mkdir -p "$D"
"$KD" -version
"$KD" -listen 127.0.0.1:56151 -data-dir "$D/data" -pg-bin-dir "$INST/bin" -pg-port 56150 -server-host 127.0.0.1 -log-format json >"$D/kingresd.log" 2>&1 &
KPID=$!
for i in $(seq 1 50); do curl -sf $API/healthz >/dev/null && break; sleep 0.2; done
j() { curl -s -H "Content-Type: application/json" "$@"; echo; }
echo "== init-cluster"; j -X POST $API/setup/init-cluster -H "X-Setup-Token: $(cat $D/data/setup-token)" \
  -d "{\"clusterName\":\"smoke\",\"admin\":{\"username\":\"root_admin\",\"password\":\"Smoke-Passw0rd-1\"},\"dataPaths\":{\"dataDirectory\":\"$D/data\"},\"memoryQuota\":{\"totalMegabytes\":1024}}" | cut -c1-160
for i in $(seq 1 100); do s=$(curl -s $API/setup/state); echo "$s" | grep -q "progressPercent\":100" && break; sleep 0.3; done
echo "== setup state"; echo "$s" | grep -o '"state":"[a-z]*"\|"progressPercent":[0-9.]*' | tr '\n' ' '; echo
TOK=$(curl -s -X POST $API/auth/login -H "Content-Type: application/json" -d '{"username":"root_admin","password":"Smoke-Passw0rd-1","issueToken":true}' | sed 's/.*"token":"\([^"]*\)".*/\1/')
A="Authorization: Bearer $TOK"
echo "== create role"; j -X POST $API/roles -H "$A" -d '{"name":"app_user","password":"App-Passw0rd-12","clusterRoles":["kingres_readonly"]}' | cut -c1-200
echo "== create database (sharding + ledger)"; T=$(j -X POST $API/databases -H "$A" -d '{"name":"shop","sharding":{"enabled":true,"bucketCount":32},"ledger":{"mode":"optIn"}}'); echo "$T" | cut -c1-120
sleep 3; curl -s $API/databases/shop -H "$A" | grep -o '"sharding":{[^}]*}\|"ledger":{[^}]*}'
q() { curl -s -X POST $API/query -H "$A" -H "Content-Type: application/json" -d "{\"database\":\"shop\",\"sql\":\"$1\"}"; echo; }
echo "== tables"; q "create table public.orders (id bigint primary key, customer text)" | cut -c1-120
q "create table public.audit_rows (id bigint primary key, note text)" | cut -c1-120
echo "== distribute"; j -X PUT $API/databases/shop/schemas/public/tables/orders/distribution -H "$A" -d '{"method":"hash","distributionColumn":"id"}' | cut -c1-120; sleep 3
curl -s $API/databases/shop/schemas/public/tables/orders -H "$A" | grep -o '"classes":\[[^]]*\]'
echo "== enable ledger"; j -X PUT $API/databases/shop/schemas/public/tables/audit_rows/ledger -H "$A" -d '{"mode":"appendOnly"}' | grep -o '"ledger":{[^}]*}'
echo "== insert with psql"; PGPASSWORD='Smoke-Passw0rd-1' "$INST/bin/psql" -X -h 127.0.0.1 -p 56150 -U root_admin -d shop -c "insert into public.audit_rows values (1, 'first')"
echo "== workbench"; q "select id, note from public.audit_rows"
echo "== digest"; j -X POST $API/databases/shop/ledger/digests -H "$A" | cut -c1-260
echo "== digests"; j $API/databases/shop/ledger/digests -H "$A" | cut -c1-120
echo "== verification (task)"; j -X POST $API/databases/shop/ledger/verifications -H "$A" -d '{"scope":"full"}' | cut -c1-110; sleep 2
echo "== ledger status"; curl -s $API/databases/shop/ledger -H "$A" | grep -o '"verificationState":"[a-z]*"|"ledgerTableCount":[0-9]*|"lastVerifiedAt":"[^"]*"' | tr '
' ' '; echo
echo "== inclusion proof (not implemented)"; j -X POST $API/databases/shop/ledger/inclusion-proofs -H "$A" -d '{"schema":"public","table":"audit_rows","primaryKey":{"id":1}}' | cut -c1-150
echo "== this server / resources / logs"; curl -s $API/this-server -H "$A" | cut -c1-200; echo; curl -s "$API/this-server/logs?limit=2" -H "$A" | cut -c1-260; echo
echo "== metrics (Prometheus, needs a token)"; curl -s -o /dev/null -w "no token: %{http_code}
" http://127.0.0.1:56151/metrics; sleep 12; curl -s http://127.0.0.1:56151/metrics -H "$A" | head -4
kill $KPID; wait $KPID 2>/dev/null
"$INST/bin/pg_ctl" -D "$D/data/instances/1/pgdata" -m immediate stop >/dev/null 2>&1; rm -rf "$D"
