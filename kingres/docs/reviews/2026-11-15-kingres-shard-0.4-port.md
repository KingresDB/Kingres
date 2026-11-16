<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review: contrib/kingres_shard 0.4 as ported to PostgreSQL 18 and 19 (commits 9944f08d01 and d504f44464)

Reviewer: independent (not the author of the code, not the reviewer of the earlier
rounds). Reviewed state: commit d504f44464 on top of 9944f08d01, that is
`git diff 37293a7e77 d504f44464 -- contrib/kingres_shard`, exported with `git archive`
into a private area (`~/kw-review7`); the working tree of the repository (which holds
the unfinished 0.5) was not used for any result. The review of 0.4
(`2026-11-15-kingres-shard-0.4.md`) gave S5-01 to S5-06 and four rows marked "partly"
(S4-06, S4-10, S3-14, S3-17); they are answered below. New findings are numbered S6-nn.

## Scope and method

Read: the commit message of `9944f08d01`; the whole diff of the source files
(`pg_compat.h`, `node_conn.c`, `dist_scan.c`, `fanout.c`, `planner.c`, `utility.c`,
`fault.c`, `node_admin.c`, `bucket_move.c`, `catalog.c`, `remote_scan.c`); the diff of the
SQL scripts (0.2-to-0.3, 0.3, 0.3-to-0.4, 0.4); the review of 0.4 (S5-01 to S5-06 and
the four "partly" rows); ADR 0023; the README sections on majors, reference tables and
restoring a node; `t/046` and `t/047`; the header `libpq/libpq-be-fe.h` and the helpers of
the PostgreSQL 19 installation. Beyond the diff I read every place that holds, frees or
returns a libpq result in all nine files named in the task (the counts: about 150
mentions of `PGresult` or `PQclear`, every `PG_TRY` around one, the one memory-context
callback `ds_reset_callback`, every early return on an error path, the abort and
subtransaction-abort callbacks of `node_conn.c`, the use of SPI contexts around
results). I did not read `forward_auth.c` beyond grepping it (it holds no result) or
`shippable.c` line by line.

Built and ran, on PostgreSQL 18.7 and 19rc2 (private copies of the two installations that
have assertions; both `pg_config` report paths inside the area; the copies are linked
against the original library directory through a RUNPATH, so every process was started
with `LD_LIBRARY_PATH` set to the copy; the copies carried the scripts of 0.5 from an
earlier install, which I removed so that nothing of 0.5 could take part; the machine was
shared, load average 4 to 9; everything under `nice`, `-j4`):

- **Whole suite, once on each major**, `ext.sh` with `EXT_WERROR=1` from a clean build: no
  compiler warning on either major; 12 of 12 pg_regress tests (including `upgrade`,
  `version`, `utility_guard`) and 47 TAP files with 5827 subtests passed on 18.7 and on
  19rc2 (837 s each, run at the same time). `t/046` is skipped in that run (it needs the
  other installation named by `KINGRES_PG18` / `KINGRES_PG19`); I ran it on its own with
  both named, once under each major: 20 of 20 subtests passed each time, with a real
  18.7 node and a real 19rc2 node. 5827 plus the 20 of `t/046` is the 5847 of the
  commit message.
- **`t/031` with `KS_INTERLEAVINGS=full` on 19rc2** (the authors ran it on 18 only), alone:
  854 of 854 subtests passed (391 s). 854 is the number the review of 0.4 got on 18.
- **A sanitizer run, 19rc2.** The extension compiled with `-fsanitize=address -O1 -g`
  (all sixteen files, no warning), `libasan` preloaded into the postmaster of a 19rc2
  installation (`detect_leaks=0`). The server itself is not instrumented, so what is
  checked is every `malloc`/`free` pair of the process (libpq results and the blocks of
  the memory contexts, which are malloc blocks) for double free, and every access made by
  the instrumented extension code for use after free. Sixteen TAP files that touch the
  result handling (`t/007 008 009 013 014 016 018 022 025 026 028 036 037 039 040 043`,
  3689 subtests) passed with no sanitizer report; my own scenarios (below) ran under it
  with no report except the one of S6-02, which it found at once.
- **A real cluster of 18 and 19**: three nodes started from `pg_upgrade` of node 3 from
  18.7 to 19rc2 and restarted (see S6-09), and the 18/19 pair of `t/046`.
- **A real version skew**: the extension of the commit 6ae61063eb (0.3) built into a copy of
  the 18.7 installation (library and SQL objects of 0.3), a cluster started with it, node 1
  then restarted with the 0.4 library and updated; then the other way (0.4 library
  everywhere, 0.3 SQL objects on nodes 2 and 3).
- **A base build** of 37293a7e77 into a second copy of 18.7, to tell what this change
  broke from what it inherited (used for S6-01 and S6-02).
