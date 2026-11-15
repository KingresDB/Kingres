-- timepart_grid: the grid of partition bounds: accepted and refused
-- intervals, cells across daylight-saving changes, leap years, month ends,
-- weeks, and the generated names.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: PostgreSQL

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

-- Accepted shapes: unit and count.
SELECT every, p.unit, p.n
  FROM unnest(ARRAY['1 hour', '2 hours', '3 hours', '4 hours', '6 hours', '8 hours',
                    '12 hours', '24 hours', '1 day', '3 days', '10 days', '7 days',
                    '1 week', '1 month', '2 months', '3 months', '4 months',
                    '6 months', '12 months', '1 year', '5 years']::interval[]) AS every,
       LATERAL kingres.timepart_interval_parts(every) p;

-- Refused shapes.
SELECT kingres.timepart_interval_parts('5 hours');
SELECT kingres.timepart_interval_parts('90 minutes');
SELECT kingres.timepart_interval_parts('1 day 1 hour');
SELECT kingres.timepart_interval_parts('1 month 1 day');
SELECT kingres.timepart_interval_parts('5 months');
SELECT kingres.timepart_interval_parts('18 months');
SELECT kingres.timepart_interval_parts('0 days');
SELECT kingres.timepart_interval_parts('-1 day');
SELECT kingres.timepart_interval_parts('10000 days');
SELECT kingres.timepart_interval_parts(NULL);
SELECT kingres.timepart__check_tz('Mars/Olympus');
SELECT kingres.timepart__check_tz('+02');

-- Cells never overlap and never leave a gap, and every instant is in the
-- cell cell_of says: for each shape and zone, over two years that contain
-- daylight-saving changes, cell_of(start(k)) = k, cell_of(start(k+1) - 1 us)
-- = k, and the cells are strictly increasing.  Zones: none, whole hour,
-- half hour (Lord Howe), a change at midnight (Havana), a half-hour offset
-- without change (Kolkata).
WITH shapes(every) AS (
    VALUES ('1 hour'::interval), ('6 hours'), ('1 day'), ('3 days'), ('1 week'),
           ('1 month'), ('3 months'), ('1 year')),
zones(tz) AS (
    VALUES ('UTC'), ('Europe/Berlin'), ('America/New_York'),
           ('Australia/Lord_Howe'), ('America/Havana'), ('Asia/Kolkata')),
base AS (
    SELECT every, tz,
           kingres.timepart_cell_of(every, tz, timestamptz '2025-01-01 00:00 UTC') AS k0,
           kingres.timepart_cell_of(every, tz, timestamptz '2026-12-31 00:00 UTC') AS k1
      FROM shapes, zones),
ks AS (
    SELECT every, tz, k
      FROM base, LATERAL generate_series(k0, least(k1, k0 + 400)) k)
SELECT every, tz, count(*) AS cells,
       bool_and(kingres.timepart_cell_of(every, tz, kingres.timepart_cell_start(every, tz, k)) = k) AS start_in_cell,
       bool_and(kingres.timepart_cell_of(every, tz, kingres.timepart_cell_start(every, tz, k + 1) - interval '1 microsecond') = k) AS end_in_cell,
       bool_and(kingres.timepart_cell_start(every, tz, k + 1) > kingres.timepart_cell_start(every, tz, k)) AS increasing
  FROM ks
 GROUP BY every, tz
 ORDER BY every, tz;

-- A day across the spring change in Berlin is 23 hours long and the next
-- cell starts where it ends; across the autumn change it is 25 hours.
SELECT k, kingres.timepart_cell_start('1 day', 'Europe/Berlin', k) AS starts,
       kingres.timepart_cell_start('1 day', 'Europe/Berlin', k + 1)
       - kingres.timepart_cell_start('1 day', 'Europe/Berlin', k) AS length
  FROM generate_series(kingres.timepart_cell_of('1 day', 'Europe/Berlin', timestamptz '2026-03-28 12:00 UTC'),
                       kingres.timepart_cell_of('1 day', 'Europe/Berlin', timestamptz '2026-03-30 12:00 UTC')) k;
SELECT k, kingres.timepart_cell_start('1 day', 'Europe/Berlin', k) AS starts,
       kingres.timepart_cell_start('1 day', 'Europe/Berlin', k + 1)
       - kingres.timepart_cell_start('1 day', 'Europe/Berlin', k) AS length
  FROM generate_series(kingres.timepart_cell_of('1 day', 'Europe/Berlin', timestamptz '2026-10-24 12:00 UTC'),
                       kingres.timepart_cell_of('1 day', 'Europe/Berlin', timestamptz '2026-10-26 12:00 UTC')) k;

-- Hours are absolute: six-hour cells over the spring change are all six
-- hours long, even though the local clock skips an hour.
SELECT count(*) AS cells,
       count(DISTINCT kingres.timepart_cell_start('6 hours', 'Europe/Berlin', k + 1)
                      - kingres.timepart_cell_start('6 hours', 'Europe/Berlin', k)) AS distinct_lengths,
       min(kingres.timepart_cell_start('6 hours', 'Europe/Berlin', k + 1)
           - kingres.timepart_cell_start('6 hours', 'Europe/Berlin', k)) AS length
  FROM generate_series(kingres.timepart_cell_of('6 hours', 'Europe/Berlin', timestamptz '2026-03-27 00:00 UTC'),
                       kingres.timepart_cell_of('6 hours', 'Europe/Berlin', timestamptz '2026-03-31 00:00 UTC')) k;

