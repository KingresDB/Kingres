-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
--
-- Per-database part of the integration test fixture: the kingres catalog of
-- kingres/docs/design/shard-mvp.md and ledger-mvp.md, written from the
-- specifications as plain tables (the extensions are not needed to test the
-- read side), plus user tables and the task table of the system schema.
--
-- Run in database kingres_cp with the psql variables :p1 and :p2 set to the
-- ports of the primary and the standby instance.

CREATE SCHEMA kingres;

CREATE TABLE kingres.node (
  node_id            int PRIMARY KEY,
  group_id           int NOT NULL,
  host               text NOT NULL,
  port               int  NOT NULL,
  role               text NOT NULL CHECK (role IN ('primary', 'standby')),
  is_active          bool NOT NULL DEFAULT true,
  should_have_shards bool NOT NULL DEFAULT true,
  server_group       text,
  UNIQUE (host, port)
);

CREATE TABLE kingres.colocation (
  colocation_id    int PRIMARY KEY,
  bucket_count     int NOT NULL CHECK (bucket_count BETWEEN 1 AND 4096),
  dist_column_type regtype NOT NULL,
  dist_collation   oid NOT NULL DEFAULT 0
);

CREATE TABLE kingres.dist_table (
  relid         regclass PRIMARY KEY,
  method        text NOT NULL CHECK (method IN ('hash')),
  dist_column   name NOT NULL,
  colocation_id int NOT NULL REFERENCES kingres.colocation
);

CREATE TABLE kingres.bucket_map (
  colocation_id int NOT NULL REFERENCES kingres.colocation,
  bucket        int NOT NULL,
  group_id      int NOT NULL,
  state         text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'moving', 'orphan')),
  PRIMARY KEY (colocation_id, bucket)
);

CREATE TABLE kingres.map_version (version bigint NOT NULL);
CREATE TABLE kingres.local_node (node_id int NOT NULL);

