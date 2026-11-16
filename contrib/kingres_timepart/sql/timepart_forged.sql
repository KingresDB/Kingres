-- SPDX-License-Identifier: AGPL-3.0-only
-- timepart_forged: policy rows and history rows that were not written by the
-- interface of this installation (a dump shaped by hand, an INSERT by a
-- superuser, a row edited by hand) must not make the maintenance act on a
-- table (K12T-03).
--
-- The row is data, and a stamp or an owner copied into data proves nothing
-- (the stamp of an installation is readable by every role, the owner of a
-- table by anybody who can see the catalog).  The maintenance acts on a policy
-- that carries a seal, which only the interface writes and a dump does not
-- carry; any other policy row is inert until a superuser adopts it, and
-- adoption checks the catalog.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never
SET kingres_timepart.trust_table_owners = on;

SET client_min_messages = warning;
DROP ROLE IF EXISTS tp_f_owner, tp_f_other;
RESET client_min_messages;
CREATE ROLE tp_f_owner NOLOGIN;
CREATE ROLE tp_f_other NOLOGIN;
CREATE SCHEMA tpf AUTHORIZATION tp_f_owner;
GRANT USAGE ON SCHEMA kingres TO tp_f_owner, tp_f_other;
GRANT USAGE ON SCHEMA tpf TO tp_f_other;
GRANT EXECUTE ON FUNCTION
    kingres.create_time_partitions(regclass, interval, timestamptz, timestamptz, name, text),
    kingres.timepart_set_policy(regclass, interval, interval, interval, text, interval, text[], jsonb, text, interval, boolean)
  TO tp_f_owner, tp_f_other;
SELECT current_user AS su \gset

-- Five tables of the owner, each with ten old daily partitions, and none with
-- a policy.  Nothing below is made by the owner's interface until it says so.
CREATE FUNCTION tpf.make(p_table name) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    EXECUTE format('CREATE TABLE tpf.%I (ts timestamptz NOT NULL) PARTITION BY RANGE (ts)', p_table);
    EXECUTE format('ALTER TABLE tpf.%I OWNER TO tp_f_owner', p_table);
    EXECUTE 'SET LOCAL ROLE tp_f_owner';
    PERFORM kingres.create_time_partitions(format('tpf.%I', p_table)::regclass,
                                           interval '1 day', '2026-05-10', '2026-05-01');
END $$;
SELECT tpf.make(t) FROM unnest(ARRAY['v1', 'v2', 'v3', 'v4', 'v5', 'n1']::name[]) t;
RESET ROLE;
CREATE FUNCTION tpf.parts(p_table name) RETURNS bigint LANGUAGE sql STABLE AS $$
    SELECT count(*) FROM pg_inherits WHERE inhparent = format('tpf.%I', p_table)::regclass $$;

-- A row as a dump or a superuser can make it: every column set by hand.
CREATE FUNCTION tpf.forge(p_table name, p_stamp uuid, p_owner name,
                          p_retention interval DEFAULT interval '1 day')
RETURNS void LANGUAGE sql AS $$
    INSERT INTO kingres.timepart_policy_data
      (schema_name, table_name, parent_oid, partition_every, timezone,
       create_ahead, retention, retention_action, run_every, enabled, stamp,
       table_owner, table_owner_oid, created_by, created_by_oid, created_at,
       updated_at, next_run_at)
    SELECT 'tpf', p_table, coalesce(to_regclass(format('tpf.%I', p_table))::oid, 0),
           interval '1 day', 'UTC', interval '1 day', p_retention, 'drop',
           interval '5 minutes', true, p_stamp, p_owner,
           coalesce((SELECT r.oid FROM pg_roles r WHERE r.rolname = p_owner), 0),
           p_owner, coalesce((SELECT r.oid FROM pg_roles r WHERE r.rolname = p_owner), 0),
           now(), now(), now() $$;