-- A zone whose change happens at midnight: the local midnight of the day of
-- the change does not exist; the cell starts at the first instant after it.
SELECT k, kingres.timepart_cell_start('1 day', 'America/Havana', k) AS starts
  FROM generate_series(kingres.timepart_cell_of('1 day', 'America/Havana', timestamptz '2026-03-07 12:00 UTC'),
                       kingres.timepart_cell_of('1 day', 'America/Havana', timestamptz '2026-03-09 12:00 UTC')) k;

-- Weeks start on Monday; N days count from 2000-01-01; months and years on
-- the first.
SELECT extract(isodow FROM kingres.timepart_cell_start('1 week', 'UTC', k)) AS weekday,
       count(*)
  FROM generate_series(1000, 1100) k GROUP BY 1;
SELECT kingres.timepart_cell_start('1 week', 'UTC', kingres.timepart_cell_of('1 week', 'UTC', timestamptz '2026-03-04 10:00 UTC')) AS week_of_wednesday;
SELECT kingres.timepart_cell_start('10 days', 'UTC', 0) AS ten_days_origin,
       kingres.timepart_cell_start('10 days', 'UTC', 1) AS ten_days_next;

-- Leap years and month ends.
SELECT k, kingres.timepart_cell_start('1 month', 'UTC', k) AS starts,
       kingres.timepart_cell_start('1 month', 'UTC', k + 1)
       - kingres.timepart_cell_start('1 month', 'UTC', k) AS length
  FROM generate_series(kingres.timepart_cell_of('1 month', 'UTC', timestamptz '2024-02-10 UTC'),
                       kingres.timepart_cell_of('1 month', 'UTC', timestamptz '2024-02-10 UTC')) k;
SELECT kingres.timepart_cell_start('1 month', 'UTC', kingres.timepart_cell_of('1 month', 'UTC', timestamptz '2023-02-28 23:59:59.999999 UTC') + 1) AS after_feb_2023,
       kingres.timepart_cell_start('1 month', 'UTC', kingres.timepart_cell_of('1 month', 'UTC', timestamptz '2024-02-29 23:59:59.999999 UTC') + 1) AS after_feb_2024,
       kingres.timepart_cell_start('1 month', 'UTC', kingres.timepart_cell_of('1 month', 'UTC', timestamptz '2026-01-31 23:59:59.999999 UTC') + 1) AS after_jan_2026,
       kingres.timepart_cell_start('1 year', 'UTC', kingres.timepart_cell_of('1 year', 'UTC', timestamptz '2024-12-31 23:59:59.999999 UTC') + 1) AS after_2024,
       kingres.timepart_cell_start('3 months', 'UTC', kingres.timepart_cell_of('3 months', 'UTC', timestamptz '2026-05-17 UTC')) AS quarter_of_may,
       kingres.timepart_cell_start('5 years', 'UTC', kingres.timepart_cell_of('5 years', 'UTC', timestamptz '2026-05-17 UTC')) AS five_years_of_2026;
-- Before the origin the grid continues backwards.
SELECT kingres.timepart_cell_start('1 month', 'UTC', kingres.timepart_cell_of('1 month', 'UTC', timestamptz '1999-12-15 UTC')) AS before_origin,
       kingres.timepart_cell_of('1 day', 'UTC', timestamptz '1999-12-31 23:59:59 UTC') AS day_before_origin;
SELECT kingres.timepart_cell_of('1 day', 'UTC', 'infinity');

-- Names: the suffix by unit, in the zone (hours in UTC).
SELECT kingres.timepart_part_name('events', '1 hour', 'Europe/Berlin', timestamptz '2026-03-29 00:00 UTC') AS hour,
       kingres.timepart_part_name('events', '1 day', 'Europe/Berlin', timestamptz '2026-03-28 23:00 UTC') AS day,
       kingres.timepart_part_name('events', '1 week', 'UTC', timestamptz '2026-03-02 00:00 UTC') AS week,
       kingres.timepart_part_name('events', '1 month', 'UTC', timestamptz '2026-03-01 00:00 UTC') AS month,
       kingres.timepart_part_name('events', '1 year', 'UTC', timestamptz '2026-01-01 00:00 UTC') AS year;
-- Two hour cells that have the same local hour (the autumn change) have
-- different names, because the hour is named in UTC.
SELECT kingres.timepart_part_name('t', '1 hour', 'Europe/Berlin', timestamptz '2026-10-25 00:00 UTC') AS first_two,
       kingres.timepart_part_name('t', '1 hour', 'Europe/Berlin', timestamptz '2026-10-25 01:00 UTC') AS second_two;
-- A long table name is cut so the whole name is 63 bytes.
SELECT length(n) AS chars, octet_length(n) AS bytes, n
  FROM (SELECT kingres.timepart_part_name(repeat('a', 63)::name, '1 day', 'UTC', timestamptz '2026-03-01 UTC')::text AS n) s;
SELECT length(n) AS chars, octet_length(n) AS bytes, n
  FROM (SELECT kingres.timepart_part_name(repeat('é', 31)::name, '1 hour', 'UTC', timestamptz '2026-03-01 UTC')::text AS n) s;
