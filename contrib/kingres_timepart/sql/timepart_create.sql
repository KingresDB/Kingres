-- timepart_create: create_time_partitions on all three key types, intervals,
-- daylight saving, tables with partitions made by hand, a default partition,
-- name conflicts, refused tables, and what a new partition inherits.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: PostgreSQL

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

-- A timestamptz table with an identity column, a generated column, a
-- primary key, an index, a check constraint, a default and a comment.
CREATE TABLE tp.ev (
    id      int GENERATED ALWAYS AS IDENTITY,
    ts      timestamptz NOT NULL,
    g       int GENERATED ALWAYS AS (id * 2) STORED,
    payload text DEFAULT 'x',
    PRIMARY KEY (id, ts),
    CONSTRAINT ev_payload_ok CHECK (payload <> 'bad')
) PARTITION BY RANGE (ts);
CREATE INDEX ev_payload_idx ON tp.ev (payload);
COMMENT ON COLUMN tp.ev.payload IS 'the payload';

SHOW lock_timeout;
SELECT kingres.create_time_partitions('tp.ev', '1 day', '2026-03-03 10:00 UTC', '2026-03-01 08:00 UTC');
SHOW lock_timeout;
SELECT * FROM tp.parts('tp.ev');
-- Idempotent: nothing is missing the second time.
SELECT kingres.create_time_partitions('tp.ev', '1 day', '2026-03-03 10:00 UTC', '2026-03-01 08:00 UTC');
-- Extend: only the new ones.
SELECT kingres.create_time_partitions('tp.ev', '1 day', '2026-03-05 00:00 UTC', '2026-03-01 08:00 UTC');
SELECT count(*) AS partitions, count(*) FILTER (WHERE true) - count(DISTINCT bounds) AS duplicates FROM tp.parts('tp.ev');
SELECT * FROM tp.gaps('tp.ev');

-- The new partitions got the structure of the parent: rows route, the
-- identity and the generated column work, indexes and the check exist.
INSERT INTO tp.ev (ts) VALUES ('2026-03-01 00:00 UTC'), ('2026-03-01 23:59:59.999999 UTC'),
                              ('2026-03-02 00:00 UTC'), ('2026-03-05 12:00 UTC');
SELECT tableoid::regclass AS part, id, ts, g, payload FROM tp.ev ORDER BY ts;
INSERT INTO tp.ev (ts, payload) VALUES ('2026-03-02 01:00 UTC', 'bad');
INSERT INTO tp.ev (ts) VALUES ('2026-03-06 00:00 UTC');
SELECT indexrelid::regclass AS index FROM pg_index WHERE indrelid = 'tp.ev_p20260302'::regclass ORDER BY 1;
SELECT conname FROM pg_constraint WHERE conrelid = 'tp.ev_p20260302'::regclass ORDER BY 1;
SELECT col_description('tp.ev_p20260302'::regclass, 4) AS comment;
SELECT pg_get_expr(d.adbin, d.adrelid) AS payload_default
  FROM pg_attrdef d JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
 WHERE d.adrelid = 'tp.ev_p20260302'::regclass AND a.attname = 'payload';
SELECT c.relkind, c.relispartition FROM pg_class c WHERE c.oid = 'tp.ev_p20260302'::regclass;

