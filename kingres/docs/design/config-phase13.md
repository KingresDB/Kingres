<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Configuration from the console: design of phase 13

Status: accepted for implementation, 2026-11-15.
Builds on the control plane of phase 3 ([control-plane-3.md](control-plane-3.md)) and on the standby
management of phase 5 ([ha-phase5.md](ha-phase5.md)). It is phase 13 of the roadmap in
[architecture.md](../architecture.md) and depends on nothing after phase 5. It revises the REST contract
to **0.8.0** (additive: no operation, field or enum value of 0.7.0 changes meaning).

The project owner asked for three things: set the time and the time zone of the servers for the database
from the console; set every PostgreSQL configuration setting of the servers, for a group of servers and for
one server; and have both documented as their own part of the roadmap, built into the system and into the
console, including the dashboard.

## Sources read

| Source | What it settled |
|---|---|
| `kingres/control-plane/internal/instance/render.go`, `manager.go`, `standby.go`, `localsettings.go` | What `kingresd` writes into `kingres-managed.conf` (the managed list), where that file is included from, `writeAtomic`, `Reload`, `Restart`, `PostmasterToken`, one instance directory per instance id (1 is the primary, 100 + group is a standby) |
| `internal/standby/agent_ops.go`, `agent_primary.go` | `hot_standby`, `primary_conninfo`, `primary_slot_name` (standby) and `synchronous_standby_names` (primary) are written into the managed file |
| `internal/ops/databases.go` | `ALTER DATABASE ... SET synchronous_commit` is how a database's durability is kept: that parameter is already taken at database level |
| `internal/ops/fanout.go`, `network.go`, `internal-api.md` | The conventions to follow: a task with one step per server, `onServer` (wait for a server that cannot be reached, say which), `POST /internal/v1/apply` as a closed set of operations with structured parameters, `replicate.apply` for servers that were behind |
| `internal/task/task.go`, `control-plane-3.md` | Steps are idempotent and resumable, one unfinished task per subject, the task never stores a secret |
| `internal/sysdb/migrations/0001..0006` | Cluster state lives in `kingres_cp` of `kingres_system` (server groups: `server_group`, `server.server_group`) |
| `internal/clock/clock.go`, `ha-phase5.md` | Decisions use monotonic time, the wall clock is for display; only durations travel between servers |
| `kingres/api/openapi.yaml` 0.7.0, `kingres/api/README.md` | Conventions: `x-required-role`, problem codes, tasks and their kinds, `Alert` kinds, audit actions as a closed enum, partial answers |
| `contrib/kingres_shard/README.md` ("Nodes must agree", "Session settings"), `src/*.c` | Which settings the sharding extension fixes on remote sessions, which it defines, with which context |
| `src/backend/access/transam/xlog.c` (`CheckRequiredParameterValues`), `src/backend/utils/misc/guc_tables.c` | The five settings a hot standby needs at least as high as its primary; the settings PostgreSQL marks `GUC_SUPERUSER_ONLY` |
| Experiments on the project's own PostgreSQL 18.7 build (section "Facts checked") | How a reload treats an invalid value, what `pg_file_settings` reports, the validation functions, per-database settings |

## Goals

1. Show, for every PostgreSQL parameter of the running version (and of every extension that defines
   parameters), the value on every server, where it comes from and whether a restart is pending.
2. Set a parameter for the cluster, for a server group, for one server, or (where PostgreSQL allows it) for
   a database; remove such a setting; see the history of a parameter and revert a change.
3. Check a value against what PostgreSQL itself says about the parameter before anything is written.
4. Apply a change as a task with a step per server, confirm by reading back what PostgreSQL did, and report
   per server: applied, pending restart, failed, waiting.
5. Show the clock, the operating-system time zone and PostgreSQL's time zones of every server and the
   difference between the clocks; raise an alert when it is too large; set the time zone of PostgreSQL for the
   cluster and for a database.

### Out of scope

- **Setting the system clock.** `kingresd` does not set the clock of a server. That needs the rights of the
  operating system's administrator (`CAP_SYS_TIME`), which `kingresd` does not and must not have, and the right
  tool is time synchronization (NTP, chrony, `systemd-timesyncd`) because a clock that is set once drifts again.
  `kingresd` measures and reports the difference, and the console says what to do when it is too large.
- Settings of roles (`ALTER ROLE ... SET`) and of a role in a database. A role setting follows the person, not the
  servers, and is not part of this phase.
- Settings that PostgreSQL does not let `kingresd` reach: `pg_hba.conf` and `pg_ident.conf` (managed by the
  network rules), `postgresql.auto.conf` (never used), the encoding and collation of a database (fixed when it is
  created; the sharding extension needs them equal on every node and checks that in `add_node`).
- Changing a managed or refused parameter in any way (sections "Managed parameters" and "Refused parameters").
- Failover: it is not available yet. The restart dialog says that restarting a primary is downtime for its group.

## Facts checked

Everything the implementation depends on was checked in the source tree or by experiment on the project's
PostgreSQL 18.7 build (an instance of its own in a throwaway directory). **[Needs verification]** items of the
first draft and their outcome:

