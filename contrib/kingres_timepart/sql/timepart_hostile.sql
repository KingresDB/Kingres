-- timepart_hostile: a real attack on the search path.  A schema that comes
-- first on the path holds a look-alike, with the same argument types, of every
-- function of pg_catalog that a function or procedure of the extension calls,
-- and look-alike operators.  Every look-alike warns "hijacked" when it is
-- called.  Every function and procedure of the extension is then called, as a
-- superuser (the scheduler) and as the owner of a table, with that path: no
-- warning may appear, no error from a look-alike, and no role may become a
-- superuser.  The procedures that commit cannot carry a SET clause, so they
-- are the ones this test is for; the others carry their own search_path.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

CREATE SCHEMA hostile;
GRANT USAGE, CREATE ON SCHEMA hostile TO tp_other;

-- The names that the bodies of the extension call: every word followed by an
-- opening parenthesis in the source of a member function, that is also a
-- function of pg_catalog.  The look-alikes take the same argument types, so
-- they win wherever hostile comes first.
DO $$
DECLARE
    r       record;
    v_names text[];
    v_made  integer := 0;
BEGIN
    SELECT array_agg(DISTINCT lower(m[1])) INTO v_names
      FROM pg_proc p
      JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
                      AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
      JOIN pg_extension e ON e.oid = d.refobjid AND e.extname = 'kingres_timepart',
      LATERAL regexp_matches(p.prosrc, '([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*\(', 'g') m;
    FOR r IN SELECT p.proname, pg_get_function_identity_arguments(p.oid) AS args,
                    pg_get_function_result(p.oid) AS res
               FROM pg_proc p
              WHERE p.pronamespace = 'pg_catalog'::regnamespace
                AND p.prokind = 'f' AND p.proargmodes IS NULL
                AND p.proname = ANY (v_names)
              ORDER BY p.proname, p.oid
    LOOP
        BEGIN
            EXECUTE format(
                'CREATE FUNCTION hostile.%I(%s) RETURNS %s LANGUAGE plpgsql AS $f$ '
                'BEGIN RAISE WARNING ''hijacked %s''; %s END $f$',
                r.proname, r.args, r.res, r.proname,
                CASE WHEN r.res = 'void' OR r.res LIKE 'SETOF %' OR r.res LIKE 'TABLE%'
                     THEN 'RETURN;' ELSE 'RETURN NULL;' END);
            v_made := v_made + 1;
        EXCEPTION WHEN OTHERS THEN
            NULL;   -- a signature plpgsql cannot take (variadic "any", ...)
        END;
    END LOOP;
    RAISE NOTICE 'look-alikes made: more than 25 is %', v_made > 25;
END $$;

-- The few the generator cannot make: format() takes a variadic "any".
CREATE FUNCTION hostile.format(text, text) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked format'; RETURN 'x'; END $$;
CREATE FUNCTION hostile.format(text, text, text) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked format'; RETURN 'x'; END $$;
CREATE FUNCTION hostile.format(text, name, name) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked format'; RETURN 'x'; END $$;
CREATE FUNCTION hostile.format(text, text, name) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked format'; RETURN 'x'; END $$;
CREATE FUNCTION hostile.format(text, name, text) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked format'; RETURN 'x'; END $$;
CREATE FUNCTION hostile.unnest(text[]) RETURNS SETOF text LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked unnest'; RETURN QUERY SELECT 'create'::text; END $$;

-- Operators: the comparisons and the concatenation on the types the bodies use.
CREATE FUNCTION hostile.op_bool(text, text) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked operator'; RETURN true; END $$;
CREATE FUNCTION hostile.op_bool(name, name) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked operator'; RETURN true; END $$;
CREATE FUNCTION hostile.op_bool(name, text) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked operator'; RETURN true; END $$;
CREATE FUNCTION hostile.op_bool(text, name) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked operator'; RETURN true; END $$;
CREATE FUNCTION hostile.op_bool(integer, integer) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked operator'; RETURN true; END $$;
CREATE FUNCTION hostile.op_bool(bigint, bigint) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked operator'; RETURN true; END $$;
CREATE FUNCTION hostile.op_bool(oid, oid) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked operator'; RETURN true; END $$;
CREATE FUNCTION hostile.op_cat(text, text) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE WARNING 'hijacked operator'; RETURN 'x'; END $$;
CREATE OPERATOR hostile.= (LEFTARG = text, RIGHTARG = text, FUNCTION = hostile.op_bool);
CREATE OPERATOR hostile.= (LEFTARG = name, RIGHTARG = name, FUNCTION = hostile.op_bool);
CREATE OPERATOR hostile.= (LEFTARG = name, RIGHTARG = text, FUNCTION = hostile.op_bool);
CREATE OPERATOR hostile.= (LEFTARG = text, RIGHTARG = name, FUNCTION = hostile.op_bool);
CREATE OPERATOR hostile.= (LEFTARG = integer, RIGHTARG = integer, FUNCTION = hostile.op_bool);
CREATE OPERATOR hostile.= (LEFTARG = bigint, RIGHTARG = bigint, FUNCTION = hostile.op_bool);
CREATE OPERATOR hostile.= (LEFTARG = oid, RIGHTARG = oid, FUNCTION = hostile.op_bool);
CREATE OPERATOR hostile.<> (LEFTARG = text, RIGHTARG = text, FUNCTION = hostile.op_bool);
CREATE OPERATOR hostile.<> (LEFTARG = name, RIGHTARG = name, FUNCTION = hostile.op_bool);
CREATE OPERATOR hostile.|| (LEFTARG = text, RIGHTARG = text, FUNCTION = hostile.op_cat);