-- Access method: the one given, and a refusal of one that does not exist.
CREATE TABLE tp.am (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.am', '1 day', '2026-03-02 UTC', '2026-03-01 UTC', 'heap');
SELECT kingres.create_time_partitions('tp.am', '1 day', '2026-03-04 UTC', '2026-03-03 UTC', 'no_such_am');
SELECT count(*) AS partitions FROM tp.parts('tp.am');

-- timestamp and date keys: the bounds are wall-clock values, whatever the
-- session time zone.
CREATE TABLE tp.nt (ts timestamp NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.dt (d date NOT NULL) PARTITION BY RANGE (d);
SET timezone = 'America/Los_Angeles';
SELECT kingres.create_time_partitions('tp.nt', '6 hours', '2026-03-01 12:00 UTC', '2026-03-01 00:00 UTC');
SELECT * FROM tp.parts('tp.nt');
SELECT kingres.create_time_partitions('tp.dt', '1 month', '2026-04-15 00:00 UTC', '2026-02-10 00:00 UTC');
SELECT * FROM tp.parts('tp.dt');
SET timezone = 'UTC';
-- With a zone, "now" for a naive key is read in that zone: 22:30 UTC on the
-- 1st is already the 2nd in Tokyo.
CREATE TABLE tp.dt2 (d date NOT NULL) PARTITION BY RANGE (d);
SELECT kingres.create_time_partitions('tp.dt2', '1 day', '2026-03-01 22:30 UTC', '2026-03-01 22:30 UTC', NULL, 'Asia/Tokyo');
SELECT * FROM tp.parts('tp.dt2');
-- Hours need a time of day.
SELECT kingres.create_time_partitions('tp.dt', '1 hour', '2026-04-15 UTC', '2026-04-15 UTC');
INSERT INTO tp.dt VALUES ('2026-02-28'), ('2026-03-01'), ('2026-04-30');
SELECT tableoid::regclass AS part, d FROM tp.dt ORDER BY d;

-- Daylight saving: daily partitions in Berlin over the spring change.  The
-- bounds are local midnights, the day of the change is 23 hours, and rows
-- at the edges land in the right partition.
CREATE TABLE tp.de (ts timestamptz NOT NULL, label text) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.de', '1 day', '2026-03-30 12:00 UTC', '2026-03-27 12:00 UTC', NULL, 'Europe/Berlin');
SELECT * FROM tp.parts('tp.de');
SELECT * FROM tp.gaps('tp.de');
INSERT INTO tp.de VALUES ('2026-03-28 22:59:59 UTC', 'last second of the 28th'),
                         ('2026-03-28 23:00:00 UTC', 'first second of the 29th'),
                         ('2026-03-29 21:59:59 UTC', 'last second of the 29th'),
                         ('2026-03-29 22:00:00 UTC', 'first second of the 30th');
SELECT tableoid::regclass AS part, ts, label FROM tp.de ORDER BY ts;
-- The same in the autumn: the day of the change is 25 hours.
CREATE TABLE tp.de2 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.de2', '1 day', '2026-10-26 12:00 UTC', '2026-10-24 12:00 UTC', NULL, 'Europe/Berlin');
SELECT * FROM tp.parts('tp.de2');
-- Hours in Berlin over the spring change: absolute lengths, named in UTC.
CREATE TABLE tp.dh (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.dh', '12 hours', '2026-03-29 12:00 UTC', '2026-03-28 12:00 UTC', NULL, 'Europe/Berlin');
SELECT * FROM tp.parts('tp.dh');
-- The session time zone changes nothing.
CREATE TABLE tp.de3 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SET timezone = 'Pacific/Auckland';
SELECT kingres.create_time_partitions('tp.de3', '1 day', '2026-03-30 12:00 UTC', '2026-03-27 12:00 UTC', NULL, 'Europe/Berlin');
SET timezone = 'UTC';
SELECT (SELECT array_agg(bounds) FROM tp.parts('tp.de3')) = (SELECT array_agg(bounds) FROM tp.parts('tp.de')) AS same_bounds;

-- Weeks, months, years, and a month interval that is longer than the range.
CREATE TABLE tp.wk (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.wk', '1 week', '2026-03-16 00:00 UTC', '2026-03-04 10:00 UTC');
SELECT * FROM tp.parts('tp.wk');
CREATE TABLE tp.yr (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.yr', '1 year', '2028-06-01 UTC', '2023-02-15 UTC');
SELECT * FROM tp.parts('tp.yr');
CREATE TABLE tp.mo (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.mo', '1 month', '2024-04-01 UTC', '2024-01-31 UTC');
SELECT * FROM tp.parts('tp.mo');

-- Partitions made by hand with other bounds: a cell that overlaps one is
-- left alone, the cells around it are created, and the gap that remains is
-- the hand-made one's.
CREATE TABLE tp.hand (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.hand_jan PARTITION OF tp.hand FOR VALUES FROM ('2026-01-01 UTC') TO ('2026-01-15 UTC');
CREATE TABLE tp.hand_wide PARTITION OF tp.hand FOR VALUES FROM ('2026-03-01 UTC') TO ('2026-05-01 UTC');
SELECT kingres.create_time_partitions('tp.hand', '1 month', '2026-07-01 UTC', '2026-01-01 UTC');
SELECT * FROM tp.parts('tp.hand');
SELECT * FROM tp.gaps('tp.hand');
-- An existing partition with the same bounds and another name counts as the cell.
CREATE TABLE tp.named (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.my_march PARTITION OF tp.named FOR VALUES FROM ('2026-03-01 UTC') TO ('2026-04-01 UTC');
SELECT kingres.create_time_partitions('tp.named', '1 month', '2026-04-15 UTC', '2026-03-01 UTC');
SELECT * FROM tp.parts('tp.named');

-- A relation that already has the name of a partition to create: the call
-- fails, and nothing of it remains (one transaction).
CREATE TABLE tp.clash (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.clash_p20260303 (x int);
SELECT kingres.create_time_partitions('tp.clash', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
SELECT count(*) AS partitions FROM tp.parts('tp.clash');
SELECT to_regclass('tp.clash_p20260301') IS NULL AS first_one_rolled_back;

-- A default partition: refused up front, with the reason.
CREATE TABLE tp.withdef (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.withdef_default PARTITION OF tp.withdef DEFAULT;
SELECT kingres.create_time_partitions('tp.withdef', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');

-- Tables that are not suitable.
CREATE TABLE tp.plain (ts timestamptz);
SELECT kingres.create_time_partitions('tp.plain', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
CREATE TABLE tp.lst (ts timestamptz NOT NULL) PARTITION BY LIST (ts);
SELECT kingres.create_time_partitions('tp.lst', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
CREATE TABLE tp.hsh (id int) PARTITION BY HASH (id);
SELECT kingres.create_time_partitions('tp.hsh', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
CREATE TABLE tp.expr (ts timestamptz NOT NULL) PARTITION BY RANGE ((ts AT TIME ZONE 'UTC'));
SELECT kingres.create_time_partitions('tp.expr', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
CREATE TABLE tp.multi (ts timestamptz NOT NULL, k int NOT NULL) PARTITION BY RANGE (ts, k);
SELECT kingres.create_time_partitions('tp.multi', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
CREATE TABLE tp.badtype (n int NOT NULL) PARTITION BY RANGE (n);
SELECT kingres.create_time_partitions('tp.badtype', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
CREATE TEMP TABLE tmp_t (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tmp_t', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');

-- Bad arguments.
SELECT kingres.create_time_partitions('tp.am', '5 hours', '2026-03-05 UTC', '2026-03-01 UTC');
SELECT kingres.create_time_partitions('tp.am', '1 day', '2026-03-01 UTC', '2026-03-05 UTC');
SELECT kingres.create_time_partitions('tp.am', '1 day', 'infinity', '2026-03-05 UTC');
SELECT kingres.create_time_partitions('tp.am', '1 day', '2026-03-05 UTC', '2026-03-01 UTC', NULL, 'Nowhere/Land');
-- Too many partitions in one call.
CREATE TABLE tp.many (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.many', '1 hour', '2030-01-01 UTC', '2026-01-01 UTC');
SELECT count(*) AS partitions FROM tp.parts('tp.many');
