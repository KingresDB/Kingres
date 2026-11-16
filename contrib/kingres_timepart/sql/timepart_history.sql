-- SPDX-License-Identifier: AGPL-3.0-only
-- timepart_history: who sees the history of a run (K12S-01), and what a
-- renamed role does to a policy (K12S-02).
--
-- The rule: a run belongs to the table it was made for and to the role that
-- owned that table when the run began.  A role sees the run when it is that
-- role or a member that inherits its privileges, or a superuser.  A table of
-- the same name is another table; a renamed table, a renamed schema and a
-- renamed role are the same.  After ALTER TABLE ... OWNER TO the earlier runs
-- stay with the earlier owner, and the runs after it belong to the new one.
-- A run whose owner role was dropped is shown to superusers only.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

SET client_min_messages = warning;
DROP ROLE IF EXISTS tp_h_alice, tp_h_alice2, tp_h_bob, tp_h_carol, tp_h_dave, tp_h_zed;
RESET client_min_messages;
CREATE ROLE tp_h_alice NOLOGIN;
CREATE ROLE tp_h_bob NOLOGIN;
CREATE ROLE tp_h_carol NOLOGIN IN ROLE tp_h_alice;
CREATE ROLE tp_h_dave NOLOGIN NOINHERIT IN ROLE tp_h_alice;
CREATE ROLE tp_h_zed NOLOGIN;
CREATE SCHEMA tph;
GRANT ALL ON SCHEMA tph TO tp_h_alice, tp_h_bob, tp_h_zed;
GRANT USAGE ON SCHEMA kingres TO tp_h_alice, tp_h_bob, tp_h_carol, tp_h_dave, tp_h_zed;
GRANT EXECUTE ON FUNCTION
    kingres.timepart_set_policy(regclass, interval, interval, interval, text, interval, text[], jsonb, text, interval, boolean),
    kingres.timepart_unset_policy(regclass),
    kingres.timepart_partitions(regclass),
    kingres.timepart_check(interval, timestamptz)
  TO tp_h_alice, tp_h_bob, tp_h_carol, tp_h_dave, tp_h_zed;

-- What the current role sees of the history of the tables named tph.<name>.
CREATE FUNCTION tp.seen(p_tables text[])
RETURNS TABLE (n bigint, run_id bigint, tbl name, owner name, state text, live boolean, actions bigint)
LANGUAGE sql STABLE AS $$
    SELECT row_number() OVER (ORDER BY r.run_id), r.run_id, r.table_name, r.table_owner, r.state,
           r.table_oid IS NOT NULL,
           (SELECT count(*) FROM kingres.timepart_run_action a WHERE a.run_id = r.run_id)
      FROM kingres.timepart_run r
     WHERE r.table_name = ANY (p_tables) ORDER BY r.run_id
$$;
GRANT USAGE ON SCHEMA tp TO tp_h_alice, tp_h_bob, tp_h_carol, tp_h_dave, tp_h_zed;
-- The runs of every table of this test as a superuser sees them, by role.
CREATE FUNCTION tp.who_sees(p_run bigint)
RETURNS text
LANGUAGE plpgsql AS $$
DECLARE
    v_role text;
    v_out  text := '';
    v_n    bigint;
BEGIN
    FOREACH v_role IN ARRAY ARRAY['tp_h_alice', 'tp_h_alice2', 'tp_h_bob', 'tp_h_carol', 'tp_h_dave', 'tp_h_zed'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role) THEN
            EXECUTE format('SET LOCAL ROLE %I', v_role);
            SELECT count(*) INTO v_n FROM kingres.timepart_run r WHERE r.run_id = p_run;
            RESET ROLE;
            IF v_n > 0 THEN v_out := v_out || ' ' || v_role; END IF;
        END IF;
    END LOOP;
    RETURN '{' || trim(v_out) || '}';
END $$;

