<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review: contrib/kingres_sketch 1.0 and contrib/kingres_timepart 0.1 (phase 12)

Reviewer: independent (not the author). Reviewed state: commit 346be75abd,
exported with `git archive` (paths `contrib/kingres_sketch`,
`contrib/kingres_timepart`, `kingres/dev`) into a private directory
(`~/kw-review4/src`). The working tree, which other people were editing, was not
used for any result. Line numbers below are those of that commit. Nothing in the
repository was changed except this file.

## Scope and method

Read completely: `kingres_sketch` (`src/kingres_sketch.c`, `kingres_sketch.h`,
`hll.c`, `topn.c`, `tdigest.c`, `kingres_sketch--1.0.sql`, `README.md`) and
`kingres_timepart` (`kingres_timepart--0.1.sql`, all 2115 lines, and
`README.md`). I did not read the test files line by line, nor the two design
documents (`analytics-phase12-queries.md`, `analytics-phase12-storage.md`); every
claim I checked was checked against the READMEs and the code.

Built and run, on PostgreSQL 18.7 (an installation of my own with assertions
enabled, copied into `~/kw-review4/install`) and, for the compatibility checks,
on PostgreSQL 19rc2 (a copy in `~/kw-review4/install19`): `kingres/dev/ext.sh`
`sync build install regress tap` for both extensions on both majors. All
pg_regress suites (6 and 8 tests) passed on 18 and on 19rc2. The TAP suites of
`kingres_sketch` (6 files, 320 tests) and `kingres_timepart` (3 files) passed on
both majors. The machine was shared and loaded (load average about 7), so I ran
everything with `nice` and at most three jobs.

Ran by hand (my sessions ran as a superuser whose name equals the extension
schema `kingres`; this put some of my first test tables into schema `kingres`,
see K12-17; every reproduction below qualifies its names and does not depend on
it):

| Experiment | Runs |
|---|---|
| Own memory-safety fuzzer for the three validators and the C entry points (see below) | 3 processes, 1.26 million inputs, no finding |
| Hash equality between values that compare equal, about 30 types, domains and collations | 1 script |
| Space-Saving guarantees after random partition and random merge order (bounds, N/k, sum of counts, items above N/k present, total) | 300 + 40 random trials |
| HLL error and bias at p = 4, 10, 12, 14, 18 and n = 1 to 10^6, around the sparse/dense switch | 27 settings, 4 to 30 sketches each |
| t-digest: monotone quantile and rank, quantile inside min and max, count, after unions of 2 to 6 digests, on ties, negative, 1e9 outliers and 1e-300 values | 100 random digests |
| Partial aggregation: hash-partitioned table, 2 workers, partitionwise aggregate on, compared with a serial plan, for hll, topn, tdigest, approx_percentile, approx_count_distinct; FILTER; GROUPING SETS; running and sliding window frames; mixed and NULL parameters; zero rows; all-NULL input | 1 script, plus 10 parameter-order probes |
| Stored values: 2 million rows into one 196612-byte hll, a 104 MB topn (compressed and external storage), a 2 MB t-digest, `VACUUM FULL`, binary `COPY` round trip, `pg_input_is_valid` | 1 script |
| Dump on 18, restore on 19rc2: stored sketches of 9 element types compared with sketches rebuilt on 19 | 1 |
| Peak memory and cancellation of `topn_union` on two 104 MB values | 1 |
| Non-owner probes of 11 functions and tables of `kingres_timepart` | 1 script |
| Attacks on the identity checks, the run lock, the search path and user code during a run (K12-08 to K12-11) | 5 scripts |
| Backend terminated at a random moment of a run, then the run repeated | 12 |
| Hand-made, misaligned, default, FK-referenced, FK-referencing and published partitioned tables | 6 tables |
| Skipped calendar days in all 600 or so time zones, 1970 to 2037 (set-based) | 1 |
| Names cut at 63 bytes, calendar limits (year 1 and 9999), `pg_dump` and restore, rename, drop, `DROP EXTENSION` | 1 script each |

The fuzzer (not in the repository): a standalone program built with `gcc -g -O1
-fsanitize=address,undefined -fno-sanitize-recover=undefined` from the sources
of `hll.c`, `topn.c`, `tdigest.c` and `kingres_sketch.c` plus a few dozen lines
that stand in for the backend (`palloc` and friends are `malloc`, so AddressSanitizer
sees the exact size of every allocation, which it cannot inside a memory context;
`ereport` is a `siglongjmp`; `Assert` aborts). It starts from 151 valid values made
by the server (hll precision 4 to 18 sparse, dense and empty; topn capacity 1 to
1000; tdigest compression 10 to 1000; the kind 4 state of `approx_percentile`),
and each input gets 1 to 4 mutations: bit flip, random byte, a 32-bit field set to
a boundary value (0, 1, 2^16, 2^31 - 1, 2^31, 2^32 - 1, 100000, 100001, 20002,
20003, 1024, 1025), a 64-bit field set to a boundary value or a double bit pattern
(NaN, infinities, denormal, largest, -0), truncation, appended or removed bytes,
copied ranges, a wrong kind byte. Every input goes to the validator of its kind
(on an exactly sized heap copy), to `sketch_kind`, and, when valid, to the SQL
entry points through hand-built `FunctionCallInfo` (4-byte and 1-byte varlena
headers): cardinality, union with a second value, fold, add, total, quantile, rank,
add, and the whole aggregate path (`deserial`, `combine`, `serial`, `final`). The
text input, binary receive, cast and `COPY` paths share these validators and were
exercised separately in a real server (stored values, `pg_input_is_valid`, binary
copy) and by the shipped `006_fuzz.pl`. No AddressSanitizer or UBSan report, no
assertion, no value that the validator accepted and an operation then rejected, no
input that took more than 0.3 s. Valgrind is not installed on the machine.

