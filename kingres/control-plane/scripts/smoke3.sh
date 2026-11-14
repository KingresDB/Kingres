#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
#
# Three-server smoke run inside WSL, driven only through the HTTP APIs (curl) and
# psql: it forms a cluster of three kingresd processes on 127.0.0.2, .3 and .4,
# joins the second from its own API, adds the third from the cluster's API,
# removes an empty server, creates a role and a sharded database with a
# distributed table, reads rows through every server, signs in to the third with
# the role made on the first, sends a mutation to a non-leader and shows the
# refusal to remove a server that owns buckets.
#
# Usage: smoke3.sh <kingresd-binary> [base-port]   (uses ~/kw-cp/install, ports base..base+8)
set -u
KD="$1"; BASE="${2:-56120}"
INST="$HOME/kw-cp/install"; D="$HOME/kw-cp/test/smoke3"
export LD_LIBRARY_PATH="$INST/lib/x86_64-linux-gnu"
PSQL="$INST/bin/psql"
rm -rf "$D"; mkdir -p "$D"
ADMIN=root_admin; APW='Smoke-Passw0rd-1'
declare -a HOST API PG PID
for i in 1 2 3; do
  HOST[$i]="127.0.0.$((i+1))"; API[$i]=$((BASE + (i-1)*3)); PG[$i]=$((BASE + (i-1)*3 + 2))
done

start() { # start node $1
  local i=$1
  "$KD" -listen "${HOST[$i]}:${API[$i]}" -internal-listen "${HOST[$i]}:$((API[$i]+1))" -data-dir "$D/n$i/data" -socket-dir "$D/n$i/s" \
    -pg-bin-dir "$INST/bin" -pg-port "${PG[$i]}" -pg-listen "127.0.0.1,${HOST[$i]}" -server-host "${HOST[$i]}" -log-format json \
    -login-rate-limit 1000 >>"$D/n$i/kingresd.log" 2>&1 &
  PID[$i]=$!
  for _ in $(seq 1 100); do curl -sf "http://${HOST[$i]}:${API[$i]}/healthz" >/dev/null && return; sleep 0.2; done
  echo "node $i did not start"; tail "$D/n$i/kingresd.log"; exit 1
}
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

say "1. create the cluster on server 1 (${HOST[1]})"
curl -s -X POST "$(base 1)/setup/init-cluster" -H "X-Setup-Token: $(cat $D/n1/data/setup-token)" -H "Content-Type: application/json" \
  -d "{\"clusterName\":\"smoke3\",\"admin\":{\"username\":\"$ADMIN\",\"password\":\"$APW\"},\"dataPaths\":{\"dataDirectory\":\"$D/n1/data\"},\"memoryQuota\":{\"totalMegabytes\":1024}}" | jq -c '{id,kind,state}'
wait_setup 1; login 1 $ADMIN $APW; echo "state: $(curl -s $(base 1)/setup/state | jq -r .state)"

say "2. join server 2 from its own API (trust on first use; fingerprint seen is logged)"
curl -s -X POST "$(base 2)/setup/join-cluster" -H "X-Setup-Token: $(cat $D/n2/data/setup-token)" -H "Content-Type: application/json" \
  -d "{\"existingNodeAddress\":\"${HOST[1]}:${API[1]}\",\"admin\":{\"username\":\"$ADMIN\",\"password\":\"$APW\"},\"dataPaths\":{\"dataDirectory\":\"$D/n2/data\"},\"memoryQuota\":{\"totalMegabytes\":1024}}" | jq -c '{id,kind,state}'
wait_setup 2; login 2 $ADMIN $APW; echo "server 2 state: $(curl -s $(base 2)/setup/state | jq -r .state)"
grep -h 'certificate seen on first contact' "$D/n2/kingresd.log" | jq -c '{msg,sha256,pinned}'

say "3. add server 3 from the cluster's API, pinning its bootstrap fingerprint"
FP=$(openssl x509 -in "$D/n3/data/pki/bootstrap.crt" -noout -fingerprint -sha256 | cut -d= -f2)
echo "fingerprint of server 3: $FP"
T=$(api 1 POST /servers "{\"address\":\"${HOST[3]}:${API[3]}\",\"setupToken\":\"$(cat $D/n3/data/setup-token)\"}" -H "X-Kingres-Pin-Sha256: $FP")
echo "$T" | jq -c '{id,kind,state}'; wait_task 1 "$(echo "$T" | jq -r .id)" && echo "task done"; login 3 $ADMIN $APW
echo "servers listed by each server:"; for i in 1 2 3; do echo "  server $i: $(api $i GET /servers '' | jq -c '[.items[] | {id, status}]')"; done

say "4. remove an empty server (server 3), then add it again"
T=$(api 1 DELETE "/servers/${HOST[3]}" ""); echo "$T" | jq -c '{id,kind,state}'; wait_task 1 "$(echo "$T" | jq -r .id)" && echo "task done"
echo "server 3 is now: $(curl -s $(base 3)/setup/state | jq -r .state); servers: $(api 1 GET /servers '' | jq -c '[.items[].id]')"
T=$(api 1 POST /servers "{\"address\":\"${HOST[3]}:${API[3]}\",\"setupToken\":\"$(cat $D/n3/data/setup-token)\"}"); wait_task 1 "$(echo "$T" | jq -r .id)" && echo "server 3 added again"; login 3 $ADMIN $APW

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
echo; echo "smoke3 finished"