- **About 50 scripted experiments of my own** on three-node clusters (the clusters stay
  up between experiments; ports 54100, 54200, 54300, 54400, 54500, 54600). The ones about
  memory ownership: a scan whose statement fails in the middle (a shipped expression that
  divides by zero on the remote node; one that divides by zero on the origin), 1500
  to 2000 repeats in one backend each; `pg_terminate_backend` and `pg_cancel_backend` of
  the participants' backends and of the origin's own backend while scans run (45 s of
  killing against 750 scans); `statement_timeout`; fault points of the scan
  (`scan_begin`, `scan_started`, `scan_after_placement`, `scan_declared_g1`,
  `scan_declared_g2`, `scan_after_declare`, `scan_after_check`, action `error`; 1260
  failing statements per major); a `LIMIT 3` scan, a lateral scan rescanned 20 times per
  statement, a cursor closed early and one abandoned at commit, a semi-join, a union of
  limits, savepoints released and rolled back; reference-table writes under every fault
  point of the two-phase commit (`before_prepare`, `after_first_prepare`, `after_prepare`,
  `after_log`, `after_commit`, `between_commit_prepared`, action `error`; 1200 per major)
  and the same with ordinary commits; 40 bucket moves one after the other in one session;
  a bucket move cancelled at each phase. For each of these the anonymous memory of the
  backend (`RssAnon` from `/proc`, not `VmRSS`, which counts shared buffers it touched)
  and the contexts of `pg_backend_memory_contexts` were read before and after.

Not done, or not fully: see "Not covered".

## Status of the S5 findings and of the "partly" rows

Verdicts: fixed / partly / not fixed (not reproduced for S5-04).

| ID | Verdict | How I checked |
|---|---|---|
| S5-01 | partly | The original (a trigger made by hand with `enable_routing = off`): `CREATE TRIGGER` and `CREATE CONSTRAINT TRIGGER` on a reference table are refused, also by a superuser with routing off, on both majors; `create_reference_table` of a table that already has a trigger is refused ("trigger "t" is not supported yet"). Nested statements of a write are planned and checked like statements of their own. A user statement cannot imitate the foreign-key exception to any effect: `PERFORM 1 FROM ONLY d WHERE ...` in a function called from a check constraint or a generated column of a reference table begins with the text of the exception and is planned locally on the origin, but on every other node (a forwarded session) the same statement is refused ("cannot prove that the statement touches only buckets of this node", or "bucket 5 of public.d is not on this node"), so the write fails and nothing is written. Not closed: a trigger on an inheritance child of a reference table (S6-03), `session_replication_role = replica` (S6-04), a generated column with a function (S6-05). And the exception is wrong for foreign keys from a distributed table to a reference table: the fix breaks them (S6-01 below is a regression of it). |
| S5-02 | fixed | A real skew with the 0.3 library on nodes 2 and 3 and 0.4 on node 1: `node 127.0.0.1:54502 runs kingres_shard 0.3, this node runs 0.4`, with the node's own message as detail and the hint to run `ALTER EXTENSION ... UPDATE`. With the 0.4 library and 0.3 SQL objects on the other nodes: `check_cluster()` reports `extension_version_differs` for both nodes and nothing else about them (no `reference_table_differs`, the false alarm of the review is gone); `move_bucket` says the node has 0.3 and a move needs 0.4. What `check_cluster()` and `move_bucket` say in the real binary skew is S6-06 (low). |
| S5-03 | fixed | An ordinary role gets "permission denied for the internal functions of kingres_shard" from `internal_partition_identity` for `public.d` and for `pg_class`, and NULL for NULL, on both majors. The other helpers that an ordinary role can execute refuse it too (`internal_map_version`, `internal_placement_hash` in both forms, `internal_lock_reference_table`); `internal_catalog_snapshot` answers it but returns only what the role can already read in `kingres.node`, `kingres.bucket_map` and the other catalog tables (the secret is not in it). |
| S5-04 | not reproduced | `after_commit:wait` on the origin, `SIGSTOP` of the postmaster and all children of one participant, `node_timeout = 3`: the session returned after 4.0 s with one warning, naming the stalled node (the one with the prepared transaction); the healthy node showed the row and no prepared transaction. Done with node 3 stopped and with node 2 stopped (the order of the answers), on 19rc2 and on 18.7: no false warning in four runs. After `SIGCONT` the stalled node had already committed (the request was in the socket). No code changed, none was needed as far as I can tell; the reviewer of 0.4 may have seen a warning that I could not provoke. |
| S5-05 | fixed | A move held at each point of its protocol with `<point>:wait:<file>`, `cancel_bucket_move` called while it is held, the file removed: `move_after_preparing` (preparing), `move_after_target_tables`, `move_after_publication`, `move_after_slot`, `move_after_subscription` (copying), `move_after_catchup` (catching_up): the cancel prints "bucket move N was stopped", the move ends with "bucket move N was cancelled", the bucket stays active on the source group, no slot, no publication, no subscription is left, all 3000 rows are there. At `move_before_switch` (switching) the cancel is refused ("is switching and cannot be cancelled") and the move completes. Both majors. A cancel that arrives before the move is registered is "already cancelled" or completes with the move, both truthful. |
| S5-06 | partly | `ADD COLUMN ... DEFAULT now()` is refused with "ALTER TABLE is not supported on distributed tables yet" / "on reference tables yet" (the right noun, same detail); `REPACK` and `CLUSTER` say the same. A node without a secret is reported as `secret_missing` and `sync_catalog()` repairs it (both majors). The README section on restoring a node is added, but its first step is not what a dump does (S6-08). |
| S4-06 (partly) | fixed | The three helpers the row names are now refused to a role without a privilege (see S5-03); a role that may read the table may still ask for its placement hash, as the row accepted. |
| S4-10 (partly) | partly, unchanged | `cleanup_moved_buckets()` removed the 43 (19rc2) and 40 (18.7) leftover orphans of my moves and the phases became `done`; `check_cluster()` stayed empty. The worker's own cleanup was not run (the clusters had `recovery_interval = 0`). The comparison of distributed tables is still structural; the commit did not touch it. |
| S3-14 (partly) | partly, unchanged | `exp(f)` and `f ^ 2` are refused for a write to a reference table; `sqrt`, `*` and `round` are shipped and gave equal copies on every node of both majors. |
| S3-17 (partly) | partly, unchanged | `rotate_secret()` works on both majors and a scan, a point write and a reference write work after it. The replay of a token without TLS remains, as documented. |

