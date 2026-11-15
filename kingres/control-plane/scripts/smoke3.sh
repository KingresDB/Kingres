#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
#
# Three-server smoke run inside WSL, driven only through the HTTP APIs (curl) and
# psql: it forms a cluster of three kingresd processes on 127.0.0.2, .3 and .4,
# joins the second from its own API, adds the third from the cluster's API,
# removes an empty server, creates a role and a sharded database with a
# distributed table, reads rows through every server, signs in to the third with
# the role made on the first, sends a mutation to a non-leader, shows the refusal
# to remove a server that owns buckets, and then the later increments: server
# groups, a ledger inclusion proof (checked by kingres-verify when its path is
# given), the stop and start of the instance of a follower, a follower that
# leaves the cluster and joins again, cluster-wide logs, alerts and metrics
# (also with a server down), and certificates that live 30 seconds being renewed
# by the servers while statements keep routing between them.
#
# Usage: smoke3.sh <kingresd-binary> [base-port] [kingres-verify-binary]
# (uses ~/kw-cp/install, ports base..base+8; the script finds nothing else by path).
set -u
KD="$1"; BASE="${2:-56120}"; VERIFY="${3:-}"
INST="$HOME/kw-cp/install"; D="$HOME/kw-cp/test/smoke3"
export LD_LIBRARY_PATH="$INST/lib/x86_64-linux-gnu"
PSQL="$INST/bin/psql"
rm -rf "$D"; mkdir -p "$D"
ADMIN=root_admin; APW='Smoke-Passw0rd-1'
declare -a HOST API PG PID
for i in 1 2 3; do
  HOST[$i]="127.0.0.$((i+1))"; API[$i]=$((BASE + (i-1)*3)); PG[$i]=$((BASE + (i-1)*3 + 2))
done
FAILED=0
check() { # check <description> <condition-result: 0 = ok>
  if [ "$2" = 0 ]; then echo "  ok: $1"; else echo "  FAILED: $1"; FAILED=$((FAILED+1)); fi
}

start() { # start node $1 (extra environment: SMOKE_ENV_$1, a list of NAME=value)
  local i=$1 e="SMOKE_ENV_$1"
  env ${!e:-} "$KD" -listen "${HOST[$i]}:${API[$i]}" -internal-listen "${HOST[$i]}:$((API[$i]+1))" -data-dir "$D/n$i/data" -socket-dir "$D/n$i/s" \
    -pg-bin-dir "$INST/bin" -pg-port "${PG[$i]}" -pg-listen "127.0.0.1,${HOST[$i]}" -server-host "${HOST[$i]}" -log-format json \
    -login-rate-limit 1000 >>"$D/n$i/kingresd.log" 2>&1 &
  PID[$i]=$!
  for _ in $(seq 1 100); do curl -sf "http://${HOST[$i]}:${API[$i]}/healthz" >/dev/null && return; sleep 0.2; done
  echo "node $i did not start"; tail "$D/n$i/kingresd.log"; exit 1
}
stop() { kill "${PID[$1]:-0}" 2>/dev/null; wait "${PID[$1]:-0}" 2>/dev/null; }
cleanup() {
  for i in 1 2 3; do
    kill "${PID[$i]:-0}" 2>/dev/null
    [ -f "$D/n$i/data/instances/1/pgdata/postmaster.pid" ] && "$INST/bin/pg_ctl" stop -D "$D/n$i/data/instances/1/pgdata" -m fast -w >/dev/null 2>&1
  done
}
trap cleanup EXIT
for i in 1 2 3; do mkdir -p "$D/n$i"; start $i; done