-- ---- the reproduction of the review ----
-- The row of a table that never had a policy: the real schema, table and
-- owner, the stamp of this installation, a retention of one day.
SELECT tpf.parts('v1') AS partitions_before;
SELECT tpf.forge('v1', kingres.timepart__stamp(), 'tp_f_owner');
\o /dev/null
CALL kingres.timepart_run('tpf.v1', '2026-05-20 UTC');
\o
-- The run did not act.  (Before the seal it dropped the ten partitions, made
-- the ones of the next day and said "done".)
SELECT tpf.parts('v1') AS partitions_after;
SELECT state, left(error, 90) AS error FROM kingres.timepart_run WHERE table_name = 'v1';
SELECT count(*) AS actions FROM kingres.timepart_run_action a JOIN kingres.timepart_run r USING (run_id) WHERE r.table_name = 'v1';
-- nothing lists it as due, a check reports it, and the adoption refuses it
SELECT count(*) AS due FROM kingres.timepart_due('2030-01-01 UTC') WHERE table_name = 'v1';
SELECT table_name, problem FROM kingres.timepart_check() WHERE table_name = 'v1';
SELECT kingres.timepart_adopt() IS NOT NULL AS adopt_ran;
SELECT count(*) AS sealed FROM kingres.timepart_policy_seal WHERE schema_name = 'tpf';

-- ---- a stamp of another installation, as a restore brings ----
-- That is a row that a backup could carry: inert until the superuser adopts
-- it, which says so, and then it is the policy of the owner's table.
SELECT tpf.forge('v2', '11111111-1111-1111-1111-111111111111', 'tp_f_owner');
\o /dev/null
CALL kingres.timepart_run('tpf.v2', '2026-05-20 UTC');
CALL kingres.timepart_run('tpf.v2', '2026-05-20 UTC');
SELECT * FROM kingres.timepart_due('2030-01-01 UTC');
\o
SELECT tpf.parts('v2') AS v2_partitions_not_adopted;
SELECT count(*) AS due FROM kingres.timepart_due('2030-01-01 UTC') WHERE table_name = 'v2';
-- A hostile dump can do this much: with the owner's name right, a superuser
-- who adopts it has a policy of the dump's choosing on that table, which acts
-- as any policy does.
SELECT kingres.timepart_adopt() IS NOT NULL AS adopt_ran;
SELECT count(*) AS due FROM kingres.timepart_due('2030-01-01 UTC') WHERE table_name = 'v2';
\o /dev/null
CALL kingres.timepart_run('tpf.v2', '2026-05-20 UTC');
\o
SELECT tpf.parts('v2') AS v2_partitions_after_adoption;

-- ---- what adoption refuses ----
-- the owner named is not the owner of the table; the role does not exist; the
-- role is a superuser that does not own it
SELECT tpf.forge('v3', '11111111-1111-1111-1111-111111111111', 'tp_f_other');
SELECT tpf.forge('v4', '11111111-1111-1111-1111-111111111111', 'tp_f_nobody');
SELECT tpf.forge('v5', '11111111-1111-1111-1111-111111111111', :'su');
-- a policy for a table that is not there is left alone
SELECT tpf.forge('gone', '11111111-1111-1111-1111-111111111111', 'tp_f_owner');
-- a policy whose values the interface would refuse
SELECT tpf.forge('n1', '11111111-1111-1111-1111-111111111111', 'tp_f_owner', interval '1 hour');
SELECT kingres.timepart_adopt() IS NOT NULL AS adopt_ran;
SELECT table_name, problem FROM kingres.timepart_check() WHERE table_name IN ('v3', 'v4', 'v5', 'gone', 'n1') ORDER BY 1;
\o /dev/null
CALL kingres.timepart_run('tpf.v3', '2026-05-20 UTC');
CALL kingres.timepart_run('tpf.v4', '2026-05-20 UTC');
CALL kingres.timepart_run('tpf.v5', '2026-05-20 UTC');
\o
SELECT table_name, tpf.parts(table_name) AS partitions FROM unnest(ARRAY['v3', 'v4', 'v5']::name[]) table_name;
SELECT table_name, state, left(error, 40) AS error FROM kingres.timepart_run WHERE table_name IN ('v3', 'v4', 'v5') ORDER BY table_name;
-- the explicit override for a row that carries the stamp of this installation:
-- a superuser says that it knows the row; the checks of the catalog still hold
SELECT kingres.timepart_adopt(true) IS NOT NULL AS adopt_ran;
DELETE FROM kingres.timepart_policy_data WHERE table_name IN ('v3', 'v4', 'v5', 'gone', 'n1');

