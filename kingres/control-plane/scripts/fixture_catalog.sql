-- SPDX-License-Identifier: PostgreSQL
--
-- Per-database part of the integration test fixture: the kingres catalog of
-- kingres/docs/design/shard-mvp.md and ledger-mvp.md, written from the
-- specifications as plain tables (the extensions are not needed to test the
-- read side), plus user tables and the assumed kingres.task table.
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

-- Assumed shape of the task table (see README, "Assumptions about the catalog").
CREATE TABLE kingres.task (
  task_id     bigint PRIMARY KEY,
  kind        text NOT NULL,
  state       text NOT NULL,
  progress    jsonb,
  error       jsonb,
  subject     jsonb,
  cancellable bool NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL,
  updated_at  timestamptz NOT NULL,
  created_by  text NOT NULL
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

INSERT INTO kingres.task VALUES
  (1, 'database.create', 'done',
   '{"percent":100,"currentStep":null,"steps":[{"name":"Create database","state":"done","detail":null,"itemsDone":1,"itemsTotal":1,"startedAt":"2026-11-14T09:58:00Z","finishedAt":"2026-11-14T09:58:02Z"}]}',
   NULL, '{"type":"database","id":"shop"}', false, '2026-11-14 09:58:00+00', '2026-11-14 09:58:02+00', 'cp_admin'),
  (2, 'rebalance', 'running', '{"percent":10,"steps":[]}', NULL, '{"type":"rebalance","id":"1"}', true,
   '2026-11-14 10:00:00+00', '2026-11-14 10:01:00+00', 'cp_admin'),
  (3, 'server.add', 'failed', NULL, '{"code":"step_failed","message":"cannot reach 10.0.0.9","step":"connect","retryable":true}',
   '{"type":"server","id":"9"}', false, '2026-11-14 10:05:00+00', '2026-11-14 10:06:00+00', 'cp_admin');

-- Grants for the grant listing.
GRANT CONNECT ON DATABASE kingres_cp TO cp_ro;
GRANT USAGE ON SCHEMA public TO cp_ro;
GRANT SELECT, INSERT ON public.orders TO cp_ro;
GRANT SELECT ON public.settings TO cp_ro;