-- ---------------------------------------------------------------------
-- The review's case: a dropped table, and another role's table of its name.
-- ---------------------------------------------------------------------
SET ROLE tp_h_alice;
CREATE TABLE tph.ta (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tph.ta', '1 day', create_ahead => '2 days', retention => '5 days');
RESET ROLE;
\o /dev/null
CALL kingres.timepart_run('tph.ta', '2026-03-10 UTC');
\o
SELECT n, tbl, owner, state, live, actions FROM tp.seen('{ta}');
SELECT tp.who_sees(min(run_id)) AS who_sees_alice_run FROM kingres.timepart_run WHERE table_name = 'ta';
SET ROLE tp_h_alice;
DROP TABLE tph.ta;
SELECT n, tbl, owner, state, live, actions FROM tp.seen('{ta}');
RESET ROLE;
-- bob makes a table of the same name
SET ROLE tp_h_bob;
CREATE TABLE tph.ta (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT n, tbl, owner, state, live, actions FROM tp.seen('{ta}');
SELECT count(*) AS runs_seen_by_bob FROM kingres.timepart_run;
SELECT count(*) AS actions_seen_by_bob FROM kingres.timepart_run_action;
SELECT count(*) AS partitions_listed FROM kingres.timepart_partitions('tph.ta');
SELECT kingres.timepart_set_policy('tph.ta', '1 day', create_ahead => '2 days');
RESET ROLE;
\o /dev/null
CALL kingres.timepart_run('tph.ta', '2026-03-10 UTC');
\o
SET ROLE tp_h_bob;
SELECT n, tbl, owner, state, live, actions FROM tp.seen('{ta}');
SELECT table_name, problem FROM kingres.timepart_check('1 hour', '2026-03-10 UTC') WHERE table_name = 'ta';
SELECT s.owner, s.state, s.live FROM kingres.timepart_policies p, LATERAL tp.seen('{ta}') s WHERE p.table_name = 'ta' AND s.run_id = p.last_run_id;
RESET ROLE;
SET ROLE tp_h_alice;
SELECT n, tbl, owner, state, live, actions FROM tp.seen('{ta}');
RESET ROLE;
-- the superuser sees both; the history is not lost
SELECT n, tbl, owner, state, live, actions FROM tp.seen('{ta}');
SELECT tp.who_sees(run_id) AS who_sees, table_owner, table_oid IS NOT NULL AS live FROM kingres.timepart_run WHERE table_name = 'ta' ORDER BY run_id;

-- A table of the same name made by the same role is another table, too:
-- its list of partitions does not hold the partitions the first one detached.
SET ROLE tp_h_alice;
CREATE TABLE tph.tb (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tph.tb', '1 day', create_ahead => '1 day', retention => '2 days', retention_action => 'detach');
RESET ROLE;
\o /dev/null
CALL kingres.timepart_run('tph.tb', '2026-03-10 UTC');
CALL kingres.timepart_run('tph.tb', '2026-03-14 UTC');
\o
SET ROLE tp_h_alice;
SELECT partition, attached FROM kingres.timepart_partitions('tph.tb') ORDER BY 1;
DROP TABLE tph.tb;
CREATE TABLE tph.tb (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT partition, attached FROM kingres.timepart_partitions('tph.tb') ORDER BY 1;
SELECT n, tbl, owner, state, live, actions FROM tp.seen('{tb}');
RESET ROLE;
DROP TABLE IF EXISTS tph.tb_p20260309, tph.tb_p20260310, tph.tb_p20260311;
DROP TABLE tph.tb;

-- ---------------------------------------------------------------------
-- Renames: a table, its schema, and the history stays with the owner.
-- ---------------------------------------------------------------------
SET ROLE tp_h_alice;
CREATE TABLE tph.tc (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tph.tc', '1 day', create_ahead => '1 day');
RESET ROLE;
\o /dev/null
CALL kingres.timepart_run('tph.tc', '2026-03-10 UTC');
\o
SET ROLE tp_h_alice;
ALTER TABLE tph.tc RENAME TO tc2;
SELECT n, tbl, owner, state, live, actions FROM tp.seen('{tc,tc2}');
RESET ROLE;
CREATE SCHEMA tph_new;
ALTER SCHEMA tph RENAME TO tph_old;
ALTER SCHEMA tph_new RENAME TO tph;
SET ROLE tp_h_alice;
SELECT n, tbl, owner, state, live, actions FROM tp.seen('{tc,tc2}');
RESET ROLE;
DROP SCHEMA tph;
ALTER SCHEMA tph_old RENAME TO tph;
SET ROLE tp_h_alice;
SELECT n, tbl, owner, state, live, actions FROM tp.seen('{tc,tc2}');
SELECT live FROM tp.seen('{tc2}');
ALTER TABLE tph.tc2 RENAME TO tc;
RESET ROLE;

-- ---------------------------------------------------------------------
-- A member that inherits the owner's privileges sees the history; one that
-- does not, until it does SET ROLE.
-- ---------------------------------------------------------------------
SELECT tp.who_sees(max(run_id)) AS who_sees_tc FROM kingres.timepart_run WHERE table_name = 'tc';
SET ROLE tp_h_dave;
SELECT count(*) AS dave_noinherit FROM kingres.timepart_run WHERE table_name = 'tc';
RESET ROLE;
SET ROLE tp_h_carol;
SELECT count(*) AS carol_inherits FROM kingres.timepart_run WHERE table_name = 'tc';
RESET ROLE;

-- ---------------------------------------------------------------------
-- ALTER TABLE ... OWNER TO: the earlier runs stay with the earlier owner, the
-- runs after the change are the new owner's.
-- ---------------------------------------------------------------------
ALTER TABLE tph.tc OWNER TO tp_h_bob;
\o /dev/null
CALL kingres.timepart_run('tph.tc', '2026-03-11 UTC');
\o
SELECT n, tbl, owner, state FROM tp.seen('{tc}');
SELECT left(r.error, 90) AS error FROM kingres.timepart_run r WHERE r.table_name = 'tc' AND r.error IS NOT NULL;
SET ROLE tp_h_alice;
SELECT n, owner, state FROM tp.seen('{tc}');
RESET ROLE;
SET ROLE tp_h_bob;
SELECT n, owner, state FROM tp.seen('{tc}');
SELECT kingres.timepart_set_policy('tph.tc', '1 day', create_ahead => '1 day');
RESET ROLE;
\o /dev/null
CALL kingres.timepart_run('tph.tc', '2026-03-12 UTC');
\o
SET ROLE tp_h_bob;
SELECT n, owner, state FROM tp.seen('{tc}');
RESET ROLE;
SET ROLE tp_h_alice;
SELECT n, owner, state FROM tp.seen('{tc}');
RESET ROLE;
SELECT n, owner, state FROM tp.seen('{tc}');

-- ---------------------------------------------------------------------
-- K12S-02: a role that is renamed is the same owner.
-- ---------------------------------------------------------------------
SET ROLE tp_h_alice;
CREATE TABLE tph.td (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tph.td', '1 day', create_ahead => '1 day');
RESET ROLE;
SELECT table_owner, created_by FROM kingres.timepart_policy WHERE table_name = 'td';
ALTER ROLE tp_h_alice RENAME TO tp_h_alice2;
SELECT table_owner, created_by FROM kingres.timepart_policy WHERE table_name = 'td';
\o /dev/null
CALL kingres.timepart_run('tph.td', '2026-03-10 UTC');
\o
SELECT state, error FROM tp.last_run('tph', 'td');
SELECT n, owner, state FROM tp.seen('{td}');
SELECT started_by = current_user AS started_by_is_the_session_user FROM kingres.timepart_run WHERE table_name = 'td';
SET ROLE tp_h_alice2;
SELECT n, owner, state FROM tp.seen('{td}');
SELECT table_owner, created_by FROM kingres.timepart_policy WHERE table_name = 'td';
SELECT kingres.timepart_set_policy('tph.td', '1 day', create_ahead => '2 days');
RESET ROLE;
-- the partitions the run made belong to the renamed role
SELECT DISTINCT pg_get_userbyid(c.relowner) AS partition_owner
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'tph.td'::regclass;
-- another role with the name the owner had is not the owner, and the one that
-- was renamed still is
CREATE ROLE tp_h_alice NOLOGIN;
GRANT ALL ON SCHEMA tph TO tp_h_alice;
GRANT USAGE ON SCHEMA kingres TO tp_h_alice;
ALTER TABLE tph.td OWNER TO tp_h_alice;
\o /dev/null
CALL kingres.timepart_run('tph.td', '2026-03-12 UTC');
\o
SELECT state, left(error, 120) AS error FROM tp.last_run('tph', 'td');
ALTER TABLE tph.td OWNER TO tp_h_alice2;
\o /dev/null
CALL kingres.timepart_run('tph.td', '2026-03-13 UTC');
\o
SELECT state, error FROM tp.last_run('tph', 'td');
SELECT n, owner, state FROM tp.seen('{td}');
SELECT tp.who_sees(run_id) AS who_sees_td_run FROM kingres.timepart_run WHERE table_name = 'td' ORDER BY run_id DESC LIMIT 1;

-- ---------------------------------------------------------------------
-- A dropped owner: only superusers see the runs, also when a role of the same
-- name is made later.
-- ---------------------------------------------------------------------
SET ROLE tp_h_zed;
CREATE TABLE tph.te (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tph.te', '1 day', create_ahead => '1 day');
RESET ROLE;
\o /dev/null
CALL kingres.timepart_run('tph.te', '2026-03-10 UTC');
\o
SELECT tp.who_sees(run_id) AS who_sees_te FROM kingres.timepart_run WHERE table_name = 'te';
DROP TABLE tph.te;
DROP OWNED BY tp_h_zed;
DROP ROLE tp_h_zed;
SELECT tp.who_sees(run_id) AS who_sees_te_owner_dropped, table_owner, table_oid IS NULL AS table_gone
  FROM kingres.timepart_run WHERE table_name = 'te';
CREATE ROLE tp_h_zed NOLOGIN;
GRANT USAGE ON SCHEMA kingres TO tp_h_zed;
SELECT tp.who_sees(run_id) AS who_sees_te_new_role_same_name, table_owner
  FROM kingres.timepart_run WHERE table_name = 'te';

-- ---------------------------------------------------------------------
-- The same through the other views and functions.
-- ---------------------------------------------------------------------
SELECT count(*) AS actions_of_te_seen_by_superuser FROM kingres.timepart_run_action a JOIN kingres.timepart_run r USING (run_id) WHERE r.table_name = 'te';
SET ROLE tp_h_zed;
SELECT count(*) AS actions_of_te_seen_by_new_zed FROM kingres.timepart_run_action a JOIN kingres.timepart_run r USING (run_id) WHERE r.table_name = 'te';
SELECT count(*) AS runs_seen_by_new_zed FROM kingres.timepart_run;
RESET ROLE;

-- Cleanup of the roles of the test.
DROP TABLE tph.ta, tph.tc, tph.td CASCADE;
DROP SCHEMA tph CASCADE;
DROP FUNCTION tp.seen(text[]), tp.who_sees(bigint);
DROP OWNED BY tp_h_alice, tp_h_alice2, tp_h_bob, tp_h_carol, tp_h_dave, tp_h_zed;
DROP ROLE tp_h_alice, tp_h_alice2, tp_h_bob, tp_h_carol, tp_h_dave, tp_h_zed;