base() { echo "http://${HOST[$1]}:${API[$1]}/api/v1"; }
TOK=()
api() { # api <node> <method> <path> [json] [extra curl args...]
  local n=$1 m=$2 p=$3 b="${4:-}"; shift 4 2>/dev/null || shift $#
  if [ -n "$b" ]; then curl -s -X "$m" "$(base $n)$p" -H "Content-Type: application/json" -H "Authorization: Bearer ${TOK[$n]:-}" "$@" -d "$b"
  else curl -s -X "$m" "$(base $n)$p" -H "Authorization: Bearer ${TOK[$n]:-}" "$@"; fi
}
login() { TOK[$1]=$(curl -s -X POST "$(base $1)/auth/login" -H "Content-Type: application/json" -d "{\"username\":\"$2\",\"password\":\"$3\",\"issueToken\":true}" | jq -r .token); }
wait_setup() { for _ in $(seq 1 600); do s=$(curl -s "$(base $1)/setup/state" | jq -r '.setupTask.state'); [ "$s" = done ] && return; [ "$s" = failed ] && { echo "setup failed on node $1"; curl -s "$(base $1)/setup/state" | jq -c .setupTask.error; exit 1; }; sleep 0.5; done; echo "setup timeout"; exit 1; }
wait_task() { for _ in $(seq 1 900); do t=$(api $1 GET "/tasks/$2" ""); s=$(echo "$t" | jq -r .state); [ "$s" = done ] && return 0; [ "$s" = failed ] && { echo "task $2 failed: $(echo "$t" | jq -c .error)"; return 1; }; sleep 0.5; done; return 1; }
sql() { PGPASSWORD="${3:-$APW}" "$PSQL" -X -A -t -h 127.0.0.1 -p "${PG[$1]}" -U "${4:-$ADMIN}" -d "$2" -c "$5" 2>&1; }
say() { echo; echo "== $*"; }
serial() { openssl x509 -in "$D/n$1/data/pki/server.crt" -noout -serial | cut -d= -f2; }

say "1. create the cluster on server 1 (${HOST[1]})"
curl -s -X POST "$(base 1)/setup/init-cluster" -H "X-Setup-Token: $(cat $D/n1/data/setup-token)" -H "Content-Type: application/json" \
  -d "{\"clusterName\":\"smoke3\",\"admin\":{\"username\":\"$ADMIN\",\"password\":\"$APW\"},\"dataPaths\":{\"dataDirectory\":\"$D/n1/data\"},\"memoryQuota\":{\"totalMegabytes\":1024}}" | jq -c '{id,kind,state}'
wait_setup 1; login 1 $ADMIN $APW; echo "state: $(curl -s $(base 1)/setup/state | jq -r .state)"

say "2. join server 2 from its own API, pinning the fingerprint that server 1 shows on its own panel"
FP1=$(api 1 GET /this-server '' | jq -r .internalFingerprint)
echo "fingerprint of server 1: $FP1"
curl -s -X POST "$(base 2)/setup/join-cluster" -H "X-Setup-Token: $(cat $D/n2/data/setup-token)" -H "Content-Type: application/json" \
  -d "{\"existingNodeAddress\":\"${HOST[1]}:${API[1]}\",\"expectedFingerprint\":\"$FP1\",\"admin\":{\"username\":\"$ADMIN\",\"password\":\"$APW\"},\"dataPaths\":{\"dataDirectory\":\"$D/n2/data\"},\"memoryQuota\":{\"totalMegabytes\":1024}}" | jq -c '{id,kind,state}'
wait_setup 2; login 2 $ADMIN $APW; echo "server 2 state: $(curl -s $(base 2)/setup/state | jq -r .state)"
grep -h 'certificate seen on first contact' "$D/n2/kingresd.log" | jq -c '{msg,sha256,pinned}'

say "3. add server 3 from the cluster's API, pinning its bootstrap fingerprint"
# a server that is not a member shows the fingerprint of its bootstrap certificate to the machine it runs on
fp3() { curl -s "$(base 3)/setup/state" | jq -r .internalFingerprint; }
FP=$(fp3)
echo "fingerprint of server 3: $FP"
T=$(api 1 POST /servers "{\"address\":\"${HOST[3]}:${API[3]}\",\"setupToken\":\"$(cat $D/n3/data/setup-token)\"}" -H "X-Kingres-Pin-Sha256: $FP")
echo "$T" | jq -c '{id,kind,state}'; wait_task 1 "$(echo "$T" | jq -r .id)" && echo "task done"; login 3 $ADMIN $APW
echo "servers listed by each server:"; for i in 1 2 3; do echo "  server $i: $(api $i GET /servers '' | jq -c '[.items[] | {id, status}]')"; done

say "4. remove an empty server (server 3), then add it again"
T=$(api 1 DELETE "/servers/${HOST[3]}" ""); echo "$T" | jq -c '{id,kind,state}'; wait_task 1 "$(echo "$T" | jq -r .id)" && echo "task done"
echo "server 3 is now: $(curl -s $(base 3)/setup/state | jq -r .state); servers: $(api 1 GET /servers '' | jq -c '[.items[].id]')"
T=$(api 1 POST /servers "{\"address\":\"${HOST[3]}:${API[3]}\",\"setupToken\":\"$(cat $D/n3/data/setup-token)\",\"expectedFingerprint\":\"$(fp3)\"}"); wait_task 1 "$(echo "$T" | jq -r .id)" && echo "server 3 added again"; login 3 $ADMIN $APW

