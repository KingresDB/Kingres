<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review: contrib/kingres_shard 0.5 (high availability in the extension)

Reviewer: independent (not the author of the code, not the reviewer of the earlier
rounds). Reviewed state: commit 5cc5f078db ("kingres_shard 0.5: standby nodes,
promote_node, prepared transactions across a failover, deadlock detection, restore
points"), exported with `git archive` of `contrib/kingres_shard` and `kingres/dev`
into a private area (`~/kw-review8`); the working tree was not used for any result.
Findings are numbered S7-nn (the findings of the 0.4 review are S5-nn).

The version attacked is the part of high availability that lives in the extension:
a failover must not lose a committed distributed transaction, must not leave one half
committed without saying so, and must not let a statement answer from two primaries
of one group.

## Scope and method

Read: the commit message, the README chapter "High availability", `src/ha.c`,
`src/failover.c`, the 0.4-to-0.5 diff of `recovery.c`, `node_conn.c`, `catalog.c`,
`planner.c`, `dist_scan.c`, `shippable.c` and `pg_compat.h`, the 0.4-to-0.5 upgrade
script (restore point, deadlock detection, standby status, privilege lines), the
function `internal_apply_catalog` of `kingres_shard--0.5.sql`, the test helper and
`t/050`, `t/052`. I did not read `node_admin.c` (the standby registration checks) or
the bucket-move code line by line, and I did not read the design documents beyond
what the README repeats.

Built and ran, on a private copy of PostgreSQL 18.7 and of 19rc2 (both with
assertions; the copies relocated: `pg_config` reports paths inside my area, the
server binaries find their libraries through `LD_LIBRARY_PATH`), the extension built
from the export with `kingres/dev/ext.sh`, one major after the other, TAP servers on
ports chosen by the test framework, everything under `nice`, `-j4`, on a shared
machine with a load average of 8 to 20:

- **Clean build and the 12 regression tests** on both majors: built, installed and
  passed on both; the build log has no warning (I did not turn warnings into errors).
- **The eight new TAP files `t/048` to `t/055`** on both majors, before any change of
  mine: 8 files, 255 subtests, all passed on 18.7 (118 s) and on 19rc2 (101 s).
- **The whole suite once on each major** (`t/001` to `t/055`, started when the
  experiments below had shown what to look at): 55 files, 6082 subtests, all passed on 18.7 (898 s) and on 19rc2 (917 s),
  the two runs at the same time. The commit message says 6102 subtests; I count 6082 on both majors (the files and the result are the same; I did not look for the difference).
- **Nine scripted TAP files of my own** (several scenarios each), written in the style of
  `t/050` with the helpers of `KingresShardTest.pm` (real streaming standbys,
  `Cluster::backup`, `init_from_backup`, `promote`) and the fault points; each
  serious one was run on 18.7 and on 19rc2 with the same result. They are listed with
  the findings. They are not part of the repository (the only file I wrote there is
  this report); each reproduction below is given as steps that can be pasted.

"Reproduced" below means I observed it. "By reading only" means I did not run it.
Where a finding needs a sentence on what the README says, the sentence is quoted from
the chapter "High availability".

## README guarantees, one by one

| # | Guarantee of the chapter "High availability" | Verdict |
|---|---|---|
| 1 | A standby owns no buckets and is skipped by routing, catalog synchronization, reference writes, fan-out, scans, recovery and moves | held (`t/048`, `t/054`; not attacked further) |
| 2 | A standby may be absent or not streaming without harm to the primaries; one that cannot be reached is registered with a warning | held (registered an unreachable standby with a quote in its host name: warning, no effect on the primaries) |
| 3 | A standby is checked when it can be reached: in recovery, same major, copy of its group's primary | held (`t/048`; a promote of a standby that is still in recovery is refused, reproduced; the checks of major and system identifier by `t/048` only) |
| 4 | `check_cluster` and `standby_status` report a standby that is unreachable, not streaming, not in recovery | held for unreachable and promoted-outside (reproduced); not streaming by `t/048` only |
| 5 | A node that is not the primary of its group is in no group for routing: forwards its clients' statements, refuses forwarded fragments, reads every group | held (reproduced with the old primary alive and reached by `promote_node`: forwarded writes and prepared statements ended on the new primary, the old primary's own shard tables did not change, a transaction that already wrote locally on it was refused at commit) |
| 6 | "It does not stop an application that writes ordinary tables on it" | held as stated (an ordinary table was written on the demoted node) |
| 7 | `promote_node`: "in one two-phase transaction on every node that can be reached"; the old primary only "if it is reachable" | **broken** (S7-04, S7-05) |
| 8 | In-flight statements: a scan or a fan-out planned against the old primary fails with KS001; a routed statement that reaches the demoted node is repeated | held for the cases I ran (prepared statements run seven times before the promotion, a session of the old primary with an open transaction; `t/049`, `t/054` on both majors); cursors held open, rescans and COPY across a promotion: not examined |
| 9 | The map group: runs on the promoted node, pushes its catalog "to every other node it can reach"; "a node that could not be reached ... is repaired by `sync_catalog()`" | **broken** (S7-04: it refuses when any other node is unreachable; S7-05: `sync_catalog()` cannot repair a node whose change is prepared) |
| 10 | The gid names the origin group and the timeline; the new primary resolves what the old one started from the decision log; old and new formats are both resolved | held (`t/050`, `t/051` on both majors; forged identifiers: S7-09) |
| 11 | With a synchronous standby the outcome is atomic "at every point of the protocol" (the origin's commit is acknowledged by the standby before the first COMMIT PREPARED; the participant's PREPARE is acknowledged by the participant's standby) | held at the six points (`t/050`, both majors); **broken** when the origin's commit wait is cancelled or terminated (S7-01) and when the participant's own `synchronous_commit` is not `on` (S7-07) |
| 12 | With an asynchronous standby a half-committed transaction is recorded in `dtx_incident` and shown by `check_cluster` | held (`t/050`, last scenario); the participant-failover case that the README says leaves no trace leaves none (reproduced, S7-07); the other one (all participants had committed) not run |
| 13 | Deadlock detection: "No false positive": a cycle is acted on only if every edge is the very same wait in both readings | **broken** (S7-02: a forged label builds a cycle that is not one; S7-03: a forged label stops the detector) |
| 14 | The victim gets SQLSTATE 40P01, "distributed deadlock detected" | held with the default `log_min_messages` (reproduced); an unrelated cancel can be rewritten (S7-10) |
| 15 | `create_restore_point`: barrier, wait for the second phase, restore points on every primary, barrier lowered; "the barrier expires by itself" | held for the quiet-state and expiry (reproduced: a barrier that nobody lowered ended at its time to live); an orphaned barrier after a cancelled caller (S7-08); the restore of three servers under load only by `t/053` (not repeated by me) |
| 16 | All functions are for superusers (`REVOKE ... FROM PUBLIC`, and a second check inside) | held (21 new or internal functions and the new table tried as an ordinary role: all refused; the ordinary role can still execute the node-to-node helpers that 0.4 left open) |

## New findings

| ID | Severity | Summary |
|---|---|---|
| S7-01 | critical | A cancelled (or terminated) wait for the synchronous standby makes the origin tell the participants to commit before the decision is on the standby: after a failover the transaction is committed on the participants and gone on the new primary, with no record |
| S7-02 | high | An ordinary role can make the detector cancel any distributed transaction: the label of a wait is the client-settable `application_name`, so a forged label builds a cycle that does not exist |
| S7-03 | high | A forged label with a non-numeric start makes every pass of the detector fail while the forged cycle exists: real distributed deadlocks are never resolved |
| S7-04 | high | `promote_node` fails when any other primary is unreachable, for an ordinary group and for the map group, against the README; two groups that fail together cannot be promoted at all |
| S7-05 | high | A failure of `promote_node` after its decision leaves the promoted node with a prepared catalog change that no extension function can commit; its group is unusable until an operator edits the node by hand |
| S7-06 | high | An old map primary that was not reached by `promote_node` and is restarted still changes the catalog, and the other nodes accept its catalog over the new one (no epoch, no version check) |
| S7-07 | high | The participant's own `synchronous_commit` is not forced: with `off` or `local` its PREPARE and COMMIT PREPARED are not acknowledged by its synchronous standby, and a failover of the participant loses its part of a committed transaction with no record |
| S7-08 | medium | A cancelled `create_restore_point` leaves the barrier up on every primary: all multi-node commits wait for up to `2 x node_timeout + 10` seconds (130 s by default) |
| S7-09 | medium | An ordinary role can prepare a transaction with a forged identifier: recovery rolls it back and writes a failover incident and a `check_cluster` alarm for a failover that never happened |
| S7-10 | low | A victim mark that did not lead to a cancel rewrites the next unrelated cancel of that backend (for 10 seconds) into "distributed deadlock detected" |

### S7-01 (critical) A cancelled wait for the synchronous standby releases the second phase

Where: `node_conn.c` (`log_decision`, `precommit`, `postcommit`): the extension makes
the local commit a synchronous one (`synchronous_commit` raised to `on`) and sends
COMMIT PREPARED after the commit returns, assuming that a commit that returned was
acknowledged by the standby. PostgreSQL does not give that: a cancel (Ctrl-C, a
`pg_cancel_backend`) or a terminate that arrives while the backend waits in
`SyncRepWaitForLSN` ends the wait with a WARNING ("canceling wait for synchronous
replication due to user request ... the transaction has committed locally, but might
not have been replicated"); the commit completes. `postcommit` then sends COMMIT
PREPARED to every participant. The README says the commit "is acknowledged by the
standby before the first COMMIT PREPARED is sent".

Steps (two nodes, node 1 map group primary, group 1 = node 2 with a streaming standby
registered as a standby node; `ANY 1 (<standby>)` in `synchronous_standby_names` of
node 2; the same as `t/050`'s helpers):

```
-- node 2: stop the standby
-- session A, node 2:
BEGIN; INSERT INTO public.kv VALUES (<key of group 1>, 'b');
       INSERT INTO public.kv VALUES (<key of group 0>, 'a'); COMMIT;   -- waits (wait_event SyncRep)
-- node 1: SELECT count(*) FROM pg_prepared_xacts;   -- 1: the participant has prepared
-- session B, node 2:
SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep';
-- session A returns; node 1: pg_prepared_xacts is empty, the row 'a' is visible
-- node 2: stop immediate; start the standby; pg_promote(); on node 1:
SELECT kingres.promote_node(<standby node id>);
-- on the promoted node:
SELECT kingres.recover_prepared_transactions();
SELECT count(*) FROM public.kv WHERE id = <key of group 0>;   -- 1 (node 1 committed)
SELECT count(*) FROM public.kv WHERE id = <key of group 1>;   -- 0 (the origin's part is gone)
SELECT count(*) FROM kingres.dtx_incident;                    -- 0
SELECT check_name FROM kingres.check_cluster();               -- only standby_unreachable
```

Observed on 18.7 and on 19rc2: exactly this (participant row 1, origin's row 0,
incidents 0, `check_cluster` says nothing about the transaction). What should happen:
with a synchronous standby configured, a transaction either commits everywhere or
nowhere, or the split is recorded. The same happens when the backend is terminated
with `pg_terminate_backend` instead (reproduced on 18.7: the participant committed, the
promoted standby has nothing, no record); a server shutdown during the wait I did not run.

Suggested fix: in the `XACT_EVENT_COMMIT` callback, before `postcommit`, check that
the commit record has been acknowledged: if `SyncRepRequested()` and the flush
position acknowledged by the synchronous standbys (`WalSndCtl->lsn[SYNC_REP_WAIT_FLUSH]`,
read under `SyncRepLock`) is below `XactLastCommitEnd`, do not send COMMIT PREPARED:
raise a WARNING and leave it to recovery, which commits it with the decision log of
the origin if the origin survives and rolls it back everywhere if a standby without
the decision is promoted. That makes the cancel visible as "committed locally, second
phase deferred" instead of a split. Failing that, the README must say that the
guarantee does not survive a cancelled or terminated commit.

### S7-02 (high) An ordinary role can make the detector cancel a distributed transaction

Where: `kingres_shard--0.4--0.5.sql` (`internal_dtx_label_of`: the label of a backend
is its `application_name` when that starts with `ks-dtx:`), `node_conn.c`
(`append_dtx_label`). The application name is a setting any client can choose with
`SET application_name` or in its connection string. The README: "Every remote session
that a transaction opens is labelled ... any other backend is its own origin."

Steps (two nodes, the detector on: `deadlock_check_interval = 1`, `deadlock_timeout =
200ms`; a role `mallory` with `LOGIN` on both nodes and DML privileges on the table;
key a in group 0, key b in group 1):

```
-- 1. mallory on node 2:
SET application_name = 'ks-dtx:1:99999:1000';
BEGIN; UPDATE public.kv SET val = 'm2' WHERE id = <b>;  SELECT pg_sleep(25);
-- 2. an innocent session T on node 1:
BEGIN; UPDATE public.kv SET val = 't' WHERE id = <a>;
UPDATE public.kv SET val = 't' WHERE id = <b>;       -- waits for mallory on node 2
-- 3. mallory on node 1:
SET application_name = 'ks-dtx:1:99999:1000';
BEGIN; UPDATE public.kv SET val = 'm1' WHERE id = <a>;   -- waits for T on node 1
```

There is no deadlock: mallory's first session releases its lock when it ends. The
detector sees two waits with the same label on two nodes (mallory waits for T on node
1, T waits for mallory on node 2), calls it a cycle, and picks the youngest start,
which is T (the forged start is 1000). T's statement fails with `40P01: distributed
deadlock detected` after about two seconds; mallory is not cancelled. Reproduced on
18.7 and 19rc2, with the default `log_min_messages` and with `error`. What should
happen: an ordinary role cannot cancel the sessions of other roles. Suggested fix: do
not trust the application name. The label is made by the origin for the remote
session; carry it where a client cannot set it (a custom GUC that only the node-to-node
session may set, checked in a `check_hook` against the forwarded-session token), and
treat an `application_name` that merely looks like a label as an ordinary backend.

### S7-03 (high) A forged label stops the detector

Where: `internal_deadlock_victims` (`split_part(v, ':', 4)::int8` on every element of
every cycle). With the same technique as S7-02 and four sessions of one ordinary role
(two on each node, labels `ks-dtx:1:1:abc` and `ks-dtx:1:2:5`, arranged so that on
node 1 the second waits for the first and on node 2 the first waits for the second),
every pass of the detector raises `invalid input syntax for type bigint: "abc"` in
`detect_distributed_deadlocks()`; the worker logs it once a second. A real deadlock
between two innocent transactions (the shape of `t/052`, two nodes) stayed unresolved
for the 12 seconds I watched (`t/052` shows it resolved within about two seconds without
the forged cycle), and would stay as long as the forged waits last. Reproduced on both
majors (11 errors in the log of the map node in 12 seconds). What should happen: a
label that is not well formed is ignored, and one bad cycle never hides another.
Suggested fix: the fix of S7-02, and in the SQL a defensive parse (`CASE WHEN ...
~ '^ks-dtx:[0-9]+:[0-9]+:[0-9]+$'`) so that a bad label can never raise.

### S7-04 (high) `promote_node` needs every other primary to be reachable

Where: `failover.c`. For each active primary that is not the old primary of the group
(and for the map group, each other node), the participant session is opened with
`kingres_shard_get_conn(host, port, false, NULL)`, which raises an error when the node
cannot be reached; only the old primary of the group gets `missing_ok`. The README says
"in one two-phase transaction on every node that can be reached" and, for the map
group, "pushes it ... to every other node it can reach ... A node that could not be
reached keeps naming the old map primary and is repaired by `kingres.sync_catalog()`".

Steps A (two groups lose their primaries together; nodes 2 and 3 are primaries of
groups 1 and 2 with standbys registered; node 1 the map primary):

```
-- stop node 2 and node 3 (immediate); pg_promote() both standbys
-- node 1:
SELECT kingres.promote_node(<standby of group 1>);
-- ERROR:  could not connect to node <node 3>        (after the warning about node 2)
SELECT kingres.promote_node(<standby of group 2>);
-- ERROR:  could not connect to node <node 2>
SELECT kingres.remove_node(2);   -- ERROR: shard group 1 still has the standby node 4
```

Steps B (the map group; node 2 down as well): stop node 1 and node 2, `pg_promote()` the
standby of node 1, and on it `SELECT kingres.promote_node(<its node id>)`: `ERROR:  could
not connect to node <node 2>`; the catalog of the promoted node is unchanged, node 3
still names the dead map primary. Reproduced on both majors. What should happen:
the node that cannot be reached is skipped with a warning and repaired later with
`sync_catalog()`, as the README says; at least a group whose primary is down must not
stop the promotion of another group's standby. Today the only exit from a double
failure, or from a failure of one group that has no standby ready while another group
needs promotion, is to edit the catalog by hand. Suggested fix: use `missing_ok` for
every participant, and make a participant that could not be reached a warning that
`sync_catalog()` repairs (it already has to cope with nodes that are not reachable);
the two-phase protocol of the rest is unchanged.

### S7-05 (high) A failed `promote_node` can leave the promoted node unrepairable

Where: `failover.c`, `recovery.c`, `forward_auth` (the node-to-node token). The change
is a two-phase transaction; the promoted node's part includes `internal_set_local_node`.
If the origin (the map primary) stops after its decision (the fault point
`after_commit`, a crash, or a COMMIT PREPARED that fails), the promoted node holds the
prepared catalog change, and its `local_node` is still the old primary's id. Recovery on
the origin then cannot list or resolve the prepared transaction of the promoted node,
because the session to node 4 is made for the id the catalog now gives it, and the
node answers `FATAL: this session claims to have been opened by another node of the
cluster but cannot prove it`.

Steps (three nodes, standby of node 2 registered as node 4):

```
-- node 2: stop immediate; pg_promote() the standby (node 4)
-- node 1: SET kingres_shard.fault_injection = 'after_commit:kill';
--         SELECT kingres.promote_node(4);          -- the backend is killed after its local commit
-- restart node 1 (the fault point kills the postmaster here: restart_after_crash is off in the tests)
-- node 1: SET kingres_shard.node_timeout = 5;
--         SELECT kingres.recover_prepared_transactions();   -- 1: node 3 resolved; node 4 "could not list ..."
-- then, each on node 1:
SELECT kingres.recover_prepared_transactions();   -- 0
SELECT kingres.promote_node(4);                   -- NOTICE: node 4 is already the primary of group 1
SELECT kingres.sync_catalog();                    -- ERROR: FATAL: this session claims ... cannot prove it
```

After all of that: node 4 still has `local_node = 2`, the old catalog (node 2 primary,
node 4 standby) and one prepared transaction that holds row locks on its catalog tables;
node 1 and node 3 name node 4 as the primary of group 1. Every session that node 1 or
node 3 opens to node 4 is refused, so group 1 cannot be used; the extension offers no
way back (a repeated `promote_node` is a notice, `sync_catalog()` fails the same way).
Reproduced on both majors. What should happen: recovery commits the prepared catalog
change on the promoted node (it is the only way the cluster can finish what its own
decision log says), and a repeated `promote_node` repairs a half-applied promotion.
Suggested fix: make the token check accept the id that the *other* side's catalog gives
the node when the node's own `local_node` row is the id of a node of the same group
that the sender's catalog calls a standby; or let recovery use the session identity of
the old primary for the promoted node; and let `promote_node` for a node that the
catalog already calls the primary compare `local_node` and the pending prepared
transactions of that node, and finish them.

### S7-06 (high) A stale map primary overrides the catalog of the others

Where: `internal_apply_catalog` (installed by the 0.5 script; no version, epoch or
origin check), `failover.c` (the old map primary is only told when it can be reached).
The README leaves the old primary to the control plane's fencing, which is a fair
division of work; the finding is what the other nodes do when it fails. Steps (two
nodes, a standby of the map primary registered as a node of group 0):

```
-- stop the map primary (node 1) immediate; pg_promote() its standby (node 3)
-- node 3: SELECT kingres.promote_node(3);      -- node 2 now names node 3 as the map primary
-- start node 1 again (not fenced; its catalog is the old one)
-- node 1: CREATE TABLE public.kv2 (id int PRIMARY KEY, val text);
--         SELECT kingres.create_distributed_table('public.kv2', 'id');   -- succeeds
SELECT node_id, group_id, role FROM kingres.node;   -- on node 2 afterwards
```

Observed: node 2 now holds `1:0:primary 2:1:primary 3:0:standby`, the catalog of the
old map primary, and a map version (9) higher than the new map primary's (8); the new
map primary still holds `1:0:standby 2:1:primary 3:0:primary`. Two catalogs, and the
other nodes follow whoever pushed last. Reproduced on 18.7 and on 19rc2. What should
happen: a node that holds a newer catalog refuses an older one. Suggested fix: carry
an epoch that `promote_node` raises (for example the timeline of the map group's
primary, or a counter in `map_version` that a promotion bumps by a large step), have
`internal_apply_catalog` refuse a snapshot with a lower epoch and have every function
that changes the catalog refuse to run on a node whose epoch is lower than that of the
nodes it pushes to (the push is the point where a stale primary meets a node that
knows better).

### S7-07 (high) The participant's PREPARE is not acknowledged by its standby unless its own setting says so

Where: `node_conn.c`. For the origin the code raises `synchronous_commit` to `on`
(`log_decision`, S3-15). The participants' sessions carry the fixed settings
(`KS_FIXED_SETTINGS_SQL`) which do not contain `synchronous_commit`, so the participant
runs PREPARE TRANSACTION and COMMIT PREPARED with whatever its configuration or the
role's setting says. With `synchronous_commit = off` or `local` they return without
the participant's standby, whatever `synchronous_standby_names` says. The README says
"`PREPARE TRANSACTION` of a participant is acknowledged by the participant's own
synchronous standby, when its group has one, before the origin decides".

Steps (two nodes; node 2 has a streaming standby that is the synchronous one; then in
`postgresql.conf` of node 2 `synchronous_commit = off`, or `ALTER ROLE app SET
synchronous_commit = off` for the role of the transaction; stop the standby):

```
-- node 1:
BEGIN; INSERT INTO public.kv VALUES (<key of group 0>, 'a');
       INSERT INTO public.kv VALUES (<key of group 1>, 'b'); COMMIT;   -- returns at once
-- node 2: stop immediate; start the standby; pg_promote(); on node 1:
SELECT kingres.promote_node(<standby>);  -- on the promoted node: recover_prepared_transactions()
SELECT count(*) FROM public.kv WHERE id = <key of group 0>;   -- 1
SELECT count(*) FROM public.kv WHERE id = <key of group 1>;   -- 0
SELECT count(*) FROM kingres.dtx_incident;                    -- 0
```

Control run (same, `synchronous_commit = on` at node 2): the commit does not return;
I let it wait for four minutes and cancelled it. So the guarantee holds for the default
and is silently absent for a setting that a performance-minded operator may well put on
the role or the server. Reproduced on 18.7 and 19rc2. What should happen: for a
distributed transaction the extension forces what it needs on every node it prepares.
Suggested fix: send `SET LOCAL synchronous_commit = on` (when the local value is below
`on` and above `off`, when it is not `remote_write`: at least `on`) in the remote
transaction before PREPARE TRANSACTION, and in the session that sends COMMIT PREPARED;
or document the requirement in the same paragraph as the one for the origin and have
`check_cluster()` report a primary whose setting is below `on`.

### S7-08 (medium) A cancelled `create_restore_point` leaves the barrier up

Where: `create_restore_point` (the handler `EXCEPTION WHEN OTHERS` that lowers the
barrier). In PL/pgSQL `OTHERS` does not catch `query_canceled`, so a cancel of the
caller (Ctrl-C, `statement_timeout`, `pg_cancel_backend`) during the wait for the
second phase or during the restore points raises past the handler. The barrier stays up
on every primary until it expires: the README documents that for a coordinator that
died, not for a caller that was cancelled, which is an ordinary event.

Steps (`kingres_shard.node_timeout = 4` to keep the test short; two nodes):

```
-- session T (node 1, fault point): SET kingres_shard.fault_injection = 'after_prepare:wait:/tmp/gate';
--     BEGIN; INSERT <group 0 key>; INSERT <group 1 key>; COMMIT;     -- stops between prepare and decision
-- session R (node 1): SET statement_timeout = '2s'; SELECT * FROM kingres.create_restore_point('rp1');
--     ERROR: canceling statement due to statement timeout
-- on both nodes: SELECT kingres.internal_restore_barrier('state');
--     {"active": true, "inflight": 1 (node 1), "generation": 1}
-- release T; then a new two-node transaction on node 1 waits 16 s before it commits
--     (ttl = 2 x 4 + 10 = 18 s)
```

Reproduced on both majors. With the default `node_timeout` of 60 seconds the wait is
up to 130 seconds for every multi-node commit of the cluster, from one `Ctrl-C`. The
expiry itself works (the commit went through at the time to live). What should happen:
the barrier is lowered when the caller is cancelled. Suggested fix: lower the barrier
from a transaction-abort path that a cancel cannot skip: either a `PG_TRY` in a C
wrapper, or an `internal_restore_barrier('off')` call registered as an abort callback
of the caller's session (as `node_conn.c` does for remote sessions), or run the whole
function in a background worker whose cancel the caller cannot interrupt.

### S7-09 (medium) A forged gid by an ordinary role produces a false failover incident

Where: `recovery.c` (`parse_gid`, `record_incident`). Any role can `PREPARE
TRANSACTION 'kingres_g0_0_77_1_2'` on any node (the only requirements are
`max_prepared_transactions` and a transaction). The identifier parses as one of the
group 0, timeline 0, which is "earlier" than the current timeline of the node that runs
recovery for group 0, so recovery rolls it back at once (it skips the check whether the
origin transaction is still running) and writes a row to `kingres.dtx_incident`; the
old format (`kingres_1_77_1_3`) is rolled back as well. After one recovery pass on node
1:

```
SELECT check_name, detail FROM kingres.check_cluster();
 prepared_rolled_back_after_failover | rolled back at ...: it belonged to timeline 0 and had no
   commit decision on timeline 1; if the old primary had committed it, other nodes may have too
```

Reproduced on both majors, with a role that has no privilege beyond connecting; each
distinct gid adds a row that stays 30 days. Two effects: an alarm that tells the
operator that a transaction may be half committed when no failover happened (the
alarm is the only signal the README gives for the asynchronous case, so a flood
buries real ones), and an ordinary user's own prepared transaction whose name begins
with `kingres_` is destroyed by the system (this part existed in 0.4 for the old
format; S2-20 limited it to numeric names). A forged gid can never be committed
(that needs a row in `dtx_log`, which only the origin writes), so the damage is to the
signal, not to data. What should happen: recovery touches only transactions that the
origin made. Suggested fix: bind the gid to something a user cannot choose, for
example record at PREPARE time, in the origin's `dtx_log` (already durable with the
decision) or in a new table written in the same transaction before PREPARE, the gids
it is about to prepare, and let recovery resolve only gids that appear there or that
lie within a window of the origin's own timeline history; or write the incident only
for gids whose origin timeline is a timeline that the group really had.

### S7-10 (low) A mark that did not lead to a cancel rewrites an unrelated cancel

Where: `ha.c` (`ha_emit_log`, `internal_mark_victim`, `consume_victim_mark`). The mark
of a victim (by pid) lasts 10 seconds and is consumed by the next error of the backend
with the code `query_canceled`, whatever its cause. `internal_cancel_waiter` marks and
cancels; if the backend left the wait between the check and the cancel (the cancel
reaches an idle backend and is ignored, or the pid is reused), the mark stays.
Reproduced the second half only, by marking the pid of a session directly as
superuser: the next `statement_timeout` of that session was reported as
`40P01: distributed deadlock detected`. The window is small and needs the detector to
hit the instant a wait ends, hence low. Suggested fix: the mark carries the
`waitstart` it was made for and the hook checks the backend is still waiting on a lock
with that start (it can read its own `MyProc->waitStatus`/lock wait), or the mark is
cleared by the caller after `pg_cancel_backend` returns when the backend did not
report an error.

## Notes

- README, "Distributed deadlock detection": "it needs `log_min_messages` to be `error` or
  lower (the default is `warning`)". The default works (reproduced: 40P01 with the default
  setting); the hook is skipped only when the error is not logged at all (`fatal`,
  `panic`). The sentence reads as the opposite and should be reworded.
- `promote_node` on a node that the catalog already calls the primary answers with a
  notice; combined with S7-05 the notice hides an unfinished promotion.
- An ordinary role can execute `internal_catalog_snapshot()`, `internal_map_version()`
  and four other node-to-node helpers that 0.4 left open; they show what the readable
  catalog tables show. No change of 0.5.
- `create_restore_point` takes `LOCK TABLE kingres.map_version IN SHARE MODE`, which does
  not exclude another caller of the same function. Two concurrent callers cannot both
  succeed wrongly by my reading of the generation check (the second `on` raises the
  generation, so the first `off` reports "not held" and the first caller fails), but
  they make each other fail. Not reproduced.

## Examined and found sound

- **The six points of the two-phase protocol, with a synchronous and with an
  asynchronous standby** (`t/050`, run on both majors, plus my own variant with the
  commit wait cancelled, S7-01): every outcome is as the README says; the rollbacks of
  earlier-timeline transactions are recorded; the new primary originates transactions
  of its own timeline that nobody confuses with the old ones.
- **Identifier formats**: both formats parse; a group number with several digits, an old
  format identifier that is pending across the upgrade and across a failover, an old and a
  new transaction of one origin (`t/051`, both majors). A forged identifier cannot be
  committed by recovery (S7-09 is about rollbacks and alarms).
- **A demoted node that was reached by `promote_node`**: it refuses `check_cluster`,
  `add_node`, `create_distributed_table` and the detector ("only on the map group
  primary"), its recovery pass does nothing, a distributed write goes to the new primary
  (the old primary's shard tables did not change), a transaction that had written locally
  there before the promotion is refused at commit with a clear message, and prepared
  statements that were planned before the promotion work after it.
- **Privileges**: 21 new and internal functions and the new table are refused to an ordinary
  role (`permission denied`), `kingres.dtx_incident` cannot be read or written by one,
  `add_node` with a quote in the host name or in the role only stores or refuses it (no
  SQL is built from the host).
- **Standby registration states**: a standby that cannot be reached is registered with a
  warning and shown as `standby_unreachable`; one promoted outside the extension is
  shown as `standby_not_in_recovery`; `remove_node` removed both.
- **Version dependence**: outside `pg_compat.h` the only test is `PG_MAJORVERSION_NUM`
  in `fault.c` (it reports the major); both majors gave the same result for every
  experiment above and for the suite.

## Not reproduced (suspected, with what would confirm)

- **Orphaned old-format identifiers.** An old-format gid names a node; if that node's
  row has been removed (`remove_node` of the old primary after it was rebuilt), the
  group of the origin is unknown and the identifier is never resolved. Would need a
  0.4 transaction left prepared across the upgrade and the removal of its origin.
- **Soft edges in the wait graph.** `pg_blocking_pids` returns the backends queued
  ahead of a waiter; a cycle through such an edge across nodes might be one that
  PostgreSQL would reorder away locally. Would need a queue with a request that is
  compatible with the holders; I found no way to make one stable for 300 ms.
- **A restore under load, repeated.** I read the barrier logic (the counter of
  transactions in flight is per origin node and counted in `precommit` for every
  two-phase commit, which includes catalog changes and the switch of a move) and found
  no gap except S7-08; I did not repeat the restore of three servers that `t/053` does.
- **Cursors held open across a promotion, rescans, `COPY`.** By reading, a cursor keeps
  the fragment sessions it opened (to the old primary, a consistent snapshot of
  before the promotion) and a fragment opened afterwards goes to the node the plan
  names, which is refused if it is demoted; not run.

## Not covered

- Promotion concurrent with `add_node`, `remove_node`, a bucket move in each phase,
  a reference-table write, DDL on a distributed table, another `promote_node`: by reading,
  all take the catalog lock (`kingres_shard_lock_catalog`) and serialize; not run.
  `t/054` (failover of the source and of the target in each phase of a move) passed on
  both majors.
- The tests that restart a promoted standby as a map group primary are those of
  `t/049`; I ran them, I did not vary them beyond S7-04 to S7-06.
- A standby registered under the wrong group, a copy of another group, a standby that
  is behind: by `t/048` only. Reads on a hot standby: not run.
- The control plane (`kingresd`), the in-tree build on 19, and the web panel.
- A server shutdown (smart or fast) during the commit wait of S7-01: by reading, the
  same code path; not run.

## Summary of the verdicts

README guarantees: 1 held, 2 held, 3 held (partly by suite), 4 held, 5 held, 6 held,
7 broken, 8 held (partly untested), 9 broken, 10 held, 11 broken in two cases (held at
the six points), 12 held, 13 broken, 14 held, 15 held with S7-08, 16 held.

Findings: critical S7-01; high S7-02 to S7-07; medium S7-08, S7-09; low S7-10.