| # | Statement | Outcome |
|---|---|---|
| V1 | A value that PostgreSQL rejects in an included file stops the reload | **Partly.** An invalid *value* is skipped alone ("configuration file contains errors; unaffected changes were applied", `pg_file_settings.error` says `invalid value for parameter ...`), the other lines of the file are applied. A *syntax* error rejects the **whole** reload of every file ("no changes were applied"), including changes of the managed file made at the same time. Therefore a file is checked **before** it is reloaded (V2) |
| V2 | `pg_file_settings` shows the problems of a file that was not loaded | **Partly (corrected during implementation).** The view parses the files when it is queried; it needs no reload, and a syntax error shows at once. But an invalid *value* gives the same text as a restart that is needed, `setting could not be applied` with `applied = false`, and an out-of-range number gives no error at all. So the step reads the file before the reload and treats that text as "pending restart" only for a parameter of context `postmaster` (for any other it is a failure and the previous file is restored); the values themselves are checked by layers 2 and 3 before anything is written, and after the reload `pg_settings.pending_restart` and the running value are the proof |
| V3 | `pg_conf_load_time()` shows that a reload happened | **Yes, except after a syntax error** (the time does not move). The step records it before the reload and waits for it to move; a syntax error never reaches the reload because of V2 |
| V4 | A later line wins, also across `include` | **Yes.** In `kingres-managed.conf` an `include_if_exists` placed after the derived defaults and before the fixed managed lines is overridden by the lines after it and overrides the lines before it and `postgresql.conf` (checked with `work_mem` and `max_connections`; `pg_settings.sourcefile` and `sourceline` name the file and line that won). A missing file is tolerated (`include_if_exists`) with a `LOG` line per read, so the file always exists |
| V5 | `postgres -C name -c name=value -D data` validates a value with PostgreSQL's own parser and check hooks, also while the instance runs | **Yes for parameters PostgreSQL defines** (unit, range, enum, `TimeZone` names, `wal_level`). **No for the parameters of an extension** (it accepted `kingres_shard.node_timeout=abc`: the library is not loaded in that mode). The options of a value are passed as separate arguments, never through a shell, so a value cannot inject a second option |
| V6 | `set_config(name, value, true)` in a transaction that is rolled back validates a value | **Yes for `user` and `superuser` contexts, extensions included** (the libraries are loaded in a session). `postmaster` and `sighup` parameters fail with "cannot be changed without restarting the server" before the value is looked at, so it is not usable for them. `ALTER DATABASE ... SET` validates too (an unknown time zone, `shared_buffers` and `log_timezone` are refused with PostgreSQL's message and nothing is stored) |
| V7 | `pg_settings` is enough to validate | **Yes for type, unit, range and enum**: `vartype` (`bool`, `integer`, `real`, `string`, `enum`), `unit` (`B`, `kB`, `8kB`, `MB`, `ms`, `s`, `min`), `min_val`, `max_val`, `enumvals`, `context`. It is not enough for string parameters (their check hooks) and not for enum aliases (`synchronous_commit = true` is accepted and not in `enumvals`); V5 and V6 cover those |
| V8 | PostgreSQL's number syntax | **Wider than anyone expects:** `0x10` is 16, `010` is 8, `1e3` is 1000, `1.5` for a setting in kB is rounded to 2, `o` alone is not a boolean but `of`, `tr`, `y` are. The checker of `kingresd` accepts a narrow, unambiguous subset (below) and refuses the rest, and a differential test compares every accepted value with PostgreSQL |
| V9 | Superuser-only parameters can be found without a list | **Yes, but the set is not what the first draft said (corrected).** `SET LOCAL ROLE pg_database_owner` and read `pg_settings`: 387 of 413 rows remain; the 26 that disappear are the superuser-only parameters (`ssl_passphrase_command`, `dynamic_library_path`, `shared_preload_libraries`, `session_preload_libraries`, `primary_conninfo`, `data_directory`, `hba_file`, `log_directory`, the TLS cipher settings, `kingres_shard.node_conninfo`, ...). **`archive_command`, `restore_command`, `archive_cleanup_command`, `recovery_end_command`, `archive_library` and `local_preload_libraries` are visible to every database user** in PostgreSQL 18 (the first draft read them from a grep of `guc_tables.c` that was wrong). They run commands or load code all the same, so they are refused by name (and by the suffixes `_command`, `_library`, `_libraries` and a name with `preload`), and their value is never returned by this API either |
| V10 | `ALTER DATABASE ... SET` reaches standbys | **Yes by construction:** `pg_db_role_setting` is a shared catalog, so it is replicated physically. The change is made on the primary instance of every server (each shard group is a cluster of its own, with its own catalog) and standbys follow |
| V11 | A hot standby needs at least the primary's values of five settings | **Yes**, `CheckRequiredParameterValues` in `xlog.c`: `max_connections`, `max_worker_processes`, `max_wal_senders`, `max_prepared_transactions`, `max_locks_per_transaction`. A standby that starts below the primary's value does not start; a running standby whose primary was restarted with a higher value pauses replay until it is restarted |
| V12 | Which settings the sharding extension needs equal on every node | See "Cluster-only parameters". The extension forces `DateStyle`, `IntervalStyle`, `extra_float_digits`, `standard_conforming_strings`, `bytea_output` and `search_path` on the sessions it opens on other nodes (`node_conn.c`), so a statement's text transport does not depend on them; it does **not** force `TimeZone`, the `lc_*` settings or `default_text_search_config`, so a function evaluated on another node (`now()::timestamp`, `to_char`, `to_tsvector`) depends on that node's value |

## Levels, precedence and the managed list

### The four levels

| Level | Stored | Applied | Scope id |
|---|---|---|---|
| `cluster` | one value per parameter | every server | none |
| `serverGroup` | per group (rack or zone: the server groups of the contract) | the servers that are in the group when the change is applied and whenever they join it | group name |
| `server` | per server | that server | server id |
| `database` | per database | `ALTER DATABASE ... SET` on the primary instance of every server | database name |

`server` beats `serverGroup` beats `cluster` beats the value in `postgresql.conf` that the instance was created
with, which beats PostgreSQL's built-in default. These first three levels are rendered into one file per
instance (below); the database level is PostgreSQL's own and, for a session in that database, beats all of
them (it is a different mechanism with PostgreSQL's own precedence: session > role+database > role > database >
configuration file). "Server group" here is the group of servers of the contract (`serverGroup` of a server),
not a shard group.

A server that is not in any server group has no group-level value. Moving a server into or out of a group
changes its effective values; `updateServer` therefore starts a configuration task for that server (increment 4),
and `server.add` includes the configuration in its steps.

### Managed parameters

Below all four levels are the values `kingresd` itself needs. They are **managed**: shown read-only with the
reason, and a request to set one at any level is refused (`config_parameter_managed`, 422). The list is
`instance.ManagedParameters`, one table in `internal/instance/managed.go` next to the renderer. The renderer
is written from it: `RenderManaged` takes the names of its fixed block from the table, and
`TestManagedListIsComplete` renders a file with every optional field set and fails when a name appears in the
output that is neither in the table nor in the derived defaults, and when the table names something that no code
path writes (so adding a setting to the renderer without registering it, or the reverse, fails a test).
The list was derived from the code, not from memory:

| Parameter | Reason | Written by |
|---|---|---|
| `listen_addresses`, `port`, `unix_socket_directories` | the address and port the other servers and `kingresd` connect to; a wrong value cuts the server off | `RenderManaged` |
| `shared_preload_libraries` | the extensions the cluster runs on (`kingres_shard`, `kingres_ledger`); also refused (below) | `RenderManaged` |
| `ssl`, `ssl_cert_file`, `ssl_key_file`, `ssl_ca_file`, `ssl_crl_file`, `ssl_min_protocol_version` | the cluster's certificates and the TLS floor | `RenderManaged` |
| `kingres_shard.node_conninfo` | certificate options of node connections | `RenderManaged` |
| `password_encryption` | verifiers are copied between servers and must be SCRAM | `RenderManaged` |
| `logging_collector`, `log_directory`, `log_filename`, `log_truncate_on_rotation` | the log reader of the console (`LogDir`) | `RenderManaged`, local settings |
| `log_min_error_statement`, `log_statement`, `log_min_duration_statement`, `log_parameter_max_length`, `log_parameter_max_length_on_error` | statement text must never be written to a log (rule CP-03) | `RenderManaged` |
| `cluster_name` | process titles, logs | `RenderManaged` |
| `hot_standby`, `primary_conninfo`, `primary_slot_name` | a standby's connection to its primary | `standby/agent_ops.go` |
| `synchronous_standby_names` | maintained by the group's primary agent from the standbys that stream | `standby/agent_primary.go` |
| `synchronous_commit` | the durability of each database is `ALTER DATABASE ... SET synchronous_commit` (`ops/databases.go`); a second writer would fight with it | `ops` |
| `wal_level` | physical replication needs at least `replica`; the sharding extension's bucket moves need `logical` ("every node"); the renderer will write it when rebalancing is implemented, and until then the name is reserved so that nobody lowers it | reserved |
| `hba_file`, `ident_file`, `config_file`, `data_directory`, `external_pid_file` | file locations; `pg_hba.conf` and `pg_ident.conf` are generated | `forbiddenExtra` of the renderer |
| `include`, `include_dir`, `include_if_exists` | not parameters of `pg_settings`; refused as a *name* so that a value or a name can never become a directive | renderer, this phase |

**Derived defaults.** `max_connections`, `max_prepared_transactions`, `shared_buffers`,
`effective_cache_size` and `maintenance_work_mem` are values `kingresd` chooses when it creates an instance
(from the server's memory quota and the default connection count). They are not managed: an operator may set
them, and the renderer writes them **before** the include of the override file, so an override wins. A server's
memory quota (`updateThisServerSettings`) keeps deriving the defaults; the Configuration page shows the source of
an effective value (`default`, `derived from the memory quota`, an override level). Floors that protect
`kingresd`'s own needs are enforced on top of the type's range: `max_prepared_transactions` at least 1
(`kingres_shard` needs two-phase commit, README of the extension), `max_wal_senders` at least
`4 + replicaCount`, `max_replication_slots` at least `2 + replicaCount` (every standby holds a sender and a slot on
its primary, `pg_basebackup` takes one more of each while a standby is built). A value below its floor is refused
with `config_value_below_floor` and the reason.

### Refused parameters

Some parameters make the **operating system user of the database run a command, load code or name a file**:
`archive_command`, `archive_library`, `archive_cleanup_command`, `restore_command`, `recovery_end_command`,
`ssl_passphrase_command`, `shared_preload_libraries`, `session_preload_libraries`, `local_preload_libraries`,
`dynamic_library_path`, `extension_control_path`, `jit_provider`, `oauth_validator_libraries`,
`output_plugin_libraries`, `ssl_library`, `krb_server_keyfile`, `ssl_dh_params_file`, `ssl_ciphers`, `ssl_groups`,
`ssl_max_protocol_version` and the like.
Whoever may set one of them can run any program as the account that owns the data directory and its keys, which
is more than `kingres_admin` is meant to be (an administrator of the *cluster's* roles and settings, not of the
operating system). **The console never sets them.** They are refused at every level (`config_parameter_refused`,
422, with the reason) and are settable only on the server, by its administrator, in `postgresql.conf` (which is
outside what `kingresd` rewrites and is not read back by it).

The rule is PostgreSQL's own classification where PostgreSQL has one: the refused set is **every parameter
that PostgreSQL hides from a role without `pg_read_all_settings`** (found by the test of V9 at run time on the
running version and on every extension), plus the command and code names of V9 and the policy additions below. That choice fails
safe: a parameter that a later PostgreSQL version or an extension marks superuser-only is refused until someone
decides to allow it. `TestRefusedSetMatchesVersion` fails when the set PostgreSQL reports changes (a new one
needs a decision, a vanished one needs the table updated), and a table of reasons maps each known name to one
of `runsCommand`, `loadsCode`, `namesFile`, `connectionString`, `tlsMaterial`, `logSetting` so the console can say why.

Policy additions (not superuser-only, still refused):

- the category `Developer Options` of `pg_settings` (`allow_system_table_mods`, `ignore_checksum_failure`,
  `zero_damaged_pages`, ...): they exist to break safety checks;
- `fsync` and `full_page_writes`: with them off a crash can corrupt a server that the cluster believes holds
  acknowledged commits (the durability promise of ADR 0011);
- `kingres_shard.fault_injection`, `kingres_shard.forwarded`, `kingres_shard.forward_token`: test hooks and
  per-session internals of the sharding extension;
- parameters of context `internal` (cannot be set at all);
- (review P13R-01) the parameters that change how every session of `kingresd` itself behaves, because its service connections, also
  those to `kingres_system`, take the cluster's and the database's defaults: `default_transaction_read_only`,
  `transaction_read_only`, `default_transaction_isolation`, `transaction_isolation`, `session_replication_role`, `statement_timeout`,
  `lock_timeout`, `idle_in_transaction_session_timeout`, `idle_session_timeout`, `transaction_timeout`, `search_path`, `row_security`,
  `default_table_access_method`, `default_tablespace`, `temp_tablespaces` and `check_function_bodies` (reason code `controlPlaneSession`).
  A cluster-wide `default_transaction_read_only = on` had put the task engine, the alert updater and the standby code into read-only
  transactions with no way back from the console. They remain settable for one role on the server, outside the console;
- (review P13R-02) the parameters that PostgreSQL accepts in a file and then fails to start with when the host cannot honour them: `huge_pages`,
  `huge_page_size`, `shared_memory_type`, `dynamic_shared_memory_type`, `min_dynamic_shared_memory`, `bonjour`, `bonjour_name`, `event_source`
  (reason code `startRisk`), and **every parameter whose name starts with `ssl`** (a bad value or a missing file stops the postmaster or
  cuts `kingresd` off; the ones the cluster sets itself stay `managed`). `unix_socket_permissions` and `unix_socket_group` are managed
  with `unix_socket_directories`, and so are `log_destination` and the `syslog_*` parameters (another destination leaves the collector
  files, which the console reads, empty). `log_line_prefix` and `log_error_verbosity` stay settable: nothing of the console parses the
  line shape.

A **stored value whose name later becomes managed or refused** (an upgrade adds a name to a list) is never written and never blocks an
apply: the leader leaves it out of the request, the server leaves it out of the file with a warning, and its **removal** (`value: null`)
is accepted for any stored value whatever the parameter has become, also when its server group or database is gone (review P13R-05).

Both the managed and the refused rows are returned by the API with `kind: managed` or `kind: refused`, the
reason code and a sentence, so the console shows them read-only with the reason instead of hiding them.

### Cluster-only parameters

Some parameters must have the same value on every server for sharding to be correct. They can be set at cluster
level and at database level (a database level value is applied on every server, so it is the same everywhere),
not at server-group or server level (`config_level_not_allowed`, 422). The list is
`config.ClusterOnly` (one table with the reason of each entry, `internal/config/scope.go`), derived from the
extension (V12):

| Parameter | Why it must agree |
|---|---|
| `TimeZone`, `timezone_abbreviations`, `log_timezone` | A function evaluated on another node (`now()::timestamp`, `date_trunc`, `current_date`) gives another result when the zone differs; the extension does not force the zone on remote sessions. `log_timezone` is kept equal so that the logs of the cluster read as one |
| `lc_monetary`, `lc_numeric`, `lc_time`, `default_text_search_config`, `DateStyle`, `IntervalStyle`, `extra_float_digits`, `standard_conforming_strings`, `bytea_output`, `default_transaction_isolation`, `default_transaction_read_only`, `default_transaction_deferrable` | Formatting, parsing and defaults that change the result of a statement; the extension fixes some of them for transport but a function that formats on the node (`to_char`) or a default that a transaction takes on the node it lands on is not covered |
| `kingres_shard.default_bucket_count`, `kingres_shard.enable_routing`, `kingres_shard.enable_distributed_scan` | They decide how a statement is routed; a node that routes differently returns different rows (README of the extension, "Planning side effects") |
| `kingres_ledger.block_interval`, `kingres_ledger.max_block_transactions` | The ledger chain is cluster-wide; blocks closed by different rules make a chain that is hard to verify |
| `max_prepared_transactions` | Two-phase commit across nodes needs a usable value on every node (floor 1 on each) |

Settings that only tune one server (`work_mem`, `shared_buffers`, `max_connections`, `kingres_shard.node_timeout`,
`kingres_shard.scan_fetch_size`, `kingres_shard.move_*`, the autovacuum and checkpoint settings, ...) are
allowed at every level. Settings that cannot be set at all are not "equal" problems: the encoding, the collation,
its provider and library version, and the extension version are checked by `kingres.add_node` (README, "Nodes
must agree") and are outside this phase.

A parameter that is not on any list is allowed at the levels its `context` allows. The database level exists for
`user` and `superuser` contexts only (V6), so `log_timezone` or `shared_buffers` offer no database level, and
`synchronous_commit` is excluded from it as managed.

## Data model

Migration `0007_config.sql` (applied by `internal/sysdb`), all in `kingres_cp` of `kingres_system`:

```sql
-- The desired value of a parameter at one level. A row is removed when the level's value is removed.
CREATE TABLE kingres_cp.config_value (
    level       text   NOT NULL CHECK (level IN ('cluster','serverGroup','server','database')),
    scope_id    text   NOT NULL DEFAULT '',        -- '' for the cluster, else group, server id or database name
    name        text   NOT NULL,                   -- the parameter, lower case (PostgreSQL's canonical name is looked up on use)
    value       text   NOT NULL,                   -- the validated, normalized text; never a secret (refused parameters cannot be stored)
    revision    bigint NOT NULL,                   -- the configuration revision that wrote it
    updated_by  text   NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (level, scope_id, name),
    CHECK ((level = 'cluster') = (scope_id = '')),
    CHECK (length(name) BETWEEN 1 AND 63 AND length(value) <= 4096)
);

CREATE SEQUENCE kingres_cp.config_revision;        -- one number per accepted change; never reused

-- One row per changed (level, scope, name) per accepted change: the audit of what the console changed.
CREATE TABLE kingres_cp.config_change (
    change_id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    revision    bigint NOT NULL,
    task_id     bigint,                            -- the config.apply task (null until it is recorded)
    level       text   NOT NULL, scope_id text NOT NULL DEFAULT '', name text NOT NULL,
    old_value   text,                              -- null: there was no value at this level
    new_value   text,                              -- null: the value at this level was removed
    changed_by  text   NOT NULL,
    changed_at  timestamptz NOT NULL DEFAULT now(),
    reverts     bigint REFERENCES kingres_cp.config_change (change_id),
    UNIQUE (task_id, level, scope_id, name)        -- a resumed step cannot record a change twice
);
CREATE INDEX config_change_name ON kingres_cp.config_change (name, change_id DESC);

-- What each server reported at its last apply or check: the five states of the page.
CREATE TABLE kingres_cp.config_server_state (
    server_id        text PRIMARY KEY,
    applied_revision bigint NOT NULL DEFAULT 0,
    state            text   NOT NULL CHECK (state IN ('applied','pendingRestart','failed','waiting')),
    detail           jsonb  NOT NULL DEFAULT '{}',  -- per parameter: state and PostgreSQL's error text
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- The clock difference threshold is a cluster setting (kingres_cp.cluster_setting, key 'clock').
```

Why a table of desired values and not "the file on each server is the truth": the console must show the wanted
state of servers that are down, it must apply to a server that joins later or changes group, it needs the old
value for history and revert, and it needs a revision to say which servers are behind. The file on a server is a
**rendering** of the table for that server at a revision, and its first line says which
(`# kingresd configuration revision N`). Hand edits of the file are overwritten on the next apply and the
README says so; a drift between file and table is shown (the server reports the revision it holds).

Values are stored as the normalized text the validator returns, so the audit shows exactly what was written.
Nothing in the tables can be a secret: refused parameters cannot be stored, and the rest of the parameters are
values PostgreSQL itself shows to every database user (V9, section "Security").

## The override file

Each instance of a server (instance 1 and every standby) gets the **same** file `kingres-config.conf` in its
data directory, rendered from the table for that server. The managed file includes it:

```
# Generated by kingresd. Do not edit: changes are overwritten.
<derived defaults: shared_buffers, effective_cache_size, maintenance_work_mem, max_connections,
 max_prepared_transactions, as before>
include_if_exists 'kingres-config.conf'
<fixed managed block, as before>
```

**Why not `ALTER SYSTEM`.** It writes `postgresql.auto.conf`, which is read after `postgresql.conf` and after
every file included from it, so it would be overridden by or fight with the managed file depending on which key
is where, it is per instance (not per server), it can be used by any superuser connection (the workbench runs as
the caller's role, which is no superuser, but the service role is), and it validates and writes in one step
without a revision. A file of our own, included at a place we choose (V4), is the one writer, is rendered from the
table, is replaced atomically and can be checked before it is loaded (V2).

**Why not text substitution** in `postgresql.conf`: nothing is ever edited in place. `postgresql.conf` stays what
`initdb` wrote plus the existing include of the managed file; `RenderManaged` places the second include, and the
file `kingres-config.conf` is always written whole.

**Rendering.** One line per parameter, sorted by name: `name = 'value'`, with the value quoted by `confValue`
(backslash and single quote doubled; NUL, CR, LF and invalid UTF-8 refused). The name is checked against
`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$` (and must exist in `pg_settings`, or in the extension's placeholder
prefix) so a name cannot carry a space, a quote, an `=` or a newline. The file's lines are generated, never
assembled from text of the request, so a value such as `x'\ninclude '/etc/passwd` is a refused value (newline) and
a value `x' include '/etc/passwd` is the quoted string `'x'' include ''/etc/passwd'`, one setting. Tests render
hostile values (quotes, backslashes, `#`, trailing backslash, non-ASCII, 4096 bytes, an `include` text) and read
the result back with `pg_file_settings` on a real instance: exactly one row per name and the value round-trips.

**Atomic write.** `writeAtomic` (temporary file in the same directory, mode 0600, rename). Before replacing, the
current file is copied to `kingres-config.conf.prev` (also atomically) so that a failed check restores it. The
file mode is 0600 like the managed file.

## Validation

A request is checked in four layers, cheapest first, and **nothing is written before every change in the request
has passed all that apply**:

1. **Names and levels** (no PostgreSQL needed): the name syntax; the level exists; the scope id exists (a server
   group, a server of the cluster, a database); managed and refused parameters are refused; cluster-only
   parameters at server-group or server level are refused; the database level only for `user` and `superuser`
   contexts.
2. **Type, unit and range from `pg_settings`** (`vartype`, `unit`, `min_val`, `max_val`, `enumvals`, `context`,
   read from the instance that answers, refreshed when its PostgreSQL version or the loaded libraries change).
   The checker implements a **narrow subset** of PostgreSQL's syntax (V8) so that what it accepts has one meaning:
   a boolean is one of `on`, `off`, `true`, `false`, `yes`, `no`, `1`, `0` (any case; normalized to `on`/`off`); an
   integer is a decimal number with an optional sign, no leading zero (except `0`), no hexadecimal, octal or
   exponent form, and for a parameter that has a unit an optional fraction and an optional unit with optional
   space (`kB`, `MB`, `GB`, `TB`, `B` for sizes; `us`, `ms`, `s`, `min`, `h`, `d` for times); the value is
   converted to the parameter's base unit (`pg_settings.unit`, which may be `8kB`), must be an exact integer there
   (no silent rounding) and lie within `min_val`..`max_val`; a real is a decimal with optional sign and fraction
   within the range; an enum value must match `enumvals` ignoring case, or be accepted by layer 3; a string must
   be valid UTF-8 of at most 4096 bytes without NUL, CR or LF. Anything else is `config_value_invalid` with a JSON
   pointer and a message that names the type, the unit and the range.
3. **PostgreSQL's own check** for what layer 2 cannot know (check hooks: time zone names, `wal_level`, `search_path`
   syntax, enum aliases): for `user` and `superuser` parameters, `SELECT set_config($1, $2, true)` in a transaction
   that is rolled back, on the answering server's instance (V6); for other parameters of PostgreSQL itself,
   `postgres -C <name> -c <name>=<value> -D <data>` run without a shell and with the value as its own argument
   (V5), with a 10-second limit; for the `sighup`/`postmaster` parameters of an extension there is no safe dry run
   (V5) and layer 2 is the check, followed by layer 4. PostgreSQL's message is passed on in the problem, cleaned
   of anything but the first line.
4. **The file is checked by PostgreSQL before it is loaded** (V2): see the apply protocol. This also covers a
   server whose build differs from the answering server's.

Layers 1 to 3 run on the leader when the request is accepted (`previewConfigChange` runs exactly the same code
and changes nothing) and **again** by each server for its own file in step 3 of the protocol. `TestValidatorMatchesPostgreSQL`
is a differential test on a real instance: for every parameter type and unit, several hundred accepted and refused
values are given to the Go checker and to PostgreSQL (`set_config` or `postgres -C`), and an accepted value must
produce the same base-unit value as PostgreSQL's `setting`, and a value the Go checker refuses is not required to be
refused by PostgreSQL (the subset is narrower on purpose).

Cross-field rules, checked in layer 1 against the effective values the change would produce: for the five
settings of V11 a standby's value must not be below its primary's at the time of a restart, which is a property of
the **order** of restarts and not of the stored values (below); `max_wal_senders`, `max_replication_slots` and
`max_prepared_transactions` floors (above); `max_connections` plus the reserved connections must stay at least
`superuser_reserved_connections + reserved_connections + 1`, which PostgreSQL checks itself at start (the preview
reads it from layer 3 and warns, it is not a hand-written table).

**As built (contract 0.17.0):** the warning is `config_reserved_connections` in the new `warnings` of the preview. `postgres -C` does not
check this rule (PostgreSQL checks it at the start of the postmaster, after the dry run), and `pg_file_settings` does not report it, so there
is no layer-3 reading to use. The leader computes the sum from the values the change gives (the stored values that decide on each server, else
the running ones) and warns for the first server where `max_connections < superuser_reserved_connections + reserved_connections + 1`. It is a
warning and not a problem because the file is valid and the instance runs until its next start; `valid` stays a function of `problems` only.

## Applying a change

The task kind is `config.apply`, the subject is `{type: config, id: cluster}` (one unfinished configuration task
at a time; a second request while one runs is 409 `operation_in_progress`). It is a task of the manager leader,
so the contract's rule "a mutating cluster operation received by another server is forwarded to the leader"
covers it.

The steps are named, idempotent and resumable (the task engine's conventions):

| Step | What it does | Failure |
|---|---|---|
| `Validate the change` | layers 1 to 3 again against the current state (the state may have moved since the preview) | the task fails, nothing was written; error code `validation_failed` and the problem text |
| `Record revision N` | in one transaction: take the next value of `config_revision`, write the `config_value` rows, insert `config_change` rows (old and new value, level, scope, who) with the task id, set the cluster's desired revision. Idempotent through the unique key on the change rows | the transaction rolls back and the step is retried by the engine; a task that fails here changed nothing |
| `Apply on server <id>` (one step per active server, the leader's own server first, then the others in server-id order) | `POST /internal/v1/config/apply` with the server's complete rendered override set at revision N and the database values (below) | see "Failure cases" |
| `Confirm` | reads the per-server state recorded by the steps, writes the final `config_change` states, and ends the task: `done` when every server is applied or pending restart; `failed` with code `config_incomplete` otherwise | n/a |

The internal call on the server (`instance.ConfigApply`), for each of its instances (instance 1, then standbys),
in this order:

1. **Re-validate** every value against this instance's own `pg_settings` (layers 1 to 3 restricted to what the
   server knows). A server on another build that rejects a value fails here with nothing written.
2. **Render** the file in memory and compare with the current file: when equal, go to 6 (the revision is
   recorded and nothing is reloaded).
3. **Write** `kingres-config.conf.prev` from the current file, then `kingres-config.conf.new`, then rename it over
   `kingres-config.conf`, with the managed file's `include_if_exists` already in place (`EnsureConfigInclude` rewrites
   the managed file from the stored settings once for instances created before this version; the layout changes,
   the effective values do not).
4. **Check before loading** (V2): `SELECT name, setting, applied, error FROM pg_file_settings WHERE sourcefile =
   <the file>`. A row with an `error` other than `setting could not be applied` makes the step restore the previous
   file and fail with PostgreSQL's text; the instance never reloads a bad file. An instance that is not running
   gets the file and the step checks it with `postgres -C` per parameter instead and reports `applied` once the
   file is written (it will read the file when it starts).
5. **Reload** (`pg_ctl reload`, as for network rules), after recording `pg_conf_load_time()`; wait up to 15 seconds
   for it to move (V3).
6. **Read back** `pg_settings` for every name in the file and every name that was in the previous file (a removed
   override must show the lower level's value): for each, `setting`, `source`, `sourcefile`, `pending_restart`. The
   state per parameter is `applied` (the running value is the wanted value), `pendingRestart` (`pending_restart`
   is true, the running value is the old one), or `failed` with PostgreSQL's text. A parameter whose running value
   differs without a pending restart is `failed` ("PostgreSQL did not take the value").
7. **Database values.** On the instance that holds the primary of the server's own shard group (instance 1): for
   each database, `ALTER DATABASE <db> SET <name> TO <value>` for each stored value and `RESET` for each that was
   removed, built with the statement builder (`sqlx`) so that identifiers are quoted and the value is a literal;
   the check is PostgreSQL's own (V6), a refusal fails the parameter with its message. A standby needs no call
   (V10). Idempotent: `SET` again changes nothing.

The server answers `{state, parameters: [{name, state, runningValue, error}]}`; the step stores it in
`config_server_state` and in the task's per-step detail, which the console shows.

### Failure cases

- **Step fails on one server.** The server restored its file (step 4) or reports the parameter as failed. The
  task **continues with the other servers**: the desired state is recorded (step 2), the cluster is to converge,
  and stopping would leave an arbitrary subset on the new value without anyone choosing which. The failed
  server's state is `failed` with the reason; the task ends `failed` with `config_incomplete` and the detail "applied
  on 10.0.0.11, 10.0.0.13; failed on 10.0.0.12: invalid value for parameter ...". Nothing is hidden: the page lists
  the server as failed and the **Retry** action sends the same internal call again (it is idempotent), and the
  operator may instead revert the change, which is a new change.
- **Server is down or unreachable.** *As built after the review (P13R-03):* the step tries once, records the server as
  `waiting` (`config_server_state`), says so in the task detail and **goes on to the next server**; the task ends `done` and no
  later change is kept out. The leader's convergence loop (every 30 seconds, `converge.go`) compares the hash of the file a
  server holds with the one the stored values give and brings the server up to date within a minute of its return. The
  design first said that the step waits and that the task never gives up (as `replicate.apply`); the review showed that a stopped
  server in the middle then kept the servers after it from the change and every later change (409
  `operation_in_progress`) out for as long as it was down. A server that is **stopped by the operator** shows `waiting` too.
- **Instance of a server is stopped but the server is reachable.** The file is written (step 4's `postgres -C`
  check) and the state is `applied` with the detail "instance not running: read at start".
- **Crash of `kingresd` between write and reload.** The file is already the wanted one; the resumed step finds it
  equal (step 2) and goes to reload and read back.
- **Leader loses its lease during the task.** The engine's fencing epoch protects the recorded state; the new
  leader resumes at the first step that is not done.
- **Reload of a good file is rejected anyway** (a syntax error from outside, a managed-file error). The readback
  sees that `pg_conf_load_time()` did not move and `pg_file_settings` shows the error: the step restores the
  previous override file and reports it.
- **Two kinds of "difference"** are shown apart: a server that is *behind* (revision lower, state `waiting` or
  `failed`) and a server whose value *differs by design* (a server-level override).

### Reverting

`revertConfigChange` takes a row of `config_change` and writes **a new change**: the value at that level and scope
goes back to `old_value` (or is removed, when `old_value` was null), with `reverts` pointing to the original, and
the same validation, preview and task. It is refused with 409 `config_change_superseded` when a later change
touched the same level, scope and name (the page offers "revert the latest change" and shows the later ones), and
with 422 when the old value is no longer valid (a parameter's range depends on the version).

### The audit log

Every accepted change is audited under the operation id (`applyConfigChange`, `revertConfigChange`,
`restartPendingInstances`, added to the closed enum `AuditAction`), a denied or failed call too. `details` of
`applyConfigChange` is `{basedOnRevision, changes: [{name, level, scopeId, oldValue, newValue}], servers, taskId}`
and the resource is `config` (review P13R-12: the revision the change gets is allocated by the task after the audit event is
written, so the event carries the revision it was checked against and the task id, whose `config_change` rows have the revision;
`newValue` is the normalized text that is written, and `revertConfigChange` carries `reverts` and the same fields). A request that
is refused is audited too, with `attempted: [{name, level, scopeId, remove}]` and **never a value**. Values are those that may be shown to every database
user (section "Security"); a value of a refused or hidden parameter can never be in a change, so none can be in
the audit. The history that the page lists (`listConfigChanges`) is `config_change`: it carries the same data and
the link to the revert.

## Restart

A parameter of context `postmaster` is **never applied by restarting on its own**. After the apply it is
`pendingRestart` on every server it concerns, and shown so on the page, in the task detail, on the server's page and
on the dashboard. An operator restarts with the operation that exists (`restartThisServerInstance`, per instance, `kingres_ops`) or with the cluster task
`restartPendingInstances` (`POST /config/restarts`, `kingres_admin`, task kind `config.restart`, subject `{config, restart}`), which restarts the
instances that wait for a restart, **one server at a time**, and stops at the first failure:

1. The request names the instances to restart (default: all that wait) and must carry `acknowledgeDowntime: true`;
   without it the answer is 400. The dialog says: *a primary that restarts is downtime for its shard group: writes
   to its buckets fail until it is back, typically a few seconds, longer after a crash or with a long recovery.
   Failover is not available yet, so nothing takes over.*
2. **Order.** For every standby/primary pair the task reads the **running** value of the five settings of V11
   and the value the restart will bring (the file). If any is going **up**, the standby is restarted before its
   primary (a standby never runs below its primary's *old or new* value: it is raised first, and a primary
   restarted afterwards is at most equal). If any is going **down**, the primary is restarted first. If one pair
   has a setting going up and another going down, no order exists that keeps a standby from being at or below its
   primary during the window: the preview and the task refuse with `config_restart_order_conflict` (409) and say
   which settings, and the operator changes one direction at a time. Pairs are independent, so the order is by
   server: within the restart of one server, its standbys and its primary follow the pair rule against the
   instances on the other servers, and the task waits after each instance until the instance accepts
   connections and (for a standby) streams again, up to 120 seconds, before the next.
3. Each instance is stopped and started with the existing instance operations (`instance.stop`, `instance.start`
   semantics; the restart of the instance that holds the system database follows the existing refusal
   (409) unless it is the only way, and says so). *As built after the review (P13R-06, P13R-02):* the instance that holds the system
   database goes through the lifecycle of the operations code (`Ops.BeginConfigRestart`: the state "restarting", the refusal of a start
   or stop meanwhile, the wait for the system database), every instance through the manager's lease gate, and the pair order uses the
   primary of each group as the leader last saw it (`group_replication_state`), not instance 1 of the server the standby was created
   for. Before the instance is stopped, PostgreSQL's parser checks the whole configuration (`postgres -C`), and a refused file stops
   the restart. When the instance cannot start with the new file (a memory size the host cannot give), the last file it ran with
   (`kingres-config.conf.good`) is put back, the instance is started again and the task fails with that text; the stored value stays, and
   the operator removes or reverts it.
4. After each instance the step reads `pending_restart` again and records the state.

A `config.restart` task is not started while a `config.apply` task is unfinished (409).

## Time

### What is shown

Per server: **its clock** (UTC, millisecond resolution), the **operating system's time zone**, PostgreSQL's
`TimeZone` and `log_timezone` (from `pg_settings`), the **difference** between the server's clock and the
answering server's, with its **uncertainty**, and when it was measured.

The OS time zone is read by `kingresd` from the `TZ` environment variable, else the target of `/etc/localtime`
(the path below `zoneinfo/`), else `/etc/timezone`; it is `null` when none can be read (it is not a
secret and not used for decisions).

### How the difference is measured

The answering server (A) asks every other server (S) over the internal API `GET /internal/v1/time` (any peer; no
state, no secrets): S answers with its wall clock `t_s` read as late as possible. A records its monotonic clock
before sending (`m0`) and after the answer (`m3`) and its own wall clock at `m0` and `m3` (`w0`, `w3`). The
estimated offset is `t_s - (w0 + w3) / 2` and the round trip is `m3 - m0` measured with the **monotonic** clock
(`internal/clock`: a step of the wall clock during the measurement cannot disturb the round trip). The offset
is exact when the two network legs take equally long; it is wrong by at most half the round trip, so the stated
**uncertainty is `+/-(round trip / 2 + 1 ms)`**. Five probes are made one after the other (200 ms apart at most) and
the one with the smallest round trip is used (the sample with least delay has the least uncertainty). Clocks are
shown as `+1.82 s +/- 0.4 ms`; a difference smaller than its uncertainty is shown as "within the measurement". The
answering server's own difference is 0 by definition.

A server that does not answer within `gather-timeout` is shown as unreachable with no difference (never as 0).

### The alert

The alert kind `clockDifference` (contract 0.8.0) is raised by the manager leader (monitor, every
`monitor-interval`) for a server whose difference from the leader, **reduced by its uncertainty**, is above the
cluster setting `clock.maxDifferenceMs` (default 1000, 10 to 600000); `warning` above the threshold, `critical` above ten
times it; it clears when the server is back within the threshold. The subject is `server:<id>`, the message says
what to do. The reduction by the uncertainty means a slow network can never raise it by itself. Why one second:
the control plane's decisions use monotonic time and do not depend on it; what suffers is the readability of logs
and the ledger's timestamps, the validity of certificates at their edges, and `now()` agreeing across the nodes
of a distributed transaction, for which a second is already a lot.

### What to do when the clocks differ (text of the console and the README)

`kingresd` does not set the clock. Run time synchronization on every server: with chrony, `chronyc tracking` and
`chronyc sources`; with `systemd-timesyncd`, `timedatectl timesync-status`; check that the servers use the same
sources and are allowed to reach them, and that a virtual machine does not also take its clock from the host.
A clock that is far off is stepped by the synchronization service (chrony `makestep`, or restart of the service),
not by hand with `date`, which leaves the service fighting the change. After the fix the difference shrinks within
a few monitor intervals and the alert clears by itself.

### The time zones

`TimeZone` and `log_timezone` are set at **cluster** level (they are cluster-only), `TimeZone` also **per
database**. The Time tab edits them through the same change flow (a cluster-level change of `TimeZone` is a
`config.apply`; a database-level change likewise). The zone is chosen from `listTimeZones`
(`GET /config/time-zones`: `pg_timezone_names` of the answering server, name, current UTC offset, whether daylight
saving time is in effect) and validated by PostgreSQL (layer 3). The server's own `TimeZone` in `postgresql.conf`
(written by `initdb` from the machine's zone) is the default below all levels, which is why the page shows
"set by initdb / not set by the console" as the source. A change of `TimeZone` does not need a restart.

## REST operations (contract 0.8.0)

A new tag `Configuration`. All answers are `application/json` and every operation declares `401`, `403`, `405`,
`422`, `503` like its neighbours.

| operationId | Method and path | Role | Answer |
|---|---|---|---|
| `listConfigParameters` | `GET /config/parameters` | `kingres_readonly` | `ConfigParameterList` (`items`, `nextCursor`, `revision`, `partial`, `missingServers`). Query: `q` (name and description), `category`, `changed` (has an override), `differing` (values differ between servers), `pendingRestart`, `kind` (`settable`, `managed`, `refused`), `limit` (1 to 500, default 50 like the other lists), `cursor`. Keyset by name |
| `getConfigParameter` | `GET /config/parameters/{name}` | `kingres_readonly` | `ConfigParameter` (404 `config_parameter_unknown`) |
| `previewConfigChange` | `POST /config/changes/preview` | `kingres_admin` | `ConfigChangePreview`: `valid`, `problems[]` (per change: pointer, code, message), `effects[]` (per server and parameter: before, after, how it takes effect: `unchanged`, `reload`, `restart`, `waits` for a server that is down), `restartServerIds`, `restartOrder[]` and `restartOrderConflict`, and (0.17.0) the optional `warnings[]` (`index`, `pointer`, `code`, `message`: `config_value_unusual`, `config_reserved_connections`; never a reason for `valid` to be false). Status 200 also when `valid` is false (it is a question, not a request). Admin because it takes the same body as the change and reads nothing a readonly role needs; the console shows it to admins only |
| `applyConfigChange` | `POST /config/changes` | `kingres_admin` | 202 `Task` (kind `config.apply`) with `Location`; 400 `validation_failed` / `config_value_invalid`, 422 `config_parameter_managed`, `config_parameter_refused`, `config_level_not_allowed`, `config_value_below_floor`, 404 `not_found` for an unknown server group, server or database, 409 `operation_in_progress` |
| `listConfigChanges` | `GET /config/changes` | `kingres_readonly` | `ConfigChangeList` (history, newest first, keyset by `changeId`); query `name`, `level`, `scopeId`, `limit`, `cursor` |
| `revertConfigChange` | `POST /config/changes/{changeId}/revert` | `kingres_admin` | 202 `Task`; 404 unknown change, 409 `config_change_superseded` |
| `listConfigServers` | `GET /config/servers` | `kingres_readonly` | `ConfigServerList`: per server its server group, `appliedRevision`, `state` (`applied`, `pendingRestart`, `failed`, `waiting`), the names that wait for a restart, the failed ones with PostgreSQL's text, and the number of overrides that concern it; `revision` |
| `getConfigSummary` | `GET /config/summary` | `kingres_readonly` | the dashboard's numbers: servers pending a restart, parameters that differ inside a server group (and should not), failed and waiting servers, largest clock difference with its server; each with the filter the page links to |
| `getClusterTime` | `GET /config/time` | `kingres_readonly` | `ClusterTime`: `measuredAt`, `maxDifferenceMs` (the threshold), `servers[]` (`serverId`, `clock`, `differenceMs`, `uncertaintyMs`, `roundTripMs`, `osTimeZone`, `timeZone`, `logTimeZone`, `reachable`) and `databases[]` (`name`, `timeZone` or null) |
| `listTimeZones` | `GET /config/time-zones` | `kingres_readonly` | `TimeZoneList` (`name`, `utcOffset`, `isDaylightSaving`), not paginated (about 600 names) |
| `restartPendingInstances` | `POST /config/restarts` | `kingres_admin` | 202 `Task` (kind `config.restart`); 400 without `acknowledgeDowntime`, 409 `nothing_pending`, `config_restart_order_conflict`, `operation_in_progress` |

Additions to existing schemas (all additive): `TaskKind` gets `config.apply` and `config.restart`; `AuditAction` gets
`applyConfigChange`, `revertConfigChange`, `restartPendingInstances`; `AlertKind` gets `clockDifference` (with its
subject at `AlertSubject`); `ClusterSettings` and its update request get `clock: {maxDifferenceMs}` (default 1000). The
README of the contract gets the rows of "Task kinds and subjects" and the rule that a value is never returned for a
hidden parameter. Old clients ignore what they do not know (the contract's rule), and the console treats a server
that answers 404 `unsupported_operation` for these as "not available on this server version yet".

The role of every operation was chosen like its neighbours: reading follows `getClusterSettings`
(`kingres_readonly`), changing follows `updateClusterSettings` (`kingres_admin`). Restarting instances one by one
stays `kingres_ops` through the existing `restartThisServerInstance`; the new cluster-wide restart task is admin
because it is a decision about the whole cluster's availability.

### Schemas of the central types

`ConfigParameter`: `name`, `category` (PostgreSQL's, as given), `description` (`short_desc`), `type`
(`bool|integer|real|string|enum`), `unit` (null when none), `min`, `max` (strings, null), `enumValues` (null),
`context` (`postmaster|sighup|superuser|user|backend|superuser-backend|internal`), `restartRequired` (`context ==
postmaster`), `defaultValue`, `kind` (`settable|managed|refused`), `reason` (code and sentence, for the other two
kinds), `hidden` (the value is never returned), `allowedLevels` (subset of `cluster`, `serverGroup`, `server`,
`database`), `clusterOnly` with its reason, `overrides[]` (`level`, `scopeId`, `value`, `revision`, `updatedBy`,
`updatedAt`), `servers[]` (`serverId`, `value` effective now, `source` one of `default`, `initdb`, `derived`, `cluster`,
`serverGroup`, `server`, `managed`, `other`, `state` of the server for this parameter, `pendingValue` when a
restart is pending, `error`), `differs` (the effective values differ between servers) and `differsInGroup`.

The effective value of a server is **read from the server** (`pg_settings.setting`), not computed; the level is
what the stored overrides say it should be, and `state` says when the two disagree (not yet applied, failed,
waiting). `source: other` is a value from `postgresql.conf`, the command line or the environment that no
override explains. A server that cannot be read is `unknown` and the answer is `partial` with `missingServers`
(the contract's convention from 0.5.0).

### Internal API (internal-api.md, internal protocol version stays 1: new calls only)

| Call | Policy | Meaning |
|---|---|---|
| `GET /internal/v1/time` | any peer | this server's wall clock (RFC 3339, nanoseconds) and nothing else |
| `GET /internal/v1/config/state` | leader | every parameter's effective value, source file and line, `pending_restart`, errors of `pg_file_settings`, the running `PostmasterToken` of each instance, the revision of the file, OS time zone; the metadata of `pg_settings` (once, from the answering server) |
| `POST /internal/v1/config/apply` | leader | the rendered override set of the server at a revision, and the database values; answers per parameter (above). Parameters are structured values, never SQL or paths; the call refuses names on the managed and refused lists even if the leader asked |
| `POST /internal/v1/config/check` | leader | validation of one value on this server (layer 3) |

## The console

The page `Configuration` (route `/configuration`) is in the Cluster navigation after Settings. Every text from
the server (parameter names, categories, descriptions, values, enum values, error texts of PostgreSQL, server ids,
group and database names) goes through `SafeName`/`safeText`; a value can be hostile (it is supplied by a role
that may set an extension's placeholder parameter, or by an administrator), so a value is never `v-html`, never used as a
label for or an `id`, never a URL, and a value's bidirectional and zero-width characters are made visible by the
existing component. All colours are tokens; every state has a text and an icon, never colour alone.

### Parameters tab

- A toolbar with a **search** box (matches name and description, debounced, `role=search`), a **category** select
  (PostgreSQL's top-level categories), and three toggles in a `fieldset`: *Only changed*, *Only differing between
  servers*, *Only pending restart*; a toggle for *Show managed and refused*. The filters are in the URL
  (`?q=&category=&changed=1&differing=1&pendingRestart=1`) so the dashboard can link to them.
- A `TableWrap` table, one row per parameter, columns: name (with a chip for `restart` when `context` is
  `postmaster`), the value per server (a compact cell per server: the value with its unit, a small label of the level
  it comes from, a state badge when not `applied`), default, range (`min` to `max`, unit, enum values in the row's details), and an **Edit**
  button. At narrow widths the row becomes a card with the same content, and the per-server cells stack. Rows that
  differ between servers carry the text "differs" with an icon. Managed and refused rows show the reason and no
  Edit button (a button that cannot work is not shown; the reason is).
- Paging by cursor ("Load more"), with a live region announcing the count.
- States: loading skeleton, empty result of a filter (with "clear filters"), error (`ProblemNotice`),
  partial (`PartialNotice` naming the servers that could not be read), unsupported (a calm notice), read-only for
  `kingres_readonly` and `kingres_stats` cannot reach it (role gate through `GateButton` and the router).

### Edit dialog (`AppDialog`)

1. **Level**: radio group *Cluster*, *Server group*, *Server*, *Database*, each with a select for its scope when
   needed; levels that the parameter does not allow are disabled with the reason ("This setting must be equal on
   every server, so it is set for the cluster."); the current override at that level is shown, with **Remove this
   override**.
2. **Value**: a control by type (switch-like select for `bool`, select for `enum`, number with a unit select for
   `integer` and `real` with the unit the server reports, text for `string`), with the range and default beside it.
   The value is **checked as it is typed** by the same subset as the server (a shared module whose test vectors are
   the server's differential test cases), and `aria-live` says what is wrong; the server's preview confirms.
3. **Preview** (debounced call of `previewConfigChange`): a table of the servers that change (before, after, how:
   *takes effect after reload*, *waits for a restart*, *no change*, *server is down: will wait*), with the text
   *"N of M servers will change. K need a restart; nothing is restarted by this change."* If a restart will be
   needed, the order and the downtime note are listed.
4. **Apply** (disabled until the preview is valid; `GateButton` for the role), then the dialog shows
   `TaskProgress` and, per server, its state *applied*, *pending restart*, *failed* (with PostgreSQL's text) or
   *waiting for the server*; a failed server has **Retry**. The dialog can be closed while the task runs; the
   Configuration page keeps showing the task banner.

### Changes tab (history and revert)

A list of changes of one parameter (the parameter is chosen from the Parameters tab or by search), newest first:
time, who, level and scope, old value, new value, task state, the revision; **Revert** on the latest change of a
level and scope, which opens the same dialog prefilled with the previous value (a new change, previewed like any
other). A change that was already superseded explains why its Revert is not offered.

### Time tab

- **Servers table** (`TableWrap`): server, clock (updated by a live refresh every 10 s with a visible time of the last measurement), difference with its
  uncertainty and a state badge (*in sync*, *differs*, *far off*, *cannot be read*), OS time zone, PostgreSQL's
  `TimeZone`, `log_timezone`. A server whose zones differ from the others' is marked.
- **What to do** panel, always visible when a difference exceeds the threshold: the text of "What to do when the
  clocks differ", with the statement that KingresDB does not set the system clock and why.
- **Cluster time zone**: a form row (`form-row`) with a searchable zone select (`listTimeZones`) and an *Apply*
  button that opens the same preview and apply dialog for `TimeZone` and `log_timezone` together.
- **Databases**: a table of databases with their time zone (or "cluster default") and *Change* per database.
- **Threshold**: the cluster setting `clock.maxDifferenceMs` (admin).

### Dashboard card

One card *Configuration* with three counts, each a link to the page filtered accordingly: servers waiting for a
restart (`/configuration?pendingRestart=1`), parameters that differ inside a server group that should not
(`/configuration?differing=1`), the largest clock difference with the server (`/configuration/time`); and, when
nonzero, the failed and waiting servers (`/configuration?state=failed`). Every count has a text, an icon and an
accessible name; a zero is shown as "none" (a state, not a blank); the card is a region with a heading, and
unavailable data is "not available" (never 0).

### Server detail

A section *Configuration* on the server's page: its group, the revision it holds, the overrides that apply to it
(level, name, value), the parameters waiting for a restart with the restart button for this server's instances
(the existing operation, with the downtime note), the failed ones with the text, and the clock and zones.

### Mock and URL switches

The mock implements the whole feature with 300 generated parameters in PostgreSQL's categories (types, units,
ranges, enum values; a few `postmaster` ones; extension parameters of `kingres_shard` and `kingres_ledger`),
`shared_buffers` pending a restart on one server, `work_mem` overridden on one server so that it differs, a clock
that is off on one server and the time zone `Asia/Tehran`. Switches (mock only): `?config=failed:<server>`,
`?config=waiting:<server>`, `?config=restart-conflict`, `?clock=off:<server>:<ms>`, `?clock=unreachable:<server>`, `?mock=hostile`
(hostile parameter names, values, descriptions and error texts), `?unsupported=listConfigParameters,...`, and
`window.__kingresMock` helpers for the browser tests.

## Security

- **No secret is readable through the console.** A value is returned only if PostgreSQL would show it to an
  ordinary database user: the superuser-only parameters (V9) are `hidden`, their values are **never** returned to any role,
  not even `kingres_admin` and not in the audit log. This covers `archive_command`, `ssl_passphrase_command`,
  `primary_conninfo` (a connection string that may carry a password), `kingres_shard.node_conninfo` and the file
  names of keys. The list is PostgreSQL's, so it follows the version and extensions.
- **No code execution through the console**: refused parameters (above) are not stored, rendered or applied, and
  the internal call checks the list again: a compromised leader cannot make a server write one.
- **Injection**: the file is generated from validated names and quoted values (rendering above); database
  values reach SQL only through `sqlx` as literals; the validation commands are run without a shell with the value
  as one argument; names are matched against a strict pattern and against `pg_settings`.
- **Resource limits**: a request carries at most 100 changes; a value at most 4096 bytes; the preview is rate
  limited by the guard's usual limits; `postgres -C` runs one at a time with a 10-second limit.
- **Denial of service by settings**: floors protect `kingresd`'s own needs (above); everything else is the
  administrator's responsibility and the preview says when a value is far outside the default (a value more than
  ten times the default of a memory parameter, or below a tenth, is flagged "unusual", not refused). Built in contract 0.17.0 as
  the warning `config_value_unusual`: exactly ten times and exactly a tenth are not flagged, a parameter that is not a memory size
  (unit `B`, `kB` or `8kB`) is never flagged, and nor is a default of zero or a removal of a value.
- **Roles**: the read operations are `kingres_readonly` like the other readers; a `kingres_stats` account
  cannot reach them; the changes are `kingres_admin`; every change is audited with old and new value.
- **The managed file keeps priority**: a configuration override can never beat a managed setting, because the
  managed block follows the include and the names are refused anyway.
- **The internal API** accepts the new calls from the leader only (apart from the time probe), as for `/apply`.

## Tests

Unit tests of `kingresd` (no database): precedence of the four levels (a table of cases); every managed name is
refused at every level, from the table, with `TestManagedListIsComplete` against the renderer; the refused set,
by name and by `GUC_SUPERUSER_ONLY` (`TestRefusedSetMatchesVersion` needs a PostgreSQL and is an integration test; the
unit test checks the table and the reasons); validation of every `pg_settings` type with units, ranges, enum and
edge forms (hexadecimal, octal, exponent, leading zero, fractions, rounding, overflow, empty, whitespace);
cluster-only parameters refused at group and server level and allowed at cluster and database level; the floors;
the override file's rendering and quoting with hostile values (quotes, newlines, backslashes, `#`, trailing
backslash, non-ASCII, `include` text, a name with a space or `=`) and the guarantee that a value cannot add a second
setting or a directive; `writeAtomic` (the file is never partly written, the previous copy is kept); the restart
order for the five settings (up, down, mixed, equal; one and several standbys); the clock arithmetic (offset,
uncertainty, the minimum-delay sample, a stepped wall clock during the probe, the alert threshold reduced by the
uncertainty); contract tests of the new operations and responses (`contracttest`).

Integration tests on a cluster of three servers of the test's own: a cluster-level change reaches every server
(file, `pg_settings`, `pg_file_settings` clean); a server-group override and a server override with the precedence
checked on each server; removing an override returns the lower level's value; a `postmaster` parameter shows
pending restart on every server and takes effect after the restart (and the order of a standby and its primary for
`max_connections` up and down); an invalid value is refused before anything changes (no file touched, no revision
spent); a value that PostgreSQL rejects in the file (injected past the validator) restores the previous file and
reports PostgreSQL's text; a server that is down gets the change when it returns; a database time zone is applied on
every server (and on a standby by replay); `TimeZone` is cluster-only at server level (refused); the clock
difference is reported and the alert is raised when `fault` steps the wall clock of one `kingresd`; history and
revert (the revert is a new change, a superseded change is refused); the audit entries (old value, new value,
level); the roles that may and may not (readonly reads, ops cannot change, stats cannot read, admin changes); the
hidden parameter's value is never in an answer; the differential validator test; `TestRefusedSetMatchesVersion`.

Console unit tests: every state (loading, empty, error, partial, unsupported, read-only, each of the four server
states), every role, hostile values and names in every place a value is shown, the live validation of every type,
the preview, the per-server progress, the revert dialog, the dashboard card and its links, the filters in the URL.
Browser tests: the layout at 1366, 683 and 320 px for the page, the dialogs and the Time tab, and the axe tests in the light and the dark theme.

## Increments

Each increment builds and passes its own tests; the contract checker passes from increment 1 on.

1. **Contract 0.8.0**: the operations, schemas, enums, the README rows; the generated operation table; the
   contract tests; the types of the console regenerated.
2. **Policy and validation in `kingresd`**: `instance.ManagedParameters`, the refused and cluster-only tables, the
   validator, the override file renderer (`RenderConfig`), `EnsureConfigInclude`, the migration, unit tests.
3. **Reading**: `config/state` and `time` internal calls, `listConfigParameters`, `getConfigParameter`,
   `listConfigServers`, `getConfigSummary`, `getClusterTime`, `listTimeZones`, the alert `clockDifference` and the
   cluster setting; integration tests of the reads.
4. **Applying**: `config.apply`, `config/apply` and `config/check`, database values, history, revert, audit, the
   hooks for `server.add` and for a server that changes group; integration tests of the changes.
5. **Restart**: `config.restart` with the order; the integration test of restarts.
6. **Console**: the API types, the mock, the page, the dialogs, the Time tab, history, the dashboard card, the server
   page, tests and browser tests.
7. **Documentation**: the READMEs of the control plane, the contract and the console, `internal-api.md`, and the
   report of what was run.

## Known limits

- Roles' own settings (`ALTER ROLE ... SET`) are out of scope. A role's setting beats the database level for that
  role; the page says so for time zones.
- The checker's number syntax is narrower than PostgreSQL's by design.
- The extension parameters of context `postmaster` or `sighup` have no dry run (V5/V6) and are caught by the
  check of the file (V2) after the value is written to the candidate file, which is before any reload.
- A cluster of servers on different PostgreSQL builds is checked per server; the preview only reads the answering
  server's metadata.
- Restarts have no failover to hide the downtime of a group (not implemented).
- The clock difference is an estimate with a stated uncertainty and is relative to the answering server, not to
  a trusted time source.

## What the implementation decided beyond this text

- **The answering server is the manager leader.** The reads of the Configuration tag are answered by the leader (a
  follower forwards them, like the task and settings reads), because only the leader can ask every server through the
  `leader`-only internal calls and holds the system database. "The answering server" above therefore means the manager
  leader: `ClusterTime.referenceServerId` names it and every difference is relative to its clock.
- **The clock alert is the leader's loop, not a monitor probe.** `clockDifference` is about another server, and the
  monitor round of a server may only reconcile alerts about itself, so the leader measures the other servers every
  10 seconds (`Leader.CheckClocks`) and reconciles each alert in the scope of that server.
- **Bringing servers up to date is a loop, not a step of `server.add`.** Every 30 seconds the leader compares, for each
  server, the hash of its override file with the file the stored values give for it and starts a `config.apply` task
  without changes for the servers that differ (a server that joined, moved to another server group, came back with a
  file restored from a failed apply, or was edited by hand). A server is tried again at most every ten minutes. The
  same task is what a change starts for the servers it names.
- **The applied revision and the failed state.** A server that fails keeps its previous file (restored) and reports the
  revision it holds; `config_server_state` keeps PostgreSQL's text per parameter, or `(server)` when the server could not
  write its file at all (every parameter that is meant to differ there is then shown failed).
- **A file that is already the wanted one is reloaded when the instance runs something else** (an earlier apply could
  write but not reload a standby that was starting).
- **Standbys.** Every instance of a server gets the file; a standby that is still being created or is starting is
  reported `waiting` and gets the file (and the reload) at the next apply or by the loop above. A standby built from a
  base backup starts with the override file of its primary's server until then (a limit).
- **Derived values in the effective value.** The value shown for a parameter is the running value in the form
  `SHOW` uses (`128MB`, `10min`), computed from `pg_settings.setting` and its unit; the pending value is the text that
  the file holds.
- **Values that PostgreSQL shows but kingresd does not.** The value of a parameter whose name says it runs a command or
  loads code (`archive_command`, `restore_command`, ...) is never returned, although PostgreSQL 18 shows it to every
  database user.

## The check against the design

A check of phase 13 against this design (2026-11-21; its result was first only a line in the README and the work log) compared the sections
above with the code and the tests. Everything else was built and tested; what was missing, and what was done:

| What the design says | Found | Done |
|---|---|---|
| The preview warns that `max_connections` would not exceed the reserved connections ("Validation") | Not built: the preview had no place for a warning on a valid request | `warnings` added to `ConfigChangePreview` (contract 0.17.0, additive), code `config_reserved_connections`; `pgconfig/warnings.go`; the console shows it. Deviation: computed from the effective values, not read from layer 3 (see "Validation") |
| The preview flags a value more than ten times or below a tenth of the default of a memory parameter ("Security") | Not built | Code `config_value_unusual`, with the edges tested (exactly ten times, exactly a tenth, other units, zero default, string); the console and its mock show both warnings |
| Integration: `pg_file_settings` is clean after a cluster-level change | Only `pg_settings` was read | `TestConfigFileSettingsAreCleanOnEveryServer` |
| Integration: a value that PostgreSQL rejects in the file, injected past the validator, restores the file and reports PostgreSQL's text |  Only a server that cannot write its file was tested (`TestConfigFailedServerIsReported`, skipped when the tests run as a user that ignores directory modes) | `TestConfigValueRejectedByPostgreSQLInTheFileRestoresTheFile`: a time zone that no checker of `kingresd` knows is stored behind the validator |
| Integration: a database time zone is applied on every server "and on a standby by replay" | Only the primary instances were read | `TestConfigDatabaseTimeZoneReachesTheStandbys` reads a standby over its socket |
| Integration: the preview on a real cluster | Only effects and problems were checked | `TestConfigPreviewWarnings`, with the answer checked against the contract |

**The independent review of 2026-11-22** ([reviews/2026-11-22-config-phase13.md](../reviews/2026-11-22-config-phase13.md), P13R-01 to P13R-15) compared the code
with this design again and found places where the design itself was wrong or silent. What changed in the design (the sections above say it where it applies):

| What the design said | Found | Done |
|---|---|---|
| Everything not refused is settable | `default_transaction_read_only` and the other session defaults put the control plane into read-only mode with no way back (P13R-01) | They are refused at every level (reason `controlPlaneSession`); "Refused parameters" lists them |
| The managed list protects the connection (`listen_addresses`, `port`, `unix_socket_directories`) | `unix_socket_permissions`, `unix_socket_group`, `huge_pages`, `ssl_*` and others cut `kingresd` off or stop the postmaster (P13R-02) | Managed or refused; a restart is checked with `postgres -C` first and a restart that cannot start puts the last good file back |
| "The step waits and never gives up" for a server that is down | A stopped server in the middle blocked the later servers and every later change (P13R-03) | The step records `waiting` and goes on; the convergence loop brings the server up to date |
| The superuser-only set is read at run time | An error gave an empty set and the classification failed open (P13R-04) | An error or an empty set fails the request (503); connection strings are always hidden |
| A refused or managed name cannot be set | A stored value that later became managed blocked every apply and could not be removed (P13R-05) | Not written, not blocking, and its removal is always accepted |
| The restart uses "the existing instance operations" | It bypassed the lifecycle and assumed instance 1 is the primary (P13R-06) | `Ops.BeginConfigRestart`, and the pair order uses the primary the leader last saw |
| Audit `details` are `{revision, changes, servers}` | Only `{changes, taskId}`, with the text the caller sent (P13R-12) | See "The audit log" |
| Task code `validation_failed` | The code was `step_failed` (P13R-15) | `validation_failed` and `config_incomplete` are coded task errors |
| (silent) two administrators on one parameter | The second silently overwrote the first, and Retry resent a stale request (P13R-11) | `expectedRevision` of `ConfigChangeRequest`, sent by the dialog |

Tests that the review found missing or too weak and that now exist: the time-zone tests use a zone that is not the host's (P13R-07), the order of the `config.restart` steps is
compared with the preview (P13R-08), `0100` and `00` are in the unit table of the checker (P13R-09), and the reserved-connections warning names every server and makes
no claim for a removed override (P13R-10).

Not a gap, found already built: the differential validator test and `TestRefusedSetMatchesVersion` (`pgconfig/pg_test.go`, run with a real
PostgreSQL), the order of restarts for a standby and its primary, the precedence of the levels, the roles, the hidden values and the audit
entries (`clustertest/config_test.go`). The console unit and browser tests of the design are as built; the unit tests of the warnings are in
`pgconfig/warnings_test.go` and the web tests of the mock and the dialog.

One test of the design cannot work exactly as written: "an invalid value in the file, injected past the validator" has no hook in `kingresd`
(there is no fault point in the agent), so the test stores the value in `kingres_cp.config_value` directly, which is the same state, and
removes it in its clean-up.

The text that the console shows for a value that PostgreSQL refuses **in the file** is what `pg_file_settings.error` holds, which is
`setting could not be applied` for every such value (PostgreSQL's own reason goes to its log only), with the name of the parameter. A
message with PostgreSQL's reason (`invalid value for parameter "TimeZone"`) is given only by layer 3, before anything is written. The
design's "reports PostgreSQL's text" for a rejected file is therefore this text; the test asserts it.