say "4b. server 3 leaves the cluster through its own API (it owns nothing), then is added again"
T=$(api 3 POST /this-server/leave-cluster '{}'); echo "$T" | jq -c '{id,kind,state,subject}'
wait_task 1 "$(echo "$T" | jq -r .id)" && echo "leave task done (read through the leader)"
for _ in $(seq 1 60); do [ "$(curl -s $(base 3)/setup/state | jq -r .state)" = uninitialized ] && break; sleep 1; done
echo "server 3 is now: $(curl -s $(base 3)/setup/state | jq -r .state); servers: $(api 1 GET /servers '' | jq -c '[.items[].id]')"
check "the revocation list of the leader holds the certificate of the server that left" "$([ -n "$(curl -s --cert $D/n1/data/pki/server.crt --key $D/n1/data/pki/server.key --cacert $D/n1/data/pki/ca.crt -H 'X-Kingres-Internal-Version: 1' https://${HOST[1]}:$((API[1]+1))/internal/v1/revoked | jq -r '.serials[]')" ]; echo $?)"
T=$(api 1 POST /servers "{\"address\":\"${HOST[3]}:${API[3]}\",\"setupToken\":\"$(cat $D/n3/data/setup-token)\",\"expectedFingerprint\":\"$(fp3)\"}"); wait_task 1 "$(echo "$T" | jq -r .id)" && echo "server 3 joined again with a new setup token and a new certificate"; login 3 $ADMIN $APW

say "5. a role and a sharded database made on server 1 exist on all three"
api 1 POST /roles "{\"name\":\"app_user\",\"password\":\"App-Passw0rd-12\",\"clusterRoles\":[\"kingres_readonly\"]}" | jq -c '{name,canLogin}'
T=$(api 1 POST /databases '{"name":"shop","sharding":{"enabled":true,"bucketCount":32}}'); wait_task 1 "$(echo "$T" | jq -r .id)" && echo "database task done"
echo "task steps: $(api 1 GET "/tasks/$(echo "$T" | jq -r .id)" '' | jq -c '[.progress.steps[].name]')"
for i in 1 2 3; do echo "  server $i: $(sql $i postgres "$APW" $ADMIN "select string_agg(datname, ',' order by datname) from pg_database where datname='shop'") / role $(sql $i postgres "$APW" $ADMIN "select count(*) from pg_roles where rolname='app_user'")"; done

say "6. create and distribute a table; its buckets are on all three servers"
sql 1 shop "$APW" $ADMIN "create table public.orders (id bigint primary key, customer text)"
T=$(api 1 PUT /databases/shop/schemas/public/tables/orders/distribution '{"method":"hash","distributionColumn":"id"}'); wait_task 1 "$(echo "$T" | jq -r .id)" && echo "distribution done"
sql 1 shop "$APW" $ADMIN "grant select, insert on public.orders to app_user"; sql 1 shop "$APW" $ADMIN "grant usage on schema public to app_user"
sql 1 shop "$APW" $ADMIN "select n.host, count(*) as buckets from kingres.bucket_map b join kingres.node n on n.group_id = b.group_id and n.role='primary' group by 1 order by 1"

say "7. insert through server 1, read each row through servers 2 and 3 (certificate-authenticated node connections)"
for k in 1 2 3 4 5 6; do sql 1 shop "$APW" $ADMIN "insert into public.orders values ($k, 'customer-$k')" >/dev/null; done
for i in 1 2 3; do echo "  server $i: $(sql $i shop "$APW" $ADMIN "select count(*), string_agg(customer, ',' order by id) from public.orders")"; done
echo "  as app_user through server 3, key 4: $(sql 3 shop 'App-Passw0rd-12' app_user 'select customer from public.orders where id = 4')"
echo "  a scan of app_user on server 3 reads every server; while the session is open each receiving side shows (pg_stat_ssl):"
(PGPASSWORD='App-Passw0rd-12' "$PSQL" -X -A -t -h 127.0.0.1 -p "${PG[3]}" -U app_user -d shop -c "select count(*) from public.orders; select pg_sleep(8)" >/dev/null 2>&1 &)
sleep 3
for i in 1 2; do echo "  on server $i:"; sql $i shop "$(cat $D/n$i/data/service-credentials)" kingresd "select 'ssl=' || s.ssl, 'client_dn=' || s.client_dn, 'role=' || a.usename from pg_stat_ssl s join pg_stat_activity a on a.pid = s.pid where s.client_dn is not null and a.usename = 'app_user'"; done

say "8. sign in to server 3 with the role made on server 1"
curl -s -X POST "$(base 3)/auth/login" -H "Content-Type: application/json" -d '{"username":"app_user","password":"App-Passw0rd-12","issueToken":true}' | jq -c 'del(.token)'

say "9. a mutation sent to server 2 (not the leader) is forwarded and audited once"
api 2 POST /roles '{"name":"forwarded_role","password":"Fwd-Passw0rd-123","clusterRoles":["kingres_stats"]}' | jq -c '{name}'
echo "leader audit events with forwardedBy: $(api 1 GET '/audit?action=createRole&limit=100' '' | jq -c '[.items[] | select(.details.forwardedBy != null) | {actor, forwardedBy: .details.forwardedBy, outcome}]')"
echo "server 2 audit (createRole by $ADMIN): $(api 2 GET '/audit?action=createRole&limit=100' '' | jq -c '[.items[] | select(.actor == "'$ADMIN'")] | length')"
for i in 1 2 3; do echo "  server $i has the role: $(sql $i postgres "$APW" $ADMIN "select count(*) from pg_roles where rolname='forwarded_role'")"; done

say "10. removing a server that owns buckets, and the map group's server, is refused"
for i in 2 1; do echo "  DELETE ${HOST[$i]}: $(api 1 DELETE "/servers/${HOST[$i]}" '' | jq -c '{status, code, detail}')"; done
echo "servers still listed: $(api 1 GET /servers '' | jq -c '[.items[].id]')"

say "11. server groups: create, assign through a follower, a group with a server cannot be deleted"
api 1 POST /server-groups '{"name":"rack-a","description":"Row 1"}' | jq -c '{name,serverIds}'
api 2 PATCH "/servers/${HOST[3]}" '{"serverGroup":"rack-a"}' | jq -c '{id,serverGroup}'
echo "  seen from server 3: $(api 3 GET /server-groups '' | jq -c '(if type == "array" then . else .items end) | map({name, serverIds})')"
R=$(api 1 DELETE /server-groups/rack-a '' -w '
%{http_code}'); CODE=$(echo "$R" | tail -1); echo "  deleting a group with a server: HTTP $CODE $(echo "$R" | head -1 | jq -c '{code}')"
check "a group with a server is not deleted" "$([ "$CODE" = 409 ]; echo $?)"
api 1 PATCH "/servers/${HOST[3]}" '{"serverGroup":null}' | jq -c '{id,serverGroup}'
R=$(api 1 DELETE /server-groups/rack-a '' -w '%{http_code}'); check "the empty group is deleted" "$([ "$R" = 204 ]; echo $?)"

say "12. a ledger database: an inclusion proof of a row, checked by kingres-verify when a path is given"
T=$(api 1 POST /databases '{"name":"audit","ledger":{"mode":"optIn"}}'); wait_task 1 "$(echo "$T" | jq -r .id)" && echo "database task done"
sql 1 audit "$APW" $ADMIN "create table public.entries (id bigint primary key, note text)" >/dev/null
api 1 PUT /databases/audit/schemas/public/tables/entries/ledger '{"mode":"appendOnly"}' | jq -c '{name, ledger}'
sql 1 audit "$APW" $ADMIN "insert into public.entries values (1, 'first'), (2, 'second')" >/dev/null
api 1 POST /databases/audit/ledger/digests '' | jq -c '{generatedBy, chains: (.chains | length)}'
api 1 POST /databases/audit/ledger/digests '' >"$D/digest-response.json"
api 1 POST /databases/audit/ledger/inclusion-proofs '{"schema":"public","table":"entries","primaryKey":{"id":1}}' >"$D/proof.json"
echo "  proof members: $(jq -c '[.format, .op, .entry_count, (.entry_path | length), (.block.block_hash != null)]' "$D/proof.json")"
check "the proof has the members of the extension's document" "$(jq -e '.format == 1 and .op == "I" and .block.block_hash != null' "$D/proof.json" >/dev/null; echo $?)"
if [ -n "$VERIFY" ] && [ -x "$VERIFY" ]; then
  jq '.chains[0].document' "$D/digest-response.json" >"$D/digest.json"
  TID=$(jq -r .table_id "$D/proof.json"); RH=$(jq -r .row_hash "$D/proof.json")
  "$VERIFY" proof --proof "$D/proof.json" --digest "$D/digest.json" --table-id "$TID" --row-hash "$RH" >"$D/verify.out" 2>&1
  check "kingres-verify accepts the proof ($(head -c 120 "$D/verify.out" | tr '\n' ' '))" "$?"
else echo "  (no kingres-verify given: the proof was not verified)"; fi

say "13. stop and start the instance of a follower (server 3) through its own API; the leader keeps answering"
IID3=$(api 3 GET /this-server/instances "" | jq -r "(if type == \"array\" then . else .items end)[0].id"); echo "  the instance id of server 3 in the catalog: $IID3"
T=$(api 3 POST /this-server/instances/$IID3/stop '{"force":true}'); echo "$T" | jq -c '{id,kind,state}'; wait_task 1 "$(echo "$T" | jq -r .id)" && echo "stop task done (read through the leader)"
echo "  server 3 answers a protected call while its instance is stopped: $(api 3 GET /cluster '' | jq -c '{status,code}')"
echo "  the leader still lists three servers: $(api 1 GET /servers '' | jq -c '[.items[].id]')"
for _ in $(seq 1 60); do [ -f "$D/n3/data/instances/1/pgdata/postmaster.pid" ] || break; sleep 1; done  # the stop task is recorded done just before the shutdown
T=$(curl -s -X POST "$(base 3)/this-server/instances/$IID3/start" -H "Authorization: Bearer ${TOK[3]}" -H "Content-Type: application/json" -d '{}')
echo "$T" | jq -c '{id,kind,state}'; wait_task 1 "$(echo "$T" | jq -r .id)" && echo "start task done"; login 3 $ADMIN $APW
echo "  server 3 reads its rows again: $(sql 3 shop "$APW" $ADMIN "select count(*) from public.orders")"

say "14. a server that owns buckets cannot leave (409), the leader's server cannot either"
echo "  server 3: $(api 3 POST /this-server/leave-cluster '{}' | jq -c '{status, code}')"
echo "  server 3 with force (force lifts only the reachability of the cluster): $(api 3 POST /this-server/leave-cluster '{"force":true}' | jq -c '{status, code}')"
echo "  server 1: $(api 1 POST /this-server/leave-cluster '{}' | jq -c '{status, code}')"

say "15. cluster-wide logs, alerts and metrics from server 2, and with server 3 stopped"
for _ in $(seq 1 30); do N=$(api 2 GET '/logs?limit=500' '' | jq '[.items[].serverId] | unique | length'); [ "$N" = 3 ] && break; sleep 2; done
echo "  logs: entries of $N servers, partial=$(api 2 GET '/logs?limit=5' '' | jq -c .partial)"
for i in 1 2 3; do sql $i postgres "$(cat $D/n$i/data/service-credentials)" kingresd "select 1" >/dev/null; done
SYS3=$(sql 3 kingres_system "$(cat $D/n3/data/service-credentials)" kingresd "insert into kingres_cp.alert (severity, kind, subject, message) values ('warning','replicationLagHigh','server:${HOST[3]}','smoke alert of server 3') returning id")
echo "  an alert in the table of server 3 (local id $(echo "$SYS3" | head -1))"
AL=$(api 2 GET '/alerts?limit=100' ''); echo "  alerts listed by server 2: $(echo "$AL" | jq -c '[.items[] | {id, kind, serverId}]')"
AID=$(echo "$AL" | jq -r '[.items[] | select(.message == "smoke alert of server 3")][0].id')
echo "  acknowledging it through server 1 (it lives on server 3): $(api 1 PUT "/alerts/$AID/acknowledgement" '' | jq -c '{id, state, acknowledgedBy}')"
FROM=$(date -u -d '-10 minutes' +%Y-%m-%dT%H:%M:%SZ); TO=$(date -u -d '+1 minute' +%Y-%m-%dT%H:%M:%SZ)
sleep 11
echo "  metrics (connections per server) from server 2: $(api 2 GET "/metrics/query?metric=connections&from=$FROM&to=$TO&step=60&groupBy=server" '' | jq -c '[.series[].labels.serverId]')"
stop 3
echo "  server 3 stopped; logs: $(api 2 GET '/logs?limit=5' '' | jq -c '{partial, missingServers}')"
echo "                    alerts: $(api 2 GET '/alerts?limit=5' '' | jq -c '{partial, missingServers}')"
start 3; for _ in $(seq 1 60); do login 3 $ADMIN $APW; [ "${TOK[3]}" != null ] && [ -n "${TOK[3]}" ] && break; sleep 1; done
echo "  server 3 is back: logs partial=$(api 2 GET '/logs?limit=5' '' | jq -c .partial)"

say "16. certificates that live 30 seconds are renewed by the servers while statements keep routing"
for i in 1 2 3; do stop $i; done
SMOKE_ENV_1="KINGRES_CERTIFICATE_LIFETIME=30s KINGRES_CERTIFICATE_CHECK_INTERVAL=1s KINGRES_CERTIFICATE_RETRY_INTERVAL=500ms"
SMOKE_ENV_2="KINGRES_CERTIFICATE_CHECK_INTERVAL=1s KINGRES_CERTIFICATE_RETRY_INTERVAL=500ms"
SMOKE_ENV_3="$SMOKE_ENV_2"
for i in 1 2 3; do start $i; done
for i in 1 2 3; do for _ in $(seq 1 60); do login $i $ADMIN $APW; [ "${TOK[$i]}" != null ] && [ -n "${TOK[$i]}" ] && break; sleep 1; done; done
for i in 1 2 3; do T=$(api 1 POST "/servers/${HOST[$i]}/certificates/renewals" ''); wait_task 1 "$(echo "$T" | jq -r .id)" && echo "  renewed now: ${HOST[$i]} ($(echo "$T" | jq -r .kind))"; done
S0="$(serial 1) $(serial 2) $(serial 3)"; echo "  serial numbers now:   $S0"
OKS=0; BAD=0
for _ in $(seq 1 22); do
  N=$(sql 2 shop "$APW" $ADMIN "select count(*) from public.orders" | head -1)
  [ "$N" = 6 ] && OKS=$((OKS+1)) || { BAD=$((BAD+1)); echo "  a read through server 2 gave: $N"; }
  sleep 2
done
S1="$(serial 1) $(serial 2) $(serial 3)"; echo "  serial numbers later: $S1"
echo "  $OKS reads through server 2 that route to every server succeeded, $BAD failed, in 44 seconds"
check "no read failed while the certificates were renewed" "$([ "$BAD" = 0 ]; echo $?)"
check "every server holds another certificate than 44 seconds ago" "$([ "$S0" != "$S1" ] && [ "$(serial 1)" != "$(echo $S0 | cut -d' ' -f1)" ] && [ "$(serial 2)" != "$(echo $S0 | cut -d' ' -f2)" ] && [ "$(serial 3)" != "$(echo $S0 | cut -d' ' -f3)" ]; echo $?)"
echo "  the renewals in the audit file of the leader: $(grep -c '"action":"certificateRenewed"' "$D/n1/data/audit.jsonl")"
echo "  the servers list the new expiry: $(api 2 GET /servers '' | jq -c '[.items[] | {id, certificateExpiresAt}]')"

say "17. revoking the certificates of server 3: the internal API and PostgreSQL refuse it, the renewal is refused"
T=$(api 1 POST "/servers/${HOST[3]}/certificates/revocations" ''); echo "$T" | jq -c '{id,kind,state}'; wait_task 1 "$(echo "$T" | jq -r .id)" && echo "revocation task done"
echo "  the leader's record: $(api 1 GET /servers '' | jq -c '[.items[] | select(.id == "'${HOST[3]}'") | {id, certificateRevokedAt}]')"
R=$(api 1 POST "/servers/${HOST[3]}/certificates/renewals" ''); echo "  renewing it: $(echo "$R" | jq -c '{status, code}')"
check "a revoked server gets no new certificate" "$([ "$(echo "$R" | jq -r .code)" = certificate_revoked ]; echo $?)"
CODE=$(curl -s -o /dev/null -w '%{http_code}' --cert "$D/n3/data/pki/server.crt" --key "$D/n3/data/pki/server.key" --cacert "$D/n1/data/pki/ca.crt" -H 'X-Kingres-Internal-Version: 1' "https://${HOST[1]}:$((API[1]+1))/internal/v1/state")
echo "  the internal API of server 1 answers the revoked certificate of server 3 with HTTP $CODE"
check "the internal API refuses a revoked certificate" "$([ "$CODE" = 401 ]; echo $?)"
echo; [ "$FAILED" = 0 ] && echo "smoke3 finished: every check passed" || echo "smoke3 finished: $FAILED check(s) FAILED"
exit $FAILED