## New findings

| ID | Severity | Summary |
|---|---|---|
| S6-01 | high | The fix of S5-01 refuses every foreign-key action of a distributed table that references a reference table: with the default `NO ACTION`, any `DELETE` or key `UPDATE` of the reference table fails, whatever the row; `ON DELETE CASCADE`, `SET NULL` and `ON UPDATE CASCADE` fail as well. It worked at 37293a7e77 |
| S6-02 | high | A statement that fails while a distributed scan has its next batch in flight, inside a subtransaction (a `ROLLBACK TO SAVEPOINT`, a PL/pgSQL `EXCEPTION` block), leaves the connection pointing at freed memory; the next statement that needs the node crashes the backend (use after free, both majors, and in 37293a7e77) |
| S6-03 | medium | A trigger on an inheritance child of a reference table (made by hand with routing off, as the README tells a superuser to repair things) runs on every node on a write to the parent and the copies diverge with no error: the refusal of S5-01 has a gap |
| S6-04 | medium | A superuser's `session_replication_role = replica` is not sent to the other nodes: a write to a reference table with a foreign key action skips the action on the origin and runs it on the others; the copies diverge with no error |
| S6-05 | low | A stored generated column of a reference table runs a user function on every node (the shippability check does not look at it); a function that is declared `IMMUTABLE` and reads node-local state gives different copies with no error |
| S6-06 | low | With a real 0.3 binary on the other nodes, `check_cluster()` says `node_unreachable` ("lost the connection") and `move_bucket` says the version "cannot be asked: lost the connection"; neither names the version |
| S6-07 | low | The `RESET check_function_bodies` added to the 0.3 scripts resets the setting of the user's session instead of putting back what it was |
| S6-08 | low | The README's first step for restoring a node is wrong: the dump of a node re-creates the extension |
| S6-09 | note | A node upgraded in place and restarted joins a mixed cluster with no warning at run time; scans, writes and two-phase commit work across 18 and 19 in everything I tried; only `check_cluster()` says so |
| S6-10 | note | Small points: a version macro outside `pg_compat.h`; the `upgrade` test compares less than it says; "kingres_shard an older version" |

### S6-01 (high) The fix of S5-01 breaks foreign keys from a distributed table to a reference table