CREATE TABLE kingres.ledger_table (
  table_id   int GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  relid      regclass NOT NULL UNIQUE,
  mode       text NOT NULL CHECK (mode IN ('append_only')),
  enabled_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE kingres.ledger_blocks (
  height         bigint PRIMARY KEY,
  hash_version   smallint NOT NULL,
  prev_hash      bytea NOT NULL,
  tx_merkle_root bytea NOT NULL,
  tx_count       int   NOT NULL,
  closed_at      timestamptz NOT NULL,
  block_hash     bytea NOT NULL
);

-- The task table of the system schema (shape of sysdb migration 1, columns
-- that the read side uses).
CREATE SCHEMA kingres_cp;
GRANT USAGE ON SCHEMA kingres_cp TO cp_service;
CREATE TABLE kingres_cp.task (
  id           bigint PRIMARY KEY,
  kind         text NOT NULL,
  state        text NOT NULL,
  subject_type text NOT NULL,
  subject_id   text NOT NULL,
  progress     jsonb NOT NULL DEFAULT '{"percent": 0, "steps": []}'::jsonb,
  error        jsonb,
  created_at   timestamptz NOT NULL,
  updated_at   timestamptz NOT NULL,
  created_by   text NOT NULL
);

GRANT USAGE ON SCHEMA kingres TO PUBLIC;
GRANT SELECT ON ALL TABLES IN SCHEMA kingres TO PUBLIC;

-- User tables.
CREATE TABLE public.orders (id bigint NOT NULL, customer bigint, amount numeric, PRIMARY KEY (id)) PARTITION BY HASH (id);
CREATE TABLE public.orders_b0000 PARTITION OF public.orders FOR VALUES WITH (MODULUS 2, REMAINDER 0);
CREATE TABLE public.orders_b0001 PARTITION OF public.orders FOR VALUES WITH (MODULUS 2, REMAINDER 1);
CREATE TABLE public.customers (id bigint PRIMARY KEY, name text);
CREATE TABLE public.events (id bigint PRIMARY KEY, note text) PARTITION BY HASH (id);
CREATE TABLE public.events_p0 PARTITION OF public.events FOR VALUES WITH (MODULUS 2, REMAINDER 0);
CREATE TABLE public.events_p1 PARTITION OF public.events FOR VALUES WITH (MODULUS 2, REMAINDER 1);
CREATE TABLE public.docs (id int PRIMARY KEY, loc point, body text);
CREATE TABLE public.settings (k text PRIMARY KEY, v text);
CREATE TABLE public.audit_log (id bigint PRIMARY KEY, note text);
CREATE SCHEMA shop;
CREATE TABLE shop.items (id int PRIMARY KEY, label text);
INSERT INTO public.settings VALUES ('a', '1'), ('b', '2');
ANALYZE public.settings;
INSERT INTO public.events SELECT g, repeat('x', 100) FROM generate_series(1, 50) g;
ANALYZE public.events;

-- Node layout: map group 0 = real primary (node 1) + real standby (node 2),
-- group 1 = unreachable primary (node 3) and an inactive standby (node 4),
-- group 2 = a primary that is being drained (node 5).
INSERT INTO kingres.node VALUES
  (1, 0, '127.0.0.1', :p1, 'primary', true,  true,  'rack-a'),
  (2, 0, 'localhost', :p2, 'standby', true,  true,  'rack-b'),
  (3, 1, '127.0.0.3', 5999, 'primary', true,  true,  'rack-b'),
  (4, 1, '127.0.0.4', 5999, 'standby', false, true,  NULL),
  (5, 2, '127.0.0.5', 5999, 'primary', true,  false, 'rack-b');
INSERT INTO kingres.local_node VALUES (1);
INSERT INTO kingres.map_version VALUES (7);

INSERT INTO kingres.colocation VALUES (1, 8, 'bigint', 0), (2, 4, 'text', 100);
INSERT INTO kingres.dist_table VALUES ('public.orders', 'hash', 'id', 1), ('public.customers', 'hash', 'id', 1);
INSERT INTO kingres.bucket_map
  SELECT 1, b, CASE WHEN b IN (6, 7) THEN 1 ELSE 0 END,
         CASE WHEN b = 6 THEN 'moving' WHEN b = 7 THEN 'orphan' ELSE 'active' END
  FROM generate_series(0, 7) b;
INSERT INTO kingres.bucket_map SELECT 2, b, 0, 'active' FROM generate_series(0, 3) b;

INSERT INTO kingres.ledger_table (relid, mode) VALUES ('public.audit_log', 'append_only');
INSERT INTO kingres.ledger_blocks VALUES
  (0, 1, decode(repeat('00', 32), 'hex'), decode(repeat('aa', 32), 'hex'), 2, '2026-11-14 08:00:00+00', decode(repeat('b0', 32), 'hex')),
  (1, 1, decode(repeat('b0', 32), 'hex'), decode(repeat('ab', 32), 'hex'), 5, '2026-11-14 09:00:00+00', decode(repeat('b1', 32), 'hex')),
  (2, 1, decode(repeat('b1', 32), 'hex'), decode(repeat('ac', 32), 'hex'), 1, '2026-11-14 10:00:00+00', decode(repeat('b2', 32), 'hex'));

INSERT INTO kingres_cp.task (id, kind, state, subject_type, subject_id, progress, error, created_at, updated_at, created_by) VALUES
  (1, 'database.create', 'done', 'database', 'shop',
   '{"percent":100,"currentStep":null,"steps":[{"name":"Create database","state":"done","detail":null,"itemsDone":1,"itemsTotal":1,"startedAt":"2026-11-14T09:58:00Z","finishedAt":"2026-11-14T09:58:02Z"}]}',
   NULL, '2026-11-14 09:58:00+00', '2026-11-14 09:58:02+00', 'cp_admin'),
  (2, 'rebalance', 'running', 'rebalance', '1', '{"percent":10,"steps":[]}', NULL,
   '2026-11-14 10:00:00+00', '2026-11-14 10:01:00+00', 'cp_admin'),
  (3, 'server.add', 'failed', 'server', '9', DEFAULT,
   '{"code":"step_failed","message":"cannot reach 10.0.0.9","step":"connect","retryable":true}',
   '2026-11-14 10:05:00+00', '2026-11-14 10:06:00+00', 'cp_admin');

-- Grants for the grant listing.
GRANT CONNECT ON DATABASE kingres_cp TO cp_ro;
GRANT USAGE ON SCHEMA public TO cp_ro;
GRANT SELECT, INSERT ON public.orders TO cp_ro;
GRANT SELECT ON public.settings TO cp_ro;

GRANT SELECT ON ALL TABLES IN SCHEMA kingres_cp TO cp_service;

-- The catalog is made of plain tables, written first (the guard of the ledger library refuses to write the tables of the extension), and the control plane reads the schema kingres only when an extension owned by a superuser has every object in
-- it (extguard).  The fixture records the two extensions and makes the tables members of them, as CREATE EXTENSION would have.
INSERT INTO pg_catalog.pg_extension (oid, extname, extowner, extnamespace, extrelocatable, extversion)
SELECT pg_catalog.pg_nextoid('pg_catalog.pg_extension'::pg_catalog.regclass, 'oid', 'pg_catalog.pg_extension_oid_index'::pg_catalog.regclass),
       v.n, (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user), 'kingres'::pg_catalog.regnamespace, false, v.v
  FROM (VALUES ('kingres_shard', '0.9'), ('kingres_ledger', '0.3')) AS v(n, v);
-- The membership is a dependency row of type e, written directly: ALTER EXTENSION ... ADD is refused for kingres_ledger by the guard of its library, which a
-- server that preloads it runs.
INSERT INTO pg_catalog.pg_depend (classid, objid, objsubid, refclassid, refobjid, refobjsubid, deptype)
SELECT 'pg_catalog.pg_class'::pg_catalog.regclass, c.oid, 0, 'pg_catalog.pg_extension'::pg_catalog.regclass, e.oid, 0, 'e'
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_extension e ON e.extname = CASE WHEN c.relname LIKE 'ledger\_%' THEN 'kingres_ledger' ELSE 'kingres_shard' END
 WHERE c.relnamespace = 'kingres'::pg_catalog.regnamespace AND c.relkind IN ('r', 'v');