Not run by me: the in-tree meson build, Windows, a big-endian machine, `pg_upgrade`
itself (I ran dump and restore across 18 and 19rc2 only), logical replication of a
published table with real subscribers (K12-15 is argued from the lock list and
from `pg_locks`, not from replication), the interplay with `kingres_shard`.

Apart from the test suites and the dump and restore into 19rc2, every
reproduction below ran on 18.7 only.

"Reproduced" below means I observed it. `~/kw-review4` is my work area.

## Findings

| ID | Severity | Component | Summary |
|---|---|---|---|
| K12-01 | medium | sketch hash | Enum values (and arrays, ranges and records of them, and `oid` and the `reg*` types of user objects) hash by OID: a stored hll of an enum column is wrong after dump and restore and differs between nodes; the README promises stability (reproduced) |
| K12-02 | low | tdigest | `tdigest_quantile` returns `Infinity` or `NaN` for finite values near the double limit, outside min and max (reproduced) |
| K12-03 | low | aggregates | A NULL precision, capacity or compression argument gives an error or an answer depending on the row order, so on the plan and the number of workers (reproduced) |
| K12-04 | low | topn | Equal `bpchar` values of different length are two items, while `hll` counts one; the README does not say so (reproduced) |
| K12-05 | note | tdigest | A stored value with `-0` in min, max or a mean is accepted and kept as is; the format is documented as canonical and `-0` as stored as `0` (reproduced) |
| K12-06 | note | topn | A call on a maximal value needs about nine times the size of the value in memory (960 MB for 104 MB); the README says "about 104 MB" (reproduced) |
| K12-07 | medium | timepart | Any role can stop the runs of any table, and so partition creation and retention, by taking a session advisory lock whose two numbers are computable from the table name (reproduced) |
| K12-08 | medium | timepart | `timepart_run` calls `unnest` unqualified and has no `search_path` setting: with a search path that lists a writable schema before `pg_catalog`, any role that can create a function there runs code as the superuser; the README says the opposite (reproduced, role became superuser) |
| K12-09 | medium | timepart | The `SECURITY DEFINER` functions take the caller's identity from `session_user`: a role with EXECUTE rights can change the policy of a table it does not own whenever a privileged session runs code of that role (a view, a function) (reproduced) |
| K12-10 | medium | timepart | A run does execute user code, as the owner and without a time limit (an `IMMUTABLE` function of an index expression of the parent runs once per partition created): one table owner can keep the scheduler's session busy for as long as it likes; the README says no user code runs (reproduced: 18 s for six partitions) |
| K12-11 | low | timepart | In the zones that skipped a calendar day (`Pacific/Apia` 2011-12-30, `Pacific/Kiritimati` and `Pacific/Kanton` 1994-12-31, `Pacific/Fakaofo` 2011-12-30, `Kwajalein` 1993-08-21) the `1 day` grid has an empty cell with a duplicate name, and creation fails (reproduced) |
| K12-12 | low | timepart | A misaligned partition is reported "left alone" and then dropped by the retire phase of the same run, with its rows (reproduced) |
| K12-13 | low | timepart | `timepart_partitions` reports the size and row estimate of any table that now has the name of a partition that a run once detached, whoever owns it (reproduced) |
| K12-14 | low | timepart | `ATTACH PARTITION` takes `SHARE ROW EXCLUSIVE` on every table the parent has a foreign key to; the README lock table does not list it and says creation blocks no writers (reproduced) |
| K12-15 | note | timepart | A partition that a foreign key references can never be retired: every run of the table is `partial` for good and the partition stays (reproduced) |
| K12-16 | note | timepart | `DROP EXTENSION` leaves the empty schema `kingres` (reproduced) |
| K12-17 | note | timepart | With a role named `kingres`, the default `search_path` `"$user", public` puts the extension schema first: unqualified `CREATE TABLE` lands in it (reproduced) |

No critical or high finding. The memory-safety work found nothing: see "Examined
and found sound".

### K12-01 (medium) Enum values hash by OID; stored hll sketches of them do not survive dump and restore