-- ---- a policy made by the interface, then edited by hand ----
SELECT kingres.timepart_set_policy('tpf.n1', '1 day', create_ahead => '1 day', retention => '10 days');
SELECT count(*) AS sealed_policies FROM kingres.timepart_policy_seal WHERE schema_name = 'tpf';
\o /dev/null
CALL kingres.timepart_run('tpf.n1', '2026-05-12 UTC');
\o
SELECT state FROM kingres.timepart_run WHERE table_name = 'n1' ORDER BY run_id DESC LIMIT 1;
-- a rename keeps the policy and its seal
ALTER TABLE tpf.n1 RENAME TO n1b;
SELECT table_name FROM kingres.timepart_policy WHERE schema_name = 'tpf' AND table_name LIKE 'n1%';
\o /dev/null
CALL kingres.timepart_run('tpf.n1b', '2026-05-13 UTC');
\o
SELECT state FROM kingres.timepart_run WHERE table_name = 'n1b' ORDER BY run_id DESC LIMIT 1;
-- the parts of the row that move with the schedule do not matter
UPDATE kingres.timepart_policy_data SET next_run_at = now(), updated_at = now(), parent_oid = 0 WHERE table_name = 'n1b';
SELECT kingres.timepart__is_sealed('tpf', 'n1b') AS still_sealed;
-- a field that decides what is done does: the policy is inert from then on
UPDATE kingres.timepart_policy_data SET retention = interval '1 day' WHERE table_name = 'n1b';
SELECT kingres.timepart__is_sealed('tpf', 'n1b') AS sealed_after_a_hand_edit;
\o /dev/null
CALL kingres.timepart_run('tpf.n1b', '2026-05-20 UTC');
\o
SELECT state, left(error, 50) AS error FROM kingres.timepart_run WHERE table_name = 'n1b' ORDER BY run_id DESC LIMIT 1;
SELECT tpf.parts('n1b') AS partitions_untouched;
-- the interface makes it right again
SELECT kingres.timepart_set_policy('tpf.n1b', '1 day', create_ahead => '1 day', retention => '10 days');
SELECT kingres.timepart__is_sealed('tpf', 'n1b') AS sealed_again;

-- ---- a role writes through the view; nothing it writes is trusted unless the
-- checks passed, and a role cannot ask for a seal for a table that is not its own
SET ROLE tp_f_owner;
SELECT kingres.timepart_set_policy('tpf.v3', '1 day', retention => '7 days');
RESET ROLE;
SELECT kingres.timepart__is_sealed('tpf', 'v3') AS owner_policy_sealed;
SET ROLE tp_f_other;
SELECT set_config('kingres_timepart.sealing', md5('tpf' || E'\x01' || 'v4') || ',', false) IS NOT NULL AS marker_set;
INSERT INTO kingres.timepart_policy (schema_name, table_name, partition_every) VALUES ('tpf', 'v4', '1 day');
SELECT kingres.timepart_set_policy('tpf.v4', '1 day');
RESET ROLE;
SELECT kingres.timepart__is_sealed('tpf', 'v4') AS policy_of_a_stranger_sealed;
-- the owner edits its own policy through the view, and the seal follows
SET ROLE tp_f_owner;
UPDATE kingres.timepart_policy SET retention = '9 days' WHERE table_name = 'v3';
RESET ROLE;
SELECT kingres.timepart__is_sealed('tpf', 'v3') AS sealed_after_the_owners_edit;
-- a superuser writing the view by hand is not the interface
INSERT INTO kingres.timepart_policy (schema_name, table_name, parent_oid, partition_every, retention)
VALUES ('tpf', 'v4', 0, '1 day', '2 days');
SELECT kingres.timepart__is_sealed('tpf', 'v4') AS superuser_by_hand_sealed;

-- ---- the seals are not data ----
-- not carried by a dump, gone with the policy, gone with a TRUNCATE
SELECT count(*) AS seal_table_in_dump FROM pg_extension e, unnest(e.extconfig) AS t(oid)
 WHERE e.extname = 'kingres_timepart' AND t.oid = 'kingres.timepart_policy_seal'::regclass;
SELECT kingres.timepart_unset_policy('tpf.v3');
SELECT count(*) AS seals_of_v3 FROM kingres.timepart_policy_seal WHERE table_name = 'v3';
SELECT tpf.forge('v3', kingres.timepart__stamp(), 'tp_f_owner');
SELECT kingres.timepart__is_sealed('tpf', 'v3') AS resurrected_row_sealed;
SELECT count(*) > 0 AS some_seals FROM kingres.timepart_policy_seal;
BEGIN;
TRUNCATE kingres.timepart_policy_data;
SELECT count(*) AS seals_after_truncate FROM kingres.timepart_policy_seal;
ROLLBACK;