Where: `fanout.c`, `is_ri_action()`. The fix lets through only statements whose text
begins with `SELECT 1 FROM ONLY `, `UPDATE ONLY ` or `DELETE FROM ONLY `, "because every
statement of ri_triggers.c names its table with ONLY". It does not: `ri_triggers.c` leaves
`ONLY` out when the referencing table is a partitioned table, and a distributed table is
one. So the statements of the actions of a foreign key from a distributed table to a
reference table are not recognised, and are planned, routed and refused as statements of
the user. The README says such a foreign key is allowed ("A foreign key from a distributed
table to a reference table is allowed", and in the list of supported `ADD CONSTRAINT`); the
tests of 0.4 only have foreign keys between reference tables (`t/043`, `t/044`), which have
`ONLY`, so 5847 subtests do not see it.

Reproduction (three nodes; psql on node 1; the same on 18.7 and 19rc2):

```
CREATE TABLE public.rpar (id int PRIMARY KEY);
SELECT kingres.create_reference_table('public.rpar');
INSERT INTO public.rpar VALUES (1), (2), (3);
CREATE TABLE public.dchild (id int PRIMARY KEY, pid int);
SELECT kingres.create_distributed_table('public.dchild', 'id', bucket_count => 12);
ALTER TABLE public.dchild ADD FOREIGN KEY (pid) REFERENCES public.rpar(id);
INSERT INTO public.dchild VALUES (1, 1);
DELETE FROM public.rpar WHERE id = 3;        -- nothing references 3
UPDATE public.rpar SET id = 30 WHERE id = 2; -- nothing references 2
```

Observed: both statements fail,

```
ERROR:  row-level locking on a statement that spans shard groups is not supported yet
CONTEXT:  SQL statement "SELECT 1 FROM "public"."dchild" x WHERE $1 OPERATOR(pg_catalog.=) "pid" FOR KEY SHARE OF x"
          SQL statement "/* kingres:reference-write */  DELETE FROM public.rpar WHERE (id = 3)"
```

With `ON DELETE CASCADE`: `ERROR: cannot prove that the statement touches only buckets of
this node ... SQL statement "DELETE FROM "public"."dchild" WHERE $1 OPERATOR(pg_catalog.=)
"pid""`, `remote node ...`; with `ON DELETE SET NULL` the same for `UPDATE "public"."dchild"
SET "pid" = NULL WHERE ...`; with `ON UPDATE CASCADE` the cascade statement `UPDATE
"public"."dchild" SET "pid" = $1 WHERE $2 OPERATOR(pg_catalog.=) "pid"` runs on a remote
node before that node's copy of the parent has the new key and fails with a foreign-key
violation ("Key (pid)=(10) is not present in table "rpar"").
Nothing is written (the writes roll back), so the failure is loud; but no row of a
reference table that a distributed table references can be deleted or have its key
changed. At 37293a7e77 (built as a second copy): the unreferenced row is deleted, the
referenced one is refused by the foreign key ("Key (id)=(1) is still referenced"), and
`ON DELETE CASCADE` removed the two children (3 of 5 rows left); `ON UPDATE CASCADE`
changed the children's key (`1->10, 2->10`) and `ON DELETE SET NULL` nulled it.

What should happen: as before. Suggested fix: recognise the shapes of ri_triggers.c with
and without `ONLY` (`SELECT 1 FROM "s"."t" x WHERE ... FOR KEY SHARE OF x`, `DELETE FROM
"s"."t" WHERE $1 OPERATOR(pg_catalog.=) "col"`, `UPDATE "s"."t" SET "col" = ... WHERE ...`)
when the table is a partitioned referencing table, and add a distributed-to-reference
foreign key with each action to a test. A sturdier fix identifies the statement by its
origin (the executor is inside a foreign-key trigger) and not by its text.

### S6-02 (high) A caught error during a distributed scan crashes the backend later

Where: `node_conn.c`, `conn_subxact_callback` (and `ds_reset_callback` in `dist_scan.c`).
A distributed scan asks for its next batch before the executor needs it, so between two
calls of the executor, and when an error ends the statement, a `FETCH` is in flight on the
connection and the connection points at the scan's state (`pending_owner`, which lives in
the memory of the statement). At the abort of the whole transaction `abort_connection`
cancels the query and closes the connection, which forgets the pointer. At the abort of a
subtransaction the callback sees `in_query`, sets `have_error`, and `continue`s: the
connection stays open, the pointer stays set, the remote savepoint level is not left
(`xact_depth` is not reduced). When the statement's memory is freed the pointer dangles,
and the next thing that uses the connection (`kingres_shard_get_conn` calls
`kingres_shard_conn_quiesce`) calls the finish function with it. `ds_reset_callback` does
not help: on 18 it only clears results, on 19 it returns at once (the new early return),
and neither touches the pointer.

Reproduction (three nodes, a distributed table `d` with 3000 rows, one `INSERT` each; the
same on 18.7, 19rc2 and, as built here, 37293a7e77):

```
SET kingres_shard.scan_fetch_size = 10;   -- also crashes with the default size
BEGIN;
SAVEPOINT s;
SELECT id, 100/(v-2500) FROM public.d ORDER BY id;   -- ERROR: division by zero
ROLLBACK TO s;
SELECT 1;                                  -- works
SELECT count(*) FROM public.d;             -- the server process is killed (signal 11)
```