Where: `src/kingres_sketch.c` 337-366 (`ks_hash_datum`: "the extended hash function
of the default hash operator class", no exclusion of types), README "The hash" and
"Stored sketches and rollups", `kingres_sketch--1.0.sql` (`hll_hash(anyelement)`
and the aggregates are `IMMUTABLE`).

What happens: PostgreSQL's `hashenumextended` hashes the OID of the enum label, not
the label. A restored or independently created enum has other OIDs, so the same
value is a different element. Every other type I tried (int2/4/8, bool, float4/8,
numeric, text, bpchar, uuid, timestamptz, interval, inet, jsonb, ranges, arrays of
those) gave the same sketch bytes on 18 and on 19rc2 after dump and restore. Only
the enum column did not. `oid` and the `reg*` types of user objects are hashed by
number too (same cause, the same effect after a restore). The README argues that
the hash functions are "stable in practice" because hash partitioning relies on
them; hash partitioning on an enum is exactly the known exception, and the README
neither lists it nor refuses it (topn refuses enums; hll does not). A rollup table
of hll sketches over an enum, a plan the README presents as the main use, silently
gives wrong distinct counts after a restore or when two nodes build their own enum.

Reproduction (any database where `kingres_sketch` is installed; the second half
needs a second database, here made by `pg_dump` and `pg_restore`):

    SET search_path = public, kingres_sketch;
    CREATE TYPE colr AS ENUM ('r','g','b');
    SELECT hll_hash('r'::colr) =
           hll_hash((SELECT oid FROM pg_enum WHERE enumtypid='colr'::regtype AND enumlabel='r'));  -- true
    CREATE TABLE ev (c colr);  INSERT INTO ev VALUES ('r'),('g'),('b');
    CREATE TABLE sk AS SELECT hll_add_agg(c) AS h FROM ev;
    -- pg_dump this database and restore it into a new one (I used 18 -> 19rc2)
    SELECT hll_cardinality(h),
           hll_cardinality((SELECT hll_union_agg(hll_add(h, c)) FROM ev))
      FROM sk;

Observed in the restored database: `3` and `6` (in the original: `3` and `3`), and
`sk.h::bytea` differs from `SELECT hll_add_agg(c) FROM ev`. The same script with
`text`, `int`, `numeric`, `float8`, `timestamptz`, `uuid` and `int[]` columns gives
equal bytes and `3`/`3`.

Should: either refuse types whose hash is not a function of the value alone (enums,
and containers of them; `oid` and the `reg*` types), as `topn` does, or hash enums
by their label (`enum_out`), and say so in the README. Add the dump and restore
case of an enum column to `005_dump_copy.pl`.

### K12-02 (low) `tdigest_quantile` can leave the interval [min, max]

Where: `src/tdigest.c` 530-560 (`curve_quantile`): `val[lo] + (val[hi] - val[lo]) *
fraction`; `td_rank` (571-600) and the merge step of `td_flush` have the same form
(`cur.mean + (it.mean - cur.mean) * ratio`; the merge step clamps its result, and
the one rank call I made at such a digest, `tdigest_rank(d, 0)`, returned 0.25).

What happens: for two finite values on opposite sides near the double limit the
difference overflows to infinity. A fraction of 0 gives `inf * 0 = NaN`, a
positive fraction gives `Infinity`. The README says values are finite and the
quantile is read from the piecewise linear curve; it does not exclude the range.

    SET search_path = public, kingres_sketch;
    SELECT tdigest_quantile(d, 0.5), tdigest_quantile(d, 0.25), tdigest_quantile(d, 0.75)
      FROM (SELECT tdigest_agg(x) d FROM (VALUES (-1.7e308::float8), (1.7e308)) v(x)) s;

Observed: `Infinity`, `NaN`, `1.7e+308`. Should: a value in [min, max]; interpolate
as `a + (b - a) * t` only when `b - a` is finite, otherwise as `a * (1 - t) + b * t`
(or `a / 2 + b / 2` based), and clamp to min and max.

### K12-03 (low) A NULL parameter argument makes the result depend on the row order

Where: `src/hll.c` 819-835 (`kingres_sketch_hll_agg_add_p`) and 784-800
(`agg_state`), the same shape in `topn.c` 919 (`agg_state`, `agg_add_k`) and
`tdigest.c` 837 (`agg_state`, `agg_compression`); README 91-93 ("takes the precision
from the first row of the group and every later row must give the same value").

What happens: a NULL parameter is read as "the state's own, or the default". A
first row with NULL creates the state with the default (14, 1000, 100); a later row
with another value then raises 22023. The other order is accepted and the NULL rows
are ignored. So the same data answers or fails depending on the scan order, on the
partitions' order and on how the rows are split between workers (by reading: a
worker whose first row is NULL has a state with the default parameter, and
`combine` with a worker whose state has the explicit parameter raises the same
22023; I saw the order dependence on a hash-partitioned table, not the combine
case).

    SET search_path = public, kingres_sketch;
    SELECT hll_precision(hll_add_agg(x, p)) FROM (VALUES ('a',12),('b',NULL::int)) v(x,p);  -- 12
    SELECT hll_precision(hll_add_agg(x, p)) FROM (VALUES ('a',NULL::int),('b',12)) v(x,p);  -- ERROR 22023

Observed on `hll_add_agg`, `approx_count_distinct`, `topn_add_agg(x, k)` and
`tdigest_agg(x, c)`: the first order answers, the second fails. Should: NULL is an
error in every row (the README says "the same value"), or NULL rows are ignored for
the parameter in every order (the state is created by the first non-NULL parameter,
and the default applies only at the final function when there was none). Add the
two orders to the tests.

### K12-04 (low) `topn` splits equal `bpchar` values; `hll` does not

Where: `src/topn.c` 697-760 (`tn_item_text`: `bpchar` is accepted and its item is the
output text, with the padding of its length), README topn section ("An item is its
canonical text ... `numeric` keeps its scale"), hash section ("`bpchar` ignores
trailing spaces", said of `hll` only).

    SET search_path = public, kingres_sketch;
    SELECT 'a'::char(3) = 'a'::char(5);                      -- true
    SELECT * FROM topn_list(topn_add(topn_add(topn_empty(), 'a'::char(3)), 'a'::char(5)));
    -- two items, 'a  ' and 'a    ', each with frequency 1
    SELECT hll_cardinality(hll_add(hll_add(hll_empty(), 'a'::char(3)), 'a'::char(5)));   -- 1

Consequence: a `topn` over `bpchar` columns of different lengths (or a column and a
cast of it) counts one value as several items; the frequencies are lower bounds
that are far from the truth. Should: trim trailing spaces for `bpchar` (the value
the equality operator compares), or refuse `bpchar` like the floating-point types
and say so; state the rule next to the `numeric` one.

### K12-05 (note) Negative zero is accepted in a stored tdigest, so bytes are not a function of content

Where: `src/tdigest.c` 419-433 (`min + 0.0`, `mean += 0.0` normalize in memory only;
the validator does not require the bit pattern of zero), README 138 ("`-0` is stored
as `0`") and the Format section ("two sketches with the same ... centroids have
identical bytes").

    SET search_path = public, kingres_sketch;
    WITH b AS (SELECT tdigest_to_bytea(tdigest_agg(0.0::float8)) v),
         c AS (SELECT set_byte(set_byte(v, 27, 128), 43, 128) v FROM b)   -- min and mean = -0.0
    SELECT tdigest_min(v::tdigest), tdigest_to_bytea(v::tdigest) = v FROM c;   -- 0, true

The value is accepted and returned unchanged, with `-0.0` in its bytes, while the
same content built by the aggregate has `+0.0`. Harmless for the estimates; it
breaks the "same content, same bytes" property that a rollup comparison or a
checksum of a stored sketch would use. Should: refuse a `-0` bit pattern in
`td_load` (as the validators refuse every other non-canonical form).

### K12-06 (note) Memory of a call is about nine times the value

Measured (`topn` capacity 100000, items of 1024 bytes, a value of 104200020 bytes;
backend high-water mark read from `/proc/self/status`): 849 MB after building it
with `topn_add_agg`, 962 MB after `topn_union(t, t)` and after `topn_union_agg` over
three copies. The README gives "at most about 104 MB" for the value, which is true,
and nothing for the working memory: the detoasted argument, the loaded state, the
second state, the sorted entry array, the merged state and the serialized result are
alive together, and none of it counts against `work_mem`. With parallel workers
the figure is per worker. A statement timeout of 300 ms was honoured (error after
0.7 s). Suggest saying so next to the capacity limit, and lowering the documented
maximum capacity for long items if that is not intended.

### K12-07 (medium) Any role can stop the runs of any table (and so partition creation and retention)

Where: `kingres_timepart--0.1.sql` 1217-1224 (`timepart__lock_keys`: the two integers
are `hashtext('kingres_timepart')` and `hashtext(quote_ident(schema) || '.' ||
quote_ident(table))`), 1335 (`pg_try_advisory_lock` in `timepart_begin_run`), 1430
(`pg_try_advisory_xact_lock(hashtext('kingres_timepart_step'), ...)` in
`timepart__step`); README Limits ("an advisory transaction lock that any session can
take") and Locks.

What happens: advisory locks are one namespace for the whole database and
`pg_advisory_lock` is executable by every role (by default). The README mentions the
second lock only as a limit; the effect is larger: a connected role with no right on
the table takes the first lock and holds it, and every `timepart_run`,
`timepart_begin_run` and so every scheduled run of that table fails with 55P03
for as long as the session lives. New partitions are then not created, inserts into
the table fail at the next boundary, and retention stops. The second lock does the
same one step later and leaves the run row `running`.

Reproduction (database `tp2` with the extension, role `evil` without any right on
`app.victim`; `app.victim` has a policy):

    -- session of the unprivileged role evil
    SELECT pg_advisory_lock(hashtext('kingres_timepart'),
                            hashtext(quote_ident('app') || '.' || quote_ident('victim')));
    -- session of the superuser
    CALL kingres.timepart_run('app.victim', '2025-01-12 00:00+00');
    -- ERROR:  another run of table app.victim is in progress  (55P03)

Observed as above; the partitions stayed at 10. With
`pg_advisory_xact_lock(hashtext('kingres_timepart_step'), <same second number>)`
in an open transaction, the run starts, its row stays `running`, and the first step
fails with "another step of a run of table app.victim is in progress".

Should: use a lock that only the extension can take: a row lock on the policy row
(a row lock cannot be held across the commits of a run, so keep a `running_pid` and
`running_since` in the policy row and check `pg_stat_activity` for the pid, as
schedulers do), or take the advisory lock with a key that includes a random value
stored in a table that only the extension owner can read. Document `REVOKE EXECUTE ON FUNCTION pg_advisory_lock(bigint) FROM PUBLIC`
(and the other advisory functions) as a deployment step until then.

### K12-08 (medium) `timepart_run` is hijackable through the caller's search path; the README says it is not

Where: `kingres_timepart--0.1.sql` 1669-1700 (`CREATE PROCEDURE kingres.timepart_run`
has no `SET search_path` and uses `unnest(phases)`, `NOT IN`, `= ANY`, `||`
unqualified); the header comment at 14-19 and README Security ("they hold only
schema-qualified calls and nothing a caller can redirect").

What happens: the body runs with the search path of whoever calls it, a
superuser. If that path lists a schema in which an untrusted role can create
functions before `pg_catalog` (explicitly, for example `public, pg_catalog`), an exact-type function there wins
over the polymorphic `pg_catalog.unnest`. The default path is safe, because
`pg_catalog` is then searched first; the README does not state the condition and
the claim in it is false.

Reproduction (role `evil` may create in `public`; the administrator runs the
maintenance with a search path that names `public` first):

    -- as evil
    CREATE FUNCTION public.unnest(text[]) RETURNS SETOF text LANGUAGE plpgsql AS $$
    BEGIN EXECUTE 'ALTER ROLE evil SUPERUSER'; RETURN QUERY SELECT 'create'::text; END $$;
    -- as the superuser
    SET search_path = public, pg_catalog;
    CALL kingres.timepart_run('app.victim', '2025-01-12 00:00+00');
    SELECT rolsuper FROM pg_roles WHERE rolname = 'evil';

Observed: `evil` is a superuser afterwards (it was not before). Should: keep the
procedure to `CALL` and `COMMIT` and move every other statement (the validation of
`phases`) into a function that carries `SET search_path = pg_catalog, kingres,
pg_temp`, and spell `OPERATOR(pg_catalog.=)` where an operator is unavoidable;
change the README sentence; add a test that runs `timepart_run` under a hostile
path.

### K12-09 (medium) The identity check uses `session_user`; a role can use a privileged session as its deputy

Where: `kingres_timepart--0.1.sql` 455-463 (`timepart__caller`: the role of `SET
ROLE` or else `session_user`), used by `timepart_set_policy` (1058),
`timepart_unset_policy`, `timepart_plan`, `timepart_partitions`, `timepart_check`;
header comment 28-35; README Security ("decides who is calling (the role set with
`SET ROLE`, or the session user)").

What happens: PostgreSQL decides privileges by `current_user`. Code of an ordinary
role can run in a privileged session with the privileges of that role only: a
function or a view owned by the role, called by the superuser (a view is executed
with its owner's rights). Inside such code the session user is still the superuser,
so `timepart__caller()` says "superuser" and the owner check passes for every
table. The README grants EXECUTE on these functions to the roles that own tables,
so a role with that grant has the means.

Reproduction (roles `vown` owns `app.victim`, `evil` owns nothing there; `evil` has
the grants of the README):

    -- as evil: the direct call is refused ("must be owner of table app.victim")
    CREATE FUNCTION app.evil_fn() RETURNS int LANGUAGE plpgsql AS $$
    BEGIN PERFORM kingres.timepart_set_policy('app.victim'::regclass, '1 day', '1 day', '1 day');
          RETURN 1; END $$;
    CREATE VIEW app.evil_report AS SELECT app.evil_fn() AS x;
    SELECT * FROM app.evil_report;                 -- ERROR: must be owner of table app.victim
    -- as the superuser (for example while looking at the view)
    SELECT * FROM app.evil_report;                 -- succeeds
    SELECT retention, created_by FROM kingres.timepart_policy WHERE table_name = 'victim';

Observed: retention changed from 5 days to 1 day, the policy otherwise intact
(`created_by` still `vown`); the plan then lists `drop` for every partition older
than one day, and the next run drops them. Should: do not trust `session_user`.
Make the policy table writable only through rules that use `current_user` of the
caller: a `SECURITY INVOKER` function with INSERT, UPDATE and DELETE granted on the
policy table, and row-level security policies `USING (pg_has_role(current_user,
(SELECT relowner FROM pg_class WHERE oid = to_regclass(...)), 'USAGE'))`; or
document that the functions must never run in a session with more rights than the
roles whose code is in it, and make the owner check require `current_user =
session_user` where that is possible.

### K12-10 (medium) A run executes user code as the owner, without a time limit

Where: `kingres_timepart--0.1.sql` 1557 (`SET LOCAL ROLE` then the statements of
`timepart__create_stmts`: `CREATE TABLE ... (LIKE ... INCLUDING INDEXES ...)`),
README Security ("A run executes no user code: `LIKE` copies defaults and
constraints as text, ...").

What happens: `INCLUDING INDEXES` creates the parent's indexes on every new table;
an index whose expression is `f(constant)` makes the planner evaluate `f` at once
(`IMMUTABLE` is a declaration, not a check). I confirmed that the function runs, as
the owner and as `current_user = vown`, once per partition created, in the
superuser's session. It cannot become the superuser: `RESET ROLE` in it is refused
("RESET is not allowed in a non-volatile function") and index builds run restricted;
I found no way to run user code unrestricted during the owner window (column
defaults, CHECK constraints, generated columns and triggers are copied, not run).
But nothing limits its duration. The run has no `statement_timeout`; it holds the
per-table advisory lock, and a scheduler that runs the due tables one after the
other in one connection (the README's model) is stuck for as long as the function
sleeps. The README sentence that no user code runs is wrong.

Reproduction (table `app.slow` owned by `vown`, daily, policy set):

    -- as vown
    CREATE FUNCTION app.slowfn(int) RETURNS int LANGUAGE plpgsql IMMUTABLE
      AS $$ BEGIN PERFORM pg_sleep(3); RETURN $1; END $$;
    CREATE INDEX ON app.slow (ts, (app.slowfn(1)));
    -- as the superuser
    CALL kingres.timepart_run('app.slow', '2025-01-10 00:00+00');

Observed: 18.0 seconds for the six partitions of that run (3 s each). A function
that never returns would never return (by reading; I did not run it). Should: `SET LOCAL statement_timeout`
(a setting, default a minute or so) for the owner window, so that a run records the
action as failed and goes on; change the README sentence.

### K12-11 (low) The `1 day` grid has an empty cell where a zone skipped a calendar day

Where: `kingres_timepart--0.1.sql` 220-250 (`timepart_cell_start`: local midnight
`AT TIME ZONE`), 590-640 (`timepart__create_rows`); README "Every cell ends exactly
where the next one starts ... where the zone has no such local time the cell starts
at the first instant after the gap".

What happens: when a whole local day does not exist, two consecutive cells start at
the same instant. The cell between them is empty, gets the name of the next cell
(`YYYYMMDD` of its start in the zone), and `ATTACH PARTITION` refuses it. A set-based
search over all zones for 1970 to 2037 found exactly these: `Kwajalein` and
`Pacific/Kwajalein` (1993-08-21), `Pacific/Enderbury`, `Pacific/Kanton`,
`Pacific/Kiritimati` (1994-12-31), `Pacific/Apia` and `Pacific/Fakaofo` (2011-12-30).
It matters for backfills of old data; the README's claim "never overlap and never
leave a gap" is about the grid and stays true, but `create_time_partitions` fails
as a whole.

    CREATE TABLE pa (ts timestamptz NOT NULL, v int) PARTITION BY RANGE (ts);
    SELECT kingres.timepart_cell_start('1 day', 'Pacific/Apia', k)
      FROM generate_series(kingres.timepart_cell_of('1 day','Pacific/Apia','2011-12-28 12:00+00'),
                           kingres.timepart_cell_of('1 day','Pacific/Apia','2011-12-28 12:00+00') + 3) k;
    SELECT kingres.create_time_partitions('pa', '1 day', '2012-01-02', '2011-12-28', NULL, 'Pacific/Apia');

Observed: cell 4381 and 4382 start at the same instant; the second call fails with
`empty range bound specified for partition "pa_p20111231"` and creates nothing
(all or nothing). Should: leave out a cell whose end is not after its start in
`timepart__create_rows`, and let `timepart_cell_of` land on the later cell; add
`Pacific/Apia` to the DST test.

### K12-12 (low) "Left alone" and then dropped

Where: README lines 66 and 202 (misaligned partitions are "left alone"), the retire
loop of `timepart__plan_rows` (813-835), which has no misaligned test.

    -- app.mis is partitioned daily; a hand-made partition off the grid, with a row:
    CREATE TABLE app.mis_hand PARTITION OF app.mis
      FOR VALUES FROM ('2025-01-03 06:00+00') TO ('2025-01-04 06:00+00');
    INSERT INTO app.mis VALUES ('2025-01-03 12:00+00', 42);
    SELECT kingres.timepart_set_policy('app.mis', '1 day', '2 days', '3 days');
    CALL kingres.timepart_run('app.mis', '2025-01-10 00:00+00');

Observed: the plan lists `misaligned mis_hand` (seq 1) and `drop mis_hand` (seq 10);
after the run `app.mis_hand` is gone and so is the row. Retention dropping old
partitions is the purpose, so the drop is arguably right, but the run's history says
"left alone" for a partition it then removes. Should: say so in the README, or
exclude misaligned partitions from retirement and report them.

### K12-13 (low) `timepart_partitions` shows the size of an unrelated table

Where: `kingres_timepart--0.1.sql` 1723-1790: the second `RETURN QUERY` takes the
names of partitions that a detach once recorded and resolves them with
`to_regclass` at call time, as `SECURITY DEFINER`, without checking that the object
is still that partition or readable by the caller.

    -- vown has a table app.leak with a policy retention_action 'detach'; a run detached app.leak_p20250103
    -- vown drops app.leak_p20250103; evil (no rights on app.leak) later creates its own app.leak_p20250103 with data
    -- as vown
    SELECT partition, attached, size_bytes, row_estimate FROM kingres.timepart_partitions('app.leak') WHERE NOT attached;

Observed: a row `app.leak_p20250103 | f | 262144 | 5000`, the size and the planner
row estimate of evil's table, which vown cannot read (`has_table_privilege` false).
The name has to be chosen with care (the owner of the parent names its table), so
the exposure is small, but it is a catalog read made with the extension owner's
rights. Should: also require `has_table_privilege(caller, oid, 'SELECT')` or
ownership for the detached ones, or store the partition's OID at detach time and
compare it.

### K12-14 (low) `ATTACH PARTITION` locks the tables the parent references, and the README says creation blocks no writers

Where: README Locks ("Creation does not block readers or writers of the parent"; the
table lists `SHARE UPDATE EXCLUSIVE` on the parent only).

    -- app.ref (id int primary key); app.fkfrom partitioned, with a foreign key to app.ref
    BEGIN;
    CREATE TABLE app.fkfrom_pz (LIKE app.fkfrom INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES);
    ALTER TABLE app.fkfrom ATTACH PARTITION app.fkfrom_pz FOR VALUES FROM ('2030-01-01+00') TO ('2030-01-02+00');
    SELECT c.relname, l.mode FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
     WHERE l.pid = pg_backend_pid() AND c.relname IN ('ref','fkfrom','fkfrom_pz') ORDER BY 1, 2;
    ROLLBACK;

Observed: on `app.ref`, `ShareRowExclusiveLock` (with `RowShareLock` and
`AccessShareLock`); on the parent `ShareUpdateExclusiveLock`. A `ShareRowExclusive`
lock conflicts with the `RowExclusive` of every INSERT, UPDATE and DELETE on
`app.ref`; the action waits for the lock for up to the create timeout (5 s, five
attempts) and, while it waits, queues every later writer of `app.ref` behind it.
Should: list the referenced tables in the README lock table and in the lock test
(`001_locks.pl`), and keep the create timeout low for tables that have foreign keys.

### K12-15 (note) A partition that a foreign key references is never retired

Reproduced: partitioned `app.fkp` with primary key `(ts, id)`, a table `app.fkc` with
a foreign key to it, a row in each, retention 2 days, run at 2025-01-10: the drop of
the referenced partition fails (`23503: removing partition ... violates foreign key
constraint`), the run is `partial`, and so is every later run, until the referencing
row is gone. The README lists "a view that depends on the partition" among the
causes; a foreign key is the same class and a common one. Behaviour is right (no
rows lost); it should be in the list, and `timepart_check` could name it.

### K12-16 (note) `DROP EXTENSION` leaves the schema

`DROP EXTENSION kingres_timepart` removed the event triggers and the three tables with
the policies and the history (as intended), and left the empty schema `kingres`
(PostgreSQL records the dependency of the extension on the schema, not the other
way). Nothing in the README says what remains; policies and history are lost without
a warning, and the partitions stay as ordinary tables.

### K12-17 (note) A role named like the schema puts the schema first on the path

With a superuser named `kingres` (the name `initdb` gives when the server runs under
that operating system user, as in my setup), the default `search_path` is
`"$user", public`, which resolves to the extension schema `kingres` first.
`CREATE TABLE pa (...)` in a fresh database created `kingres.pa`; the objects of
the extension are then reachable unqualified, and user tables in that schema make
`DROP EXTENSION` and a later `DROP SCHEMA` harder. None of the extension's own
code is affected (all of it is qualified). Suggest a line in the README, or a schema
name that is not also a likely role name.

## Not reproduced

- **Quadratic load time of a `topn` value with colliding item hashes.** `tn_find`
  uses a 32-bit `hash_bytes` and a chained table sized to the capacity; a value
  whose 100000 items all fall in one bucket makes `tn_load` take about 5 * 10^9
  chain steps, with no `CHECK_FOR_INTERRUPTS`. I could not build such a value cheaply
  (it needs about 2^18 candidate strings per colliding item to match the low bits of
  a table that has grown to 262144 buckets), so I did not confirm the run time; the
  same weakness exists for a stream of attacker-chosen items through `topn_add_agg`,
  where it is the usual hash flooding. A seeded hash would remove it.
- **Unqualified-name capture in the two `SECURITY INVOKER` DDL helpers.** They carry
  `SET search_path = pg_catalog, kingres, pg_temp` and qualify every object; I found
  no capture. Objects pre-created by another role in a schema called `kingres` (if
  the extension were installed into an existing schema) were not tried.
- **Replica identity of new partitions.** The parent's `REPLICA IDENTITY FULL` is
  not copied to partitions made by `LIKE`; it is not copied by native
  `PARTITION OF` either (I compared), so I do not count it as a finding of this
  extension. I did not run a subscriber.
- **Wrong answers in parallel hll/topn/tdigest.** None found: for hll the bytes of
  the parallel and the serial result were equal in all 7 groups; for the others the
  totals were equal and `approx_percentile(0.9)` differed by 51 on a range of
  100003 (0.05 percent).
- **A hang or crash in a deserial function through a real partial aggregate.** The
  state functions take `internal` and cannot be called from SQL (`cannot cast type
  unknown to internal`), and PostgreSQL has no SQL path that feeds a foreign state
  into `deserialfn`, so I could only reach the deserial functions through my
  fuzzer, which found nothing.

## Not covered

- The two design documents were not compared with the code.
- `pg_upgrade` itself (only dump and restore from 18 to 19rc2, and the equality of
  `self_test()`, which was identical on both majors), and a restore into a database
  with another encoding or another default collation.
- Big-endian hashing (the README says "verified by reading"; I did not run it).
- Hash joins, hash indexes and hash partitioning on a `hll` expression.
- Logical replication with a subscriber (K12-14 and the replica-identity note are
  from catalogs and locks only), `DETACH ... CONCURRENTLY`, `pg_cron`, the control
  plane's scheduler.
- Sub-partitioned partitions beyond reading the code (`DETACH` and `DROP TABLE` of a
  partitioned child should drop its children; I did not run it), foreign-table
  partitions, tables with row-level security (the run bypasses it as the owner; I did
  not run one), hash and list partitioned tables beyond the refusal messages.
- A run with thousands of partitions (the README says every action re-plans; I did
  not time it), the 10000-cell limit, `kingres_timepart.*` settings out of range.
- A crash of the postmaster in the middle of a run (the shipped `002_interrupt.pl`
  does it and passed; I only terminated backends), a long transaction holding
  `ACCESS EXCLUSIVE` on the parent (the shipped `001_locks.pl` passed).
- Windows, meson in-tree builds, performance, valgrind.

## Examined and found sound

- Every validator: header, parameters, length against parameters, sparse order and
  range, register values, the canonical choice of sparse or dense, counter order,
  count and error invariants, centroid order and weights; no out-of-bounds read, no
  overflow, no unbounded allocation (the largest allocation is bounded by the
  documented value sizes; a corrupt count is refused before any allocation).
- Text, binary and cast inputs (including 1-byte varlena headers and values
  compressed or stored externally: a 196612-byte hll uncompressed external, a
  2 MB topn compressed, read after `VACUUM FULL`); `pg_input_is_valid`; binary
  `COPY` round trip.
- hll: the estimator at p = 4 to 18 and n = 1 to 10^6 has no visible bias at the
  sparse/dense switch (the two forms hold the same registers); the measured
  relative standard deviation is below `1.04 / sqrt(m)` for p >= 10 (for p = 4 and
  n = 1000 it was 32 percent against 26, which is the small-m limit of the formula,
  not a defect); `hll_fold` equals the sketch built at the lower precision;
  unions are idempotent and the bytes equal those of one backend.
- topn: after random partitions and random merge orders, 340 trials, no bound,
  N/k, sum or missing-item violation; the validator never rejected a value the
  merge produced.
- tdigest: counts exact, quantiles monotone and inside min and max, rank monotone, for
  100 random digests built by unions (except K12-02 at the double limit).
- Partial aggregation: parallel plan, partitionwise aggregate, FILTER, GROUPING SETS,
  running and sliding window frames, zero rows, all-NULL input, mixed parameters
  (apart from K12-03).
- Hash equality of values that compare equal: numeric scale, `-0`, NaN, bpchar
  padding, interval, timestamptz, inet, jsonb, ranges, arrays, domains, a
  nondeterministic collation (hashed by sort key, as the README says), `int2` to
  `int8` (equal, as the README says) (apart from K12-01).
- Cross-major: `self_test()` identical on 18.7 and 19rc2; stored sketches of eight
  of nine element types identical after dump and restore from 18 to 19rc2.
- Non-owner access to `kingres_timepart`: `create_time_partitions`,
  `drop_old_time_partitions`, `timepart_set_policy`, `timepart_unset_policy`,
  `timepart_plan`, `timepart_partitions`, the direct helpers `timepart_create_one`,
  `timepart_retire_one`, and the policy table are all refused for a role that does not
  own the table; `timepart_check` shows only own tables.
- A backend terminated at a random moment of a run (12 times): the next run marks the
  old one `interrupted`, ends `done`, the partition count is right and no standalone
  table is left behind. `lock_timeout` is restored by every function that sets it.
- Dump and restore of policies and history, the continuing `run_id` sequence, the
  event triggers (present, enabled always, following a rename).
- Name cut at 63 bytes: the second table's action fails with `name_conflict` and
  nothing is renamed or overwritten, as the README says. A default partition that
  holds a row of the new range makes the action fail and the run `partial`, as
  documented. Calendar limits (year 9998 to 9999, year 1) are accepted; BC dates fail
  with a clear error.