-- ---- history rows that a dump shaped by hand ----
-- A run names the table, the owner and a state.  Whoever the owner it names is
-- sees it; nothing acts on it.  A role that does not exist, or a superuser,
-- is seen by superusers only.
SELECT kingres.timepart_set_policy('tpf.n1b', '1 day', create_ahead => '1 day', retention => '10 days');
CREATE FUNCTION tpf.forge_run(p_state text, p_stamp uuid, p_owner name) RETURNS bigint LANGUAGE sql AS $$
    INSERT INTO kingres.timepart_run_data
      (schema_name, table_name, as_of, trigger, state, started_by, stamp,
       table_oid, table_owner_oid, table_owner, backend_pid, backend_start)
    SELECT 'tpf', 'n1b', '2026-05-30', 'sql', p_state, p_owner, p_stamp,
           to_regclass('tpf.n1b')::oid,
           coalesce((SELECT r.oid FROM pg_roles r WHERE r.rolname = p_owner), 0), p_owner,
           pg_backend_pid() + 1000000, now()
    RETURNING run_id $$;
CREATE TEMP TABLE forged AS
SELECT 'owner'::text AS who, tpf.forge_run('done', kingres.timepart__stamp(), 'tp_f_owner') AS run_id
UNION ALL SELECT 'nobody', tpf.forge_run('done', kingres.timepart__stamp(), 'tp_f_nobody')
UNION ALL SELECT 'superuser', tpf.forge_run('done', kingres.timepart__stamp(), :'su')
UNION ALL SELECT 'running', tpf.forge_run('running', kingres.timepart__stamp(), 'tp_f_owner');
GRANT SELECT ON forged TO tp_f_owner, tp_f_other;
SELECT 'superuser sees' AS who, f.who AS row, EXISTS (SELECT 1 FROM kingres.timepart_run r WHERE r.run_id = f.run_id) AS seen FROM forged f ORDER BY f.run_id;
SET ROLE tp_f_owner;
SELECT 'the named owner sees' AS who, f.who AS row, EXISTS (SELECT 1 FROM kingres.timepart_run r WHERE r.run_id = f.run_id) AS seen FROM forged f ORDER BY f.run_id;
RESET ROLE;
SET ROLE tp_f_other;
SELECT 'another role sees' AS who, f.who AS row, EXISTS (SELECT 1 FROM kingres.timepart_run r WHERE r.run_id = f.run_id) AS seen FROM forged f ORDER BY f.run_id;
RESET ROLE;
-- a forged run that says "running" on a backend that is not there does not
-- stop the table: the next run goes on, and marks it
SELECT kingres.timepart_begin_run('tpf.n1b', '2026-05-30 UTC') > 0 AS begun;
SELECT f.who, r.state, r.error FROM forged f JOIN kingres.timepart_run_data r USING (run_id) WHERE f.who = 'running';
-- and a role cannot drive a run that is not its own: not the forged run of
-- another table, not through the owner's own functions
SELECT kingres.timepart_grant_driver('tp_f_other');
SET ROLE tp_f_other;
SELECT kingres.timepart_finish_run((SELECT run_id FROM forged WHERE who = 'owner'));
SELECT kingres.timepart_begin_run('tpf.n1b', '2026-05-30 UTC');
RESET ROLE;
SELECT kingres.timepart_revoke_driver('tp_f_other');

-- a function of a role that has no table, put in the WHERE clause of each view,
-- is never given a row of anybody else's table (security_barrier): it would
-- say so with a NOTICE
CREATE FUNCTION tpf.leaky(p anyelement) RETURNS boolean COST 0.00001 LANGUAGE plpgsql AS $$
BEGIN RAISE NOTICE 'leaked %', p; RETURN true; END $$;
GRANT EXECUTE ON FUNCTION tpf.leaky(anyelement) TO tp_f_other;
SET ROLE tp_f_other;
SELECT count(*) AS policies_seen FROM kingres.timepart_policy WHERE tpf.leaky(table_name);
SELECT count(*) AS runs_seen FROM kingres.timepart_run WHERE tpf.leaky(table_name);
SELECT count(*) AS actions_seen FROM kingres.timepart_run_action WHERE tpf.leaky(partition_name);
SELECT count(*) AS policies_seen FROM kingres.timepart_policies WHERE tpf.leaky(table_name);
RESET ROLE;

-- Cleanup.
DROP SCHEMA tpf CASCADE;
DELETE FROM kingres.timepart_run_data WHERE schema_name = 'tpf';
DROP OWNED BY tp_f_owner, tp_f_other;
DROP ROLE tp_f_owner, tp_f_other;