Observed: "server closed the connection unexpectedly"; the log has `client backend ... was
terminated by signal 11`, and every other session of the server is disconnected while it
recovers. Variants, all on both majors: the same in a PL/pgSQL block with an `EXCEPTION`
clause around `FOR r IN SELECT id FROM public.d LOOP PERFORM 1/0; END LOOP;` followed by
any statement that needs another node (first the error "connection to node ... is in an
unknown state", then the crash on the next try); the block alone in an autocommit `DO`
(no crash, but the `DO` fails at its own commit with "unexpected nested remote transaction
at commit" after it handled the error); a cursor opened inside the savepoint and rolled
back (the next remote statement fails with "connection to node ... is in an unknown
state. Roll back the transaction and try again"). With the extension built with
`-fsanitize=address` and `libasan` preloaded the first variant reports at once:

```
ERROR: AddressSanitizer: heap-use-after-free ... READ of size 1
    #0 frag_collect  src/dist_scan.c:859
    #1 ds_finish_pending  src/dist_scan.c:898
    #2 kingres_shard_conn_quiesce  src/node_conn.c:639
    #3 kingres_shard_get_conn  src/node_conn.c:518
    #4 ds_start  src/dist_scan.c:1015
freed by: AllocSetReset (the memory context of the failed statement)
```

What should happen: the error is handled, the transaction continues, the next statement
runs. Suggested fix: in the subtransaction abort callback, when a query is in flight or
`pending_owner` is set, drain or cancel it and clear `pending_owner` (as
`abort_connection` does for a whole transaction) and put the remote savepoint level right
(`ROLLBACK TO SAVEPOINT` after the drain), and make `ds_reset_callback` on every major
clear the pending owner of each fragment that is still in flight. A test with the
sequence above, once with the error in the scan and once in the body of the loop, on both
majors. This is not introduced by the port, but the port is where the callback was
rewritten and the new early return makes the callback unable to fix it on 19.

### S6-03 (medium) A trigger on an inheritance child of a reference table

The fix of S5-01 refuses `CREATE TRIGGER` on a table that is a reference table. A table
that inherits from one is not a reference table, so a trigger on it is accepted, and the
child's rows take part in every write to the parent (`UPDATE parent` updates the child's
rows and fires its triggers). `CREATE TABLE ... INHERITS (reference table)` is refused
with routing on; the refusal tells a superuser to run the statement by hand on every
node with routing off, which is how the review of 0.4 made its trigger.

Reproduction (18.7 and 19rc2; three nodes; `r2 (id int PRIMARY KEY, seen int)` is a
reference table; the next block as superuser on every node):

```
SET kingres_shard.enable_routing = off;
CREATE FUNCTION public.trg() RETURNS trigger LANGUAGE plpgsql
  AS $$ BEGIN NEW.seen := inet_server_port(); RETURN NEW; END $$;
CREATE TABLE public.rc () INHERITS (public.r2);
CREATE TRIGGER t BEFORE INSERT OR UPDATE ON public.rc FOR EACH ROW EXECUTE FUNCTION public.trg();
INSERT INTO public.rc VALUES (50, 0);
-- node 1, routing on:
INSERT INTO public.r2 VALUES (1, 0);
UPDATE public.r2 SET seen = 1;
```

Observed: the `UPDATE` succeeds; `SELECT * FROM public.r2` gives `1|1`, `50|<port of that node>`
on each node, so three different values; `check_cluster()` reports
`reference_table_differs` for nodes 2 and 3. What should happen: the write is refused,
or the copies are equal. Suggested fix: refuse the creation of an inheritance child or a
trigger on a table that inherits from a reference table (also with routing off), or
refuse a write to a reference table that has any child; the same for partitions
attached to one.

### S6-04 (medium) `session_replication_role = replica` is not sent with the write

`session_replication_role` is a superuser's setting that turns off the triggers of a
session, foreign-key actions included. The fragment of a write to a reference table runs
in a session of each other node that has the default role, so the origin skips what the
others do.

Reproduction (18.7 and 19rc2; superuser on node 1; `rparent (id PRIMARY KEY)` and
`rchild (id PRIMARY KEY, pid int)` are reference tables, then `ALTER TABLE rchild ADD
FOREIGN KEY (pid) REFERENCES rparent(id) ON DELETE CASCADE`; rows `rparent` 1, 2 and
`rchild` (10,1), (20,2)):

```
SET session_replication_role = replica;
DELETE FROM public.rparent WHERE id = 1;
```

Observed: no error; `rparent` has 2 on every node; `rchild` has `10->1, 20->2` on node 1
and `20->2` on nodes 2 and 3; `check_cluster()` reports `reference_table_differs` for
nodes 2 and 3. What should happen: the same on every node, or a refusal of a write to a
reference table in a session with the role `replica`. Suggested fix: refuse (or send the
setting to the other nodes) when `session_replication_role` is not `origin` and a
reference table is written; say so in the README table of paths.

### S6-05 (low) A generated column runs user code on every node

A stored generated column (`GENERATED ALWAYS AS (f(...)) STORED`) is computed by the
executor of each node; the shippability check looks at the statement, not at the table's
generation expressions. PostgreSQL only demands `IMMUTABLE`, which is a promise, not a
check. A function that is declared `IMMUTABLE` and reads node-local state gives each copy
a different value. Reproduction (both majors): `CREATE FUNCTION public.who() RETURNS int
LANGUAGE sql IMMUTABLE AS $$ SELECT inet_server_port() $$;` on every node, `CREATE TABLE
public.r1 (id int PRIMARY KEY, seen int GENERATED ALWAYS AS (public.who()) STORED);
SELECT kingres.create_reference_table('public.r1'); INSERT INTO public.r1 VALUES (1);`
gives `1|<port>` with a different port on each node, no error, `check_cluster()` reports
the difference. It needs a function that lies, so it is low; but it is the same effect as
the trigger of S5-01 and the README says a write "reaches every copy or is refused". A
function that reads a distributed table is refused on the other nodes (forwarded
session), which I checked. Suggested fix: refuse, at `create_reference_table` and at
`ALTER TABLE` of a reference table, a generation expression that is not shippable by the
rules of a write; or document the rule next to the trigger refusal.

### S6-06 (low) A real 0.3 binary is reported as unreachable

With the 0.3 library on nodes 2 and 3 and 0.4 on node 1 (`ALTER EXTENSION UPDATE` run
there), a scan or a reference write from node 1 names the versions (S5-02 is fixed for
that), but

```
SELECT check_name, node_id, object, detail FROM kingres.check_cluster();
  node_unreachable|2|127.0.0.1:54502|lost the connection to node 127.0.0.1:54502
  node_unreachable|3|127.0.0.1:54503|lost the connection to node 127.0.0.1:54503
SELECT kingres.move_bucket(1, 1, 1);
  ERROR: node 2 (127.0.0.1:54502) cannot be asked for its kingres_shard version: lost the connection ...
```

The nodes are reachable; the question about the version goes through the session that the
0.3 node refuses ("claims to have been opened by another node ... but cannot prove it"),
which `kingres_shard_report_error` knows how to explain. What should happen: the same
sentence as for a scan. Suggested fix: in `check_cluster` and
`internal_check_node_versions`, ask for the version the way `remote_extension_version`
does (a session that claims nothing) and report `extension_version_differs`.

### S6-07 (low) The 0.3 scripts reset the session's `check_function_bodies`

The change brackets one function in `kingres_shard--0.3.sql` and `kingres_shard--0.2--0.3.sql`
with `SET LOCAL check_function_bodies = off;` and `RESET check_function_bodies;`. `RESET`
puts back the default, not the value the session had. Reproduction (19rc2; the 0.4
install is not affected, only a path that runs a 0.3 script):

```
SET check_function_bodies = off;
CREATE EXTENSION kingres_shard VERSION '0.2';
ALTER EXTENSION kingres_shard UPDATE TO '0.3';
SHOW check_function_bodies;      -- on
```

A restore that sets `check_function_bodies = off` and then updates the extension through
0.3 continues with `on`. Suggested fix: use only `SET LOCAL` (the transaction of the
script ends with the script), or save and restore with `set_config`.
What is created on 18 is the same as before the change: I installed 0.3 from the base
commit and from this commit on 18.7 and compared the definition (`pg_get_functiondef`)
of every function of the schema, their dependencies and the number of members of the
extension: identical (see "Examined and found sound").

### S6-08 (low) Restoring a node: the first step of the README

Step 1 says to restore into a database that has no `kingres_shard`, so that "the
partitions come back as plain tables", and to run `CREATE EXTENSION` only if the node is
to be a new node. The dump of a node contains `CREATE EXTENSION IF NOT EXISTS kingres_shard`
(a dump of my node 2 has it at line 26 and also has the `kingres.orphan_*` tables of
moves, which are not members of the extension), so the restore creates the extension
(without a catalog, as the text then needs) whatever the reader does. Restore into a
database with the extension and no catalog worked without an error (the partition had its
266 rows). I did not follow steps 2 and 3 to the end (see "Not covered"). Suggested fix:
say that the dump creates the extension, and what to do about the leftover
`kingres.orphan_*` tables.

### S6-09 (note) A node upgraded in place joins a mixed cluster silently

`add_node` is the only place that looks at the major of a node at registration, and
`move_bucket` and `check_cluster` look at it when run. A node of an existing cluster that
is upgraded to the other major (I used `pg_upgrade` of node 3 of a three-node 18.7 cluster
to 19rc2 and restarted it with the same preloaded library) goes on serving: a count and a
sum over a distributed table, a distributed `UPDATE`, a join with a reference table,
a write to a reference table with delete, the catalog, the secret, all gave the
expected answers across the three nodes. `check_cluster()` reported
`server_major_differs` for node 3, and `move_bucket` refused every move between node 3 and
a node of the other major ("they run different PostgreSQL major versions (18 and 19)") but
allowed one between nodes 1 and 2 of the same cluster. I looked for a wrong answer and did
not find one. A standby of another major cannot exist (physical replication), and I found
no other way to register a node of another major than the `fault_injection` setting
that the test uses (which a superuser can set, as the file says). `sync_catalog` does not
change the set of nodes. A move that is running while the target is upgraded loses its
control connection and is undone by the session; the check at the start of a move is not
repeated. Nothing is wrong that I could show; the statement "one major per cluster" holds at
registration and is reported, not enforced, afterwards. If it is to be enforced, the
connection of a node (`kingres_shard_get_conn`) could compare `PQserverVersion` with the
major of this node when it opens.

### S6-10 (note) Small points

- `fault.c` uses `PG_MAJORVERSION_NUM` (the major that `add_node` compares with); ADR 0023
  says code outside `pg_compat.h` does not test the version. It is a constant, not a
  test, and `fault.c` is where the override lives, so I count it as a note.
- The `upgrade` regression test builds a snapshot of the schema (columns, constraints,
  indexes, functions with a digest of their bodies, privileges, triggers, event
  triggers, comments, members) and compares fresh 0.4 with each upgrade path. It does not
  compare view definitions, policies, owners, column privileges, sequence parameters,
  event-trigger tags or defaults of columns beyond the `default=` text. I wrote a
  snapshot that does (643 lines: all of those plus the dependencies of the functions) and
  found no difference between a fresh 0.4 and 0.1, 0.2 or 0.3 updated to 0.4, on either
  major, nor between fresh 0.4 on 18 and on 19. So the claim is true, but the test would
  not notice a difference in a view.
- `check_cluster()` says "the node runs kingres_shard an older version" for a node whose
  state function has no version (`COALESCE(..., 'an older version')` in a sentence that
  already says "kingres_shard %s").

## Examined and found sound

- **Result ownership on 19, by reading.** Every holder of a result was followed to its
  clear: `f->res` of a scan (the only result kept across calls) is made by `frag_collect`
  in the query's context, which is what the statement's scan lives in; the early return
  in `ds_reset_callback` is correct (the wrapper callbacks, registered later, run first,
  and no other place stores a result in `f->res`); `frag_drain` restores the context on
  its caught error; the abort and subtransaction-abort callbacks make their results in
  the aborting transaction's context and clear them before they return; `ctl_run`,
  `store_result`, `store_remote_rows` and `fetch_remote` clear in `PG_FINALLY` or before
  the error; `kingres_shard_report_error` copies the fields before it clears; no result
  is kept across `SPI_finish()`; `recover_prepared_transactions` makes and clears its
  results inside one loop body. The wrapper accessors are NULL-safe (`PQntuples(NULL)`
  and the others), so the `res == NULL` paths of 18 behave the same on 19. I found no
  double free, no double clear and no use after free of the three classes on 19, and the
  sanitizer run found none either. S6-02 is a use after free of a different object (the
  scan state), not of a result.
- **Memory on both majors.** Every scenario of the method above left the anonymous memory
  of the backend flat after the first few hundred statements (for example 3024 to
  3340 KB and no further growth over 1500 failing scans on 19rc2; 2844 to 3160 KB on
  18.7), with no growth of `pg_backend_memory_contexts` beyond the first loading of the
  catalog caches; 40 consecutive bucket moves in one session: 3024 to 6620 KB once,
  then flat; 40,000 single-row updates in one function call: +600 KB on both. The
  apparent growth of `VmRSS` of about 0.5 KB per two-phase commit is shared buffers
  touched, not a leak (`RssAnon` is flat).
- **Behaviour on 18.** Every wrapper of `pg_compat.h` is an identity on 18
  (`KS_TABLE_BEGINSCAN`, `ks_result_relations`, `KS_CLEAR_RESULT_RELATIONS`,
  `KS_T_CLUSTER_STMT`, the planner argument macros); the types of the results are the
  plain ones, so nothing is freed differently. The suite and my scenarios gave the
  same results on 18.7 as before the change, apart from what S6-01 changes on both.
- **The planner hook on 19.** `EXPLAIN`, `EXPLAIN (ANALYZE, VERBOSE, BUFFERS)`,
  `EXPLAIN (PLAN_ADVICE)` (the 19 module that uses the explain state), `EXPLAIN ANALYZE`
  of an `INSERT`, `UPDATE` and `DELETE`, a prepared statement explained twice, with
  `pg_stat_statements` (with planning tracked), `pg_plan_advice` and `auto_explain`
  (nested statements logged, with `log_analyze`) loaded before `kingres_shard` in
  `shared_preload_libraries` (that one order only): correct plans and answers, no error. A function that
  plans again while the outer statement is planned (an immutable PL/pgSQL function that
  reads a distributed table, folded during planning; `WHERE v = f(5)`) gave the right
  row. An error during planning (`WHERE id = 1/0`) followed by an `EXPLAIN (PLAN_ADVICE)`
  worked: the saved state is restored by the `PG_FINALLY`.
- **Statements that 19 added or changed.** The node tags that differ between 18.7 and
  19rc2 are `RepackStmt` (replaces `ClusterStmt`), `WaitStmt`, `PublicationAllObjSpec`
  and executor or planner internals. `REPACK` on a distributed table, with `USING INDEX`,
  with `(CONCURRENTLY)`, on a bucket partition, and on a reference table is refused with
  "REPACK is not supported on distributed/reference tables yet"; `CLUSTER` the same;
  `REPACK` without a table, `CLUSTER` without a table and `VACUUM FULL` pass, as
  `CLUSTER` and `VACUUM FULL` do on 18 (they rewrite this node's tables and nothing
  else). `WAIT FOR` and the publication spec do not touch a table of the user. The new
  `INSERT ... ON CONFLICT DO SELECT` of 19 (with `FOR UPDATE`, `WHERE`, `RETURNING`) on a
  distributed table (local and remote key, several rows refused as before) and on a
  reference table gave the right rows.
- **The SQL scripts.** Fresh 0.4 against the update from 0.1, 0.2 and 0.3, on both majors,
  with my snapshot (see S6-10): no difference; fresh 0.4 on 18 against on 19: none. 0.3
  installed from the base and from this commit on 18.7: the definition of every function,
  the dependencies and the member count are identical (`md5` of the concatenated
  definitions equal; 164 dependencies, 109 members), the session setting of
  `check_function_bodies` aside (S6-07). 0.3 installed on 19rc2:
  `kingres.internal_move_subscription_status` fails with `column st.sync_error_count
  does not exist` even for a subscription that does not exist, until the update to 0.4
  (the README says so).
- **Version and majors at registration.** `add_node` of a node of the other major is
  refused with the two majors named (`t/046`, both ways); a cluster of an 18.7 node and
  a 19rc2 node made with the test's fault setting is reported by `check_cluster()`;
  `move_bucket` between them is refused in both directions and records nothing.
- **Reference-table writes by path after the change.** A write that calls a function
  that is not built in is refused; `ON CONFLICT DO UPDATE` and `DO NOTHING`, a prepared
  statement and `EXPLAIN ANALYZE` of a write behave as in 0.4. Foreign keys between
  reference tables (the `ONLY` shape): `t/043` and `t/044` pass; I did not run my own.
- **Two-phase commit and recovery under faults.** 1200 transactions per major with an
  `error` at each of the six commit points, then `recover_prepared_transactions()`: no
  prepared transaction left on any node, the same row count on every node, no growth.
- **`rotate_secret()`** on both majors followed by a scan, a point write and a reference
  write.

## Not reproduced (suspected, with what would confirm)

- **A wrong answer in a mixed 18/19 cluster.** None found; a function whose result differs
  between 18 and 19 and is shipped in a predicate would show it. To confirm: evaluate
  the built-in immutable functions with the rarest behaviour (text search, date
  formatting, collation-dependent `ORDER BY` with an ICU or libc collation) in a
  predicate on a distributed table across an 18/19 cluster and compare with one node.
- **A mixed cluster by a move started before the upgrade and resumed after.** A move is
  not resumed, it is undone; the target's loss of connection ends it. I did not run a
  `pg_upgrade` of the target while a move was held at a fault point.
- **S5-04** (see the table): not provoked in four runs.
- **S6-02 reached through `statement_timeout` or a cancel of the origin** rather than an
  error inside a subtransaction: 750 cancelled scans and a `statement_timeout` loop ran
  without a crash, because the transaction aborted as a whole and `abort_connection`
  did its work; I did not construct the case of a cancel that a PL/pgSQL handler
  catches (it should crash like the division by zero).
- **Nested foreign-key shapes in a write to a reference table from a trigger of a
  distributed table** (a trigger on `dchild` that itself writes a reference table):
  not run.

## Not covered

- The recovery worker's own cleanup of orphans (`recovery_interval` was 0), and the
  background worker's recovery of prepared transactions under the sanitizer.
- `pg_restore` into a running cluster with routing on, steps 2 and 3 of the README's
  restore section, `add_node` of a restored node.
- A real cluster of three 19 nodes through the whole suite under a sanitizer (sixteen of
  the 47 files ran under it); a build of the server itself with the sanitizer (so use of
  freed memory by uninstrumented server code is not seen).
- `kingres_columnar` and `kingresd`; the control plane's use of `check_cluster()`.
- A physical standby of a node, promotion and failover (`t/048` and later belong to 0.5,
  which I did not read).
- TLS between nodes, other locales, a synchronous standby (S3-15).
- `forward_auth.c` and `shippable.c` beyond what the experiments touched.
- A stress of reference-table writers against moves and DDL (as in the earlier review).

## Summary of the verdicts

S5-01 partly, S5-02 fixed, S5-03 fixed, S5-04 not reproduced, S5-05 fixed, S5-06 partly;
S4-06 fixed, S4-10 partly (unchanged), S3-14 partly (unchanged), S3-17 partly
(unchanged).

New: S6-01 high (foreign keys from a distributed to a reference table fail since the fix
of S5-01), S6-02 high (crash after a caught error during a scan), S6-03 medium
(trigger on an inheritance child of a reference table), S6-04 medium
(`session_replication_role = replica`), S6-05 low (generated column), S6-06 low
(skew reported as unreachable), S6-07 low (`RESET check_function_bodies`), S6-08 low
(README restore step), S6-09 note (mixed cluster after an in-place upgrade),
S6-10 note (small points).

Whole suite: 12 of 12 regression tests and 47 of 47 TAP files (5827 subtests) on 18.7
and on 19rc2, no warning under `-Werror`; `t/046` 20 of 20 under each major with both
installations; sixteen TAP files under a sanitizer on 19rc2 with no report;
`t/031` with `KS_INTERLEAVINGS=full` on 19rc2: 854 of 854.