-- The attack: the path of the caller lists hostile before pg_catalog.
CREATE TABLE tp.hz (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.hz2 (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tp.hz2 OWNER TO tp_owner;
GRANT USAGE, CREATE ON SCHEMA tp TO tp_owner;
SET search_path = hostile, pg_catalog, public;
-- A control: the look-alikes do win for a caller that does not protect itself.
SELECT quote_ident('control');

-- Every function and procedure of the extension, as the superuser.
SELECT kingres.create_time_partitions('tp.hz', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('tp.hz', '1 day', create_ahead => '2 days', retention => '3 days');
SELECT count(*) AS planned FROM kingres.timepart_plan('tp.hz', '2026-03-05 UTC');
SELECT count(*) AS partitions FROM kingres.timepart_partitions('tp.hz');
SELECT count(*) AS problems FROM kingres.timepart_check('1 hour', '2026-03-05 UTC') WHERE table_name LIKE 'hz%';
SELECT count(*) AS due FROM kingres.timepart_due('2100-01-01 UTC') WHERE table_name LIKE 'hz';
SELECT kingres.timepart_begin_run('tp.hz', '2026-03-05 UTC', 'manual') AS run_id \gset
CALL kingres.timepart_run_phase(:run_id, 'create');
CALL kingres.timepart_run_phase(:run_id, 'convert');
CALL kingres.timepart_run_phase(:run_id, 'retire');
SELECT kingres.timepart_run_action(:run_id, 'create');
SELECT kingres.timepart_finish_run(:run_id);
SELECT kingres.timepart_prune_runs('tp', 'hz', '2026-03-05 UTC');
SELECT kingres.timepart_action_timeout();
\o /dev/null
CALL kingres.timepart_run('tp.hz', '2026-03-06 UTC');
CALL kingres.timepart_run('tp.hz', '2026-03-07 UTC', '{create,retire}');
\o
SELECT kingres.drop_old_time_partitions('tp.hz', '2026-03-03 UTC');
CALL kingres.convert_partition('tp.hz', 'tp.hz_p20260304');
CALL kingres.convert_old_partitions('tp.hz', '2026-03-03 UTC');
CALL kingres.timepart_run('tp.hz', '2026-03-07 UTC', '{create,explode}');
SELECT kingres.timepart_unset_policy('tp.hz');

-- And as the owner of a table, who is no superuser.
SET ROLE tp_owner;
SELECT kingres.create_time_partitions('tp.hz2', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('tp.hz2', '1 day', create_ahead => '2 days', retention => '3 days');
SELECT count(*) AS planned FROM kingres.timepart_plan('tp.hz2', '2026-03-05 UTC');
SELECT count(*) AS partitions FROM kingres.timepart_partitions('tp.hz2');
SELECT count(*) AS problems FROM kingres.timepart_check('1 hour', '2026-03-05 UTC') WHERE table_name LIKE 'hz%';
SELECT count(*) AS visible_policies FROM kingres.timepart_policy;
SELECT kingres.drop_old_time_partitions('tp.hz2', '2026-03-03 UTC');
SELECT kingres.timepart_unset_policy('tp.hz2');
RESET ROLE;

-- The run of a table owned by a role that is not a superuser, under the same path.
SELECT kingres.timepart_set_policy('tp.hz2', '1 day', create_ahead => '2 days', retention => '3 days');
\o /dev/null
CALL kingres.timepart_run('tp.hz2', '2026-03-08 UTC');
\o
RESET search_path;
-- Both runs ended well, and no look-alike was called (no warning above).
SELECT table_name, state FROM kingres.timepart_run WHERE table_name IN ('hz', 'hz2') ORDER BY run_id DESC LIMIT 2;

-- The hostile role that can create functions in hostile: still no superuser.
SELECT rolname, rolsuper FROM pg_roles WHERE rolname IN ('tp_other', 'tp_owner') ORDER BY 1;

DROP TABLE tp.hz, tp.hz2;
REVOKE ALL ON SCHEMA tp FROM tp_owner;
SET client_min_messages = warning;
DROP SCHEMA hostile CASCADE;
RESET client_min_messages;
